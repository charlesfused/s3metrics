package walksource

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 serves a flat key->size map as if it were a bucket, honouring Prefix,
// Delimiter, ContinuationToken, and a page size, so pagination and shard
// discovery are exercised the way the real API would exercise them.
type fakeS3 struct {
	keys     map[string]int64
	classes  map[string]string // key -> storage class; absent means the field is nil
	pageSize int
	err      error
	calls    int
}

func newFakeS3(keys map[string]int64) *fakeS3 {
	return &fakeS3{keys: keys, classes: map[string]string{}, pageSize: 1000}
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input,
	_ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.calls++
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

	d, err := discoverShards(context.Background(), f, "bucket", "", 8)
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

	d, err := discoverShards(context.Background(), f, "bucket", "", 8)
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

	d, err := discoverShards(context.Background(), f, "bucket", "", 8)
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

	d, err := discoverShards(context.Background(), f, "bucket", "", 8)
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
		"root.txt":         1,
		"data/direct.txt":  2,
		"data/2025/a.txt":  4,
		"data/2026/b.txt":  8,
	})

	d, err := discoverShards(context.Background(), f, "bucket", "", 8)
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
		"other/x.txt":    1,
		"data/2025/a.txt": 2,
		"data/2026/b.txt": 4,
	})

	d, err := discoverShards(context.Background(), f, "bucket", "data/", 8)
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

func TestDiscoverPaginates(t *testing.T) {
	keys := map[string]int64{}
	for i := 0; i < 250; i++ {
		keys[string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"] = 1
	}
	f := newFakeS3(keys)
	f.pageSize = 10

	d, err := discoverShards(context.Background(), f, "bucket", "", 8)
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

	if _, err := discoverShards(context.Background(), f, "bucket", "", 8); err == nil {
		t.Fatal("discoverShards() error = nil, want the API error propagated")
	}
}
