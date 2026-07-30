package walksource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/charlesfused/s3metrics/internal/progress"
)

// fakeVersion is one entry in a key's version history.
//
// Histories are ordered newest-first, as S3 orders them: index 0 is the current
// version and the only entry with IsLatest set. A history whose head is a delete
// marker describes a key that reads as deleted — it has no current object, so it
// appears in the version listing and not in the ListObjectsV2 one.
type fakeVersion struct {
	size         int64
	class        string
	deleteMarker bool
}

// fakeS3 serves a flat key->size map as if it were a bucket, honouring Prefix,
// Delimiter, ContinuationToken, and a page size, so pagination and shard
// discovery are exercised the way the real API would exercise them.
//
// It serves ListObjectVersions from the same fixture, so one bucket can be
// walked both ways and the two answers compared. Task 18 needs that: the
// version-aware walk must see everything the current-only walk sees, plus the
// noncurrent versions and delete markers.
//
// calls is atomic because Task 12's collector calls ListObjectsV2 from
// multiple worker goroutines concurrently — discoverShards (Task 11) never
// did, so this fixture was never exercised concurrently until now.
type fakeS3 struct {
	keys    map[string]int64
	classes map[string]string // key -> storage class; absent means the field is nil

	// versions is the full history per key. Nil means "derive one current
	// version per key from keys", which makes any existing fixture walkable
	// through either API with the same answer.
	versions map[string][]fakeVersion

	pageSize int
	err      error
	calls    atomic.Int64
}

func newFakeS3(keys map[string]int64) *fakeS3 {
	return &fakeS3{keys: keys, classes: map[string]string{}, pageSize: 1000}
}

// newFakeVersions builds a bucket from explicit version histories, deriving the
// current-version view so both listings describe the same bucket.
func newFakeVersions(history map[string][]fakeVersion) *fakeS3 {
	f := &fakeS3{
		keys:     map[string]int64{},
		classes:  map[string]string{},
		versions: history,
		pageSize: 1000,
	}
	for k, vs := range history {
		if len(vs) == 0 || vs[0].deleteMarker {
			continue // current state of the key is "deleted"
		}
		f.keys[k] = vs[0].size
		if vs[0].class != "" {
			f.classes[k] = vs[0].class
		}
	}
	return f
}

// history returns the per-key version histories, deriving a single current
// version per key when the fixture did not specify any.
func (f *fakeS3) history() map[string][]fakeVersion {
	if f.versions != nil {
		return f.versions
	}
	h := make(map[string][]fakeVersion, len(f.keys))
	for k, size := range f.keys {
		h[k] = []fakeVersion{{size: size, class: f.classes[k]}}
	}
	return h
}

// verEntry is one row of a ListObjectVersions response: a common prefix, or one
// version of one key.
type verEntry struct {
	name      string // the key, or the common prefix when isPrefix
	versionID string
	isPrefix  bool
	v         fakeVersion
}

// versionEntries flattens the fixture into the ordering S3 lists it in: by key,
// then newest version first. Version ids are generated so that their
// lexicographic order matches history order, which is what lets the marker
// comparison below be a simple string compare.
func (f *fakeS3) versionEntries(prefix, delim string) []verEntry {
	hist := f.history()

	keys := make([]string, 0, len(hist))
	for k := range hist {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var entries []verEntry
	seen := map[string]bool{}
	for _, k := range keys {
		if delim != "" {
			rest := k[len(prefix):]
			if i := strings.Index(rest, delim); i >= 0 {
				cp := prefix + rest[:i+len(delim)]
				if !seen[cp] {
					seen[cp] = true
					entries = append(entries, verEntry{name: cp, isPrefix: true})
				}
				continue
			}
		}
		for i, v := range hist[k] {
			entries = append(entries, verEntry{
				name:      k,
				versionID: fmt.Sprintf("v%03d", i),
				v:         v,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return entries[i].versionID < entries[j].versionID
	})
	return entries
}

// ListObjectVersions serves the fixture's full version history.
//
// Marker semantics match the real API's: NextKeyMarker and NextVersionIdMarker
// name the *last entry returned*, and a request carrying them resumes strictly
// after it. Two consequences the implementation has to live with, and which this
// fake therefore enforces:
//
//   - A page that ends mid-key can only be resumed with both markers. Sent the
//     key marker alone, S3 skips the rest of that key's history and moves to the
//     next key — the versions in between are silently lost, with no error.
//   - A page that ends on a common prefix carries no version marker at all, so
//     the key marker alone has to be enough in that case.
func (f *fakeS3) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput,
	_ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}

	entries := f.versionEntries(aws.ToString(in.Prefix), aws.ToString(in.Delimiter))

	start := 0
	if km := aws.ToString(in.KeyMarker); km != "" {
		vm := aws.ToString(in.VersionIdMarker)
		start = len(entries)
		for i, e := range entries {
			// Strictly after (km, vm). With no version marker the whole of km's
			// group — every remaining version, or the whole common prefix — is
			// skipped, which is exactly how versions go missing for real.
			if e.name > km || (e.name == km && vm != "" && e.versionID > vm) {
				start = i
				break
			}
		}
	}

	end := start + f.pageSize
	truncated := end < len(entries)
	if end > len(entries) {
		end = len(entries)
	}

	out := &s3.ListObjectVersionsOutput{IsTruncated: aws.Bool(truncated)}
	if truncated {
		last := entries[end-1]
		out.NextKeyMarker = aws.String(last.name)
		if !last.isPrefix {
			out.NextVersionIdMarker = aws.String(last.versionID)
		}
	}

	for _, e := range entries[start:end] {
		switch {
		case e.isPrefix:
			out.CommonPrefixes = append(out.CommonPrefixes,
				s3types.CommonPrefix{Prefix: aws.String(e.name)})
		case e.v.deleteMarker:
			out.DeleteMarkers = append(out.DeleteMarkers, s3types.DeleteMarkerEntry{
				Key:       aws.String(e.name),
				VersionId: aws.String(e.versionID),
				IsLatest:  aws.Bool(e.versionID == "v000"),
			})
		default:
			ov := s3types.ObjectVersion{
				Key:       aws.String(e.name),
				VersionId: aws.String(e.versionID),
				Size:      aws.Int64(e.v.size),
				IsLatest:  aws.Bool(e.versionID == "v000"),
			}
			if e.v.class != "" {
				ov.StorageClass = s3types.ObjectVersionStorageClass(e.v.class)
			}
			out.Versions = append(out.Versions, ov)
		}
	}
	return out, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input,
	_ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}

	prefix := aws.ToString(in.Prefix)
	delim := aws.ToString(in.Delimiter)

	var matching []string
	for k := range f.keys {
		if strings.HasPrefix(k, prefix) {
			matching = append(matching, k)
		}
	}
	sort.Strings(matching)

	// Collapse into entries: either a direct object or a common prefix.
	type entry struct {
		name     string
		isPrefix bool
	}
	var entries []entry
	seen := map[string]bool{}
	for _, k := range matching {
		if delim != "" {
			rest := k[len(prefix):]
			if i := strings.Index(rest, delim); i >= 0 {
				cp := prefix + rest[:i+len(delim)]
				if !seen[cp] {
					seen[cp] = true
					entries = append(entries, entry{name: cp, isPrefix: true})
				}
				continue
			}
		}
		entries = append(entries, entry{name: k})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	start := 0
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		for i, e := range entries {
			if e.name == tok {
				start = i
				break
			}
		}
	}
	end := start + f.pageSize
	truncated := end < len(entries)
	if end > len(entries) {
		end = len(entries)
	}

	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(truncated)}
	if truncated {
		out.NextContinuationToken = aws.String(entries[end].name)
	}
	for _, e := range entries[start:end] {
		if e.isPrefix {
			out.CommonPrefixes = append(out.CommonPrefixes,
				s3types.CommonPrefix{Prefix: aws.String(e.name)})
			continue
		}
		obj := s3types.Object{Key: aws.String(e.name), Size: aws.Int64(f.keys[e.name])}
		if c, ok := f.classes[e.name]; ok {
			obj.StorageClass = s3types.ObjectStorageClass(c)
		}
		out.Contents = append(out.Contents, obj)
	}
	return out, nil
}

func TestDiscoverFlatBucketYieldsNoShardsAndCountsLoose(t *testing.T) {
	f := newFakeS3(map[string]int64{"a.txt": 10, "b.txt": 20, "c.txt": 30})

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if len(d.Shards) != 0 {
		t.Errorf("Shards = %v, want none for a flat bucket", d.Shards)
	}
	if d.Loose.Objects != 3 || d.Loose.Bytes != 60 {
		t.Errorf("Loose = {Objects:%d Bytes:%d}, want {3 60}", d.Loose.Objects, d.Loose.Bytes)
	}
}

func TestDiscoverWideBucketShardsAtDepthOne(t *testing.T) {
	keys := map[string]int64{}
	for _, p := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		keys[p+"/1.txt"] = 1
	}
	f := newFakeS3(keys)

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if len(d.Shards) != 9 {
		t.Errorf("len(Shards) = %d, want 9 — enough prefixes at depth 1 to stop there", len(d.Shards))
	}
	if d.Loose.Objects != 0 {
		t.Errorf("Loose.Objects = %d, want 0 — every key is under a prefix", d.Loose.Objects)
	}
}

func TestDiscoverNarrowTopLevelDescendsToDepthTwo(t *testing.T) {
	// One top-level prefix, four below it. Depth 1 alone would give a single
	// shard and no parallelism at all; this is the layout depth 2 rescues.
	keys := map[string]int64{}
	for _, y := range []string{"2023", "2024", "2025", "2026"} {
		keys["data/"+y+"/x.txt"] = 1
	}
	f := newFakeS3(keys)

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if len(d.Shards) != 4 {
		t.Errorf("Shards = %v, want the four data/YYYY/ prefixes", d.Shards)
	}
	for _, s := range d.Shards {
		if !strings.HasPrefix(s, "data/") || strings.Count(s, "/") != 2 {
			t.Errorf("shard %q is not a depth-2 prefix", s)
		}
	}
}

func TestDiscoverStopsAtDepthTwo(t *testing.T) {
	// Three levels deep with one prefix each. Discovery must not keep descending.
	f := newFakeS3(map[string]int64{"a/b/c/x.txt": 1})

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	for _, s := range d.Shards {
		if strings.Count(s, "/") > 2 {
			t.Errorf("shard %q is deeper than depth 2", s)
		}
	}
}

func TestDiscoverKeepsLooseObjectsFromEveryLevel(t *testing.T) {
	// Loose objects at the root AND directly under the single top-level prefix.
	// Descending replaces data/ as a shard, so its direct children would be lost
	// unless discovery banks them as loose.
	f := newFakeS3(map[string]int64{
		"root.txt":        1,
		"data/direct.txt": 2,
		"data/2025/a.txt": 4,
		"data/2026/b.txt": 8,
	})

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if d.Loose.Objects != 2 || d.Loose.Bytes != 3 {
		t.Errorf("Loose = {Objects:%d Bytes:%d}, want {2 3} — root.txt and data/direct.txt",
			d.Loose.Objects, d.Loose.Bytes)
	}
}

func TestDiscoverHonoursRootPrefix(t *testing.T) {
	f := newFakeS3(map[string]int64{
		"other/x.txt":     1,
		"data/2025/a.txt": 2,
		"data/2026/b.txt": 4,
	})

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "data/", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	for _, s := range d.Shards {
		if !strings.HasPrefix(s, "data/") {
			t.Errorf("shard %q escaped the --prefix scope", s)
		}
	}
	if d.Loose.Objects != 0 {
		t.Errorf("Loose.Objects = %d, want 0", d.Loose.Objects)
	}
}

func TestDiscoverKeepsParentShardsWhenDescentFindsNothing(t *testing.T) {
	// Two top-level prefixes, files directly inside them and no deeper nesting.
	// Descending finds no grandchildren, so discovery must keep a/ and b/ as
	// shards rather than dissolving them into loose objects — otherwise the walk
	// gets no parallelism at all on a very common layout.
	f := newFakeS3(map[string]int64{
		"a/1.txt": 1, "a/2.txt": 2,
		"b/1.txt": 4, "b/2.txt": 8,
	})

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if len(d.Shards) != 2 {
		t.Errorf("Shards = %v, want the two top-level prefixes kept", d.Shards)
	}
	if d.Loose.Objects != 0 {
		t.Errorf("Loose.Objects = %d, want 0 — those objects belong to the shards",
			d.Loose.Objects)
	}
}

func TestDiscoverKeepsFlatSiblingsAsShards(t *testing.T) {
	// a/ is nested, b/ is flat. Descending must not dissolve b/ into loose
	// objects just because its sibling had children.
	f := newFakeS3(map[string]int64{
		"a/x/1.txt": 1, "a/y/1.txt": 1,
		"b/1.txt": 1, "b/2.txt": 1, "b/3.txt": 1,
		"root.txt": 1,
	})

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}

	var sawB bool
	for _, s := range d.Shards {
		if s == "b/" {
			sawB = true
		}
	}
	if !sawB {
		t.Errorf("Shards = %v, want b/ kept as a shard", d.Shards)
	}
	if d.Loose.Objects != 1 {
		t.Errorf("Loose.Objects = %d, want 1 (root.txt only)", d.Loose.Objects)
	}
}

func TestDiscoverCountsProgressWhileListing(t *testing.T) {
	// A flat bucket is listed entirely inside discovery — walkShards never runs
	// — so if discovery does not report, the reporter shows 0 for the whole run.
	f := newFakeS3(map[string]int64{"a.txt": 1, "b.txt": 2, "c.txt": 3})
	rep := progress.New(io.Discard, true, time.Hour)

	if _, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, rep); err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if got := rep.Objects(); got != 3 {
		t.Errorf("Reporter.Objects() = %d, want 3 — discovery must report what it lists", got)
	}
}

func TestDiscoverPaginates(t *testing.T) {
	keys := map[string]int64{}
	for i := 0; i < 250; i++ {
		keys[string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"] = 1
	}
	f := newFakeS3(keys)
	f.pageSize = 10

	d, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil)
	if err != nil {
		t.Fatalf("discoverShards() error = %v", err)
	}
	if d.Loose.Objects != int64(len(keys)) {
		t.Errorf("Loose.Objects = %d, want %d — pagination dropped keys", d.Loose.Objects, len(keys))
	}
}

func TestDiscoverPropagatesError(t *testing.T) {
	f := newFakeS3(nil)
	f.err = errors.New("boom")

	if _, err := discoverShards(context.Background(), currentSource{api: f}, "bucket", "", 8, nil); err == nil {
		t.Fatal("discoverShards() error = nil, want the API error propagated")
	}
}
