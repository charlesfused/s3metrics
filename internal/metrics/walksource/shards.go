package walksource

import (
	"context"

	"github.com/charlesfused/s3metrics/internal/progress"
)

// maxShardDepth caps how far discovery descends looking for parallelism.
// bucket/data/YYYY/ is the layout depth 2 rescues; past that, the cost of the
// discovery listings starts eating the parallelism they buy.
const maxShardDepth = 2

const delimiter = "/"

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
//
// src is the same source the walk will use. That is load-bearing rather than
// tidy: discovery banks loose objects into the result, so a discovery that
// listed current versions while the walk listed all of them would report a
// number that is neither.
func discoverShards(ctx context.Context, src objectSource, bucket, rootPrefix string,
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
			prefixes, loose, err := src.listLevel(ctx, bucket, p, rep)
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
