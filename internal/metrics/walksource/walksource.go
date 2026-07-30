package walksource

import (
	"context"
	"time"

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

	// IncludeVersions swaps ListObjectsV2 for ListObjectVersions, so the walk
	// counts noncurrent versions and delete markers as well as current objects.
	// That is what CloudWatch counts, and on a versioned bucket it is the only
	// way the two modes can be reconciled.
	IncludeVersions bool
}

// Collector computes bucket metrics by listing every object.
type Collector struct {
	src    objectSource
	region string
	opts   Options
	now    func() time.Time
}

// New returns a Collector walking through api.
//
// The source is chosen once, here, and both discovery and the walk are handed
// the same one — see objectSource for why that matters.
func New(api BucketAPI, region string, opts Options) *Collector {
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}

	var src objectSource = currentSource{api: api}
	if opts.IncludeVersions {
		src = versionSource{api: api}
	}
	return &Collector{src: src, region: region, opts: opts, now: time.Now}
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

	d, err := discoverShards(ctx, c.src, bucket, c.opts.Prefix, c.opts.Concurrency, c.opts.Progress)
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

	// Only a version-aware walk may report these. Without --include-versions the
	// listing cannot see a noncurrent version or a delete marker at all, and
	// "none found" would be a claim this run never tested — so they stay null.
	if c.opts.IncludeVersions {
		markers, noncurrent := total.DeleteMarkers, total.NoncurrentVersions
		report.DeleteMarkers = &markers
		report.NoncurrentVersions = &noncurrent
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
				agg, err := c.src.walkPrefix(ctx, bucket, prefix, c.opts.Progress)
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
