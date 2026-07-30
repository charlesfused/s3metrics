// Package awsx builds AWS clients and works out which region a bucket lives in.
package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/charlesfused/s3metrics/internal/errs"
)

// BucketLocationAPI is the slice of the S3 client that region resolution needs.
type BucketLocationAPI interface {
	GetBucketLocation(ctx context.Context, in *s3.GetBucketLocationInput,
		opts ...func(*s3.Options)) (*s3.GetBucketLocationOutput, error)
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
// is a permission plenty of roles lack; if it fails and the config names a
// region, that is a usable answer.
func ResolveRegion(ctx context.Context, api BucketLocationAPI, bucket, flagRegion, cfgRegion string) (string, error) {
	if flagRegion != "" {
		return flagRegion, nil
	}

	out, err := api.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(bucket)})
	if err == nil {
		return normalizeConstraint(string(out.LocationConstraint)), nil
	}

	if cfgRegion != "" {
		return cfgRegion, nil
	}

	return "", errs.Classify(err, "s3:GetBucketLocation")
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
