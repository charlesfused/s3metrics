package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
