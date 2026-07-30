package walksource

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/charlesfused/s3metrics/internal/progress"
)

// versionHistory is the fixture the version-source tests share: one key with
// three versions and a delete marker on top, one plain current-only key, and one
// key whose entire history is a delete marker.
func versionHistory() map[string][]fakeVersion {
	return map[string][]fakeVersion{
		"a.txt": {
			{deleteMarker: true},
			{size: 100},
			{size: 50, class: "GLACIER"},
			{size: 25},
		},
		"b.txt": {{size: 7}},
		"c.txt": {{deleteMarker: true}},
	}
}

func TestVersionSourceCountsVersionsAndDeleteMarkers(t *testing.T) {
	f := newFakeVersions(versionHistory())

	agg, err := versionSource{api: f}.walkPrefix(context.Background(), "bucket", "", nil)
	if err != nil {
		t.Fatalf("walkPrefix() error = %v", err)
	}

	// 4 entries under a.txt, 1 under b.txt, 1 under c.txt.
	if agg.Objects != 6 {
		t.Errorf("Objects = %d, want 6 — every version and marker counts", agg.Objects)
	}
	if agg.Bytes != 182 { // 100 + 50 + 25 + 7; markers carry none
		t.Errorf("Bytes = %d, want 182", agg.Bytes)
	}
	if agg.DeleteMarkers != 2 {
		t.Errorf("DeleteMarkers = %d, want 2", agg.DeleteMarkers)
	}
	// a.txt's three object versions are all noncurrent (a marker is on top);
	// b.txt's single version is current.
	if agg.NoncurrentVersions != 3 {
		t.Errorf("NoncurrentVersions = %d, want 3", agg.NoncurrentVersions)
	}
	if got := agg.Classes["GLACIER"]; got.Bytes != 50 || got.Objects != 1 {
		t.Errorf("GLACIER = %+v, want {Bytes:50 Objects:1} — a noncurrent version keeps its class", got)
	}
}

// A delete marker has no storage class, so no class row may absorb it. This is
// what makes the README's reconciliation identity necessary in the first place.
func TestVersionSourceDeleteMarkersAreInNoStorageClass(t *testing.T) {
	f := newFakeVersions(versionHistory())

	agg, err := versionSource{api: f}.walkPrefix(context.Background(), "bucket", "", nil)
	if err != nil {
		t.Fatalf("walkPrefix() error = %v", err)
	}

	var classed int64
	for _, sc := range agg.StorageClasses() {
		classed += *sc.ObjectCount
	}
	if classed != 4 {
		t.Errorf("sum of class counts = %d, want 4 — the two markers belong to no class", classed)
	}
	if classed+agg.DeleteMarkers != agg.Objects {
		t.Errorf("sum(classes)=%d + markers=%d != Objects=%d", classed, agg.DeleteMarkers, agg.Objects)
	}
}

// The pagination trap: a key with more versions than fit in one page must be
// resumed with both KeyMarker and VersionIdMarker. The fake refuses a mid-key
// resume that carries only the key marker, so an implementation that forgets the
// version marker fails here rather than silently miscounting a real bucket.
func TestVersionSourceCarriesBothPaginationMarkers(t *testing.T) {
	history := map[string][]fakeVersion{"big.txt": {}}
	for i := 0; i < 25; i++ {
		history["big.txt"] = append(history["big.txt"], fakeVersion{size: 1})
	}
	f := newFakeVersions(history)
	f.pageSize = 4 // forces six mid-key resumes

	agg, err := versionSource{api: f}.walkPrefix(context.Background(), "bucket", "", nil)
	if err != nil {
		t.Fatalf("walkPrefix() error = %v", err)
	}
	if agg.Objects != 25 || agg.Bytes != 25 {
		t.Errorf("got {%d objects, %d bytes}, want {25, 25} — pagination lost or repeated versions",
			agg.Objects, agg.Bytes)
	}
	if agg.NoncurrentVersions != 24 {
		t.Errorf("NoncurrentVersions = %d, want 24", agg.NoncurrentVersions)
	}
}

func TestVersionSourceListLevelSplitsPrefixesFromEntries(t *testing.T) {
	f := newFakeVersions(map[string][]fakeVersion{
		"root.txt":   {{size: 1}, {size: 2}},
		"data/a.txt": {{deleteMarker: true}, {size: 4}},
		"logs/b.txt": {{size: 8}},
	})

	prefixes, loose, err := versionSource{api: f}.listLevel(context.Background(), "bucket", "", nil)
	if err != nil {
		t.Fatalf("listLevel() error = %v", err)
	}
	if len(prefixes) != 2 {
		t.Errorf("prefixes = %v, want data/ and logs/", prefixes)
	}
	// Only root.txt sits at this level, and it has two versions.
	if loose.Objects != 2 || loose.Bytes != 3 {
		t.Errorf("loose = {Objects:%d Bytes:%d}, want {2 3}", loose.Objects, loose.Bytes)
	}
	if loose.NoncurrentVersions != 1 {
		t.Errorf("loose.NoncurrentVersions = %d, want 1", loose.NoncurrentVersions)
	}
}

// Discovery must report what it lists for the same reason the current source
// does: a flat versioned bucket is listed entirely inside discovery, so without
// this the reporter sits at zero for the slowest run the tool has.
func TestVersionSourceListLevelReportsProgress(t *testing.T) {
	f := newFakeVersions(versionHistory())
	rep := progress.New(io.Discard, true, time.Hour)

	if _, _, err := (versionSource{api: f}).listLevel(context.Background(), "bucket", "", rep); err != nil {
		t.Fatalf("listLevel() error = %v", err)
	}
	if got := rep.Objects(); got != 6 {
		t.Errorf("Reporter.Objects() = %d, want 6 — versions and markers both count", got)
	}
}

func TestVersionSourcePropagatesError(t *testing.T) {
	f := newFakeVersions(nil)
	f.err = errors.New("boom")

	if _, err := (versionSource{api: f}).walkPrefix(context.Background(), "bucket", "", nil); err == nil {
		t.Fatal("walkPrefix() error = nil, want the API error propagated")
	}
	if _, _, err := (versionSource{api: f}).listLevel(context.Background(), "bucket", "", nil); err == nil {
		t.Fatal("listLevel() error = nil, want the API error propagated")
	}
}

// Discovery and the walk must use the same source. If discovery kept listing
// current versions while the walk listed every version, loose objects would be
// counted one way and sharded objects another — a silent undercount that no
// single-shape fixture would catch.
func TestDiscoveryUsesTheVersionSourceToo(t *testing.T) {
	f := newFakeVersions(map[string][]fakeVersion{
		// Loose at the root, with history — this is what discovery counts itself.
		"root.txt": {{deleteMarker: true}, {size: 1}, {size: 2}},
		"a/1.txt":  {{size: 4}},
		"b/1.txt":  {{size: 8}},
	})

	d, err := discoverShards(context.Background(), versionSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if d.Loose.Objects != 3 || d.Loose.Bytes != 3 {
		t.Errorf("Loose = {Objects:%d Bytes:%d}, want {3 3} — root.txt's whole history",
			d.Loose.Objects, d.Loose.Bytes)
	}
	if d.Loose.DeleteMarkers != 1 {
		t.Errorf("Loose.DeleteMarkers = %d, want 1", d.Loose.DeleteMarkers)
	}
}
