package walksource

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/charlesfused/s3metrics/internal/progress"
)

// ListObjectsAPI is the slice of the S3 client a current-version walk needs.
type ListObjectsAPI interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input,
		opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// ListVersionsAPI is the slice a version-aware walk needs.
type ListVersionsAPI interface {
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput,
		opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
}

// BucketAPI is what a Collector is built from. Which half it uses depends on
// Options.IncludeVersions, and that is decided per run rather than per client,
// so both are required up front. The real *s3.Client satisfies this for free.
type BucketAPI interface {
	ListObjectsAPI
	ListVersionsAPI
}

// objectSource lists a bucket. The two implementations differ only in whether
// they see noncurrent versions and delete markers.
//
// The interface exists because discovery counts loose objects as it goes. If
// discovery kept calling ListObjectsV2 while the walk called ListObjectVersions,
// keys sitting at a discovery level would be counted current-only and keys under
// a shard counted all-versions — a silent undercount that grows with however
// many objects happen to live at the top of the bucket. Both phases take the
// same source, so that cannot happen.
type objectSource interface {
	// listLevel lists one prefix with a delimiter, returning the child prefixes
	// and an aggregate of the entries sitting directly at that level.
	listLevel(ctx context.Context, bucket, prefix string, rep *progress.Reporter) ([]string, Aggregate, error)

	// walkPrefix lists everything under prefix, recursively.
	walkPrefix(ctx context.Context, bucket, prefix string, rep *progress.Reporter) (Aggregate, error)
}

// currentSource lists current versions only — what ListObjectsV2 returns, and
// what every walk did before --include-versions existed.
type currentSource struct{ api ListObjectsAPI }

// listLevel reports what it lists to rep (nil-safe). Discovery can be the whole
// run — a flat bucket is listed entirely here and walkShards never executes — so
// without this the reporter would print "scanned 0 objects" for the slowest case
// the tool has. The count is what has been listed, not what will be in the final
// total: objects under a parent kept as a shard are listed again by the walk, so
// a mixed layout counts them twice. Progress is advisory; the report's numbers
// come from the Aggregates, which are exact.
func (s currentSource) listLevel(ctx context.Context, bucket, prefix string,
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

		out, err := s.api.ListObjectsV2(ctx, in)
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

// walkPrefix lists every object under prefix. No delimiter here: this is the
// full recursive listing, unlike discovery's level-by-level walk.
func (s currentSource) walkPrefix(ctx context.Context, bucket, prefix string,
	rep *progress.Reporter) (Aggregate, error) {
	var (
		agg   Aggregate
		token *string
	)
	for {
		out, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
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
		rep.AddObjects(int64(len(out.Contents)))

		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return agg, nil
		}
		token = out.NextContinuationToken
	}
}

// versionSource lists every version and every delete marker — what CloudWatch's
// NumberOfObjects counts, and what ListObjectsV2 cannot see.
type versionSource struct{ api ListVersionsAPI }

func (s versionSource) listLevel(ctx context.Context, bucket, prefix string,
	rep *progress.Reporter) ([]string, Aggregate, error) {
	var (
		prefixes []string
		loose    Aggregate
		keyMark  *string
		verMark  *string
	)
	for {
		in := &s3.ListObjectVersionsInput{
			Bucket:          aws.String(bucket),
			Delimiter:       aws.String(delimiter),
			KeyMarker:       keyMark,
			VersionIdMarker: verMark,
		}
		if prefix != "" {
			in.Prefix = aws.String(prefix)
		}

		out, err := s.api.ListObjectVersions(ctx, in)
		if err != nil {
			return nil, Aggregate{}, err
		}

		for _, cp := range out.CommonPrefixes {
			if p := aws.ToString(cp.Prefix); p != "" {
				prefixes = append(prefixes, p)
			}
		}
		rep.AddObjects(addPage(&loose, out))

		if !more(out) {
			return prefixes, loose, nil
		}
		keyMark, verMark = out.NextKeyMarker, out.NextVersionIdMarker
	}
}

func (s versionSource) walkPrefix(ctx context.Context, bucket, prefix string,
	rep *progress.Reporter) (Aggregate, error) {
	var (
		agg     Aggregate
		keyMark *string
		verMark *string
	)
	for {
		out, err := s.api.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket:          aws.String(bucket),
			Prefix:          aws.String(prefix),
			KeyMarker:       keyMark,
			VersionIdMarker: verMark,
		})
		if err != nil {
			return Aggregate{}, err
		}

		rep.AddObjects(addPage(&agg, out))

		if !more(out) {
			return agg, nil
		}
		keyMark, verMark = out.NextKeyMarker, out.NextVersionIdMarker
	}
}

// addPage folds one page of versions and delete markers into agg, returning how
// many entries it counted so the caller can report progress.
//
// Delete markers go in without a size or a class because DeleteMarkerEntry
// carries neither. That is the whole reason object counts can diverge far more
// than byte totals do.
func addPage(agg *Aggregate, out *s3.ListObjectVersionsOutput) int64 {
	for _, v := range out.Versions {
		agg.AddVersion(aws.ToInt64(v.Size), versionClassOf(v), aws.ToBool(v.IsLatest))
	}
	for range out.DeleteMarkers {
		agg.AddDeleteMarker()
	}
	return int64(len(out.Versions) + len(out.DeleteMarkers))
}

// more reports whether another page is waiting.
//
// Both markers are carried forward by the callers, never just the key. A key
// with more versions than fit in one page is resumed mid-key, and the version
// marker is the only thing that says where — drop it and versions are silently
// repeated or skipped, with no error to notice.
//
// NextVersionIdMarker is allowed to be nil while NextKeyMarker is set (the page
// broke on a common prefix), which is why only the key marker is checked here.
func more(out *s3.ListObjectVersionsOutput) bool {
	return aws.ToBool(out.IsTruncated) && out.NextKeyMarker != nil
}
