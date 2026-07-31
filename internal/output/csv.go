package output

import (
	"encoding/csv"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/charlesfused/s3metrics/internal/metrics"
)

type csvRenderer struct{ noHeader bool }

var csvHeader = []string{
	"bucket", "region", "source", "as_of",
	"storage_class", "size_bytes", "object_count", "overhead",
}

// deleteMarkerClass is the pseudo-class the CSV format uses to account for
// delete markers. It is not a real S3 storage class and exists in this file
// only — see classLines.
const deleteMarkerClass = "DELETE_MARKER"

// Render emits long-format CSV: one ALL row carrying the totals, then one row
// per storage class. prefix and duration_ms are deliberately absent — they are
// run metadata, and repeating them on every row would corrupt aggregation, and
// so are the three versioning fields for the same reason.
//
// The exception is delete markers, which are not metadata but objects: under
// --include-versions the ALL row counts them and no storage class does, so
// without a row of their own the long format silently stops adding up. CSV is
// the format most likely to be fed to something that checks totals, so it gets
// a synthesised DELETE_MARKER row instead. The two identities are therefore:
//
//	CSV:  sum(class rows) == ALL row
//	JSON: object_count = sum(storage_classes[].object_count) + delete_markers
func (c csvRenderer) Render(w io.Writer, r *metrics.Report) error {
	cw := csv.NewWriter(w)

	if !c.noHeader {
		if err := cw.Write(csvHeader); err != nil {
			return err
		}
	}

	asOf := r.AsOf.UTC().Format(time.RFC3339)
	rows := [][]string{{
		r.Bucket, r.Region, r.Source, asOf, "ALL",
		strconv.FormatInt(r.TotalSizeBytes, 10),
		strconv.FormatInt(r.ObjectCount, 10),
		"false",
	}}

	for _, line := range classLines(r) {
		count := "" // empty field means null, matching the JSON encoding
		if line.ObjectCount != nil {
			count = strconv.FormatInt(*line.ObjectCount, 10)
		}
		rows = append(rows, []string{
			r.Bucket, r.Region, r.Source, asOf, line.Class,
			strconv.FormatInt(line.SizeBytes, 10),
			count,
			strconv.FormatBool(line.Overhead),
		})
	}

	if err := cw.WriteAll(rows); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// classLines returns the report's storage classes plus, when a version-aware
// walk found any, a DELETE_MARKER line sorted in among them.
//
// The synthesised line is built here rather than pushed onto
// Report.StorageClasses, which has to keep meaning actual S3 storage classes:
// JSON and the table both render that slice directly, and neither should claim
// a class that does not exist. A nil DeleteMarkers means the run never looked,
// and a zero means it looked and found none — neither warrants a row.
func classLines(r *metrics.Report) []metrics.StorageClassStat {
	if r.DeleteMarkers == nil || *r.DeleteMarkers == 0 {
		return r.StorageClasses
	}

	lines := make([]metrics.StorageClassStat, len(r.StorageClasses), len(r.StorageClasses)+1)
	copy(lines, r.StorageClasses)
	lines = append(lines, metrics.StorageClassStat{
		Class:       deleteMarkerClass,
		SizeBytes:   0, // a delete marker carries no data
		ObjectCount: r.DeleteMarkers,
	})

	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Class < lines[j].Class })
	return lines
}
