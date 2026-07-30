package errs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRenderTextWithHint(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, New(CodeNoCredentials, "no AWS credentials found", "run aws configure"), false)

	want := "s3metrics: no AWS credentials found\n  hint: run aws configure\n"
	if got := buf.String(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestRenderTextWithoutHintOmitsHintLine(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, New(CodeInternal, "boom", ""), false)

	want := "s3metrics: boom\n"
	if got := buf.String(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestRenderJSONShape(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, New(CodeNoMetrics, "no CloudWatch datapoints", "try --mode walk"), true)

	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (got %q)", err, buf.String())
	}
	if got.Error.Code != "no_metrics" {
		t.Errorf("code = %q, want %q", got.Error.Code, "no_metrics")
	}
	if got.Error.Message != "no CloudWatch datapoints" {
		t.Errorf("message = %q", got.Error.Message)
	}
	if got.Error.Hint != "try --mode walk" {
		t.Errorf("hint = %q", got.Error.Hint)
	}
}

func TestRenderJSONOmitsEmptyHint(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, New(CodeInternal, "boom", ""), true)

	if strings.Contains(buf.String(), "hint") {
		t.Errorf("Render() = %q, want no hint key when hint is empty", buf.String())
	}
}

func TestRenderInternalTextSurfacesTheCause(t *testing.T) {
	// CodeInternal's Msg is a placeholder, so without the cause the entire
	// diagnostic is "s3metrics: unexpected error" — useless to anyone.
	var buf bytes.Buffer
	Render(&buf, Wrap(errors.New("endpoint resolution failed"),
		CodeInternal, "unexpected error", ""), false)

	want := "s3metrics: unexpected error: endpoint resolution failed\n"
	if got := buf.String(); got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestRenderInternalJSONCarriesTheCause(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, Wrap(errors.New("endpoint resolution failed"),
		CodeInternal, "unexpected error", ""), true)

	var got jsonEnvelope
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (got %q)", err, buf.String())
	}
	if got.Error.Cause != "endpoint resolution failed" {
		t.Errorf("cause = %q, want %q", got.Error.Cause, "endpoint resolution failed")
	}
	if got.Error.Message != "unexpected error" {
		t.Errorf("message = %q, want the Msg alone — the cause has its own field", got.Error.Message)
	}
}

func TestRenderClassifiedErrorHidesTheCause(t *testing.T) {
	// A classified error's Msg and Hint already say everything useful. Appending
	// the SDK's own wording would only add noise.
	cause := errors.New("operation error S3: ListObjectsV2, https response error")

	var text bytes.Buffer
	Render(&text, Wrap(cause, CodeNoCredentials, "no AWS credentials found", "run aws configure"), false)
	if strings.Contains(text.String(), "https response error") {
		t.Errorf("Render() = %q, want no SDK cause for a classified error", text.String())
	}

	var asJSON bytes.Buffer
	Render(&asJSON, Wrap(cause, CodeNoCredentials, "no AWS credentials found", "run aws configure"), true)
	if strings.Contains(asJSON.String(), "cause") {
		t.Errorf("Render() = %q, want no cause key for a classified error", asJSON.String())
	}
}

func TestRenderFindsACategorisedErrorThroughWrapping(t *testing.T) {
	// Callers wrap with fmt.Errorf freely; Render must still find the category
	// rather than falling back to internal.
	err := fmt.Errorf("outer: %w",
		fmt.Errorf("middle: %w", New(CodeThrottled, "throttled by AWS", "retry later")))

	var buf bytes.Buffer
	Render(&buf, err, true)

	if !strings.Contains(buf.String(), `"code":"throttled"`) {
		t.Errorf("Render() = %q, want code throttled found through the wrapping", buf.String())
	}
	if !strings.Contains(buf.String(), "retry later") {
		t.Errorf("Render() = %q, want the categorised error's hint", buf.String())
	}
}

func TestRenderUncategorisedErrorBecomesInternal(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, errors.New("something odd"), true)

	if !strings.Contains(buf.String(), `"code":"internal"`) {
		t.Errorf("Render() = %q, want code internal", buf.String())
	}
	if !strings.Contains(buf.String(), "something odd") {
		t.Errorf("Render() = %q, want the original message preserved", buf.String())
	}
}
