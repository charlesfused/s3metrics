// Package updater checks GitHub Releases for a newer build and installs it.
//
// Nothing here touches the network in tests: BaseURL points at an
// httptest.Server, which exercises the same code path as the real API.
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/charlesfused/s3metrics/internal/buildinfo"
	"github.com/charlesfused/s3metrics/internal/errs"
)

const (
	defaultBaseURL = "https://api.github.com"
	defaultRepo    = "charlesfused/s3metrics"

	// requestTimeout bounds an explicit --check-update or --self-update lookup.
	requestTimeout = 10 * time.Second
)

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Release is the subset of GitHub's release payload this tool needs.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// FindAsset returns the named asset.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client talks to the GitHub Releases API.
type Client struct {
	BaseURL string
	Repo    string
	Token   string
	Version string
	HTTP    *http.Client
}

// New builds a Client from the environment. S3METRICS_UPDATE_REPO repoints the
// update source without a rebuild; GITHUB_TOKEN lifts the 60-per-hour
// anonymous rate limit.
func New() *Client {
	repo := os.Getenv("S3METRICS_UPDATE_REPO")
	if repo == "" {
		repo = defaultRepo
	}
	return &Client{
		BaseURL: defaultBaseURL,
		Repo:    repo,
		Token:   os.Getenv("GITHUB_TOKEN"),
		Version: buildinfo.Version,
		HTTP:    &http.Client{Timeout: requestTimeout},
	}
}

// Latest fetches the newest published release.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(c.BaseURL, "/"), c.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUpdateFailed, "could not build the release request", "")
	}
	c.setHeaders(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUpdateFailed,
			"could not reach the release server",
			"check connectivity, or set S3METRICS_UPDATE_REPO if you host releases elsewhere")
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		return nil, errs.New(errs.CodeUpdateFailed,
			"GitHub API rate limit exceeded",
			"set GITHUB_TOKEN to raise the limit, or retry later")
	case resp.StatusCode == http.StatusNotFound:
		return nil, errs.New(errs.CodeUpdateFailed,
			"no published release found for "+c.Repo,
			"check S3METRICS_UPDATE_REPO, or publish a release first")
	case resp.StatusCode != http.StatusOK:
		return nil, errs.New(errs.CodeUpdateFailed,
			fmt.Sprintf("release lookup failed with HTTP %d", resp.StatusCode), "")
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, errs.Wrap(err, errs.CodeUpdateFailed, "could not parse the release response", "")
	}
	return &rel, nil
}

func (c *Client) setHeaders(req *http.Request) {
	c.setUnauthenticatedHeaders(req)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// setUnauthenticatedHeaders sets everything setHeaders does except the
// credentials, for requests to hosts the release payload named rather than
// hosts we chose.
func (c *Client) setUnauthenticatedHeaders(req *http.Request) {
	// GitHub rejects requests without a User-Agent outright.
	req.Header.Set("User-Agent", "s3metrics/"+buildinfo.Version)
	req.Header.Set("Accept", "application/vnd.github+json")
}

// sameHost reports whether two URLs name the same host.
//
// Fails closed: a URL that will not parse is the same host as nothing, so an
// unparseable asset URL loses the credentials rather than gaining them.
func sameHost(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil || ua.Host == "" {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return ua.Host == ub.Host
}

// IsNewer reports whether latest is a strictly greater release than current.
//
// An unstamped build returns false: "dev" has no position in the version
// ordering, so claiming an upgrade is available would be a guess.
func IsNewer(latest, current string) bool {
	if current == "" || current == "dev" {
		return false
	}
	l, c := normalizeVersion(latest), normalizeVersion(current)
	if !semver.IsValid(l) || !semver.IsValid(c) {
		return false
	}
	return semver.Compare(l, c) > 0
}

func normalizeVersion(v string) string {
	if v == "" {
		return v
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// AssetName returns the archive name for this platform, matching GoReleaser's
// default name_template. GoReleaser's .Version strips a leading v, so the tag's
// prefix must be trimmed here too.
func AssetName(tag string) string {
	return fmt.Sprintf("s3metrics_%s_%s_%s.tar.gz",
		strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
}

// IsDev reports whether this client's build is unstamped and therefore has no
// position in the version ordering — nothing can be compared against a release
// tag, so no update decision is meaningful.
//
// This is deliberately a method on Client rather than a package-level function
// reading buildinfo.Version: the global is always "dev" under `go test`, so a
// package-level gate can never be exercised, and every gate written that way so
// far has shipped untested.
func (c *Client) IsDev() bool { return c.Version == "" || c.Version == "dev" }

// Comparable reports whether this build's version can be ordered against a
// release tag. `git describe --tags --always` falls back to a bare commit SHA
// in a repo with no tags, and a SHA has no position in the version ordering —
// reporting it as up to date would be a guess dressed as an answer.
//
// "dev" and "" are also not valid semver, so Comparable subsumes IsDev; that
// overlap is intentional. Callers that want the more specific unstamped-build
// message must still check IsDev first.
func (c *Client) Comparable() bool {
	return semver.IsValid(normalizeVersion(c.Version))
}
