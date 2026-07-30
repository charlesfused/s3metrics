package cli

import (
	"errors"
	"flag"
	"io"
	"testing"
	"time"

	"github.com/charlesfused/s3metrics/internal/errs"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]string{"--bucket", "b"}, io.Discard)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if c.Bucket != "b" {
		t.Errorf("Bucket = %q, want %q", c.Bucket, "b")
	}
	if c.Mode != ModeMetrics {
		t.Errorf("Mode = %q, want %q", c.Mode, ModeMetrics)
	}
	if c.Format != "json" {
		t.Errorf("Format = %q, want json", c.Format)
	}
	if c.Concurrency != 8 {
		t.Errorf("Concurrency = %d, want 8", c.Concurrency)
	}
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0", c.Timeout)
	}
}

func TestParseAcceptsBothDashStyles(t *testing.T) {
	for _, args := range [][]string{
		{"-bucket", "b", "-format", "csv"},
		{"--bucket", "b", "--format", "csv"},
	} {
		c, err := Parse(args, io.Discard)
		if err != nil {
			t.Fatalf("Parse(%v) error = %v", args, err)
		}
		if c.Format != "csv" {
			t.Errorf("Parse(%v) Format = %q, want csv", args, c.Format)
		}
	}
}

func TestParseWalkFlags(t *testing.T) {
	c, err := Parse([]string{
		"--bucket", "b", "--mode", "walk",
		"--prefix", "data/", "--concurrency", "16",
		"--timeout", "5m",
	}, io.Discard)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if c.Prefix != "data/" {
		t.Errorf("Prefix = %q", c.Prefix)
	}
	if c.Concurrency != 16 {
		t.Errorf("Concurrency = %d, want 16", c.Concurrency)
	}
	if c.Timeout != 5*time.Minute {
		t.Errorf("Timeout = %v, want 5m", c.Timeout)
	}
}

func TestParseRejections(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing bucket", []string{}},
		{"prefix with metrics mode", []string{"--bucket", "b", "--prefix", "data/"}},
		{"concurrency with metrics mode", []string{"--bucket", "b", "--concurrency", "4"}},
		{"no-header with json", []string{"--bucket", "b", "--no-header"}},
		{"no-header with table", []string{"--bucket", "b", "--format", "table", "--no-header"}},
		{"zero concurrency", []string{"--bucket", "b", "--mode", "walk", "--concurrency", "0"}},
		{"negative concurrency", []string{"--bucket", "b", "--mode", "walk", "--concurrency", "-1"}},
		{"unknown mode", []string{"--bucket", "b", "--mode", "scan"}},
		{"unknown format", []string{"--bucket", "b", "--format", "yaml"}},
		{"negative timeout", []string{"--bucket", "b", "--timeout", "-1s"}},
		{"two update actions", []string{"--check-update", "--self-update"}},
		{"version with self-update", []string{"--version", "--self-update"}},
		{"positional argument", []string{"--bucket", "b", "extra"}},
		{"unknown flag", []string{"--bucket", "b", "--nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.args, io.Discard)
			if err == nil {
				t.Fatalf("Parse(%v) error = nil, want a usage error", tt.args)
			}
			if got := errs.ExitCode(err); got != 2 {
				t.Errorf("Parse(%v) exit code = %d, want 2", tt.args, got)
			}
		})
	}
}

func TestParseUpdateActionsDoNotNeedBucket(t *testing.T) {
	for _, args := range [][]string{
		{"--version"},
		{"--check-update"},
		{"--self-update"},
	} {
		if _, err := Parse(args, io.Discard); err != nil {
			t.Errorf("Parse(%v) error = %v, want nil", args, err)
		}
	}
}

func TestParseHelpReturnsErrHelp(t *testing.T) {
	_, err := Parse([]string{"--help"}, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("Parse(--help) error = %v, want flag.ErrHelp", err)
	}
}

func TestUpdateCheckEnabled(t *testing.T) {
	tests := []struct {
		name        string
		noCheckFlag bool
		env         string
		isTTY       bool
		want        bool
	}{
		{"tty, nothing suppressing", false, "", true, true},
		{"flag suppresses", true, "", true, false},
		{"env suppresses", false, "1", true, false},
		{"not a tty suppresses", false, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("S3METRICS_NO_UPDATE_CHECK", tt.env)
			c := &Config{NoUpdateCheck: tt.noCheckFlag}
			if got := c.UpdateCheckEnabled(tt.isTTY); got != tt.want {
				t.Errorf("UpdateCheckEnabled(%v) = %v, want %v", tt.isTTY, got, tt.want)
			}
		})
	}
}
