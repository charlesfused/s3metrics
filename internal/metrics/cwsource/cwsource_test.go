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

	lastInput *cloudwatch.GetMetricDataInput
}

func (f *fakeCW) ListMetrics(_ context.Context, in *cloudwatch.ListMetricsInput,
	_ ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
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

func storageTypeOf(q cwtypes.MetricDataQuery) string {
	if q.MetricStat == nil || q.MetricStat.Metric == nil {
		return ""
	}
	if aws.ToString(q.MetricStat.Metric.MetricName) == "NumberOfObjects" {
		return "NumberOfObjects"
	}
	for _, d := range q.MetricStat.Metric.Dimensions {
		if aws.ToString(d.Name) == "StorageType" {
			return aws.ToString(d.Value)
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
