# Releasing

Cutting a release is one command. Most of this document is about the things that
are not obvious, and about the one contract that breaks `--self-update` silently
if you get it wrong.

## Cut a release

```bash
git checkout main && git pull
git tag -a v0.2.0 -m "v0.2.0 — short summary of what changed"
git push origin v0.2.0
```

Pushing a tag matching `v*` triggers `.github/workflows/release.yml`. Nothing
else is needed — do not create the GitHub release by hand, GoReleaser does it.

Watch it:

```bash
gh run watch $(gh run list --workflow=Release --limit 1 --json databaseId -q '.[0].databaseId') --exit-status
```

## What happens automatically

The release workflow, in order:

1. Checks out with `fetch-depth: 0` — GoReleaser needs full history for the changelog.
2. **Runs `gofmt -l .`, `go vet ./...`, and `go test -race ./...`** before building anything.
3. Runs GoReleaser, which builds six binaries, packages them, writes `checksums.txt`, and publishes the GitHub release.

Step 2 exists because the workflow fires on *any* `v*` tag, including one on a
commit that never went through CI. Without it, tagging an unverified commit
would ship it. If the tests fail, the job stops and no release is published —
but **the tag still exists**, so see "When a release fails" below.

Built artifacts: `linux`, `darwin`, `windows` × `amd64`, `arm64`, all as
`.tar.gz`, plus `checksums.txt` (SHA256).

## The naming contract

This is the one that matters.

`.goreleaser.yaml`'s `name_template` and `updater.AssetName` must produce
byte-identical strings. GoReleaser's `{{ .Version }}` **strips the leading `v`**,
which is why `AssetName` calls `strings.TrimPrefix(tag, "v")`:

```
tag v0.2.0  →  s3metrics_0.2.0_linux_amd64.tar.gz
```

If the two ever disagree, `--self-update` fails on **every** release with "no
asset for this platform", and nothing in CI catches it — the updater's own tests
build synthetic archives, so they pass regardless of what GoReleaser actually
names things.

**If you change either side, change both.** They live in:

- `.goreleaser.yaml` → `archives[].name_template` and `project_name`
- `internal/updater/release.go` → `AssetName`

`TestAssetNameMatchesGoReleaserLayout` pins the Go side. To check both sides
against each other before tagging, see "Verify before you tag" below.

Related, and also deliberate: **archives are `tar.gz` on every platform,
including Windows.** There is no `format_overrides`. The self-updater therefore
has exactly one archive format to handle in the code path that replaces a running
binary, and one format is one fewer thing to get wrong there. Do not "fix" this
to `.zip` for Windows without also teaching `extractBinary` about zip.

## Verify before you tag

Reproduce the release locally without publishing anything:

```bash
make snapshot        # requires goreleaser; writes dist/
ls dist/*.tar.gz
```

Then confirm the names match what the updater will look for. This is worth doing
any time you touch either side of the naming contract:

```bash
# what GoReleaser produced
ls dist/ | grep "$(go env GOOS)_$(go env GOARCH)"

# what AssetName expects — run it, do not eyeball the format string
cat > /tmp/an_test.go <<'EOF'
package updater
import ("fmt"; "testing")
func TestPrintAssetName(t *testing.T) { fmt.Println(AssetName("v0.2.0")) }
EOF
cp /tmp/an_test.go internal/updater/zz_an_test.go
go test -run TestPrintAssetName ./internal/updater/ -v | grep s3metrics_
rm internal/updater/zz_an_test.go
```

The two strings must be identical apart from the version, which differs between
a snapshot (`0.0.0-SNAPSHOT-<sha>`) and a real tag.

## Verify after you tag

```bash
gh release view v0.2.0 --json assets -q '.assets[].name'
```

Expect seven entries: six archives and `checksums.txt`.

Then prove the update path works from a released binary rather than trusting it:

```bash
cd "$(mktemp -d)"
curl -sSL https://github.com/charlesfused/s3metrics/releases/download/v0.1.0/s3metrics_0.1.0_linux_amd64.tar.gz | tar xz
./s3metrics --version        # 0.1.0
./s3metrics --check-update   # should name the new version
./s3metrics --self-update
./s3metrics --version        # should now be the new version
```

Use a genuinely older release as the starting point — updating from the version
you just published only proves the no-op path.

## Versioning

Tags are semver with a `v` prefix: `v0.2.0`. The prefix is required — the
workflow triggers on `v*`, and `IsNewer` normalises both sides through
`golang.org/x/mod/semver`, which wants it.

`prerelease: auto` in `.goreleaser.yaml` means a tag like `v0.3.0-rc.1` is
published as a GitHub prerelease automatically. Note that `--self-update` uses
GitHub's `releases/latest` endpoint, which **skips prereleases** — so an rc will
not be offered to users on a stable version. That is intended.

## The repo must stay public

`--self-update` downloads assets from `browser_download_url` on `github.com`, and
deliberately **strips the `Authorization` header when the asset host differs from
the API host** (`sameHost` in `internal/updater/release.go`). On a private repo
that URL returns 404 without credentials, so self-update would break for everyone.

Making this repo private is therefore a breaking change to the update mechanism,
not a visibility setting. It would first need `Apply` reworked to fetch via the
release-asset API URL (`api.github.com/repos/OWNER/REPO/releases/assets/<id>` with
`Accept: application/octet-stream`), which is same-host and so keeps its auth.

## Things not to do

**Do not delete and re-push a tag.** Binaries in the wild cache the result of
their update check for 24h under `os.UserCacheDir()/s3metrics/update-check.json`.
A user who checked while the old tag existed will be told a version is available
that no longer resolves. Ship `v0.2.1` instead.

**Do not edit or re-upload a release asset without re-uploading `checksums.txt`.**
`--self-update` verifies SHA256 before touching the installed binary, so a
mismatched pair makes every update abort. That is the correct behaviour — the
abort leaves the user's binary byte-identical — but the error reads like tampering,
which is exactly what it is designed to look like.

**Do not hand-edit the published release notes into something the changelog
contradicts.** `changelog:` in `.goreleaser.yaml` excludes `^docs:` and `^test:`
commits; if you want something to appear, use a different prefix.

## When a release fails

The tag exists as soon as you push it, whether or not the workflow succeeded.

- **Tests failed in the workflow.** Nothing was published. Fix on `main`, then
  delete the tag locally and remotely and re-push it *only if no release was
  created*: `git tag -d v0.2.0 && git push origin :refs/tags/v0.2.0`. If a
  release object was already created, ship `v0.2.1` instead.
- **GoReleaser failed partway.** Check whether a release object exists
  (`gh release view v0.2.0`). A partial release with some assets missing is worse
  than none — delete it (`gh release delete v0.2.0`) before retrying, or the
  updater may find a release whose asset for some platform does not exist.
- **`--self-update` reports "no asset for this platform"** on a release that looks
  complete: the naming contract broke. Compare the published asset names against
  `AssetName` as described above.

## Keeping versions in sync

Three places pin the Go version and must agree:

- `go.mod` → `go 1.25.0` (a floor imposed by the dependencies, not a preference —
  `aws-sdk-go-v2` needs 1.24 and the `golang.org/x` modules need 1.25)
- `.github/workflows/ci.yml` → `go-version: "1.25"`
- `.github/workflows/release.yml` → `go-version: "1.25"`

CI will not catch a mismatch here; a release built on an older toolchain than
`go.mod` demands will fail at build time in the workflow, which is at least loud.
