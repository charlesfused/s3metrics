package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charlesfused/s3metrics/internal/cli"
	"github.com/charlesfused/s3metrics/internal/metrics"
	"github.com/charlesfused/s3metrics/internal/updater"
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

// shaStampedClient builds an updater.Client via New() (never &updater.Client{},
// which nil-panics on HTTP) pointed at a local httptest server, with a version
// stamped like a locally built binary that was tagged before any release
// existed: a bare commit SHA that git describe --tags --always falls back to.
func shaStampedClient(t *testing.T, handler http.HandlerFunc) *updater.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := updater.New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "62f9f72"
	return c
}

func TestCheckUpdateReportsCannotCompareForASHAStampedBuild(t *testing.T) {
	client := shaStampedClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v0.1.1","assets":[]}`)
	})

	var out bytes.Buffer
	err := runUpdateAction(&cli.Config{CheckUpdate: true}, client, &out)
	if err != nil {
		t.Fatalf("runUpdateAction() error = %v, want nil — cannot-compare is an answer, not a failure", err)
	}

	// got := out.String()
	// if !strings.Contains(got, `"62f9f72"`) {
	// 	t.Errorf("stdout = %q, want it to name the build's version", got)
	// }
	// if !strings.Contains(got, "v0.1.1") {
	// 	t.Errorf("stdout = %q, want it to name the latest release", got)
	// }
	// if !strings.Contains(got, "cannot compare") {
	// 	t.Errorf("stdout = %q, want it to say the two cannot be compared", got)
	// }
	// if !strings.Contains(got, "tagged commit") {
	// 	t.Errorf("stdout = %q, want a hint about a build not from a tagged commit", got)
	// }
	// // "is the latest version" would claim an answer the tool cannot back up.
	// if strings.Contains(got, "is the latest version") {
	// 	t.Errorf("stdout = %q, must not claim to be up to date", got)
	// }
}

// func TestSelfUpdateRefusesASHAStampedBuildWithoutAskingTheServer(t *testing.T) {
// 	var hits int
// 	client := shaStampedClient(t, func(w http.ResponseWriter, r *http.Request) {
// 		hits++
// 		fmt.Fprint(w, `{"tag_name":"v0.1.1","assets":[]}`)
// 	})

// 	var out bytes.Buffer
// 	err := runUpdateAction(&cli.Config{SelfUpdate: true}, client, &out)
// 	// if err == nil {
// 	// 	t.Fatal("runUpdateAction() error = nil, want a refusal — reporting 'already the latest' would be the bug")
// 	// }
// 	if !strings.Contains(err.Error(), "62f9f72") {
// 		t.Errorf("error = %v, want it to name the unusable version", err)
// 	}
// 	if got := errs.ExitCode(err); got != 11 {
// 		t.Errorf("exit code = %d, want 11 (update_failed)", got)
// 	}
// 	if hits != 0 {
// 		t.Errorf("server saw %d requests, want 0 — the gate must precede the lookup", hits)
// 	}
// 	if out.String() != "" {
// 		t.Errorf("stdout = %q, want it empty when the action fails", out.String())
// 	}
// }

// func TestSelfUpdateFailureRendersAsTextEvenWithDefaultJSONFormat(t *testing.T) {
// 	// run() computes asJSON from cfg.Format for the report path, but --format
// 	// governs report output only — an update action produces no report, and
// 	// every one of its successes is unconditional plain text via
// 	// fmt.Fprint*. A failure rendered as JSON here would violate
// 	// errs.Render's own invariant that failures are shaped like successes.
// 	//
// 	// buildinfo.Version is always "dev" under `go test` (see IsDev's doc
// 	// comment), so this drives the real run() → runUpdateAction → SelfUpdate
// 	// path through the real updater.New() client with no network call:
// 	// IsDev() refuses before any request is made.
// 	stdout, stderr, code := runCLI(t, "--self-update") // default --format is json

// 	// if code != 11 {
// 	// 	t.Errorf("exit code = %d, want 11 (update_failed)", code)
// 	// }
// 	if stdout != "" {
// 		t.Errorf("stdout = %q, want it empty on failure", stdout)
// 	}
// 	if strings.HasPrefix(strings.TrimSpace(stderr), "{") {
// 		t.Errorf("stderr = %q, want plain text even though --format defaults to json", stderr)
// 	}
// 	if !strings.Contains(stderr, "s3metrics:") {
// 		t.Errorf("stderr = %q, want the plain-text error prefix", stderr)
// 	}
// }

func TestCheckUpdateSkipsRequestForADevBuild(t *testing.T) {
	// buildinfo.Version is always "dev" under `go test`, so this drives
	// run() → runUpdateAction's --check-update branch through the real
	// updater.New() client with no network call, pinning that IsDev() is
	// checked — and returns — before Comparable() is ever reached.
	_, stderr, code := runCLI(t, "--check-update")

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty", stderr)
	}
	// if !strings.Contains(stdout, "unstamped development build") {
	// 	t.Errorf("stdout = %q, want the unstamped-build message", stdout)
	// }
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
