package updater

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackgroundCheckDisabledYieldsNothing(t *testing.T) {
	ch := StartBackgroundCheck(New(), false)

	select {
	case msg, ok := <-ch:
		if ok {
			t.Errorf("disabled check yielded %q, want a closed empty channel", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("disabled check never closed its channel")
	}
}

func TestBackgroundCheckReportsNewerVersion(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v9.9.9","assets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	select {
	case msg := <-StartBackgroundCheck(c, true):
		if msg == "" {
			t.Fatal("got an empty notice, want one naming v9.9.9")
		}
		if !strings.Contains(msg, "v9.9.9") || !strings.Contains(msg, "--self-update") {
			t.Errorf("notice = %q, want it to name v9.9.9 and --self-update", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notice arrived")
	}
}

func TestBackgroundCheckSilentWhenCurrent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	for msg := range StartBackgroundCheck(c, true) {
		if msg != "" {
			t.Errorf("notice = %q, want nothing when already current", msg)
		}
	}
}

func TestBackgroundCheckSurvivesServerFailure(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	select {
	case msg, ok := <-StartBackgroundCheck(c, true):
		if ok && msg != "" {
			t.Errorf("notice = %q, want silence when the check fails", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a failing check never closed its channel — it must never hang a run")
	}
}

func TestFreshCacheSkipsTheNetwork(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)

	path, err := CachePath()
	if err != nil {
		t.Fatalf("CachePath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entry := cacheEntry{LastChecked: time.Now(), LatestVersion: "v5.0.0"}
	b, _ := json.Marshal(entry)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"tag_name":"v9.9.9","assets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	msg := <-StartBackgroundCheck(c, true)

	if hits != 0 {
		t.Errorf("server was hit %d times, want 0 — a cache under 24h must not call out", hits)
	}
	if !strings.Contains(msg, "v5.0.0") {
		t.Errorf("notice = %q, want it to use the cached v5.0.0", msg)
	}
}

func TestBackgroundCheckSkipsDevBuild(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"tag_name":"v9.9.9","assets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "dev"

	for msg := range StartBackgroundCheck(c, true) {
		t.Errorf("notice = %q, want silence for an unstamped build", msg)
	}
	if hits != 0 {
		t.Errorf("server was hit %d times, want 0 — a dev build must not spend rate limit", hits)
	}
}

func TestBackgroundCheckSkipsUncomparableVersion(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// A bare commit SHA has no position in the version ordering, so a check
	// could never produce a notice — same treatment as a dev build.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"tag_name":"v9.9.9","assets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "62f9f72"

	for msg := range StartBackgroundCheck(c, true) {
		t.Errorf("notice = %q, want silence for an uncomparable version", msg)
	}
	if hits != 0 {
		t.Errorf("server was hit %d times, want 0 — an uncomparable version must not spend rate limit", hits)
	}
}

func TestStaleCacheTriggersRefresh(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)

	path, _ := CachePath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	entry := cacheEntry{
		LastChecked:   time.Now().Add(-48 * time.Hour),
		LatestVersion: "v2.0.0",
	}
	b, _ := json.Marshal(entry)
	os.WriteFile(path, b, 0o644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v9.9.9","assets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BaseURL = srv.URL
	c.Repo = "owner/repo"
	c.Version = "v1.0.0"

	msg := <-StartBackgroundCheck(c, true)

	if !strings.Contains(msg, "v9.9.9") {
		t.Errorf("notice = %q, want the refreshed v9.9.9", msg)
	}
}
