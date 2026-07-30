package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

func TestRecomputeExcludesOverheadFromTotal(t *testing.T) {
	r := &Report{
		StorageClasses: []StorageClassStat{
			{Class: "GLACIER", SizeBytes: 1000},
			{Class: "GLACIER_OBJECT_OVERHEAD", SizeBytes: 32, Overhead: true},
			{Class: "STANDARD", SizeBytes: 500},
		},
	}
	r.Recompute()

	if r.TotalSizeBytes != 1500 {
		t.Errorf("TotalSizeBytes = %d, want 1500 (overhead excluded)", r.TotalSizeBytes)
	}
	if len(r.StorageClasses) != 3 {
		t.Errorf("len(StorageClasses) = %d, want 3 — overhead rows stay visible", len(r.StorageClasses))
	}
}

func TestRecomputeSortsByClass(t *testing.T) {
	r := &Report{
		StorageClasses: []StorageClassStat{
			{Class: "STANDARD"},
			{Class: "DEEP_ARCHIVE"},
			{Class: "GLACIER"},
		},
	}
	r.Recompute()

	want := []string{"DEEP_ARCHIVE", "GLACIER", "STANDARD"}
	for i, w := range want {
		if r.StorageClasses[i].Class != w {
			t.Errorf("StorageClasses[%d].Class = %q, want %q", i, r.StorageClasses[i].Class, w)
		}
	}
}

func TestRecomputeLeavesObjectCountAlone(t *testing.T) {
	r := &Report{
		ObjectCount:    42,
		StorageClasses: []StorageClassStat{{Class: "STANDARD", SizeBytes: 1}},
	}
	r.Recompute()

	if r.ObjectCount != 42 {
		t.Errorf("ObjectCount = %d, want 42 — Recompute must not touch it", r.ObjectCount)
	}
}

func TestNilObjectCountMarshalsAsNull(t *testing.T) {
	r := &Report{
		Bucket:         "b",
		Region:         "us-east-1",
		Source:         SourceCloudWatch,
		AsOf:           time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		StorageClasses: []StorageClassStat{{Class: "STANDARD", SizeBytes: 1, ObjectCount: nil}},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"object_count":null`) {
		t.Errorf("marshalled = %s, want a null object_count", b)
	}
}

func TestNonNilObjectCountMarshalsAsNumber(t *testing.T) {
	r := &Report{StorageClasses: []StorageClassStat{{Class: "STANDARD", ObjectCount: i64(7)}}}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"object_count":7`) {
		t.Errorf("marshalled = %s, want object_count 7", b)
	}
}

func TestEmptyPrefixIsOmitted(t *testing.T) {
	b, err := json.Marshal(&Report{Bucket: "b"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(b), "prefix") {
		t.Errorf("marshalled = %s, want no prefix key when unset", b)
	}
}
