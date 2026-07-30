// Package output renders a metrics report. Renderers know nothing about AWS;
// they take a struct and produce bytes.
package output

import (
	"io"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

// Format names a supported output encoding.
const (
	FormatJSON  = "json"
	FormatCSV   = "csv"
	FormatTable = "table"
)

// Renderer writes a report in one encoding.
type Renderer interface {
	Render(w io.Writer, r *metrics.Report) error
}

// New returns the renderer for format. noHeader applies to CSV only; the caller
// is responsible for rejecting it against other formats (see internal/cli).
func New(format string, noHeader bool) (Renderer, error) {
	switch format {
	case FormatJSON:
		return jsonRenderer{}, nil
	case FormatCSV:
		return csvRenderer{noHeader: noHeader}, nil
	case FormatTable:
		return tableRenderer{}, nil
	default:
		return nil, errs.New(errs.CodeUsage,
			"unknown --format value: "+format,
			"valid values are json, csv, table")
	}
}
