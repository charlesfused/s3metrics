// Package awsx builds AWS clients and works out which region a bucket lives in.
package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/charlesfused/s3metrics/internal/errs"
)

// BucketLocationAPI is the slice of the S3 client that region resolution needs.
type BucketLocationAPI interface {
	GetBucketLocation(ctx context.Context, in *s3.GetBucketLocationInput,
		opts ...func(*s3.Options)) (*s3.GetBucketLocationOutput, error)
}

// BucketVersioningAPI is the slice of the S3 client the versioning lookup needs.
type BucketVersioningAPI interface {
	GetBucketVersioning(ctx context.Context, in *s3.GetBucketVersioningInput,
		opts ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
}

// IsVersioned reports whether the bucket retains noncurrent object versions.
//
// Suspended counts as versioned. Suspending stops S3 creating new versions but
// destroys none of the existing ones, so every noncurrent version and delete
// marker made while versioning was on is still there — and still counted by
// CloudWatch while invisible to a plain listing. Only an absent Status means the
// bucket was never versioned.
//
// The result is a pointer so that "unknown" is expressible. s3:GetBucketVersioning
// is a permission plenty of roles lack, and this answer is advisory: on any
// error the caller is expected to ignore it and report null rather than abort,
// the same principle as the best-effort bucket-location lookup above. The error
// is returned all the same, for a caller that wants to say why.
func IsVersioned(ctx context.Context, api BucketVersioningAPI, bucket string) (*bool, error) {
	out, err := api.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return nil, err
	}

	versioned := out.Status == s3types.BucketVersioningStatusEnabled ||
		out.Status == s3types.BucketVersioningStatusSuspended
	return &versioned, nil
}

// Load reads the ambient AWS configuration. A region is not required here —
// ResolveRegion settles that afterwards, once we can ask the bucket itself.
func Load(ctx context.Context, profile, region string) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRetryer(func() aws.Retryer { return retry.NewAdaptiveMode() }),
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, errs.Wrap(err, errs.CodeNoCredentials,
			"could not load AWS configuration",
			"check ~/.aws/config and ~/.aws/credentials, or the profile named by --profile")
	}
	return cfg, nil
}

// ResolveRegion determines which region to talk to.
//
// This matters more than it looks: CloudWatch publishes a bucket's storage
// metrics in the bucket's own region, and querying the wrong one returns an
// empty result set with no error at all — the single most confusing failure this
// tool can produce.
//
// Order: an explicit --region, then the bucket's own location, then whatever the
// AWS config says. The bucket lookup is best-effort, because s3:GetBucketLocation
// is a permission plenty of roles lack; if it fails, the config region is a
// usable answer.
//
// A config region is a precondition for the lookup rather than a fallback after
// it: the SDK needs a region to build an endpoint at all, so with nothing
// configured the call cannot even be attempted.
func ResolveRegion(ctx context.Context, api BucketLocationAPI, bucket, flagRegion, cfgRegion string) (string, error) {
	if flagRegion != "" {
		return flagRegion, nil
	}

	// Without a region the SDK cannot even build an endpoint, so the lookup
	// below would fail with an opaque endpoint-resolution error that matches no
	// classification branch and lands on CodeInternal. Say plainly what is
	// missing instead — this is a plausible first-run state: static keys in the
	// environment, no AWS_REGION, no --region.
	if cfgRegion == "" {
		return "", errs.New(errs.CodeUsage,
			"no AWS region configured",
			"pass --region, set AWS_REGION, or add a region to your AWS profile")
	}

	out, err := api.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(bucket)})
	if err == nil {
		return normalizeConstraint(string(out.LocationConstraint)), nil
	}
	return cfgRegion, nil
}

// normalizeConstraint turns a LocationConstraint into a real region name.
func normalizeConstraint(c string) string {
	switch c {
	case "":
		// us-east-1 predates the field, so S3 reports it as absent.
		return "us-east-1"
	case "EU":
		// Legacy alias still returned for some long-lived buckets.
		return "eu-west-1"
	default:
		return c
	}
}

// NewS3 builds an S3 client pinned to region.
//
// The adaptive retryer is deliberate: a sharded walk fans out enough
// ListObjectsV2 calls to earn 503 SlowDown, and adaptive mode rate-limits the
// client itself rather than retrying into the same wall.
func NewS3(cfg aws.Config, region string) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if region != "" {
			o.Region = region
		}
		o.Retryer = retry.NewAdaptiveMode()
	})
}

// NewCloudWatch builds a CloudWatch client pinned to region.
func NewCloudWatch(cfg aws.Config, region string) *cloudwatch.Client {
	return cloudwatch.NewFromConfig(cfg, func(o *cloudwatch.Options) {
		if region != "" {
			o.Region = region
		}
		o.Retryer = retry.NewAdaptiveMode()
	})
}
