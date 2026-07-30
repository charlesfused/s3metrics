package walksource

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

var walkNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func collectWith(t *testing.T, api ListObjectsAPI, opts Options) (*metrics.Report, error) {
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
	inner ListObjectsAPI
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
