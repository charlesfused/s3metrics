// Package errs gives every failure a category, a remediation hint, and a stable
// process exit code, so a caller can branch on the exit status instead of
// pattern-matching on message text.
package errs

import "errors"

// Code names a category of failure. The set is closed: adding one means adding
// an exit code, and exit codes are part of the CLI's contract.
type Code string

const (
	CodeInternal           Code = "internal"
	CodeUsage              Code = "usage"
	CodeNoCredentials      Code = "no_credentials"
	CodeExpiredCredentials Code = "expired_credentials"
	CodeAccessDenied       Code = "access_denied"
	CodeBucketNotFound     Code = "bucket_not_found"
	CodeNoMetrics          Code = "no_metrics"
	CodeThrottled          Code = "throttled"
	CodeNetwork            Code = "network"
	CodeCanceled           Code = "canceled"
	CodeUpdateFailed       Code = "update_failed"
)

// exitCodes is the published contract. Never renumber an entry.
var exitCodes = map[Code]int{
	CodeInternal:           1,
	CodeUsage:              2,
	CodeNoCredentials:      3,
	CodeExpiredCredentials: 4,
	CodeAccessDenied:       5,
	CodeBucketNotFound:     6,
	CodeNoMetrics:          7,
	CodeThrottled:          8,
	CodeNetwork:            9,
	CodeCanceled:           10,
	CodeUpdateFailed:       11,
}

// Error is a categorised failure. Msg is what went wrong in plain language;
// Hint is what the user should do about it.
type Error struct {
	Code Code
	Msg  string
	Hint string
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Msg
	}
	return e.Msg + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a categorised error with no underlying cause.
func New(code Code, msg, hint string) *Error {
	return &Error{Code: code, Msg: msg, Hint: hint}
}

// Wrap builds a categorised error around an existing cause.
func Wrap(err error, code Code, msg, hint string) *Error {
	return &Error{Code: code, Msg: msg, Hint: hint, Err: err}
}

// ExitCode returns the process exit status for err: 0 for nil, the mapped code
// for a categorised error anywhere in the chain, and 1 for anything else.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		if code, ok := exitCodes[e.Code]; ok {
			return code
		}
	}
	return 1
}
