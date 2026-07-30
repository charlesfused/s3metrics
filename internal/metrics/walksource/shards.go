package walksource

import (
	"context"
	"strings"

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
// into Loose as it goes — descending replaces a level's prefix shards with its
// children but keeps what that level already counted.
//
// Loose objects are aggregated on the spot rather than kept as a slice: a flat
// bucket with a million keys would otherwise be held entirely in memory.
func discoverShards(ctx context.Context, api ListObjectsAPI, bucket, rootPrefix string, want int) (discovery, error) {
	var d discovery
	level := []string{rootPrefix}
	var expanded []string

	// maxShardDepth is an absolute depth from the bucket root, not a count of
	// iterations from rootPrefix: a --prefix that already names one or more path
	// segments has already spent that much of the depth budget. Without this
	// adjustment, a narrow rootPrefix causes discovery to overshoot the real tree
	// by exactly that many levels, converting what should be walkable shards into
	// prematurely-summed loose objects (still fully counted, just not the
	// intended shard split). At least one discovery pass always runs regardless
	// of how deep rootPrefix already is.
	effectiveMaxDepth := maxShardDepth - strings.Count(rootPrefix, delimiter)
	if effectiveMaxDepth < 1 {
		effectiveMaxDepth = 1
	}

	for depth := 0; depth < effectiveMaxDepth; depth++ {
		expanded = nil
		for _, p := range level {
			prefixes, loose, err := listLevel(ctx, api, bucket, p)
			if err != nil {
				return discovery{}, err
			}
			d.Loose.Merge(loose)
			expanded = append(expanded, prefixes...)
		}

		// Nothing below this level: everything under it was loose and is counted.
		if len(expanded) == 0 {
			break
		}
		// Enough parallelism — stop here rather than pay for another listing pass.
		if len(expanded) >= want {
			break
		}
		level = expanded
	}

	d.Shards = expanded
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
