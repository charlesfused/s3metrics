package walksource

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
	"github.com/charlesfused/s3metrics/internal/progress"
)

// Options configure a walk.
type Options struct {
	Prefix      string
	Concurrency int
	Progress    *progress.Reporter
}

// Collector computes bucket metrics by listing every object.
type Collector struct {
	api    ListObjectsAPI
	region string
	opts   Options
	now    func() time.Time
}

// New returns a Collector walking through api.
func New(api ListObjectsAPI, region string, opts Options) *Collector {
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	return &Collector{api: api, region: region, opts: opts, now: time.Now}
}

// SetClock replaces the time source, making AsOf and DurationMS deterministic
// in tests.
func (c *Collector) SetClock(now func() time.Time) { c.now = now }

// Collect implements metrics.Collector.
//
// Shape of the work: discover disjoint prefixes, then walk them in parallel.
// Each worker accumulates into its own Aggregate and nothing is shared, so the
// merge afterwards needs no lock — a stronger guarantee than a mutex that
// happens to be correct today.
func (c *Collector) Collect(ctx context.Context, bucket string) (*metrics.Report, error) {
	started := c.now()

	// An already-dead context must fail here rather than somewhere downstream.
	// Without this the outcome depends on which branch a select happens to take
	// when both are ready, which is not a thing to leave to chance.
	if err := ctx.Err(); err != nil {
		return nil, errs.Classify(err, "s3:ListBucket")
	}

	d, err := discoverShards(ctx, c.api, bucket, c.opts.Prefix, c.opts.Concurrency, c.opts.Progress)
	if err != nil {
		return nil, errs.Classify(err, "s3:ListBucket")
	}

	total := d.Loose // objects already counted while planning

	if len(d.Shards) > 0 {
		walked, err := c.walkShards(ctx, bucket, d.Shards)
		if err != nil {
			return nil, errs.Classify(err, "s3:ListBucket")
		}
		total.Merge(walked)
	}

	report := &metrics.Report{
		Bucket:         bucket,
		Region:         c.region,
		Source:         metrics.SourceWalk,
		AsOf:           started,
		ObjectCount:    total.Objects,
		StorageClasses: total.StorageClasses(),
		Prefix:         c.opts.Prefix,
	}
	report.Recompute()
	report.DurationMS = c.now().Sub(started).Milliseconds()
	return report, nil
}

// walkShards fans the shard list out across workers.
//
// A single shard is not a special case: workers is min(concurrency, len(shards)),
// so a flat bucket walks through exactly the same code path with one worker.
func (c *Collector) walkShards(ctx context.Context, bucket string, shards []string) (Aggregate, error) {
	workers := c.opts.Concurrency
	if workers > len(shards) {
		workers = len(shards)
	}

	c.opts.Progress.SetShards(int64(len(shards)))

	g, ctx := errgroup.WithContext(ctx)
	ch := make(chan string)

	// Each worker owns results[i] exclusively — no other goroutine reads or
	// writes it until Wait returns, so there is nothing to race on.
	results := make([]Aggregate, workers)

	for i := 0; i < workers; i++ {
		i := i
		g.Go(func() error {
			for prefix := range ch {
				agg, err := c.walkPrefix(ctx, bucket, prefix)
				if err != nil {
					return err
				}
				results[i].Merge(agg)
				c.opts.Progress.ShardDone()
			}
			return nil
		})
	}

	g.Go(func() error {
		defer close(ch)
		for _, s := range shards {
			select {
			case ch <- s:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return Aggregate{}, err
	}

	var merged Aggregate
	for _, r := range results {
		merged.Merge(r)
	}
	return merged, nil
}

// walkPrefix lists every object under prefix. No delimiter here: this is the
// full recursive listing, unlike discovery's level-by-level walk.
func (c *Collector) walkPrefix(ctx context.Context, bucket, prefix string) (Aggregate, error) {
	var (
		agg   Aggregate
		token *string
	)
	for {
		out, err := c.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return Aggregate{}, err
		}

		for _, obj := range out.Contents {
			agg.Add(aws.ToInt64(obj.Size), storageClassOf(obj))
		}
		c.opts.Progress.AddObjects(int64(len(out.Contents)))

		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return agg, nil
		}
		token = out.NextContinuationToken
	}
}
