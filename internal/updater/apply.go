package updater

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charlesfused/s3metrics/internal/errs"
)

// checksumsAsset is the file GoReleaser publishes alongside the archives.
const checksumsAsset = "checksums.txt"

// maxArchiveBytes caps what will be read out of a release archive. A single
// static binary is a few tens of megabytes; this bounds a hostile or corrupt
// archive from filling the disk.
const maxArchiveBytes = 512 << 20

// ResolveExecutable returns the real path of the running binary, following any
// symlink so the replacement lands on the actual file rather than the link.
func ResolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", errs.Wrap(err, errs.CodeUpdateFailed,
			"could not determine the path of the running binary", "")
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil // a symlink that cannot be resolved is not fatal
	}
	return resolved, nil
}

// resolveExecutable is a variable purely so a test can point an update at a
// stand-in binary rather than at the running test binary. Production behaviour
// is unchanged: it is ResolveExecutable and nothing else ever reassigns it.
var resolveExecutable = ResolveExecutable

// SelfUpdate fetches the latest release and installs it over this binary.
// It returns the version installed.
func (c *Client) SelfUpdate(ctx context.Context) (string, error) {
	// Gate on the client's own version, not the package-level buildinfo.Version:
	// the global is always "dev" under `go test`, which made this gate
	// untestable and left the whole Latest → IsNewer → Apply flow unexercised.
	// Same fix as StartBackgroundCheck.
	if c.IsDev() {
		return "", errs.New(errs.CodeUpdateFailed,
			"this is an unstamped development build",
			"install a released binary, or build with the Makefile so the version is stamped in")
	}
	// Comparable subsumes IsDev, but the checks above earn a more specific
	// message; this one catches everything else that cannot be ordered, such
	// as a bare commit SHA from a repo with no tags. Gated before the request
	// for the same reason as IsDev: a lookup could never produce a usable
	// decision, so there is nothing worth spending it on.
	if !c.Comparable() {
		return "", errs.New(errs.CodeUpdateFailed,
			fmt.Sprintf("this build's version %q cannot be compared against a release", c.Version),
			"this binary was not built from a tagged commit — install a released binary, or build with the Makefile so the version is stamped in")
	}

	rel, err := c.Latest(ctx)
	if err != nil {
		return "", err
	}
	if !IsNewer(rel.TagName, c.Version) {
		return c.Version, nil // already current; the caller decides what to say
	}

	exe, err := resolveExecutable()
	if err != nil {
		return "", err
	}
	if err := c.Apply(ctx, rel, exe); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// Apply downloads the release archive for this platform, verifies it, and
// replaces exePath.
//
// The ordering is the entire point: nothing touches the installed binary until
// the downloaded bytes have been checked against the published SHA256. A
// mismatch aborts with the original file byte-identical.
func (c *Client) Apply(ctx context.Context, rel *Release, exePath string) error {
	wantName := AssetName(rel.TagName)

	asset, ok := rel.FindAsset(wantName)
	if !ok {
		return errs.New(errs.CodeUpdateFailed,
			"release "+rel.TagName+" has no asset named "+wantName,
			fmt.Sprintf("no build was published for %s/%s", runtime.GOOS, runtime.GOARCH))
	}

	// The temp file must share a filesystem with the target: os.Rename across
	// devices fails with EXDEV, and /tmp is very often a separate filesystem.
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".s3metrics-update-*")
	if err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed,
			"the install directory is not writable: "+dir,
			"re-run with sudo, or install to a directory you own")
	}
	tmpPath := tmp.Name()
	// Cleanup is unconditional: on success the file has already been renamed
	// away and this is a harmless no-op.
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	sum, err := c.download(ctx, asset.URL, tmp)
	if err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed, "could not finish writing the download", "")
	}

	if err := c.verify(ctx, rel, wantName, sum); err != nil {
		return err
	}

	// CreateTemp rather than a fixed name: it opens with O_EXCL and a random
	// suffix, so a symlink pre-planted at a predictable path in the install
	// directory cannot be followed. Same directory as the target so the rename
	// below stays on one filesystem.
	extracted, err := os.CreateTemp(dir, ".s3metrics-new-*")
	if err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed,
			"could not create a staging file in "+dir,
			"re-run with sudo, or install to a directory you own")
	}
	extractedPath := extracted.Name()
	defer func() {
		extracted.Close()
		os.Remove(extractedPath)
	}()

	if err := extractBinary(tmpPath, extracted); err != nil {
		return err
	}
	if err := extracted.Close(); err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed, "could not finish writing the new binary", "")
	}
	// CreateTemp makes the file 0600; the installed binary must be executable.
	if err := os.Chmod(extractedPath, 0o755); err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed, "could not mark the new binary executable", "")
	}

	return replaceBinary(extractedPath, exePath)
}

// download streams url into w, returning the SHA256 of what was written.
//
// url comes out of the release payload, so it names whatever host that payload
// says. Credentials are only attached when that host is the API host we already
// trust — otherwise a hostile or compromised release would collect GITHUB_TOKEN
// simply by pointing an asset elsewhere. Public release assets need no auth, so
// this costs nothing in the normal case.
func (c *Client) download(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeUpdateFailed, "could not build the download request", "")
	}
	if sameHost(url, c.BaseURL) {
		c.setHeaders(req)
	} else {
		c.setUnauthenticatedHeaders(req)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeUpdateFailed, "could not download the release archive", "")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errs.New(errs.CodeUpdateFailed,
			fmt.Sprintf("downloading the release archive failed with HTTP %d", resp.StatusCode), "")
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxArchiveBytes)); err != nil {
		return "", errs.Wrap(err, errs.CodeUpdateFailed, "could not read the release archive", "")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verify compares the downloaded archive's digest against the published one.
func (c *Client) verify(ctx context.Context, rel *Release, assetName, gotSum string) error {
	checksums, ok := rel.FindAsset(checksumsAsset)
	if !ok {
		return errs.New(errs.CodeUpdateFailed,
			"release "+rel.TagName+" publishes no "+checksumsAsset,
			"the release is incomplete; refusing to install an unverified binary")
	}

	var buf strings.Builder
	if _, err := c.download(ctx, checksums.URL, &buf); err != nil {
		return err
	}

	want, ok := ParseChecksums(strings.NewReader(buf.String()))[assetName]
	if !ok {
		return errs.New(errs.CodeUpdateFailed,
			"no checksum published for "+assetName,
			"the release is incomplete; refusing to install an unverified binary")
	}
	if !strings.EqualFold(want, gotSum) {
		return errs.New(errs.CodeUpdateFailed,
			"checksum mismatch for "+assetName,
			"the download does not match the published checksum — aborting, the installed binary is unchanged")
	}
	return nil
}

// ParseChecksums reads a `<sha256>  <filename>` file, skipping anything
// malformed rather than failing the whole update over one bad line.
func ParseChecksums(r io.Reader) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		out[fields[1]] = fields[0]
	}
	return out
}

// extractBinary pulls the s3metrics executable out of a gzipped tar and writes
// it into dest. It takes an open file rather than a path so no second
// open-by-name is needed — the caller creates dest exclusively.
func extractBinary(archivePath string, dest *os.File) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed, "could not open the downloaded archive", "")
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed, "the downloaded archive is not valid gzip", "")
	}
	defer gz.Close()

	wantName := "s3metrics"
	if runtime.GOOS == "windows" {
		wantName = "s3metrics.exe"
	}

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errs.New(errs.CodeUpdateFailed,
				"the release archive contains no "+wantName+" binary", "")
		}
		if err != nil {
			return errs.Wrap(err, errs.CodeUpdateFailed, "could not read the release archive", "")
		}
		if filepath.Base(hdr.Name) != wantName || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if _, err := io.Copy(dest, io.LimitReader(tr, maxArchiveBytes)); err != nil {
			return errs.Wrap(err, errs.CodeUpdateFailed, "could not extract the new binary", "")
		}
		return nil
	}
}

// replaceBinary swaps newPath over exePath.
//
// On Unix os.Rename is atomic and legal even while the target is executing.
// Windows refuses to rename over a running image, so the current binary is
// moved aside first and restored if the swap fails.
func replaceBinary(newPath, exePath string) error {
	if runtime.GOOS == "windows" {
		old := exePath + ".old"
		os.Remove(old)
		if err := os.Rename(exePath, old); err != nil {
			return errs.Wrap(err, errs.CodeUpdateFailed,
				"could not move the current binary aside",
				"re-run with Administrator privileges, or close other instances")
		}
		if err := os.Rename(newPath, exePath); err != nil {
			// The install failed. Try to put the original back, and if even that
			// fails, say plainly where it is — otherwise the user is left with no
			// binary and no idea the backup exists.
			if restoreErr := os.Rename(old, exePath); restoreErr != nil {
				return errs.Wrap(err, errs.CodeUpdateFailed,
					"could not install the new binary, and the original could not be restored",
					"your original binary is preserved at "+old+" — rename it back to "+exePath)
			}
			return errs.Wrap(err, errs.CodeUpdateFailed,
				"could not install the new binary",
				"the original binary was restored and is unchanged")
		}
		os.Remove(old) // best effort; Windows may hold the running image open
		return nil
	}

	if err := os.Rename(newPath, exePath); err != nil {
		return errs.Wrap(err, errs.CodeUpdateFailed,
			"could not replace the binary at "+exePath,
			"the file is not writable — re-run with sudo, or install to a directory you own")
	}
	return nil
}
