package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesfused/s3metrics/internal/cli"
	"github.com/charlesfused/s3metrics/internal/metrics"
)

func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = run(args, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

func TestVersionExitsZeroAndPrintsToStdout(t *testing.T) {
	stdout, _, code := runCLI(t, "--version")

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "s3metrics") {
		t.Errorf("stdout = %q, want the version line", stdout)
	}
}

func TestMissingBucketIsUsageError(t *testing.T) {
	stdout, stderr, code := runCLI(t)

	if code != 2 {
		t.Errorf("exit code = %d, want 2 (usage)", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty on failure", stdout)
	}
	if !strings.Contains(stderr, "--bucket") {
		t.Errorf("stderr = %q, want it to name the missing flag", stderr)
	}
}

func TestUsageErrorInJSONModeIsStillText(t *testing.T) {
	// Parsing failed, so --format was never validated. Rendering the error as
	// JSON would be claiming a format we do not know the user asked for.
	_, stderr, code := runCLI(t, "--format", "json", "--prefix", "x")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if strings.HasPrefix(strings.TrimSpace(stderr), "{") {
		t.Errorf("stderr = %q, want plain text for a usage error", stderr)
	}
}

func TestHelpExitsZero(t *testing.T) {
	_, _, code := runCLI(t, "--help")

	if code != 0 {
		t.Errorf("exit code = %d, want 0 for --help", code)
	}
}

func TestInvalidFlagCombinationsExitTwo(t *testing.T) {
	tests := [][]string{
		{"--bucket", "b", "--mode", "nope"},
		{"--bucket", "b", "--format", "nope"},
		{"--bucket", "b", "--no-header"},
		{"--check-update", "--self-update"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, _, code := runCLI(t, args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
		})
	}
}

// The note exists because the first real-bucket run produced a 2.66x gap in
// object count between the two modes and the tool explained neither number. It
// is advisory output, so it follows the update notice's discipline exactly: only
// when it can be read, and only when it is actually true.
func TestVersionNote(t *testing.T) {
	yes, no := true, false
	walk := &cli.Config{Mode: cli.ModeWalk}
	report := func(v *bool) *metrics.Report { return &metrics.Report{Versioned: v} }

	tests := []struct {
		name   string
		cfg    *cli.Config
		report *metrics.Report
		isTTY  bool
		want   bool
	}{
		{"versioned walk on a tty", walk, report(&yes), true, true},
		{"not a tty", walk, report(&yes), false, false},
		{"bucket is not versioned", walk, report(&no), true, false},
		{"versioning unknown", walk, report(nil), true, false},
		{"metrics mode already counts them", &cli.Config{Mode: cli.ModeMetrics}, report(&yes), true, false},
		{"already including versions", &cli.Config{Mode: cli.ModeWalk, IncludeVersions: true}, report(&yes), true, false},
		{"no report at all", walk, nil, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := versionNote(tt.cfg, tt.report, tt.isTTY)
			if (got != "") != tt.want {
				t.Errorf("versionNote() = %q, want printed = %v", got, tt.want)
			}
			if tt.want && !strings.Contains(got, "--include-versions") {
				t.Errorf("versionNote() = %q, want it to name the switch that fixes this", got)
			}
		})
	}
}

func TestErrorOutputIsValidJSONWhenRequested(t *testing.T) {
	// The SDK is pointed at a closed local port, so the failure path runs to
	// completion with no outbound traffic and no dependence on what AWS would
	// have said. Whatever goes wrong, stderr must parse as JSON and stdout must
	// stay empty.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://127.0.0.1:1")
	t.Setenv("AWS_RETRY_MODE", "standard")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")

	stdout, stderr, code := runCLI(t,
		"--bucket", "unreachable", "--format", "json",
		"--timeout", "10s", "--no-update-check")

	if code == 0 {
		t.Fatalf("exit code = 0, want a failure against an unreachable endpoint; stdout = %q", stdout)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty on failure", stdout)
	}

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\nstderr = %q", err, stderr)
	}
	if envelope.Error.Code == "" {
		t.Error("error.code is empty")
	}
}
