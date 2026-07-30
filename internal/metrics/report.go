// Package metrics defines the report both collection modes produce.
//
// Both modes emit the same schema. A field a mode cannot populate is explicitly
// null rather than zero, because zero bytes and unknown bytes are different
// answers and a consumer must be able to tell them apart.
package metrics

import (
	"context"
	"sort"
	"time"
)

// Source values recorded in Report.Source.
const (
	SourceCloudWatch = "cloudwatch"
	SourceWalk       = "walk"
)

// Report is one bucket's metrics at one point in time.
type Report struct {
	Bucket string `json:"bucket"`
	Region string `json:"region"`
	Source string `json:"source"`

	// AsOf is when the data was true, not when it was fetched. CloudWatch
	// storage metrics publish daily, so a cloudwatch-sourced AsOf can trail
	// wall-clock time by 24-48h. Walk mode sets it to the walk start.
	AsOf time.Time `json:"as_of"`

	// Versioned reports whether the bucket keeps noncurrent versions, which is
	// the dominant reason the two modes disagree. Null when the lookup was
	// denied: s3:GetBucketVersioning is advisory here and never fails a run.
	// True for a suspended bucket too — suspending stops new versions being
	// created but keeps every existing one.
	Versioned *bool `json:"versioned"`

	TotalSizeBytes int64 `json:"total_size_bytes"`
	ObjectCount    int64 `json:"object_count"`

	// DeleteMarkers and NoncurrentVersions decompose ObjectCount on a versioned
	// bucket. Both are null unless the run was a walk with --include-versions:
	// CloudWatch does not break them out, and a plain listing cannot see them.
	//
	// Delete markers carry no size and no storage class, so they appear in no
	// storage-class row. Hence the reconciliation identity:
	//
	//	object_count = sum(storage_classes[].object_count) + delete_markers
	DeleteMarkers      *int64 `json:"delete_markers"`
	NoncurrentVersions *int64 `json:"noncurrent_versions"`

	StorageClasses []StorageClassStat `json:"storage_classes"`

	Prefix     string `json:"prefix,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// StorageClassStat is one storage class's contribution to a bucket.
type StorageClassStat struct {
	Class     string `json:"class"`
	SizeBytes int64  `json:"size_bytes"`

	// ObjectCount is nil in cloudwatch mode: the NumberOfObjects metric only
	// publishes for StorageType=AllStorageTypes, so there is no per-class count
	// to report. A pointer, not a zero, so absent is distinguishable from empty.
	ObjectCount *int64 `json:"object_count"`

	// Overhead marks a CloudWatch storage type that measures per-object
	// bookkeeping rather than object data (Glacier metadata, for instance).
	// Always false in walk mode. Excluded from TotalSizeBytes.
	Overhead bool `json:"overhead"`
}

// Collector produces a Report for one bucket.
type Collector interface {
	Collect(ctx context.Context, bucket string) (*Report, error)
}

// Recompute sorts the storage classes and derives TotalSizeBytes from them.
//
// Total counts non-overhead classes only. That is the single definition of
// "size" in this program: it is what a walk can measure by summing object
// sizes, so both modes report a comparable number. Overhead rows stay in the
// slice, letting a caller add them back to reconcile against a bill.
//
// ObjectCount is deliberately untouched — in cloudwatch mode it comes from a
// separate metric, not from summing the classes.
func (r *Report) Recompute() {
	sort.Slice(r.StorageClasses, func(i, j int) bool {
		return r.StorageClasses[i].Class < r.StorageClasses[j].Class
	})

	var total int64
	for _, sc := range r.StorageClasses {
		if !sc.Overhead {
			total += sc.SizeBytes
		}
	}
	r.TotalSizeBytes = total
}
