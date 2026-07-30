package errs

import (
	"context"
	"errors"
	"net"
	"strings"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// credentialChainMarkers are substrings the SDK uses when the credential chain
// comes up empty. Matching on text is a last resort, used here because
// aws-sdk-go-v2 exports no typed error for this case — the failure arrives as a
// generic error wrapped in a smithy.OperationError. Checked only after every
// typed check has missed.
var credentialChainMarkers = []string{
	"failed to refresh cached credentials",
	"no EC2 IMDS role found",
	"failed to retrieve credentials",
	"NoCredentialProviders",
}

// bucketNotFound builds the not-found error. Both the typed-error branch and
// the API-code switch reach it, so the message and hint stay in one place.
func bucketNotFound(err error) *Error {
	return Wrap(err, CodeBucketNotFound, "bucket not found",
		"check the bucket name, and pass --region if the bucket is not in your default region")
}

// Classify maps any error into a categorised *Error. perm is the IAM action the
// caller was attempting; it is used to make an access-denied hint actionable.
// An error that is already categorised is returned unchanged, so classification
// is idempotent and the innermost, most specific verdict wins.
func Classify(err error, perm string) *Error {
	if err == nil {
		return nil
	}

	var already *Error
	if errors.As(err, &already) {
		return already
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Wrap(err, CodeCanceled, "operation canceled",
			"the --timeout elapsed or the process was interrupted")
	}

	var noSuchBucket *s3types.NoSuchBucket
	var notFound *s3types.NotFound
	if errors.As(err, &noSuchBucket) || errors.As(err, &notFound) {
		return bucketNotFound(err)
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if e := classifyAPICode(err, apiErr.ErrorCode(), perm); e != nil {
			return e
		}
	}

	msg := err.Error()
	for _, marker := range credentialChainMarkers {
		if strings.Contains(msg, marker) {
			return Wrap(err, CodeNoCredentials, "no AWS credentials found",
				"set AWS_PROFILE, pass --profile, or run aws configure")
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return Wrap(err, CodeNetwork, "network error talking to AWS",
			"check connectivity, proxy settings, and any VPC endpoint configuration")
	}

	return Wrap(err, CodeInternal, "unexpected error", "")
}

// classifyAPICode maps a Smithy API error code. Returns nil when the code is not
// one we recognise, so the caller can keep trying other strategies.
func classifyAPICode(err error, code, perm string) *Error {
	switch code {
	case "NoSuchBucket", "NotFound":
		return bucketNotFound(err)

	case "AccessDenied", "AccessDeniedException", "Forbidden", "AuthorizationHeaderMalformed":
		return Wrap(err, CodeAccessDenied, "access denied by AWS",
			"the caller identity is missing "+perm+" on this resource")

	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidClientTokenId",
		"UnrecognizedClientException", "MissingAuthenticationToken":
		return Wrap(err, CodeNoCredentials, "AWS rejected the credentials",
			"set AWS_PROFILE, pass --profile, or run aws configure")

	case "ExpiredToken", "ExpiredTokenException", "RequestExpired", "RequestTimeTooSkewed":
		return Wrap(err, CodeExpiredCredentials, "AWS credentials have expired",
			"refresh them — for SSO profiles run: aws sso login")

	case "SlowDown", "Throttling", "ThrottlingException", "TooManyRequestsException",
		"RequestLimitExceeded", "ProvisionedThroughputExceededException":
		return Wrap(err, CodeThrottled, "AWS is throttling requests",
			"lower --concurrency and retry")

	case "RequestTimeout", "ServiceUnavailable", "InternalError", "RequestError":
		return Wrap(err, CodeNetwork, "AWS request failed",
			"transient service or network problem — retry")
	}
	return nil
}
