package walksource

import "testing"

func TestAggregateAdd(t *testing.T) {
	var a Aggregate
	a.Add(100, "STANDARD")
	a.Add(50, "STANDARD")
	a.Add(25, "GLACIER")

	if a.Objects != 3 {
		t.Errorf("Objects = %d, want 3", a.Objects)
	}
	if a.Bytes != 175 {
		t.Errorf("Bytes = %d, want 175", a.Bytes)
	}
	if got := a.Classes["STANDARD"]; got.Bytes != 150 || got.Objects != 2 {
		t.Errorf("STANDARD = %+v, want {Bytes:150 Objects:2}", got)
	}
}

func TestAggregateMerge(t *testing.T) {
	var a, b Aggregate
	a.Add(100, "STANDARD")
	b.Add(50, "STANDARD")
	b.Add(25, "GLACIER")

	a.Merge(b)

	if a.Objects != 3 || a.Bytes != 175 {
		t.Errorf("after Merge: Objects = %d, Bytes = %d, want 3 and 175", a.Objects, a.Bytes)
	}
	if got := a.Classes["STANDARD"]; got.Bytes != 150 {
		t.Errorf("STANDARD.Bytes = %d, want 150", got.Bytes)
	}
	if got := a.Classes["GLACIER"]; got.Objects != 1 {
		t.Errorf("GLACIER.Objects = %d, want 1", got.Objects)
	}
}

func TestMergeIntoEmptyAggregate(t *testing.T) {
	var empty, b Aggregate
	b.Add(10, "STANDARD")

	empty.Merge(b) // must not panic on a nil Classes map

	if empty.Bytes != 10 {
		t.Errorf("Bytes = %d, want 10", empty.Bytes)
	}
}

func TestAggregateAddVersion(t *testing.T) {
	var a Aggregate
	a.AddVersion(100, "STANDARD", true) // current
	a.AddVersion(50, "STANDARD", false) // noncurrent
	a.AddVersion(25, "GLACIER", false)  // noncurrent

	if a.Objects != 3 {
		t.Errorf("Objects = %d, want 3 — every version is an object", a.Objects)
	}
	if a.Bytes != 175 {
		t.Errorf("Bytes = %d, want 175", a.Bytes)
	}
	if a.NoncurrentVersions != 2 {
		t.Errorf("NoncurrentVersions = %d, want 2", a.NoncurrentVersions)
	}
	if a.DeleteMarkers != 0 {
		t.Errorf("DeleteMarkers = %d, want 0", a.DeleteMarkers)
	}
	if got := a.Classes["STANDARD"]; got.Bytes != 150 || got.Objects != 2 {
		t.Errorf("STANDARD = %+v, want {Bytes:150 Objects:2}", got)
	}
}

func TestAggregateAddDeleteMarkerHasNoSizeAndNoClass(t *testing.T) {
	var a Aggregate
	a.AddVersion(100, "STANDARD", true)
	a.AddDeleteMarker()
	a.AddDeleteMarker()

	if a.Objects != 3 {
		t.Errorf("Objects = %d, want 3 — a delete marker is an object to CloudWatch", a.Objects)
	}
	if a.Bytes != 100 {
		t.Errorf("Bytes = %d, want 100 — a delete marker carries no bytes", a.Bytes)
	}
	if a.DeleteMarkers != 2 {
		t.Errorf("DeleteMarkers = %d, want 2", a.DeleteMarkers)
	}
	if len(a.Classes) != 1 {
		t.Errorf("Classes = %v, want STANDARD only — a delete marker has no storage class", a.Classes)
	}
}

// The reconciliation identity the README promises: the per-class counts plus the
// delete markers must account for every object counted.
func TestAggregateReconciliationIdentity(t *testing.T) {
	var a Aggregate
	a.AddVersion(10, "STANDARD", true)
	a.AddVersion(20, "STANDARD", false)
	a.AddVersion(30, "GLACIER", false)
	a.AddDeleteMarker()
	a.AddDeleteMarker()
	a.AddDeleteMarker()

	var classed int64
	for _, sc := range a.StorageClasses() {
		if sc.ObjectCount == nil {
			t.Fatalf("%s.ObjectCount = nil, want a real count", sc.Class)
		}
		classed += *sc.ObjectCount
	}
	if got := classed + a.DeleteMarkers; got != a.Objects {
		t.Errorf("sum(classes) + delete markers = %d, want Objects = %d", got, a.Objects)
	}
}

func TestMergeCarriesVersionCounters(t *testing.T) {
	var a, b Aggregate
	a.AddVersion(10, "STANDARD", false)
	a.AddDeleteMarker()
	b.AddVersion(20, "STANDARD", false)
	b.AddDeleteMarker()
	b.AddDeleteMarker()

	a.Merge(b)

	if a.DeleteMarkers != 3 {
		t.Errorf("DeleteMarkers = %d, want 3 — Merge must carry them", a.DeleteMarkers)
	}
	if a.NoncurrentVersions != 2 {
		t.Errorf("NoncurrentVersions = %d, want 2 — Merge must carry them", a.NoncurrentVersions)
	}
}

func TestStorageClassesAreSortedAndCounted(t *testing.T) {
	var a Aggregate
	a.Add(1, "STANDARD")
	a.Add(2, "DEEP_ARCHIVE")
	a.Add(3, "GLACIER")

	got := a.StorageClasses()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"DEEP_ARCHIVE", "GLACIER", "STANDARD"}
	for i, w := range want {
		if got[i].Class != w {
			t.Errorf("[%d].Class = %q, want %q", i, got[i].Class, w)
		}
		if got[i].ObjectCount == nil {
			t.Errorf("[%d].ObjectCount = nil, want a real count — a walk knows this", i)
		}
		if got[i].Overhead {
			t.Errorf("[%d].Overhead = true, want false — a walk cannot see overhead", i)
		}
	}
}
