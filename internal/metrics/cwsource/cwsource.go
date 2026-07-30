package cwsource

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

const (
	namespace = "AWS/S3"

	// lookback covers the worst case comfortably. These metrics publish once a
	// day, some hours after the period they describe, so a 48h-stale reading is
	// normal; 72h leaves margin without widening the window enough to matter.
	lookback = 72 * time.Hour

	// period matches the metrics' own daily resolution.
	period = int32(86400)

	// stat is what AWS documents for these gauges.
	stat = "Average"

	// allStorageTypes is the only StorageType NumberOfObjects publishes for,
	// which is why there is no per-class object count in this mode.
	allStorageTypes = "AllStorageTypes"
)

// API is the slice of the CloudWatch client this collector needs.
type API interface {
	ListMetrics(ctx context.Context, in *cloudwatch.ListMetricsInput,
		opts ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error)
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput,
		opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// Collector reads a bucket's daily storage metrics from CloudWatch.
type Collector struct {
	api    API
	region string
	now    func() time.Time
}

// New returns a Collector reading metrics from api. region is recorded on the
// report; the client passed in must already be pinned to it.
func New(api API, region string) *Collector {
	return &Collector{api: api, region: region, now: time.Now}
}

// SetClock replaces the time source. Tests use it to make the query window
// deterministic.
func (c *Collector) SetClock(now func() time.Time) { c.now = now }

// queryPlan records which StorageType each generated query id refers to.
type queryPlan struct {
	queries []cwtypes.MetricDataQuery
	byID    map[string]string // query id -> StorageType, or allStorageTypes for the count
}

// Collect implements metrics.Collector.
func (c *Collector) Collect(ctx context.Context, bucket string) (*metrics.Report, error) {
	started := c.now()

	storageTypes, err := c.discover(ctx, bucket)
	if err != nil {
		return nil, errs.Classify(err, "cloudwatch:ListMetrics")
	}
	if len(storageTypes) == 0 {
		return nil, noMetricsErr(bucket)
	}

	plan := buildQueries(bucket, storageTypes)

	results, err := c.fetch(ctx, plan, started)
	if err != nil {
		return nil, errs.Classify(err, "cloudwatch:GetMetricData")
	}

	report := &metrics.Report{
		Bucket: bucket,
		Region: c.region,
		Source: metrics.SourceCloudWatch,
	}

	var newest time.Time
	var sawAny bool

	for id, dp := range results {
		storageType, ok := plan.byID[id]
		if !ok {
			continue
		}
		sawAny = true
		if dp.timestamp.After(newest) {
			newest = dp.timestamp
		}

		if storageType == allStorageTypes {
			report.ObjectCount = int64(dp.value)
			continue
		}
		report.StorageClasses = append(report.StorageClasses, metrics.StorageClassStat{
			Class:     ClassName(storageType),
			SizeBytes: int64(dp.value),
			// Deliberately nil: NumberOfObjects has no per-StorageType dimension,
			// so a per-class count does not exist to report.
			ObjectCount: nil,
			Overhead:    IsOverhead(storageType),
		})
	}

	if !sawAny {
		return nil, noMetricsErr(bucket)
	}

	report.AsOf = newest
	report.Recompute()
	report.DurationMS = c.now().Sub(started).Milliseconds()
	return report, nil
}

// discover lists which BucketSizeBytes StorageType dimensions actually exist for
// this bucket, so the query set matches reality instead of a hardcoded guess.
func (c *Collector) discover(ctx context.Context, bucket string) ([]string, error) {
	var (
		storageTypes []string
		seen         = map[string]bool{}
		nextToken    *string
	)
	for {
		out, err := c.api.ListMetrics(ctx, &cloudwatch.ListMetricsInput{
			Namespace:  aws.String(namespace),
			MetricName: aws.String("BucketSizeBytes"),
			Dimensions: []cwtypes.DimensionFilter{
				{Name: aws.String("BucketName"), Value: aws.String(bucket)},
			},
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, m := range out.Metrics {
			for _, d := range m.Dimensions {
				if aws.ToString(d.Name) != "StorageType" {
					continue
				}
				if st := aws.ToString(d.Value); st != "" && !seen[st] {
					seen[st] = true
					storageTypes = append(storageTypes, st)
				}
			}
		}
		if out.NextToken == nil || aws.ToString(out.NextToken) == "" {
			return storageTypes, nil
		}
		nextToken = out.NextToken
	}
}

// buildQueries produces one query per storage type plus the object-count query.
// Query ids must start with a lowercase letter, hence the m/ n prefixes.
func buildQueries(bucket string, storageTypes []string) queryPlan {
	plan := queryPlan{byID: map[string]string{}}

	for i, st := range storageTypes {
		id := fmt.Sprintf("m%d", i)
		plan.byID[id] = st
		plan.queries = append(plan.queries, sizeQuery(id, bucket, st))
	}

	const countID = "n0"
	plan.byID[countID] = allStorageTypes
	plan.queries = append(plan.queries, countQuery(countID, bucket))

	return plan
}

func sizeQuery(id, bucket, storageType string) cwtypes.MetricDataQuery {
	return cwtypes.MetricDataQuery{
		Id: aws.String(id),
		MetricStat: &cwtypes.MetricStat{
			Metric: &cwtypes.Metric{
				Namespace:  aws.String(namespace),
				MetricName: aws.String("BucketSizeBytes"),
				Dimensions: []cwtypes.Dimension{
					{Name: aws.String("BucketName"), Value: aws.String(bucket)},
					{Name: aws.String("StorageType"), Value: aws.String(storageType)},
				},
			},
			Period: aws.Int32(period),
			Stat:   aws.String(stat),
		},
	}
}

func countQuery(id, bucket string) cwtypes.MetricDataQuery {
	return cwtypes.MetricDataQuery{
		Id: aws.String(id),
		MetricStat: &cwtypes.MetricStat{
			Metric: &cwtypes.Metric{
				Namespace:  aws.String(namespace),
				MetricName: aws.String("NumberOfObjects"),
				Dimensions: []cwtypes.Dimension{
					{Name: aws.String("BucketName"), Value: aws.String(bucket)},
					{Name: aws.String("StorageType"), Value: aws.String(allStorageTypes)},
				},
			},
			Period: aws.Int32(period),
			Stat:   aws.String(stat),
		},
	}
}

type datapoint struct {
	value     float64
	timestamp time.Time
}

// fetch runs the query plan and keeps the most recent datapoint per query.
// ScanByTimestampDescending means index 0 is the newest, so the first value wins.
func (c *Collector) fetch(ctx context.Context, plan queryPlan, end time.Time) (map[string]datapoint, error) {
	out := map[string]datapoint{}
	var nextToken *string

	for {
		resp, err := c.api.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
			MetricDataQueries: plan.queries,
			StartTime:         aws.Time(end.Add(-lookback)),
			EndTime:           aws.Time(end),
			ScanBy:            cwtypes.ScanByTimestampDescending,
			NextToken:         nextToken,
		})
		if err != nil {
			return nil, err
		}

		for _, r := range resp.MetricDataResults {
			id := aws.ToString(r.Id)
			if id == "" || len(r.Values) == 0 || len(r.Timestamps) == 0 {
				continue
			}
			if _, exists := out[id]; exists {
				continue // a later page can only be older
			}
			out[id] = datapoint{value: r.Values[0], timestamp: r.Timestamps[0].UTC()}
		}

		if resp.NextToken == nil || aws.ToString(resp.NextToken) == "" {
			return out, nil
		}
		nextToken = resp.NextToken
	}
}

// noMetricsErr is its own category because an empty CloudWatch response is the
// most confusing outcome of the default mode. Reporting "no error, zero bytes"
// would be a lie: the bucket may well hold data that has simply not been
// published yet.
func noMetricsErr(bucket string) error {
	return errs.New(errs.CodeNoMetrics,
		"no CloudWatch storage metrics found for bucket "+bucket,
		"the bucket may be new or empty — these metrics publish once a day. "+
			"Check --region, or use --mode walk to measure the bucket directly")
}
