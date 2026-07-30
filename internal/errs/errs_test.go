package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCodeMapping(t *testing.T) {
	tests := []struct {
		code Code
		want int
	}{
		{CodeInternal, 1},
		{CodeUsage, 2},
		{CodeNoCredentials, 3},
		{CodeExpiredCredentials, 4},
		{CodeAccessDenied, 5},
		{CodeBucketNotFound, 6},
		{CodeNoMetrics, 7},
		{CodeThrottled, 8},
		{CodeNetwork, 9},
		{CodeCanceled, 10},
		{CodeUpdateFailed, 11},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			if got := ExitCode(New(tt.code, "boom", "")); got != tt.want {
				t.Errorf("ExitCode(%s) = %d, want %d", tt.code, got, tt.want)
			}
		})
	}
}

func TestExitCodeNilIsZero(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}
}

func TestExitCodeUnknownErrorIsOne(t *testing.T) {
	if got := ExitCode(errors.New("plain")); got != 1 {
		t.Errorf("ExitCode(plain error) = %d, want 1", got)
	}
}

func TestExitCodeFindsWrappedError(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", New(CodeThrottled, "slow down", ""))
	if got := ExitCode(wrapped); got != 8 {
		t.Errorf("ExitCode(wrapped throttled) = %d, want 8", got)
	}
}

func TestErrorUnwrapsCause(t *testing.T) {
	cause := errors.New("root cause")
	e := Wrap(cause, CodeNetwork, "dial failed", "check connectivity")

	if !errors.Is(e, cause) {
		t.Error("errors.Is(e, cause) = false, want true")
	}
	if got := e.Error(); got != "dial failed: root cause" {
		t.Errorf("Error() = %q, want %q", got, "dial failed: root cause")
	}
}

func TestErrorWithoutCauseOmitsSeparator(t *testing.T) {
	if got := New(CodeUsage, "missing --bucket", "").Error(); got != "missing --bucket" {
		t.Errorf("Error() = %q, want %q", got, "missing --bucket")
	}
}
