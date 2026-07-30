package cwsource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

var fixedNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// fakeCW serves canned CloudWatch responses. storageTypes drives ListMetrics;
// values maps a StorageType (or "NumberOfObjects") to its most recent datapoint.
type fakeCW struct {
	storageTypes []string
	values       map[string]float64
	timestamps   map[string]time.Time
	listErr      error
	dataErr      error

	lastListInput *cloudwatch.ListMetricsInput
	lastInput     *cloudwatch.GetMetricDataInput
}

func (f *fakeCW) ListMetrics(_ context.Context, in *cloudwatch.ListMetricsInput,
	_ ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.lastListInput = in

	// Serve only the query the collector is supposed to send. A wrong namespace,
	// metric name, or dimension filter yields nothing — so a typo in discover()
	// surfaces as a no-metrics failure instead of passing silently.
	if aws.ToString(in.Namespace) != "AWS/S3" || aws.ToString(in.MetricName) != "BucketSizeBytes" {
		return &cloudwatch.ListMetricsOutput{}, nil
	}
	if len(in.Dimensions) != 1 || aws.ToString(in.Dimensions[0].Name) != "BucketName" {
		return &cloudwatch.ListMetricsOutput{}, nil
	}

	out := &cloudwatch.ListMetricsOutput{}
	for _, st := range f.storageTypes {
		out.Metrics = append(out.Metrics, cwtypes.Metric{
			Namespace:  aws.String("AWS/S3"),
			MetricName: aws.String("BucketSizeBytes"),
			Dimensions: []cwtypes.Dimension{
				{Name: aws.String("BucketName"), Value: aws.String("b")},
				{Name: aws.String("StorageType"), Value: aws.String(st)},
			},
		})
	}
	return out, nil
}

func (f *fakeCW) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput,
	_ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	if f.dataErr != nil {
		return nil, f.dataErr
	}
	f.lastInput = in

	out := &cloudwatch.GetMetricDataOutput{}
	for _, q := range in.MetricDataQueries {
		key := storageTypeOf(q)
		v, ok := f.values[key]
		if !ok {
			// No datapoint for this query — CloudWatch returns the result with
			// empty Values rather than omitting it.
			out.MetricDataResults = append(out.MetricDataResults,
				cwtypes.MetricDataResult{Id: q.Id})
			continue
		}
		ts := fixedNow.Add(-24 * time.Hour)
		if t, ok := f.timestamps[key]; ok {
			ts = t
		}
		out.MetricDataResults = append(out.MetricDataResults, cwtypes.MetricDataResult{
			Id:         q.Id,
			Timestamps: []time.Time{ts},
			Values:     []float64{v},
		})
	}
	return out, nil
}

// storageTypeOf resolves which metric a query is asking for. It returns "" for
// anything that is not a query the collector should be sending, so a wrong
// namespace or metric name produces no datapoint rather than a silent pass.
func storageTypeOf(q cwtypes.MetricDataQuery) string {
	if q.MetricStat == nil || q.MetricStat.Metric == nil {
		return ""
	}
	m := q.MetricStat.Metric
	if aws.ToString(m.Namespace) != "AWS/S3" {
		return ""
	}
	switch aws.ToString(m.MetricName) {
	case "NumberOfObjects":
		return "NumberOfObjects"
	case "BucketSizeBytes":
		for _, d := range m.Dimensions {
			if aws.ToString(d.Name) == "StorageType" {
				return aws.ToString(d.Value)
			}
		}
	}
	return ""
}

func collect(t *testing.T, f *fakeCW) (*metrics.Report, error) {
	t.Helper()
	c := New(f, "us-east-1")
	c.SetClock(func() time.Time { return fixedNow })
	return c.Collect(context.Background(), "b")
}

func TestCollectBuildsReport(t *testing.T) {
	f := &fakeCW{
		storageTypes: []string{"StandardStorage", "GlacierStorage", "GlacierObjectOverhead"},
		values: map[string]float64{
			"StandardStorage":       1000,
			"GlacierStorage":        500,
			"GlacierObjectOverhead": 32,
			"NumberOfObjects":       42,
		},
	}
	r, err := collect(t, f)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if r.Source != metrics.SourceCloudWatch {
		t.Errorf("Source = %q, want %q", r.Source, metrics.SourceCloudWatch)
	}
	if r.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", r.Region)
	}
	if r.ObjectCount != 42 {
		t.Errorf("ObjectCount = %d, want 42", r.ObjectCount)
	}
	if r.TotalSizeBytes != 1500 {
		t.Errorf("TotalSizeBytes = %d, want 1500 (overhead excluded)", r.TotalSizeBytes)
	}
	if len(r.StorageClasses) != 3 {
		t.Fatalf("len(StorageClasses) = %d, want 3 — overhead rows must remain visible", len(r.StorageClasses))
	}
}

func TestCollectLeavesPerClassObjectCountNil(t *testing.T) {
	f := &fakeCW{
		storageTypes: []string{"StandardStorage"},
		values:       map[string]float64{"StandardStorage": 1000, "NumberOfObjects": 7},
	}
	r, err := collect(t, f)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, sc := range r.StorageClasses {
		if sc.ObjectCount != nil {
			t.Errorf("%s.ObjectCount = %d, want nil — CloudWatch has no per-class count",
				sc.Class, *sc.ObjectCount)
		}
	}
}

func TestCollectMarksOverheadRows(t *testing.T) {
	f := &fakeCW{
		storageTypes: []string{"GlacierStorage", "GlacierObjectOverhead"},
		values: map[string]float64{
			"GlacierStorage": 500, "GlacierObjectOverhead": 32, "NumberOfObjects": 1,
		},
	}
	r, err := collect(t, f)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	byClass := map[string]metrics.StorageClassStat{}
	for _, sc := range r.StorageClasses {
		byClass[sc.Class] = sc
	}
	if !byClass["GLACIER_OBJECT_OVERHEAD"].Overhead {
		t.Error("GLACIER_OBJECT_OVERHEAD.Overhead = false, want true")
	}
	if byClass["GLACIER"].Overhead {
		t.Error("GLACIER.Overhead = true, want false")
	}
}

func TestCollectAsOfIsNewestDatapointNotNow(t *testing.T) {
	stale := fixedNow.Add(-40 * time.Hour)
	fresher := fixedNow.Add(-20 * time.Hour)

	f := &fakeCW{
		storageTypes: []string{"StandardStorage", "GlacierStorage"},
		values: map[string]float64{
			"StandardStorage": 1, "GlacierStorage": 1, "NumberOfObjects": 1,
		},
		timestamps: map[string]time.Time{
			"StandardStorage": stale,
			"GlacierStorage":  fresher,
			"NumberOfObjects": stale,
		},
	}
	r, err := collect(t, f)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !r.AsOf.Equal(fresher) {
		t.Errorf("AsOf = %v, want the newest datapoint %v (never time.Now)", r.AsOf, fresher)
	}
}

func TestCollectUnknownStorageTypePassesThrough(t *testing.T) {
	f := &fakeCW{
		storageTypes: []string{"SomeNewStorage"},
		values:       map[string]float64{"SomeNewStorage": 99, "NumberOfObjects": 1},
	}
	r, err := collect(t, f)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(r.StorageClasses) != 1 || r.StorageClasses[0].Class != "SomeNewStorage" {
		t.Fatalf("StorageClasses = %+v, want one row named SomeNewStorage", r.StorageClasses)
	}
	if r.TotalSizeBytes != 99 {
		t.Errorf("TotalSizeBytes = %d, want 99 — an unknown class counts as real data", r.TotalSizeBytes)
	}
}

func TestCollectNoMetricsWhenListMetricsIsEmpty(t *testing.T) {
	_, err := collect(t, &fakeCW{})

	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeNoMetrics {
		t.Fatalf("Collect() error = %v, want code %s", err, errs.CodeNoMetrics)
	}
}

func TestCollectNoMetricsWhenNoDatapoints(t *testing.T) {
	f := &fakeCW{
		storageTypes: []string{"StandardStorage"},
		values:       map[string]float64{}, // metric exists, but no datapoint in window
	}
	_, err := collect(t, f)

	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeNoMetrics {
		t.Fatalf("Collect() error = %v, want code %s", err, errs.CodeNoMetrics)
	}
}

func TestCollectQueryWindowIs72Hours(t *testing.T) {
	f := &fakeCW{
		storageTypes: []string{"StandardStorage"},
		values:       map[string]float64{"StandardStorage": 1, "NumberOfObjects": 1},
	}
	if _, err := collect(t, f); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	in := f.lastInput
	if in == nil {
		t.Fatal("GetMetricData was never called")
	}
	if !aws.ToTime(in.EndTime).Equal(fixedNow) {
		t.Errorf("EndTime = %v, want %v", aws.ToTime(in.EndTime), fixedNow)
	}
	if want := fixedNow.Add(-72 * time.Hour); !aws.ToTime(in.StartTime).Equal(want) {
		t.Errorf("StartTime = %v, want %v", aws.ToTime(in.StartTime), want)
	}
	if in.ScanBy != cwtypes.ScanByTimestampDescending {
		t.Errorf("ScanBy = %v, want TimestampDescending", in.ScanBy)
	}
}

func TestCollectQueriesTheRightMetrics(t *testing.T) {
	f := &fakeCW{
		storageTypes: []string{"StandardStorage"},
		values:       map[string]float64{"StandardStorage": 1, "NumberOfObjects": 1},
	}
	if _, err := collect(t, f); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if f.lastListInput == nil {
		t.Fatal("ListMetrics was never called")
	}
	if got := aws.ToString(f.lastListInput.Namespace); got != "AWS/S3" {
		t.Errorf("ListMetrics Namespace = %q, want AWS/S3", got)
	}
	if got := aws.ToString(f.lastListInput.MetricName); got != "BucketSizeBytes" {
		t.Errorf("ListMetrics MetricName = %q, want BucketSizeBytes", got)
	}
	if len(f.lastListInput.Dimensions) != 1 ||
		aws.ToString(f.lastListInput.Dimensions[0].Name) != "BucketName" ||
		aws.ToString(f.lastListInput.Dimensions[0].Value) != "b" {
		t.Errorf("ListMetrics Dimensions = %+v, want one BucketName=b filter", f.lastListInput.Dimensions)
	}

	var sawSize, sawCount bool
	for _, q := range f.lastInput.MetricDataQueries {
		m := q.MetricStat.Metric
		if got := aws.ToString(m.Namespace); got != "AWS/S3" {
			t.Errorf("query %s Namespace = %q, want AWS/S3", aws.ToString(q.Id), got)
		}
		switch aws.ToString(m.MetricName) {
		case "BucketSizeBytes":
			sawSize = true
		case "NumberOfObjects":
			sawCount = true
			var storageType string
			for _, d := range m.Dimensions {
				if aws.ToString(d.Name) == "StorageType" {
					storageType = aws.ToString(d.Value)
				}
			}
			// NumberOfObjects publishes only for AllStorageTypes; any other
			// dimension value would silently return nothing.
			if storageType != "AllStorageTypes" {
				t.Errorf("NumberOfObjects StorageType = %q, want AllStorageTypes", storageType)
			}
		default:
			t.Errorf("unexpected MetricName %q", aws.ToString(m.MetricName))
		}
	}
	if !sawSize || !sawCount {
		t.Errorf("queries: sawSize=%v sawCount=%v, want both", sawSize, sawCount)
	}
}

func TestCollectClassifiesListMetricsError(t *testing.T) {
	f := &fakeCW{listErr: errors.New("operation error: AccessDeniedException")}
	_, err := collect(t, f)
	if err == nil {
		t.Fatal("Collect() error = nil, want an error")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("Collect() error = %T, want *errs.Error", err)
	}
}
