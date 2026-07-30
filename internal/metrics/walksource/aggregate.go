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

// Merge folds other into a.
func (a *Aggregate) Merge(other Aggregate) {
	a.Objects += other.Objects
	a.Bytes += other.Bytes

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
