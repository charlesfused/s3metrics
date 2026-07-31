package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// cacheTTL is how long a check result stands before we ask again. Users do
	// not need to hear about a release the minute it lands.
	cacheTTL = 24 * time.Hour

	// backgroundTimeout bounds the check hard. The run it rides alongside must
	// never wait on it.
	backgroundTimeout = 3 * time.Second
)

type cacheEntry struct {
	LastChecked   time.Time `json:"last_checked"`
	LatestVersion string    `json:"latest_version"`
}

// CachePath is where the last check result is remembered.
func CachePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "s3metrics", "update-check.json"), nil
}

// StartBackgroundCheck kicks off a best-effort availability check.
//
// The returned channel is always non-nil and always eventually closed, yielding
// at most one notice, so a caller can drain it with a non-blocking select and
// never special-case the disabled path.
//
// This is strictly subordinate to the real work: it must never delay a run, fail
// a run, or change an exit code. Every failure inside is swallowed, and the
// context is its own — a --timeout on the walk has no bearing on it.
func StartBackgroundCheck(c *Client, enabled bool) <-chan string {
	ch := make(chan string, 1)

	// A dev, unstamped, or otherwise unorderable build (e.g. a bare commit SHA
	// from a repo with no tags) has no position in the version ordering, so a
	// check could never produce a notice — skip the request entirely rather
	// than spend rate-limit budget on it. c.Comparable() reads c.Version, not
	// the package-level buildinfo.Version: the global is always "dev" under
	// `go test`, which is what made the original package-level gate untestable.
	if !enabled || !c.Comparable() {
		close(ch)
		return ch
	}

	go func() {
		defer close(ch)

		ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
		defer cancel()

		latest, err := c.cachedLatest(ctx)
		if err != nil || latest == "" {
			return // silence is the correct outcome for a failed check
		}
		if !IsNewer(latest, c.Version) {
			return
		}
		ch <- fmt.Sprintf("a newer version %s is available — run: s3metrics --self-update", latest)
	}()

	return ch
}

// cachedLatest returns the latest known release tag, consulting the on-disk
// cache before the network. A cache write failure is not an error: the check is
// advisory, and a missing cache only means asking again sooner.
func (c *Client) cachedLatest(ctx context.Context) (string, error) {
	path, err := CachePath()
	if err != nil {
		// No cache directory: fall back to a live lookup rather than giving up.
		rel, err := c.Latest(ctx)
		if err != nil {
			return "", err
		}
		return rel.TagName, nil
	}

	if entry, ok := readCache(path); ok && time.Since(entry.LastChecked) < cacheTTL {
		return entry.LatestVersion, nil
	}

	rel, err := c.Latest(ctx)
	if err != nil {
		return "", err
	}

	writeCache(path, cacheEntry{LastChecked: time.Now(), LatestVersion: rel.TagName})
	return rel.TagName, nil
}

func readCache(path string) (cacheEntry, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(b, &entry); err != nil {
		return cacheEntry{}, false
	}
	return entry, true
}

// writeCache is best-effort by design; a failure here must not surface anywhere.
func writeCache(path string, entry cacheEntry) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}
