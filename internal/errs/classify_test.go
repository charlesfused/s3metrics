package errs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code + " message"}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"nosuchbucket typed", &s3types.NoSuchBucket{}, CodeBucketNotFound},
		{"notfound typed", &s3types.NotFound{}, CodeBucketNotFound},
		{"nosuchbucket code", apiErr("NoSuchBucket"), CodeBucketNotFound},
		{"notfound code", apiErr("NotFound"), CodeBucketNotFound},
		{"access denied", apiErr("AccessDenied"), CodeAccessDenied},
		{"access denied exception", apiErr("AccessDeniedException"), CodeAccessDenied},
		{"forbidden", apiErr("Forbidden"), CodeAccessDenied},
		{"invalid key", apiErr("InvalidAccessKeyId"), CodeNoCredentials},
		{"bad signature", apiErr("SignatureDoesNotMatch"), CodeNoCredentials},
		{"expired token", apiErr("ExpiredToken"), CodeExpiredCredentials},
		{"expired token exception", apiErr("ExpiredTokenException"), CodeExpiredCredentials},
		{"clock skew", apiErr("RequestTimeTooSkewed"), CodeExpiredCredentials},
		{"slow down", apiErr("SlowDown"), CodeThrottled},
		{"throttling", apiErr("Throttling"), CodeThrottled},
		{"throttling exception", apiErr("ThrottlingException"), CodeThrottled},
		{"too many requests", apiErr("TooManyRequestsException"), CodeThrottled},
		{"deadline", context.DeadlineExceeded, CodeCanceled},
		{"canceled", context.Canceled, CodeCanceled},
		{"unknown", errors.New("who knows"), CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.err, "s3:ListBucket")
			if got.Code != tt.want {
				t.Errorf("Classify(%v).Code = %s, want %s", tt.err, got.Code, tt.want)
			}
			if got.Msg == "" {
				t.Error("Classify() produced an empty Msg")
			}
		})
	}
}

func TestClassifyNilIsNil(t *testing.T) {
	if got := Classify(nil, "s3:ListBucket"); got != nil {
		t.Errorf("Classify(nil) = %v, want nil", got)
	}
}

func TestClassifyPreservesAlreadyClassified(t *testing.T) {
	orig := New(CodeNoMetrics, "no datapoints", "try --mode walk")
	got := Classify(fmt.Errorf("wrapped: %w", orig), "cloudwatch:GetMetricData")

	if got != orig {
		t.Errorf("Classify() = %v, want the original *Error returned unchanged", got)
	}
}

func TestClassifyAccessDeniedNamesThePermission(t *testing.T) {
	got := Classify(apiErr("AccessDenied"), "cloudwatch:GetMetricData")

	if !strings.Contains(got.Hint, "cloudwatch:GetMetricData") {
		t.Errorf("Hint = %q, want it to name cloudwatch:GetMetricData", got.Hint)
	}
}

func TestClassifyCredentialChainFailure(t *testing.T) {
	// The SDK has no exported type for a credential-chain failure; it arrives as
	// a wrapped generic error. This is the shape it takes in practice.
	err := errors.New("operation error S3: ListObjectsV2, get identity: get credentials: " +
		"failed to refresh cached credentials, no EC2 IMDS role found")

	if got := Classify(err, "s3:ListBucket"); got.Code != CodeNoCredentials {
		t.Errorf("Classify(credential chain failure).Code = %s, want %s", got.Code, CodeNoCredentials)
	}
}

func TestClassifyNetworkError(t *testing.T) {
	var netErr net.Error = &net.DNSError{Err: "no such host", Name: "s3.amazonaws.com"}

	if got := Classify(fmt.Errorf("dial: %w", netErr), "s3:ListBucket"); got.Code != CodeNetwork {
		t.Errorf("Classify(net error).Code = %s, want %s", got.Code, CodeNetwork)
	}
}
