package walksource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

var walkNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// showCount renders a nullable count for a failure message, so that "unknown"
// reads as null rather than as a pointer address.
func showCount(p *int64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *p)
}

func collectWith(t *testing.T, api BucketAPI, opts Options) (*metrics.Report, error) {
	t.Helper()
	c := New(api, "us-east-1", opts)
	c.SetClock(func() time.Time { return walkNow })
	return c.Collect(context.Background(), "bucket")
}

func TestCollectFlatBucket(t *testing.T) {
	f := newFakeS3(map[string]int64{"a.txt": 10, "b.txt": 20, "c.txt": 30})

	r, err := collectWith(t, f, Options{Concurrency: 8})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if r.ObjectCount != 3 {
		t.Errorf("ObjectCount = %d, want 3", r.ObjectCount)
	}
	if r.TotalSizeBytes != 60 {
		t.Errorf("TotalSizeBytes = %d, want 60", r.TotalSizeBytes)
	}
	if r.Source != metrics.SourceWalk {
		t.Errorf("Source = %q, want %q", r.Source, metrics.SourceWalk)
	}
	if !r.AsOf.Equal(walkNow) {
		t.Errorf("AsOf = %v, want the walk start %v", r.AsOf, walkNow)
	}
}

func TestCollectShardedBucket(t *testing.T) {
	keys := map[string]int64{}
	for i := 0; i < 12; i++ {
		keys[fmt.Sprintf("p%02d/obj.txt", i)] = int64(i + 1)
	}
	f := newFakeS3(keys)

	r, err := collectWith(t, f, Options{Concurrency: 4})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if r.ObjectCount != 12 {
		t.Errorf("ObjectCount = %d, want 12", r.ObjectCount)
	}
	if want := int64(78); r.TotalSizeBytes != want { // 1+2+...+12
		t.Errorf("TotalSizeBytes = %d, want %d", r.TotalSizeBytes, want)
	}
}

// TestCoverageInvariant is the test that matters: a sharded walk and a forced
// sequential walk over identical data must agree exactly. If sharding ever drops
// or double-counts an object, this fails.
func TestCoverageInvariant(t *testing.T) {
	fixtures := map[string]map[string]int64{
		"flat": {
			"a.txt": 1, "b.txt": 2, "c.txt": 3,
		},
		"wide": func() map[string]int64 {
			m := map[string]int64{}
			for i := 0; i < 20; i++ {
				m[fmt.Sprintf("p%02d/obj.txt", i)] = int64(i)
			}
			return m
		}(),
		"deep and narrow": {
			"data/2023/a.txt": 1, "data/2024/b.txt": 2,
			"data/2025/c.txt": 4, "data/2026/d.txt": 8,
		},
		"loose objects at every level": {
			"root.txt": 1, "data/direct.txt": 2,
			"data/2025/a.txt": 4, "data/2026/b.txt": 8,
			"logs/x.txt": 16,
		},
		"mixed depths": {
			"a.txt": 1, "b/1.txt": 2, "b/c/2.txt": 4, "b/c/d/3.txt": 8,
			"e/f.txt": 16, "g.txt": 32,
		},
	}

	for name, keys := range fixtures {
		t.Run(name, func(t *testing.T) {
			seq, err := collectWith(t, newFakeS3(keys), Options{Concurrency: 1})
			if err != nil {
				t.Fatalf("sequential Collect() error = %v", err)
			}
			par, err := collectWith(t, newFakeS3(keys), Options{Concurrency: 8})
			if err != nil {
				t.Fatalf("parallel Collect() error = %v", err)
			}

			var wantBytes int64
			for _, v := range keys {
				wantBytes += v
			}

			if seq.ObjectCount != int64(len(keys)) || seq.TotalSizeBytes != wantBytes {
				t.Errorf("sequential = {%d objects, %d bytes}, want {%d, %d}",
					seq.ObjectCount, seq.TotalSizeBytes, len(keys), wantBytes)
			}
			if par.ObjectCount != seq.ObjectCount || par.TotalSizeBytes != seq.TotalSizeBytes {
				t.Errorf("parallel = {%d objects, %d bytes}, sequential = {%d, %d} — sharding changed the answer",
					par.ObjectCount, par.TotalSizeBytes, seq.ObjectCount, seq.TotalSizeBytes)
			}
		})
	}
}

// TestCoverageInvariantWithVersions is TestCoverageInvariant's counterpart for
// the version source, and it exists for a specific failure: discovery counts
// loose objects as it goes, so if it kept using ListObjectsV2 while the walk used
// ListObjectVersions, loose keys would be counted current-only and sharded keys
// all-versions. Fixtures below deliberately mix loose and sharded keys.
//
// Totals are computed here from the fixture directly, not from the other run, so
// concurrency 1 and 8 agreeing on a wrong number is not enough to pass.
func TestCoverageInvariantWithVersions(t *testing.T) {
	fixtures := map[string]struct {
		history  map[string][]fakeVersion
		pageSize int
	}{
		"flat with histories": {history: map[string][]fakeVersion{
			"a.txt": {{size: 1}, {size: 2}},
			"b.txt": {{deleteMarker: true}, {size: 4}},
			"c.txt": {{size: 8}},
		}},
		"wide": {history: func() map[string][]fakeVersion {
			m := map[string][]fakeVersion{}
			for i := 0; i < 20; i++ {
				m[fmt.Sprintf("p%02d/obj.txt", i)] = []fakeVersion{
					{deleteMarker: true}, {size: int64(i)}, {size: int64(i) * 2},
				}
			}
			return m
		}()},
		"deep and narrow": {history: map[string][]fakeVersion{
			"data/2023/a.txt": {{size: 1}, {size: 1}},
			"data/2024/b.txt": {{deleteMarker: true}},
			"data/2025/c.txt": {{size: 4}},
			"data/2026/d.txt": {{size: 8}, {deleteMarker: true}, {size: 8}},
		}},
		"loose objects at every level": {history: map[string][]fakeVersion{
			"root.txt":        {{size: 1}, {size: 1}},
			"data/direct.txt": {{deleteMarker: true}, {size: 2}},
			"data/2025/a.txt": {{size: 4}},
			"data/2026/b.txt": {{size: 8}, {size: 16}},
			"logs/x.txt":      {{size: 16}},
		}},
		"mixed depths": {history: map[string][]fakeVersion{
			"a.txt":       {{size: 1}},
			"b/1.txt":     {{size: 2}, {deleteMarker: true}},
			"b/c/2.txt":   {{size: 4}, {size: 4}, {size: 4}},
			"b/c/d/3.txt": {{size: 8}},
			"e/f.txt":     {{deleteMarker: true}, {size: 16}},
			"g.txt":       {{size: 32}},
		}},
		// Small pages force resumes in the middle of a key's history, which is
		// only survivable if both pagination markers are carried.
		"paginated mid-key": {pageSize: 3, history: map[string][]fakeVersion{
			"a/deep.txt": func() []fakeVersion {
				var vs []fakeVersion
				for i := 0; i < 11; i++ {
					vs = append(vs, fakeVersion{size: 1})
				}
				return vs
			}(),
			"b/deep.txt": func() []fakeVersion {
				vs := []fakeVersion{{deleteMarker: true}}
				for i := 0; i < 9; i++ {
					vs = append(vs, fakeVersion{size: 2})
				}
				return vs
			}(),
			"loose.txt": {{size: 5}, {size: 5}, {size: 5}, {size: 5}},
		}},
	}

	for name, tt := range fixtures {
		t.Run(name, func(t *testing.T) {
			newFake := func() *fakeS3 {
				f := newFakeVersions(tt.history)
				if tt.pageSize > 0 {
					f.pageSize = tt.pageSize
				}
				return f
			}

			seq, err := collectWith(t, newFake(), Options{Concurrency: 1, IncludeVersions: true})
			if err != nil {
				t.Fatalf("sequential Collect() error = %v", err)
			}
			par, err := collectWith(t, newFake(), Options{Concurrency: 8, IncludeVersions: true})
			if err != nil {
				t.Fatalf("parallel Collect() error = %v", err)
			}

			var wantObjects, wantBytes, wantMarkers, wantNoncurrent int64
			for _, vs := range tt.history {
				for i, v := range vs {
					wantObjects++
					if v.deleteMarker {
						wantMarkers++
						continue
					}
					wantBytes += v.size
					if i > 0 {
						wantNoncurrent++
					}
				}
			}

			check := func(label string, r *metrics.Report) {
				t.Helper()
				if r.ObjectCount != wantObjects || r.TotalSizeBytes != wantBytes {
					t.Errorf("%s = {%d objects, %d bytes}, want {%d, %d}",
						label, r.ObjectCount, r.TotalSizeBytes, wantObjects, wantBytes)
				}
				if r.DeleteMarkers == nil || *r.DeleteMarkers != wantMarkers {
					t.Errorf("%s DeleteMarkers = %s, want %d", label, showCount(r.DeleteMarkers), wantMarkers)
				}
				if r.NoncurrentVersions == nil || *r.NoncurrentVersions != wantNoncurrent {
					t.Errorf("%s NoncurrentVersions = %s, want %d", label, showCount(r.NoncurrentVersions), wantNoncurrent)
				}

				// The identity the README publishes.
				var classed int64
				for _, sc := range r.StorageClasses {
					if sc.ObjectCount == nil {
						t.Fatalf("%s %s.ObjectCount = nil, want a real count", label, sc.Class)
					}
					classed += *sc.ObjectCount
				}
				if classed+wantMarkers != r.ObjectCount {
					t.Errorf("%s: sum(classes)=%d + delete_markers=%d != object_count=%d",
						label, classed, wantMarkers, r.ObjectCount)
				}
			}
			check("sequential", seq)
			check("parallel", par)
		})
	}
}

// Without --include-versions a walk sees current versions only, and it must say
// so with nulls rather than claiming zero of something it never looked for.
func TestCollectWithoutIncludeVersionsLeavesVersionCountsNull(t *testing.T) {
	f := newFakeVersions(versionHistory())

	r, err := collectWith(t, f, Options{Concurrency: 4})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if r.DeleteMarkers != nil || r.NoncurrentVersions != nil {
		t.Errorf("DeleteMarkers = %v, NoncurrentVersions = %v, want both null",
			r.DeleteMarkers, r.NoncurrentVersions)
	}
	// b.txt only: a.txt and c.txt read as deleted, so a plain listing skips them.
	if r.ObjectCount != 1 || r.TotalSizeBytes != 7 {
		t.Errorf("got {%d objects, %d bytes}, want {1, 7} — current versions only",
			r.ObjectCount, r.TotalSizeBytes)
	}
}

// Zero delete markers on a versioned bucket is a real answer and must not encode
// the same way as "never looked".
func TestCollectWithIncludeVersionsReportsZeroesNotNulls(t *testing.T) {
	f := newFakeVersions(map[string][]fakeVersion{"a.txt": {{size: 3}}})

	r, err := collectWith(t, f, Options{Concurrency: 1, IncludeVersions: true})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if r.DeleteMarkers == nil || *r.DeleteMarkers != 0 {
		t.Errorf("DeleteMarkers = %v, want a real 0", r.DeleteMarkers)
	}
	if r.NoncurrentVersions == nil || *r.NoncurrentVersions != 0 {
		t.Errorf("NoncurrentVersions = %v, want a real 0", r.NoncurrentVersions)
	}
}

func TestCollectWithVersionsPropagatesWorkerError(t *testing.T) {
	history := map[string][]fakeVersion{}
	for i := 0; i < 20; i++ {
		history[fmt.Sprintf("p%02d/obj.txt", i)] = []fakeVersion{{size: 1}}
	}
	api := &failAfterN{inner: newFakeVersions(history), n: 1}

	_, err := collectWith(t, api, Options{Concurrency: 4, IncludeVersions: true})
	if err == nil {
		t.Fatal("Collect() error = nil, want the worker error propagated")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("Collect() error = %T, want *errs.Error", err)
	}
}

// denyAll refuses every call the way AWS does, with a Smithy API error, so
// errs.Classify takes its access-denied branch rather than the catch-all.
type denyAll struct{}

func (denyAll) ListObjectsV2(context.Context, *s3.ListObjectsV2Input,
	...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}
}

func (denyAll) ListObjectVersions(context.Context, *s3.ListObjectVersionsInput,
	...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}
}

// ListObjectVersions is gated by s3:ListBucketVersions, which is a different IAM
// action from s3:ListBucket. Naming the wrong one sends a denied user to add a
// permission they already hold — the precise diagnostic failure this task exists
// to remove, on the code path this task added.
func TestCollectNamesTheDeniedIAMAction(t *testing.T) {
	tests := []struct {
		name            string
		includeVersions bool
		want            string
		notWant         string
	}{
		{"current-version walk", false, "s3:ListBucket", "s3:ListBucketVersions"},
		{"version-aware walk", true, "s3:ListBucketVersions", "s3:ListBucket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := collectWith(t, denyAll{},
				Options{Concurrency: 4, IncludeVersions: tt.includeVersions})
			if err == nil {
				t.Fatal("Collect() error = nil, want an access-denied error")
			}

			var e *errs.Error
			if !errors.As(err, &e) {
				t.Fatalf("Collect() error = %T, want *errs.Error", err)
			}
			if e.Code != errs.CodeAccessDenied {
				t.Fatalf("Code = %s, want %s", e.Code, errs.CodeAccessDenied)
			}

			// Matched with the trailing words, because "s3:ListBucketVersions"
			// contains "s3:ListBucket" — a bare substring check would pass for
			// both rows and prove nothing.
			if !strings.Contains(e.Hint, tt.want+" on this resource") {
				t.Errorf("Hint = %q, want it to name %s", e.Hint, tt.want)
			}
			if strings.Contains(e.Hint, tt.notWant+" on this resource") {
				t.Errorf("Hint = %q, must not name %s", e.Hint, tt.notWant)
			}
		})
	}
}

func TestCollectHonoursPrefix(t *testing.T) {
	f := newFakeS3(map[string]int64{
		"data/2025/a.txt": 1, "data/2026/b.txt": 2, "other/c.txt": 100,
	})

	r, err := collectWith(t, f, Options{Concurrency: 4, Prefix: "data/"})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if r.TotalSizeBytes != 3 {
		t.Errorf("TotalSizeBytes = %d, want 3 — other/ must be excluded", r.TotalSizeBytes)
	}
	if r.Prefix != "data/" {
		t.Errorf("Prefix = %q, want data/ recorded on the report", r.Prefix)
	}
}

func TestCollectAggregatesStorageClasses(t *testing.T) {
	f := newFakeS3(map[string]int64{"a.txt": 10, "b.txt": 20, "c.txt": 5})
	f.classes["a.txt"] = "GLACIER"
	f.classes["b.txt"] = "GLACIER"
	// c.txt has no class set, exercising the STANDARD default.

	r, err := collectWith(t, f, Options{Concurrency: 1})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	byClass := map[string]metrics.StorageClassStat{}
	for _, sc := range r.StorageClasses {
		byClass[sc.Class] = sc
	}
	if got := byClass["GLACIER"]; got.SizeBytes != 30 || got.ObjectCount == nil || *got.ObjectCount != 2 {
		t.Errorf("GLACIER = %+v, want 30 bytes across 2 objects", got)
	}
	if got := byClass["STANDARD"]; got.SizeBytes != 5 {
		t.Errorf("STANDARD = %+v, want 5 bytes (missing class defaults to STANDARD)", got)
	}
	for _, sc := range r.StorageClasses {
		if sc.Overhead {
			t.Errorf("%s.Overhead = true, want false — a walk never sees overhead", sc.Class)
		}
	}
}

// failAfterN wraps a fake and starts failing once a shard walk begins, so one
// worker's error must unwind the rest.
type failAfterN struct {
	inner BucketAPI
	n     int32
	calls atomic.Int32
}

func (f *failAfterN) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input,
	opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.calls.Add(1) > f.n {
		return nil, errors.New("simulated API failure")
	}
	return f.inner.ListObjectsV2(ctx, in, opts...)
}

func (f *failAfterN) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput,
	opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	if f.calls.Add(1) > f.n {
		return nil, errors.New("simulated API failure")
	}
	return f.inner.ListObjectVersions(ctx, in, opts...)
}

func TestCollectPropagatesWorkerError(t *testing.T) {
	keys := map[string]int64{}
	for i := 0; i < 20; i++ {
		keys[fmt.Sprintf("p%02d/obj.txt", i)] = 1
	}
	api := &failAfterN{inner: newFakeS3(keys), n: 1} // discovery succeeds, walking fails

	_, err := collectWith(t, api, Options{Concurrency: 4})
	if err == nil {
		t.Fatal("Collect() error = nil, want the worker error propagated")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("Collect() error = %T, want *errs.Error", err)
	}
}

func TestCollectRespectsCancellation(t *testing.T) {
	keys := map[string]int64{}
	for i := 0; i < 50; i++ {
		keys[fmt.Sprintf("p%02d/obj.txt", i)] = 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before we start

	c := New(newFakeS3(keys), "us-east-1", Options{Concurrency: 8})
	c.SetClock(func() time.Time { return walkNow })

	_, err := c.Collect(ctx, "bucket")
	if err == nil {
		t.Fatal("Collect() error = nil, want a cancellation error")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeCanceled {
		t.Errorf("Collect() error code = %v, want %s", err, errs.CodeCanceled)
	}
}

func TestCollectEmptyBucket(t *testing.T) {
	r, err := collectWith(t, newFakeS3(map[string]int64{}), Options{Concurrency: 4})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if r.ObjectCount != 0 || r.TotalSizeBytes != 0 {
		t.Errorf("got {%d objects, %d bytes}, want zeroes", r.ObjectCount, r.TotalSizeBytes)
	}
	if r.StorageClasses == nil {
		t.Error("StorageClasses = nil, want an empty slice so JSON renders [] not null")
	}
}
