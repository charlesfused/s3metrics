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

	if asJSON {
		// Encoder appends a newline. An encoding failure here has nowhere left
		// to go, so fall back to the text form rather than losing the error.
		if encErr := json.NewEncoder(w).Encode(jsonEnvelope{
			Error: jsonError{Code: e.Code, Message: e.Msg, Hint: e.Hint},
		}); encErr == nil {
			return
		}
	}

	fmt.Fprintf(w, "s3metrics: %s\n", e.Msg)
	if e.Hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", e.Hint)
	}
}
