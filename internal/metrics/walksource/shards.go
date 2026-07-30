package walksource

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/charlesfused/s3metrics/internal/progress"
)

// maxShardDepth caps how far discovery descends looking for parallelism.
// bucket/data/YYYY/ is the layout depth 2 rescues; past that, the cost of the
// discovery listings starts eating the parallelism they buy.
const maxShardDepth = 2

const delimiter = "/"

// ListObjectsAPI is the slice of the S3 client a walk needs.
type ListObjectsAPI interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input,
		opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// discovery is the plan for a walk: prefixes to fan out over, plus the objects
// already counted while working out that plan.
type discovery struct {
	Shards []string
	Loose  Aggregate
}

// discoverShards works out how to split the bucket into disjoint prefixes.
//
// Coverage invariant: every object lands in exactly one shard, or in Loose.
// CommonPrefixes from a single delimiter query are disjoint by construction, so
// prefix shards never overlap. Keys at a discovery level that contain no further
// delimiter belong to no child prefix, so each level banks its own loose objects
// into Loose as it goes.
//
// Descent is non-destructive, and the decision is made per parent rather than
// per level. A parent that yields child prefixes is replaced by them; a parent
// that yields none cannot be split further, so it stays a shard and what this
// pass counted under it is discarded — walking it re-lists exactly those
// objects, so nothing is lost and nothing is double-counted. Deciding per level
// instead would dissolve the childless parents of a mixed layout (some prefixes
// nested, some flat) into loose objects walked serially inside discovery, which
// is most of the parallelism gone on a very common bucket shape.
//
// Loose objects are aggregated on the spot rather than kept as a slice: a flat
// bucket with a million keys would otherwise be held entirely in memory.
//
// rep is nil-safe; discovery can list a whole flat bucket without walkShards
// ever running, so counting here is the only way progress is not stuck at zero.
func discoverShards(ctx context.Context, api ListObjectsAPI, bucket, rootPrefix string,
	want int, rep *progress.Reporter) (discovery, error) {

	var d discovery
	level := []string{rootPrefix}
	var shards []string

	for depth := 0; depth < maxShardDepth; depth++ {
		var (
			next       []string
			levelLoose Aggregate
		)
		for _, p := range level {
			prefixes, loose, err := listLevel(ctx, api, bucket, p, rep)
			if err != nil {
				return discovery{}, err
			}

			// A parent with no children below it cannot be split further. Keep it
			// as a shard and discard what this pass counted under it — walking it
			// re-lists exactly those objects, so nothing is lost or double-counted.
			// At depth 0 there is no parent to fall back to, so its objects are
			// genuinely loose.
			if len(prefixes) == 0 && depth > 0 {
				next = append(next, p)
				continue
			}

			levelLoose.Merge(loose)
			next = append(next, prefixes...)
		}

		if len(next) == 0 {
			d.Loose.Merge(levelLoose)
			break
		}

		// This level's own objects belong to no child prefix, so they are loose
		// no matter how much further discovery descends.
		d.Loose.Merge(levelLoose)
		shards = next

		if len(next) >= want {
			break
		}
		level = next
	}

	d.Shards = shards
	return d, nil
}

// listLevel lists one prefix with a delimiter, returning the child prefixes and
// an aggregate of the objects sitting directly at that level.
//
// It reports what it lists to rep (nil-safe). Discovery can be the whole run —
// a flat bucket is listed entirely here and walkShards never executes — so
// without this the reporter would print "scanned 0 objects" for the slowest
// case the tool has. The count is what has been listed, not what will be in the
// final total: objects under a parent kept as a shard are listed again by the
// walk, so a mixed layout counts them twice. Progress is advisory; the report's
// numbers come from the Aggregates, which are exact.
func listLevel(ctx context.Context, api ListObjectsAPI, bucket, prefix string,
	rep *progress.Reporter) ([]string, Aggregate, error) {
	var (
		prefixes []string
		loose    Aggregate
		token    *string
	)
	for {
		in := &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Delimiter:         aws.String(delimiter),
			ContinuationToken: token,
		}
		if prefix != "" {
			in.Prefix = aws.String(prefix)
		}

		out, err := api.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, Aggregate{}, err
		}

		for _, cp := range out.CommonPrefixes {
			if p := aws.ToString(cp.Prefix); p != "" {
				prefixes = append(prefixes, p)
			}
		}
		for _, obj := range out.Contents {
			loose.Add(aws.ToInt64(obj.Size), storageClassOf(obj))
		}
		rep.AddObjects(int64(len(out.Contents)))

		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return prefixes, loose, nil
		}
		token = out.NextContinuationToken
	}
}
