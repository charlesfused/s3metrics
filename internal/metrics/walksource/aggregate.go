// Package walksource computes bucket metrics by listing every object.
package walksource

import (
	"sort"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

// ClassStat is one storage class's running totals.
type ClassStat struct {
	Bytes   int64
	Objects int64
}

// Aggregate accumulates a walk's results.
//
// Each worker owns one of these outright and never shares it, which is why the
// concurrent walk needs no mutex: the merge happens serially once every worker
// has finished.
type Aggregate struct {
	Objects int64
	Bytes   int64

	// DeleteMarkers and NoncurrentVersions stay zero unless the walk ran against
	// the version source; a current-only listing cannot see either.
	DeleteMarkers      int64
	NoncurrentVersions int64

	Classes map[string]ClassStat
}

// Add records one object.
func (a *Aggregate) Add(size int64, class string) {
	if a.Classes == nil {
		a.Classes = map[string]ClassStat{}
	}
	a.Objects++
	a.Bytes += size

	cs := a.Classes[class]
	cs.Bytes += size
	cs.Objects++
	a.Classes[class] = cs
}

// AddVersion records one object version, current or not.
//
// Every version is an object as far as the count goes — that is precisely what
// CloudWatch's NumberOfObjects measures — so this is Add plus a tally of the
// noncurrent ones, which exist only to explain the difference.
func (a *Aggregate) AddVersion(size int64, class string, latest bool) {
	a.Add(size, class)
	if !latest {
		a.NoncurrentVersions++
	}
}

// AddDeleteMarker records one delete marker.
//
// A marker counts as an object and nothing else: DeleteMarkerEntry carries
// neither a size nor a storage class, so there is no byte total and no class row
// it could belong to. On the bucket that prompted this feature they were 88% of
// a sampled page, which is why the object counts diverged 2.66x while the byte
// totals differed by 13%.
func (a *Aggregate) AddDeleteMarker() {
	a.Objects++
	a.DeleteMarkers++
}

// Merge folds other into a.
func (a *Aggregate) Merge(other Aggregate) {
	a.Objects += other.Objects
	a.Bytes += other.Bytes
	a.DeleteMarkers += other.DeleteMarkers
	a.NoncurrentVersions += other.NoncurrentVersions

	if len(other.Classes) == 0 {
		return
	}
	if a.Classes == nil {
		a.Classes = make(map[string]ClassStat, len(other.Classes))
	}
	for class, stat := range other.Classes {
		cs := a.Classes[class]
		cs.Bytes += stat.Bytes
		cs.Objects += stat.Objects
		a.Classes[class] = cs
	}
}

// StorageClasses renders the aggregate as report rows, sorted by class name.
//
// Every row carries a real ObjectCount — unlike the CloudWatch source, a walk
// counts objects per class directly. No row is ever overhead: a walk sums object
// sizes, and per-object storage overhead is not visible in a listing.
func (a Aggregate) StorageClasses() []metrics.StorageClassStat {
	out := make([]metrics.StorageClassStat, 0, len(a.Classes))
	for class, stat := range a.Classes {
		count := stat.Objects
		out = append(out, metrics.StorageClassStat{
			Class:       class,
			SizeBytes:   stat.Bytes,
			ObjectCount: &count,
			Overhead:    false,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out
}

// storageClassOf reads an object's class, defaulting to STANDARD.
//
// S3 omits the field for standard-class objects in some responses, and the SDK
// models it as an empty enum value rather than a pointer, so an empty string
// means "standard", not "unknown".
func storageClassOf(obj s3types.Object) string {
	if c := string(obj.StorageClass); c != "" {
		return c
	}
	return "STANDARD"
}

// versionClassOf reads a version's class, defaulting to STANDARD for the same
// reason storageClassOf does. The SDK models the two as distinct enum types even
// though the values are identical, so this cannot be shared.
func versionClassOf(v s3types.ObjectVersion) string {
	if c := string(v.StorageClass); c != "" {
		return c
	}
	return "STANDARD"
}
