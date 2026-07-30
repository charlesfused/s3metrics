package output

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/charlesfused/s3metrics/internal/metrics"
)

type tableRenderer struct{}

// Render produces the only human-facing format, so it humanizes sizes. The
// summary block and the per-class table are written through separate tabwriters
// so their column widths are computed independently.
func (tableRenderer) Render(w io.Writer, r *metrics.Report) error {
	summary := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(summary, "Bucket\t%s\n", r.Bucket)
	fmt.Fprintf(summary, "Region\t%s\n", r.Region)
	fmt.Fprintf(summary, "Source\t%s\n", r.Source)
	// The three versioning fields are printed only when they are known. A row
	// reading "0" would claim a measurement that a metrics run, or a walk
	// without --include-versions, never made.
	if r.Versioned != nil {
		fmt.Fprintf(summary, "Versioned\t%s\n", yesNo(*r.Versioned))
	}
	fmt.Fprintf(summary, "As of\t%s\n", r.AsOf.UTC().Format(time.RFC3339))
	fmt.Fprintf(summary, "Total size\t%s\n", humanBytes(r.TotalSizeBytes))
	fmt.Fprintf(summary, "Objects\t%d\n", r.ObjectCount)
	// Delete markers carry no bytes and no storage class, so they show up in the
	// object count and in none of the rows below. Naming them here is what stops
	// that looking like an arithmetic bug.
	if r.DeleteMarkers != nil {
		fmt.Fprintf(summary, "Delete markers\t%d\n", *r.DeleteMarkers)
	}
	if r.NoncurrentVersions != nil {
		fmt.Fprintf(summary, "Noncurrent versions\t%d\n", *r.NoncurrentVersions)
	}
	if r.Prefix != "" {
		fmt.Fprintf(summary, "Prefix\t%s\n", r.Prefix)
	}
	fmt.Fprintf(summary, "Duration\t%dms\n", r.DurationMS)
	if err := summary.Flush(); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	classes := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(classes, "STORAGE CLASS\tSIZE\tOBJECTS\tOVERHEAD")
	for _, sc := range r.StorageClasses {
		count := "-" // nil object count renders as a dash, not a zero
		if sc.ObjectCount != nil {
			count = fmt.Sprintf("%d", *sc.ObjectCount)
		}
		fmt.Fprintf(classes, "%s\t%s\t%s\t%s\n",
			sc.Class, humanBytes(sc.SizeBytes), count, yesNo(sc.Overhead))
	}
	return classes.Flush()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
