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
