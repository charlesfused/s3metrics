package output

import (
	"encoding/json"
	"io"

	"github.com/charlesfused/s3metrics/internal/metrics"
)

type jsonRenderer struct{}

func (jsonRenderer) Render(w io.Writer, r *metrics.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Encode appends a trailing newline.
	return enc.Encode(r)
}
