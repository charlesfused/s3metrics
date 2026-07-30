package errs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type jsonEnvelope struct {
	Error jsonError `json:"error"`
}

type jsonError struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	// Cause is populated only for CodeInternal. A classified error's Msg and
	// Hint already say everything useful; dumping SDK internals alongside them
	// would be noise. An unclassified failure has nothing else to offer, so
	// without this the whole diagnostic is "unexpected error".
	Cause string `json:"cause,omitempty"`
}

// Render writes err to w. When asJSON is true the caller asked for machine
// readable output, so failures are structured the same way successes are.
// w is always stderr in practice; stdout stays clean.
func Render(w io.Writer, err error, asJSON bool) {
	if err == nil {
		return
	}

	e := &Error{Code: CodeInternal, Msg: err.Error()}
	var categorised *Error
	if errors.As(err, &categorised) {
		e = categorised
	}

	// CodeInternal's Msg is a placeholder ("unexpected error" and the like) with
	// no hint, so the wrapped cause is the only thing that says what happened.
	var cause string
	if e.Code == CodeInternal && e.Err != nil {
		cause = e.Err.Error()
	}

	if asJSON {
		// Encoder appends a newline. An encoding failure here has nowhere left
		// to go, so fall back to the text form rather than losing the error.
		if encErr := json.NewEncoder(w).Encode(jsonEnvelope{
			Error: jsonError{Code: e.Code, Message: e.Msg, Hint: e.Hint, Cause: cause},
		}); encErr == nil {
			return
		}
	}

	msg := e.Msg
	if cause != "" {
		msg = e.Error() // Msg + ": " + Err.Error()
	}

	fmt.Fprintf(w, "s3metrics: %s\n", msg)
	if e.Hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", e.Hint)
	}
}
