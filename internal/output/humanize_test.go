package output

import "testing"

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
		{10995116277760, "10.0 TiB"},
		{1125899906842624, "1.0 PiB"},
		{1048575, "1.0 MiB"},
		{1073741823, "1.0 GiB"},
		{1099511627775, "1.0 TiB"},
		{1125899906842623, "1.0 PiB"},
		{9223372036854775807, "8.0 EiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
