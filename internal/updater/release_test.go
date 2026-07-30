package updater

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/charlesfused/s3metrics/internal/errs"
)

func releaseJSON(tag string, assetNames ...string) string {
	var assets []string
	for _, n := range assetNames {
		assets = append(assets, fmt.Sprintf(
			`{"name":%q,"browser_download_url":"https://example.invalid/%s"}`, n, n))
	}
	return fmt.Sprintf(`{"tag_name":%q,"assets":[%s]}`, tag, strings.Join(assets, ","))
}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"
	return c
}

func TestLatestParsesRelease(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/repos/owner/repo/releases/latest"; r.URL.Path != want {
			t.Errorf("request path = %q, want %q", r.URL.Path, want)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent header is empty; GitHub rejects such requests")
		}
		fmt.Fprint(w, releaseJSON("v1.2.0", "s3metrics_1.2.0_linux_amd64.tar.gz", "checksums.txt"))
	})

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if rel.TagName != "v1.2.0" {
		t.Errorf("TagName = %q, want v1.2.0", rel.TagName)
	}
	if len(rel.Assets) != 2 {
		t.Errorf("len(Assets) = %d, want 2", len(rel.Assets))
	}
}

func TestLatestSendsAuthorizationWhenTokenSet(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")

	var gotAuth string
	// testClient calls New(), which reads GITHUB_TOKEN at construction — so this
	// exercises the env path rather than assigning Token directly.
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, releaseJSON("v1.0.0"))
	})

	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
}

func TestLatestRateLimited(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	})

	_, err := c.Latest(context.Background())
	if err == nil {
		t.Fatal("Latest() error = nil, want a rate-limit error")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("Latest() error = %v, want it to mention the rate limit", err)
	}
	if got := errs.ExitCode(err); got != 11 {
		t.Errorf("exit code = %d, want 11 (update_failed)", got)
	}
}

func TestLatestNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("Latest() error = nil, want an error for a missing repo or release")
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.0", "v1.1.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.1.0", "v1.2.0", false},
		{"v2.0.0", "v1.99.99", true},
		{"1.2.0", "v1.1.0", true},   // missing v prefix is normalised
		{"v1.2.0", "1.1.0", true},   // on either side
		{"v1.2.0", "dev", false},    // an unstamped build is not comparable
		{"v1.2.0", "", false},
		{"garbage", "v1.0.0", false},
		{"v1.0.0", "garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.latest+" vs "+tt.current, func(t *testing.T) {
			if got := IsNewer(tt.latest, tt.current); got != tt.want {
				t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
			}
		})
	}
}

func TestAssetNameMatchesGoReleaserLayout(t *testing.T) {
	got := AssetName("v1.2.0")
	want := fmt.Sprintf("s3metrics_1.2.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	if got != want {
		t.Errorf("AssetName(v1.2.0) = %q, want %q", got, want)
	}
	if strings.Contains(got, "_v") {
		t.Error("asset name kept the v prefix; GoReleaser's .Version strips it")
	}
}

func TestFindAsset(t *testing.T) {
	rel := &Release{Assets: []Asset{
		{Name: "checksums.txt", URL: "u1"},
		{Name: "s3metrics_1.0.0_linux_amd64.tar.gz", URL: "u2"},
	}}

	if a, ok := rel.FindAsset("checksums.txt"); !ok || a.URL != "u1" {
		t.Errorf("FindAsset(checksums.txt) = %+v, %v", a, ok)
	}
	if _, ok := rel.FindAsset("nope.tar.gz"); ok {
		t.Error("FindAsset(nope.tar.gz) ok = true, want false")
	}
}
