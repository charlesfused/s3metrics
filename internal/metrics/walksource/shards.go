package walksource

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
// Descent is non-destructive: a level's prefixes only replace the parent level's
// shards once that level itself yields further child prefixes. A pass that finds
// no children below it cannot improve the split, so its shard list is discarded
// and the parent shards are kept instead — the objects the discarded pass already
// counted get re-listed when those parent shards are walked, so nothing is lost
// and nothing is double-counted. Without this, a bucket with flat top-level
// prefixes (bucket/a/*, bucket/b/*, no further nesting) would have its shards
// dissolved into loose objects, leaving the walk with no parallelism at all.
//
// Loose objects are aggregated on the spot rather than kept as a slice: a flat
// bucket with a million keys would otherwise be held entirely in memory.
func discoverShards(ctx context.Context, api ListObjectsAPI, bucket, rootPrefix string, want int) (discovery, error) {
	var d discovery
	level := []string{rootPrefix}
	var shards []string

	for depth := 0; depth < maxShardDepth; depth++ {
		var (
			next       []string
			levelLoose Aggregate
		)
		for _, p := range level {
			prefixes, loose, err := listLevel(ctx, api, bucket, p)
			if err != nil {
				return discovery{}, err
			}
			levelLoose.Merge(loose)
			next = append(next, prefixes...)
		}

		// A pass that finds no child prefixes cannot improve the split. Below the
		// first level, discard what it counted and keep the parent shards: those
		// prefixes get walked, which re-lists exactly the objects just discarded,
		// so nothing is lost and nothing is counted twice. Committing instead
		// would dissolve good shards into loose objects and leave the walk with no
		// parallelism at all.
		if len(next) == 0 {
			if depth == 0 {
				// Nothing above to fall back to — this level is the whole answer.
				d.Loose.Merge(levelLoose)
			}
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
func listLevel(ctx context.Context, api ListObjectsAPI, bucket, prefix string) ([]string, Aggregate, error) {
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

		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return prefixes, loose, nil
		}
		token = out.NextContinuationToken
	}
}
