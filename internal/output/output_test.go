package output

import (
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

func sampleReport() *metrics.Report {
	return &metrics.Report{
		Bucket:         "mybucket",
		Region:         "us-east-1",
		Source:         metrics.SourceCloudWatch,
		AsOf:           time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		TotalSizeBytes: 10995116277760,
		ObjectCount:    1500000,
		DurationMS:     812,
		StorageClasses: []metrics.StorageClassStat{
			{Class: "GLACIER", SizeBytes: 1099511627776},
			{Class: "GLACIER_OBJECT_OVERHEAD", SizeBytes: 48000000000, Overhead: true},
			{Class: "STANDARD", SizeBytes: 9895604649984},
		},
	}
}

// walkReport is what walk mode actually produces: a prefix scope, and a real
// per-class object count on every row. sampleReport covers neither — CloudWatch
// leaves ObjectCount nil and never sets Prefix — so without this fixture the
// precise mode's rendered output is not pinned anywhere.
func walkReport() *metrics.Report {
	i64 := func(n int64) *int64 { return &n }
	return &metrics.Report{
		Bucket: "walkbucket",
		Region: "us-west-2",
		Source: metrics.SourceWalk,
		AsOf:   time.Date(2026, 7, 29, 9, 15, 0, 0, time.UTC),
		// 8 GiB + 1 GiB + 512 MiB, all non-overhead: walk mode never reports
		// overhead, so the total is simply the sum of the classes.
		TotalSizeBytes: 10200547328,
		ObjectCount:    4210,
		DurationMS:     48213,
		Prefix:         "logs/2026/",
		StorageClasses: []metrics.StorageClassStat{
			{Class: "GLACIER_IR", SizeBytes: 536870912, ObjectCount: i64(10)},
			{Class: "STANDARD", SizeBytes: 8589934592, ObjectCount: i64(3900)},
			{Class: "STANDARD_IA", SizeBytes: 1073741824, ObjectCount: i64(300)},
		},
	}
}

// versionedWalkReport is what `--mode walk --include-versions` produces on a
// versioned bucket: every noncurrent version and delete marker counted. It pins
// the three fields Task 18 added, and the reconciliation identity between them —
// 640 + 260 + 100 classed objects plus 4200 delete markers is the 5200 total,
// because a delete marker belongs to no storage class.
func versionedWalkReport() *metrics.Report {
	i64 := func(n int64) *int64 { return &n }
	yes := true
	return &metrics.Report{
		Bucket:             "versionedbucket",
		Region:             "eu-central-1",
		Source:             metrics.SourceWalk,
		AsOf:               time.Date(2026, 7, 29, 18, 30, 0, 0, time.UTC),
		Versioned:          &yes,
		TotalSizeBytes:     3221225472,
		ObjectCount:        5200,
		DeleteMarkers:      i64(4200),
		NoncurrentVersions: i64(900),
		DurationMS:         91044,
		StorageClasses: []metrics.StorageClassStat{
			{Class: "GLACIER", SizeBytes: 1073741824, ObjectCount: i64(260)},
			{Class: "STANDARD", SizeBytes: 2147483648, ObjectCount: i64(640)},
			{Class: "STANDARD_IA", SizeBytes: 0, ObjectCount: i64(100)},
		},
	}
}

func emptyReport() *metrics.Report {
	return &metrics.Report{
		Bucket:         "emptybucket",
		Region:         "eu-west-1",
		Source:         metrics.SourceWalk,
		AsOf:           time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		DurationMS:     15,
		StorageClasses: []metrics.StorageClassStat{},
	}
}

func assertGolden(t *testing.T, got, goldenName string) {
	t.Helper()
	path := filepath.Join("testdata", goldenName)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v", path, err)
	}
	if got != string(want) {
		t.Errorf("output mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", goldenName, got, want)
	}
}

func render(t *testing.T, format string, noHeader bool, r *metrics.Report) string {
	t.Helper()
	rend, err := New(format, noHeader)
	if err != nil {
		t.Fatalf("New(%q) error = %v", format, err)
	}
	var buf bytes.Buffer
	if err := rend.Render(&buf, r); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func TestRenderGolden(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		noHeader bool
		report   *metrics.Report
		golden   string
	}{
		{"json", "json", false, sampleReport(), "report.json"},
		{"csv", "csv", false, sampleReport(), "report.csv"},
		{"csv no header", "csv", true, sampleReport(), "report_noheader.csv"},
		{"table", "table", false, sampleReport(), "report.txt"},
		{"json walk", "json", false, walkReport(), "walk.json"},
		{"csv walk", "csv", false, walkReport(), "walk.csv"},
		{"table walk", "table", false, walkReport(), "walk.txt"},
		{"json versioned walk", "json", false, versionedWalkReport(), "versioned.json"},
		{"csv versioned walk", "csv", false, versionedWalkReport(), "versioned.csv"},
		{"table versioned walk", "table", false, versionedWalkReport(), "versioned.txt"},
		{"json empty", "json", false, emptyReport(), "empty.json"},
		{"csv empty", "csv", false, emptyReport(), "empty.csv"},
		{"table empty", "table", false, emptyReport(), "empty.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGolden(t, render(t, tt.format, tt.noHeader, tt.report), tt.golden)
		})
	}
}

func TestNewRejectsUnknownFormat(t *testing.T) {
	_, err := New("yaml", false)
	if err == nil {
		t.Fatal("New(\"yaml\") error = nil, want a usage error")
	}

	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("New(\"yaml\") error = %T, want *errs.Error", err)
	}
	if e.Code != errs.CodeUsage {
		t.Errorf("error code = %s, want %s", e.Code, errs.CodeUsage)
	}
}

// CSV stays a per-storage-class table. versioned, delete_markers, and
// noncurrent_versions are run metadata like prefix and duration_ms: repeating
// them on every row would corrupt any aggregation done over the file.
func TestCSVOmitsRunMetadataFields(t *testing.T) {
	got := render(t, "csv", false, versionedWalkReport())

	// The header names the columns, so it is the honest place to assert. The
	// bucket in this fixture is called versionedbucket, which a substring search
	// over the whole document would happily match.
	header, _, ok := strings.Cut(got, "\n")
	if !ok {
		t.Fatalf("csv output has no header row:\n%s", got)
	}
	for _, absent := range []string{"versioned", "delete_markers", "noncurrent_versions"} {
		for _, col := range strings.Split(header, ",") {
			if col == absent {
				t.Errorf("csv header contains column %q, want it omitted: %s", absent, header)
			}
		}
	}

	// And no row may have grown: eight columns, as before.
	for i, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if n := len(strings.Split(line, ",")); n != 8 {
			t.Errorf("csv row %d has %d columns, want 8: %s", i, n, line)
		}
	}
}

// Under --include-versions the ALL row counts delete markers while no storage
// class does, so without a DELETE_MARKER row the long format stops summing — and
// CSV is the format most likely to be fed to something that checks totals. The
// row is synthesised by the renderer, never added to Report.StorageClasses,
// which has to keep meaning real S3 storage classes for JSON and the table.
func TestCSVClassRowsSumToTheALLRow(t *testing.T) {
	rows := parseCSV(t, render(t, "csv", false, versionedWalkReport()))

	var (
		allSize, allCount     int64
		classSize, classCount int64
		sawDeleteMarker       bool
	)
	for _, row := range rows {
		size, err := strconv.ParseInt(row["size_bytes"], 10, 64)
		if err != nil {
			t.Fatalf("size_bytes %q: %v", row["size_bytes"], err)
		}
		count, err := strconv.ParseInt(row["object_count"], 10, 64)
		if err != nil {
			t.Fatalf("object_count %q: %v", row["object_count"], err)
		}

		if row["storage_class"] == "ALL" {
			allSize, allCount = size, count
			continue
		}
		if row["storage_class"] == "DELETE_MARKER" {
			sawDeleteMarker = true
			if size != 0 {
				t.Errorf("DELETE_MARKER size_bytes = %d, want 0 — a marker carries no data", size)
			}
			if row["overhead"] != "false" {
				t.Errorf("DELETE_MARKER overhead = %q, want false", row["overhead"])
			}
		}
		classSize += size
		classCount += count
	}

	if !sawDeleteMarker {
		t.Fatal("no DELETE_MARKER row, so the class rows cannot account for the ALL row")
	}
	if classCount != allCount {
		t.Errorf("class rows total %d objects, ALL row says %d", classCount, allCount)
	}
	if classSize != allSize {
		t.Errorf("class rows total %d bytes, ALL row says %d", classSize, allSize)
	}
}

// The row is a CSV-only device. Adding it to the report would make JSON and the
// table claim a storage class that does not exist in S3.
func TestDeleteMarkerRowIsAbsentFromJSONAndTable(t *testing.T) {
	for _, format := range []string{"json", "table"} {
		if got := render(t, format, false, versionedWalkReport()); strings.Contains(got, "DELETE_MARKER") {
			t.Errorf("%s output contains a DELETE_MARKER class row:\n%s", format, got)
		}
	}
}

// Without --include-versions there is nothing to account for, so the row must
// not appear at all — a 0 would claim a measurement the run never made.
func TestCSVOmitsDeleteMarkerRowWhenUnknown(t *testing.T) {
	if got := render(t, "csv", false, walkReport()); strings.Contains(got, "DELETE_MARKER") {
		t.Errorf("csv output contains a DELETE_MARKER row for a walk that never counted them:\n%s", got)
	}
}

// parseCSV turns rendered CSV into header-keyed rows.
func parseCSV(t *testing.T, in string) []map[string]string {
	t.Helper()
	records, err := csv.NewReader(strings.NewReader(in)).ReadAll()
	if err != nil {
		t.Fatalf("parsing csv: %v\n%s", err, in)
	}
	if len(records) < 2 {
		t.Fatalf("csv has %d records, want a header and at least one row", len(records))
	}

	header := records[0]
	out := make([]map[string]string, 0, len(records)-1)
	for _, rec := range records[1:] {
		row := make(map[string]string, len(header))
		for i, col := range header {
			row[col] = rec[i]
		}
		out = append(out, row)
	}
	return out
}

// A nil field means "not measured" and must render as a dash-free absence in the
// table rather than as a zero, which would be a different claim entirely.
func TestTableOmitsUnknownVersioningFields(t *testing.T) {
	got := render(t, "table", false, walkReport())

	for _, absent := range []string{"Versioned", "Delete markers", "Noncurrent versions"} {
		if strings.Contains(got, absent) {
			t.Errorf("table output contains %q for a report that never measured it:\n%s", absent, got)
		}
	}
}

func TestJSONEndsWithNewline(t *testing.T) {
	got := render(t, "json", false, sampleReport())
	if got == "" || got[len(got)-1] != '\n' {
		t.Error("JSON output must end with a newline so it composes with line-oriented tools")
	}
}
