package awsx

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/charlesfused/s3metrics/internal/errs"
)

type fakeLocation struct {
	constraint s3types.BucketLocationConstraint
	err        error
	calls      int
}

func (f *fakeLocation) GetBucketLocation(context.Context, *s3.GetBucketLocationInput,
	...func(*s3.Options)) (*s3.GetBucketLocationOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &s3.GetBucketLocationOutput{LocationConstraint: f.constraint}, nil
}

func TestResolveRegionPrefersFlag(t *testing.T) {
	api := &fakeLocation{constraint: "eu-west-1"}

	got, err := ResolveRegion(context.Background(), api, "b", "ap-south-1", "us-east-2")
	if err != nil {
		t.Fatalf("ResolveRegion() error = %v", err)
	}
	if got != "ap-south-1" {
		t.Errorf("ResolveRegion() = %q, want ap-south-1", got)
	}
	if api.calls != 0 {
		t.Errorf("GetBucketLocation called %d times, want 0 — an explicit --region needs no lookup", api.calls)
	}
}

func TestResolveRegionFromBucket(t *testing.T) {
	api := &fakeLocation{constraint: "eu-west-1"}

	got, err := ResolveRegion(context.Background(), api, "b", "", "us-east-2")
	if err != nil {
		t.Fatalf("ResolveRegion() error = %v", err)
	}
	if got != "eu-west-1" {
		t.Errorf("ResolveRegion() = %q, want eu-west-1", got)
	}
}

func TestResolveRegionLegacyConstraints(t *testing.T) {
	tests := []struct {
		constraint s3types.BucketLocationConstraint
		want       string
	}{
		// An empty LocationConstraint means us-east-1: the original region
		// predates the field, so S3 returns nothing for it.
		{"", "us-east-1"},
		// "EU" is the legacy alias S3 still returns for some old buckets.
		{"EU", "eu-west-1"},
		{"us-west-2", "us-west-2"},
	}
	for _, tt := range tests {
		t.Run(string(tt.constraint), func(t *testing.T) {
			api := &fakeLocation{constraint: tt.constraint}

			// A non-empty cfgRegion is required to reach the lookup at all: with
			// nothing configured the SDK could not build an endpoint, so
			// ResolveRegion refuses before calling.
			got, err := ResolveRegion(context.Background(), api, "b", "", "us-east-2")
			if err != nil {
				t.Fatalf("ResolveRegion() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveRegion(%q) = %q, want %q", tt.constraint, got, tt.want)
			}
		})
	}
}

func TestResolveRegionFallsBackToConfigOnAccessDenied(t *testing.T) {
	// s3:GetBucketLocation is a permission many roles lack. When it is denied but
	// the config already names a region, that is a usable answer — failing the
	// whole run over an optional lookup would be wrong.
	api := &fakeLocation{err: &s3types.NoSuchBucket{}}

	got, err := ResolveRegion(context.Background(), api, "b", "", "us-east-2")
	if err != nil {
		t.Fatalf("ResolveRegion() error = %v", err)
	}
	if got != "us-east-2" {
		t.Errorf("ResolveRegion() = %q, want the config region us-east-2", got)
	}
}

type fakeVersioning struct {
	status s3types.BucketVersioningStatus
	err    error
}

func (f *fakeVersioning) GetBucketVersioning(context.Context, *s3.GetBucketVersioningInput,
	...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &s3.GetBucketVersioningOutput{Status: f.status}, nil
}

func TestIsVersioned(t *testing.T) {
	tests := []struct {
		name   string
		status s3types.BucketVersioningStatus
		want   bool
	}{
		{"enabled", s3types.BucketVersioningStatusEnabled, true},
		// A suspended bucket still holds every noncurrent version and delete
		// marker created while versioning was on, so the divergence between the
		// two modes persists. Suspended is versioned for our purposes.
		{"suspended", s3types.BucketVersioningStatusSuspended, true},
		// S3 omits Status entirely for a bucket that was never versioned.
		{"never versioned", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsVersioned(context.Background(), &fakeVersioning{status: tt.status}, "b")
			if err != nil {
				t.Fatalf("IsVersioned() error = %v", err)
			}
			if got == nil {
				t.Fatal("IsVersioned() = nil, want a known answer")
			}
			if *got != tt.want {
				t.Errorf("IsVersioned() = %v, want %v", *got, tt.want)
			}
		})
	}
}

// s3:GetBucketVersioning is a permission plenty of roles lack. A denial must
// leave the answer unknown, never fail the run — the caller ignores the error
// and reports null.
func TestIsVersionedReturnsNilOnError(t *testing.T) {
	got, err := IsVersioned(context.Background(), &fakeVersioning{err: errors.New("access denied")}, "b")
	if err == nil {
		t.Error("IsVersioned() error = nil, want the API error returned for the caller to ignore")
	}
	if got != nil {
		t.Errorf("IsVersioned() = %v, want nil — an unknown answer, not a guess", *got)
	}
}

func TestResolveRegionErrorsWhenNothingIsAvailable(t *testing.T) {
	api := &fakeLocation{err: errors.New("boom")}

	_, err := ResolveRegion(context.Background(), api, "b", "", "")
	if err == nil {
		t.Fatal("ResolveRegion() error = nil, want an error when no region can be determined")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("ResolveRegion() error = %T, want *errs.Error", err)
	}
	// Usage, not internal: nothing failed, the user simply has no region
	// configured anywhere, and the message must say so.
	if e.Code != errs.CodeUsage {
		t.Errorf("error code = %s, want %s", e.Code, errs.CodeUsage)
	}
	if api.calls != 0 {
		t.Errorf("GetBucketLocation called %d times, want 0 — the SDK cannot build an endpoint without a region", api.calls)
	}
}
