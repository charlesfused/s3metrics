package output

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"

	"github.com/charlesfused/s3metrics/internal/metrics"
)

type csvRenderer struct{ noHeader bool }

var csvHeader = []string{
	"bucket", "region", "source", "as_of",
	"storage_class", "size_bytes", "object_count", "overhead",
}

// Render emits long-format CSV: one ALL row carrying the totals, then one row
// per storage class. prefix and duration_ms are deliberately absent — they are
// run metadata, and repeating them on every row would corrupt aggregation.
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

	for _, sc := range r.StorageClasses {
		count := "" // empty field means null, matching the JSON encoding
		if sc.ObjectCount != nil {
			count = strconv.FormatInt(*sc.ObjectCount, 10)
		}
		rows = append(rows, []string{
			r.Bucket, r.Region, r.Source, asOf, sc.Class,
			strconv.FormatInt(sc.SizeBytes, 10),
			count,
			strconv.FormatBool(sc.Overhead),
		})
	}

	if err := cw.WriteAll(rows); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}
