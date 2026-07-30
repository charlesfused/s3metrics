package output

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	if _, err := New("yaml", false); err == nil {
		t.Fatal("New(\"yaml\") error = nil, want a usage error")
	}
}

func TestJSONEndsWithNewline(t *testing.T) {
	got := render(t, "json", false, sampleReport())
	if got == "" || got[len(got)-1] != '\n' {
		t.Error("JSON output must end with a newline so it composes with line-oriented tools")
	}
}
