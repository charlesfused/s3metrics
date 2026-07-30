package errs

import (
	"bytes"
	"encoding/json"
	"errors"
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
