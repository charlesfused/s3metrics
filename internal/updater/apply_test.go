package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildArchive returns a gzipped tar containing a single binary with the given
// contents, plus its SHA256.
func buildArchive(t *testing.T, binaryName, contents string) ([]byte, string) {
	t.Helper()

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: binaryName,
		Mode: 0o755,
		Size: int64(len(contents)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(contents)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	sum := sha256.Sum256(raw.Bytes())
	return raw.Bytes(), hex.EncodeToString(sum[:])
}

// updateServer serves a release, an archive, and a checksums file. When
// corruptChecksum is set, the published checksum will not match the archive.
func updateServer(t *testing.T, tag, archiveName string, archive []byte, sum string, corruptChecksum bool) *httptest.Server {
	t.Helper()

	published := sum
	if corruptChecksum {
		published = strings.Repeat("0", 64)
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[
			{"name":%q,"browser_download_url":"%s/dl/%s"},
			{"name":"checksums.txt","browser_download_url":"%s/dl/checksums.txt"}
		]}`, tag, archiveName, srv.URL, archiveName, srv.URL)
	})
	mux.HandleFunc("/dl/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", published, archiveName)
	})
	return srv
}

// installFakeBinary writes a stand-in for the running executable. The real
// binary is never the target of a test.
func installFakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "s3metrics")
	if err := os.WriteFile(path, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return path
}

func TestApplyHappyPath(t *testing.T) {
	archiveName := AssetName("v1.2.0")
	archive, sum := buildArchive(t, "s3metrics", "NEW BINARY")
	srv := updateServer(t, "v1.2.0", archiveName, archive, sum, false)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	exe := installFakeBinary(t)
	if err := c.Apply(context.Background(), rel, exe); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("reading replaced binary: %v", err)
	}
	if string(got) != "NEW BINARY" {
		t.Errorf("binary contents = %q, want %q", got, "NEW BINARY")
	}

	info, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the executable bit set", info.Mode().Perm())
	}
}

func TestApplySetsExecutableBit(t *testing.T) {
	archiveName := AssetName("v1.2.0")
	archive, sum := buildArchive(t, "s3metrics", "NEW BINARY")
	srv := updateServer(t, "v1.2.0", archiveName, archive, sum, false)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	exe := installFakeBinary(t)
	if err := c.Apply(context.Background(), rel, exe); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	info, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// CreateTemp makes files 0600, so without the explicit chmod the installed
	// binary would not be executable.
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed mode = %v, want the executable bit set", info.Mode().Perm())
	}
}

func TestApplyChecksumMismatchLeavesBinaryUntouched(t *testing.T) {
	archiveName := AssetName("v1.2.0")
	archive, sum := buildArchive(t, "s3metrics", "MALICIOUS")
	srv := updateServer(t, "v1.2.0", archiveName, archive, sum, true) // published sum is wrong

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	exe := installFakeBinary(t)
	before, _ := os.ReadFile(exe)

	err = c.Apply(context.Background(), rel, exe)
	if err == nil {
		t.Fatal("Apply() error = nil, want a checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("Apply() error = %v, want it to name the checksum", err)
	}

	after, _ := os.ReadFile(exe)
	if !bytes.Equal(before, after) {
		t.Error("the installed binary changed despite a checksum failure")
	}

	// No debris left behind in the install directory.
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 1 {
		t.Errorf("install dir holds %d entries, want 1 — the temp download was not cleaned up", len(entries))
	}
}

func TestApplyMissingPlatformAsset(t *testing.T) {
	archive, sum := buildArchive(t, "s3metrics", "NEW")
	// Publish an asset for a platform this test is definitely not running on.
	srv := updateServer(t, "v1.2.0", "s3metrics_1.2.0_plan9_sparc.tar.gz", archive, sum, false)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	err = c.Apply(context.Background(), rel, installFakeBinary(t))
	if err == nil {
		t.Fatal("Apply() error = nil, want an error for a missing platform asset")
	}
	if !strings.Contains(err.Error(), AssetName("v1.2.0")) {
		t.Errorf("Apply() error = %v, want it to name the asset it looked for", err)
	}
}

func TestApplyUnwritableTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permissions cannot be exercised")
	}

	archiveName := AssetName("v1.2.0")
	archive, sum := buildArchive(t, "s3metrics", "NEW")
	srv := updateServer(t, "v1.2.0", archiveName, archive, sum, false)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	exe := installFakeBinary(t)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o500); err != nil { // read+execute, no write
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err = c.Apply(context.Background(), rel, exe)
	if err == nil {
		t.Fatal("Apply() error = nil, want a permission error")
	}
	if !strings.Contains(err.Error(), "writable") && !strings.Contains(err.Error(), "permission") {
		t.Errorf("Apply() error = %v, want it to explain the permission problem", err)
	}
}

// pointExecutableAt makes SelfUpdate target path instead of the running test
// binary, restoring the real resolver when the test finishes.
func pointExecutableAt(t *testing.T, path string) {
	t.Helper()
	prev := resolveExecutable
	resolveExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { resolveExecutable = prev })
}

func TestSelfUpdateRefusesADevBuildWithoutAskingTheServer(t *testing.T) {
	// The gate must fire before any request: an unstamped build has no position
	// in the version ordering, so there is nothing a lookup could decide.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "dev"

	_, err := c.SelfUpdate(context.Background())
	if err == nil {
		t.Fatal("SelfUpdate() error = nil, want a refusal for an unstamped build")
	}
	if !strings.Contains(err.Error(), "development build") {
		t.Errorf("SelfUpdate() error = %v, want it to name the unstamped build", err)
	}
	if hits != 0 {
		t.Errorf("server saw %d requests, want 0 — the gate must precede the lookup", hits)
	}

	// An empty version is the same situation.
	c.Version = ""
	if _, err := c.SelfUpdate(context.Background()); err == nil {
		t.Error("SelfUpdate() error = nil for an empty version, want a refusal")
	}
}

func TestSelfUpdateNoOpsWhenAlreadyCurrent(t *testing.T) {
	archiveName := AssetName("v1.2.0")
	archive, sum := buildArchive(t, "s3metrics", "NEW BINARY")
	srv := updateServer(t, "v1.2.0", archiveName, archive, sum, false)

	exe := installFakeBinary(t)
	pointExecutableAt(t, exe)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.2.0" // the published release is not newer

	got, err := c.SelfUpdate(context.Background())
	if err != nil {
		t.Fatalf("SelfUpdate() error = %v", err)
	}
	if got != "v1.2.0" {
		t.Errorf("SelfUpdate() = %q, want the current version back", got)
	}

	// Nothing may be installed when nothing is newer.
	contents, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("reading binary: %v", err)
	}
	if string(contents) != "OLD BINARY" {
		t.Errorf("binary contents = %q, want it untouched", contents)
	}
}

func TestSelfUpdateInstallsANewerRelease(t *testing.T) {
	// The whole flow: Latest → IsNewer → resolve the target → Apply.
	archiveName := AssetName("v1.2.0")
	archive, sum := buildArchive(t, "s3metrics", "NEW BINARY")
	srv := updateServer(t, "v1.2.0", archiveName, archive, sum, false)

	exe := installFakeBinary(t)
	pointExecutableAt(t, exe)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	got, err := c.SelfUpdate(context.Background())
	if err != nil {
		t.Fatalf("SelfUpdate() error = %v", err)
	}
	if got != "v1.2.0" {
		t.Errorf("SelfUpdate() = %q, want v1.2.0", got)
	}

	contents, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("reading replaced binary: %v", err)
	}
	if string(contents) != "NEW BINARY" {
		t.Errorf("binary contents = %q, want %q", contents, "NEW BINARY")
	}
}

func TestSelfUpdateRefusesAnUnverifiedArchive(t *testing.T) {
	// A corrupt checksum must stop the flow at Apply, leaving the binary intact.
	archiveName := AssetName("v1.2.0")
	archive, sum := buildArchive(t, "s3metrics", "MALICIOUS")
	srv := updateServer(t, "v1.2.0", archiveName, archive, sum, true)

	exe := installFakeBinary(t)
	pointExecutableAt(t, exe)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	if _, err := c.SelfUpdate(context.Background()); err == nil {
		t.Fatal("SelfUpdate() error = nil, want a checksum failure")
	}

	contents, _ := os.ReadFile(exe)
	if string(contents) != "OLD BINARY" {
		t.Errorf("binary contents = %q, want it untouched", contents)
	}
}

func TestDownloadSendsCredentialsOnlyToTheAPIHost(t *testing.T) {
	// asset.URL comes out of the release payload, so it can name any host. The
	// token must not follow it there.
	var foreignAuth string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignAuth = r.Header.Get("Authorization")
		w.Write([]byte("payload"))
	}))
	t.Cleanup(foreign.Close)

	var apiAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("Authorization")
		w.Write([]byte("payload"))
	}))
	t.Cleanup(api.Close)

	c := New()
	c.BaseURL = api.URL
	c.Token = "super-secret"

	if _, err := c.download(context.Background(), foreign.URL+"/asset.tar.gz", io.Discard); err != nil {
		t.Fatalf("download() error = %v", err)
	}
	if foreignAuth != "" {
		t.Errorf("foreign host saw Authorization = %q, want none", foreignAuth)
	}

	if _, err := c.download(context.Background(), api.URL+"/asset.tar.gz", io.Discard); err != nil {
		t.Fatalf("download() error = %v", err)
	}
	if apiAuth != "Bearer super-secret" {
		t.Errorf("API host saw Authorization = %q, want the bearer token", apiAuth)
	}
}

func TestSameHostFailsClosed(t *testing.T) {
	tests := []struct {
		name, a, b string
		want       bool
	}{
		{"identical", "https://api.github.com/x", "https://api.github.com", true},
		{"different host", "https://evil.example/x", "https://api.github.com", false},
		{"different port is a different host", "http://h:1/x", "http://h:2", false},
		{"unparseable asset url", "https://%zz", "https://api.github.com", false},
		{"unparseable base url", "https://api.github.com/x", "https://%zz", false},
		{"hostless asset url", "/relative/path", "https://api.github.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameHost(tt.a, tt.b); got != tt.want {
				t.Errorf("sameHost(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestParseChecksums(t *testing.T) {
	in := strings.NewReader(
		"abc123  s3metrics_1.0.0_linux_amd64.tar.gz\n" +
			"def456  s3metrics_1.0.0_darwin_arm64.tar.gz\n" +
			"\n" +
			"malformed-line\n")

	got := ParseChecksums(in)

	if got["s3metrics_1.0.0_linux_amd64.tar.gz"] != "abc123" {
		t.Errorf("linux checksum = %q, want abc123", got["s3metrics_1.0.0_linux_amd64.tar.gz"])
	}
	if got["s3metrics_1.0.0_darwin_arm64.tar.gz"] != "def456" {
		t.Errorf("darwin checksum = %q, want def456", got["s3metrics_1.0.0_darwin_arm64.tar.gz"])
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 — malformed and blank lines must be skipped", len(got))
	}
}
