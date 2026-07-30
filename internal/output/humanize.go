package output

import (
	"fmt"
	"math"
)

// humanBytes renders a byte count in binary units for the table format only.
// JSON and CSV always carry raw integers — a machine-readable format must not
// lose precision to rounding.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	units := "KMGTPE"
	value, exp := float64(n)/unit, 0
	for value >= unit && exp < len(units)-1 {
		value /= unit
		exp++
	}

	// %.1f rounds, so a value just under the next boundary (1023.999) would
	// print as "1024.0 KiB" — a reading that cannot exist. Promote it to the
	// next unit instead, matching what the rounded number actually means.
	if math.Round(value*10)/10 >= unit && exp < len(units)-1 {
		value /= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", value, units[exp])
}
