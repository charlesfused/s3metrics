# s3metrics — Design

**Date:** 2026-07-29
**Status:** Approved
**Module:** `github.com/csb1582/s3metrics`
**Go:** 1.22

## 1. Purpose

A single-binary CLI that reports size and object-count metrics for one S3 bucket.
It reads AWS's free daily CloudWatch storage metrics by default, or walks the bucket
and computes the numbers directly when the user asks for it. Output is JSON, CSV, or
a human table. The binary updates itself from GitHub Releases.

## 2. Scope

**In scope**

- One bucket per invocation, selected by `--bucket`.
- Two collection modes: CloudWatch snapshot (default) and full bucket walk.
- Three output formats: JSON (default), CSV, table.
- Explicit self-update plus a rate-limited background availability check.
- Typed, categorised error handling with distinct exit codes.

**Out of scope**

- Multiple buckets per invocation.
- Historical time series; only the most recent snapshot is reported.
- S3 Storage Lens.
- Versioned-object and delete-marker accounting.
- Cross-account role assumption beyond what a named AWS profile already provides.

## 3. Architecture

Three independent halves that do not know about each other: **collect** (AWS),
**render** (struct to bytes), **update** (GitHub). `main` is a thin wire-up that maps a
returned error to an exit code.

```
cmd/s3metrics/main.go         thin: parse -> collect -> render -> exit code
internal/cli/                 flag definition, validation -> Config
internal/metrics/             domain types + Collector interface
internal/metrics/cwsource/    CloudWatch collector
internal/metrics/walksource/  concurrent sharded S3 walk collector
internal/output/              Renderer interface + json.go / csv.go / table.go
internal/awsx/                AWS config load, profile and region resolution
internal/errs/                typed codes, exit codes, structured rendering
internal/updater/             GitHub release check, verify, atomic self-replace
internal/buildinfo/           Version / Commit / Date via -ldflags
```

Two interfaces carry the design:

```go
type Collector interface {
    Collect(ctx context.Context, bucket string) (*Report, error)
}

type Renderer interface {
    Render(w io.Writer, r *Report) error
}
```

Each collector declares the **narrow AWS interface it needs** rather than depending on
`*s3.Client` or `*cloudwatch.Client`:

```go
type listObjectsAPI interface {
    ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input,
        opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}
```

Tests inject a plain fake struct. No network, no mock framework, no build tags.
`internal/updater` takes a base URL for the same reason, so `httptest.Server` stands in
for GitHub.

## 4. CLI surface

Flags only, stdlib `flag`. Both `-x` and `--x` forms parse.

| Flag | Default | Notes |
|---|---|---|
| `--bucket NAME` | — | required |
| `--region REGION` | resolved | see §7 |
| `--profile NAME` | — | AWS shared-config profile |
| `--mode metrics\|walk` | `metrics` | |
| `--format json\|csv\|table` | `json` | |
| `--prefix P` | — | walk mode only |
| `--concurrency N` | `8` | walk mode only, must be >= 1 |
| `--timeout DURATION` | none | ceiling on the whole run |
| `--no-header` | false | CSV only |
| `--check-update` | — | report and exit |
| `--self-update` | — | replace and exit |
| `--no-update-check` | false | suppress background nag |
| `--version` | — | print version, commit, build date |

**Validation rules.** Invalid combinations are usage errors (exit 2), never silently
ignored:

- `--bucket` missing, unless `--version`, `--check-update`, or `--self-update` is given.
- `--prefix` or `--concurrency` supplied with `--mode metrics`.
- `--no-header` supplied with a non-CSV format.
- `--concurrency` < 1.
- Unknown `--mode` or `--format` value.
- More than one of `--check-update`, `--self-update`, `--version`.

## 5. Data model

```go
type Report struct {
    Bucket         string             `json:"bucket"`
    Region         string             `json:"region"`
    Source         string             `json:"source"`           // "cloudwatch" | "walk"
    AsOf           time.Time          `json:"as_of"`            // RFC3339
    TotalSizeBytes int64              `json:"total_size_bytes"`
    ObjectCount    int64              `json:"object_count"`
    StorageClasses []StorageClassStat `json:"storage_classes"`
    Prefix         string             `json:"prefix,omitempty"`
    DurationMS     int64              `json:"duration_ms"`
}

type StorageClassStat struct {
    Class       string `json:"class"`
    SizeBytes   int64  `json:"size_bytes"`
    ObjectCount *int64 `json:"object_count"` // nil in cloudwatch mode
    Overhead    bool   `json:"overhead"`     // always false in walk mode
}
```

Both modes emit the same schema. Fields a mode cannot populate are explicitly `null`,
never a zero value.

**The one asymmetry.** CloudWatch's `NumberOfObjects` metric publishes only for
`StorageType=AllStorageTypes`; there is no per-class object count. So in `metrics` mode
the top-level `object_count` is real and every `StorageClassStat.ObjectCount` is `null`.
It is `*int64` precisely so absent is distinguishable from zero.

**`AsOf` is load-bearing.** CloudWatch storage metrics publish once daily, some hours
after the period they cover, so a reading can be 24–48h stale. `AsOf` carries the
datapoint's own timestamp, not `time.Now()`. In walk mode it is the walk start time.

`StorageClasses` is sorted by class name for deterministic output across all formats.

## 6. Output formats

**JSON** (default): one object, raw byte counts, newline-terminated, two-space indent.

**CSV**: long format. One `ALL` row carrying totals, then one row per storage class,
sorted by class. An empty field means null.

```
bucket,region,source,as_of,storage_class,size_bytes,object_count,overhead
mybucket,us-east-1,cloudwatch,2026-07-28T00:00:00Z,ALL,10995116277760,1500000,false
mybucket,us-east-1,cloudwatch,2026-07-28T00:00:00Z,GLACIER,9000000000000,,false
mybucket,us-east-1,cloudwatch,2026-07-28T00:00:00Z,GLACIER_OVERHEAD,48000000000,,true
```

Header suppressed by `--no-header`. `as_of` is RFC3339. CSV deliberately omits `prefix`
and `duration_ms`: they are run metadata, not per-row facts, and repeating them on every
row would corrupt aggregation. Callers needing them should use JSON.

**Table**: `text/tabwriter`, humanized sizes (`10.0 TiB`) since it is the only format
intended for eyes. JSON and CSV always carry raw bytes.

**stdout discipline.** stdout receives the complete report or nothing. The rendered
report is buffered and flushed only on success, so a failure mid-collection can never
emit a partial JSON document. All diagnostics, progress, and errors go to stderr.

## 7. Region resolution

CloudWatch storage metrics for a bucket live in the **bucket's own region**. Querying
the wrong region returns an empty result set with no error, which is the most common
cause of "my metrics are empty".

Resolution order:

1. `--region` if given.
2. `s3:GetBucketLocation` on the bucket, using a client built from the ambient config.
3. The AWS config's own region, as a last resort.

The resolved region is recorded in `Report.Region` so the output is self-describing.

## 8. Metrics mode (CloudWatch)

1. **Discover** available metrics: `cloudwatch:ListMetrics` for namespace `AWS/S3`,
   dimension `BucketName=<bucket>`. This yields the `StorageType` values that actually
   exist for this bucket rather than guessing from a hardcoded list.
2. **Query**: `GetMetricData` with one query per discovered `BucketSizeBytes` storage
   type, plus one for `NumberOfObjects` at `StorageType=AllStorageTypes`.
   - `StartTime` = now − 72h, `EndTime` = now, `Period` = 86400, `Stat` = `Average`.
   - `ScanBy = TimestampDescending`; take the first (most recent) datapoint per query.
   - The 72h lookback gives margin over the ~48h worst-case publish lag.
3. **Classify** each `StorageType`:
   - A name ending in `ObjectOverhead` or `SizeOverhead` is **overhead**
     (`GlacierObjectOverhead`, `GlacierS3ObjectOverhead`, `DeepArchiveObjectOverhead`,
     `StandardIASizeOverhead`, …).
   - Everything else is a real storage class.
   - A suffix rule, not a fixed list, so storage classes AWS adds later are counted as
     real data rather than silently dropped.
4. **Total**: `TotalSizeBytes` sums **only the real storage classes**. Overhead rows
   still appear in `StorageClasses` with `Overhead: true`, so a user can reconcile
   against the bill by adding them back. This makes the two modes directly comparable —
   walk mode sums per-object `Size`, which structurally cannot include overhead.
5. **Naming**: `StorageType` values map to canonical class names
   (`StandardStorage` → `STANDARD`, `GlacierStorage` → `GLACIER`,
   `GlacierInstantRetrievalStorage` → `GLACIER_IR`, …). Unknown values pass through as
   their raw `StorageType` name rather than being discarded.
6. **`AsOf`** is the newest timestamp among the returned datapoints.

If `ListMetrics` returns nothing, or `GetMetricData` yields no datapoints, that is the
`no_metrics` error (§10) — not a zero-valued success. An empty result almost always
means the bucket is new, empty, or in a different region.

## 9. Walk mode (concurrent sharded)

**Shard discovery.** `ListObjectsV2` with `Delimiter: "/"` (and `--prefix` if given)
returns `CommonPrefixes` — top-level "directories" — plus any loose objects at that
level. If fewer prefixes than `--concurrency` are found, descend one more level and
re-split. Depth is capped at 2: `bucket/data/2026/…` is the layout this rescues, and
beyond depth 2 discovery cost starts eating the parallelism it buys.

**Coverage invariant.** Every object must land in exactly one shard. `CommonPrefixes`
returned for a single delimiter query are disjoint by construction, so prefix shards
never overlap. Loose objects — keys at a discovery level that contain no further
delimiter — are not covered by any child prefix, so **each discovery level contributes
its own loose-object shard**, counted directly from the discovery response rather than
re-listed. Descending a level replaces that level's prefix shards with its children but
keeps its loose-object shard. A test asserts total object count from a sharded walk
equals that of a forced-sequential walk over the same fake data.

**Flat-bucket fallback.** If discovery still finds fewer than 2 shards, the bucket is
effectively flat; sharding buys nothing. Fall back to a single sequential paginated
walk through the same code path with `concurrency = 1`.

> **Known limitation, stated plainly:** this design's speedup is a function of prefix
> shape. A bucket with a million keys in one flat namespace walks sequentially no matter
> what `--concurrency` says. Documented rather than papered over.

**Execution.** `errgroup.WithContext` with N workers reading from a shard channel.

- Each worker paginates its own prefix and accumulates into a **worker-local** aggregate
  (size, count, `map[string]classStat`). No shared mutable state, no mutex.
- Workers return their aggregate; the merge is serial after `g.Wait()` returns.
- The race detector has nothing to find because nothing is shared — a stronger guarantee
  than a lock that happens to be correct today.
- The first error cancels the context; all workers unwind and the error propagates.

**Cancellation** has exactly one mechanism: the context. `--timeout` and SIGINT
(`signal.NotifyContext`) both enter through it.

**Object accounting.** Size is `Object.Size`; class is `Object.StorageClass`, defaulting
to `STANDARD` when the field is absent. Every `StorageClassStat` has `Overhead: false`
and a non-nil `ObjectCount`.

**Throttling.** High fan-out on `ListObjectsV2` earns `503 SlowDown`. The S3 client uses
`retry.NewAdaptiveMode()` — client-side rate limiting that backs off under pressure
rather than retrying blindly — and `--concurrency` defaults to a deliberately modest 8.

**Progress.** An atomic object counter plus a 2s ticker goroutine printing
`scanned 1,240,000 objects · 3/64 shards remaining` to **stderr, only when stderr is a
TTY**. Piped and CI output stay clean.

## 10. Error handling

```go
type Error struct {
    Code Code
    Msg  string
    Hint string
    Err  error
}
```

| Code | Exit | Hint |
|---|---|---|
| `usage` | 2 | flag conflict or missing `--bucket` |
| `no_credentials` | 3 | set `AWS_PROFILE`, pass `--profile`, or run `aws configure` |
| `expired_credentials` | 4 | run `aws sso login --profile X` |
| `access_denied` | 5 | names the permission actually needed |
| `bucket_not_found` | 6 | check bucket name and region |
| `no_metrics` | 7 | bucket may be new or empty; CloudWatch publishes daily — try `--mode walk` |
| `throttled` | 8 | lower `--concurrency` |
| `network` | 9 | connectivity or endpoint problem |
| `canceled` | 10 | timeout exceeded or interrupted |
| `update_failed` | 11 | checksum mismatch, no asset for platform, target not writable |
| `internal` | 1 | unexpected; includes the wrapped cause |

**Classification** is one table-driven function inspecting Smithy `APIError` codes and
SDK typed errors: `*types.NoSuchBucket`, `NoSuchBucket`, `NotFound`, `AccessDenied`,
`AccessDeniedException`, `InvalidAccessKeyId`, `SignatureDoesNotMatch`, `ExpiredToken`,
`ExpiredTokenException`, `RequestTimeTooSkewed`, `SlowDown`, `Throttling`,
`ThrottlingException`, credential-retrieval failures from `config.LoadDefaultConfig`,
and `context.DeadlineExceeded` / `context.Canceled`.

`no_metrics` earns its own code deliberately: an empty CloudWatch response is the most
confusing outcome of the default mode, and reporting "no error, zero bytes" would be a
lie.

**Rendering.** With `--format json`, stderr receives a structured object; otherwise
plain text. Either way stdout stays empty and the process exits with the mapped code.

```json
{"error":{"code":"no_credentials","message":"no AWS credentials found","hint":"set AWS_PROFILE, pass --profile, or run aws configure"}}
```

```
s3metrics: no AWS credentials found
  hint: set AWS_PROFILE, pass --profile, or run aws configure
```

**Required IAM permissions**, surfaced in `--help` and the README:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["s3:GetBucketLocation", "s3:ListBucket"],
      "Resource": "arn:aws:s3:::BUCKET" },
    { "Effect": "Allow",
      "Action": ["cloudwatch:ListMetrics", "cloudwatch:GetMetricData"],
      "Resource": "*" }
  ]
}
```

`s3:ListBucket` is needed only for walk mode; the CloudWatch actions only for metrics
mode.

## 11. Self-update

Version, commit, and build date are injected into `internal/buildinfo` via
`-ldflags -X`. An un-stamped local build reports `dev` and **refuses** `--self-update`
with a clear message rather than "updating" a binary whose version it cannot reason
about.

**Release source.** `https://api.github.com/repos/{owner}/{repo}/releases/latest`,
defaulting to `csb1582/s3metrics` and overridable at runtime via the
`S3METRICS_UPDATE_REPO` environment variable (`owner/repo` form). Requests carry a
10s timeout and an explicit `User-Agent` — GitHub rejects requests without one — and
honour `GITHUB_TOKEN` when set, lifting the 60/hr anonymous rate limit. Versions are
compared with `golang.org/x/mod/semver`.

**Verify, then replace.** The ordering is the entire point:

1. Resolve the real path: `os.Executable()` then `filepath.EvalSymlinks`.
2. Download the platform asset (`s3metrics_<version>_<os>_<arch>.tar.gz`, GoReleaser's
   default naming) to a temp file **in the same directory as the executable**.
   `os.Rename` across filesystems fails with `EXDEV`, and `/tmp` is very often a
   different filesystem.
3. Fetch `checksums.txt` and verify SHA256 for that exact asset. On mismatch: delete the
   temp file, abort, leave the original binary byte-identical.
4. Extract the binary from the archive, `chmod 0755`.
5. `os.Rename` over the running binary — atomic on Unix and legal while executing.
6. On Windows, a running `.exe` cannot be renamed over: move the current one aside to
   `.old`, rename the new one into place, then best-effort delete `.old`.

A non-writable target (binary in `/usr/local/bin` owned by root) surfaces as
`update_failed` with a hint to re-run under `sudo` — not a permission stack trace.

**Background availability check.** Cache at
`os.UserCacheDir()/s3metrics/update-check.json` holding `{last_checked, latest_version}`.
On a normal metrics run, if the cached entry is older than 24h, the check runs in a
goroutine under a 3s context while collection proceeds. If it completes and finds a newer
release, one line goes to stderr **after** the report has been written:

```
a newer version v1.3.0 is available — run: s3metrics --self-update
```

If it is slow, fails, or is rate-limited, it is dropped silently and the cache is not
updated.

> **Hard constraint:** the background check must never delay, fail, or alter the exit
> code of a metrics run. It is best-effort and strictly subordinate to the actual work.

Suppressed by `--no-update-check`, `S3METRICS_NO_UPDATE_CHECK=1`, or stderr not being a
TTY — the same TTY rule that governs progress output.

## 12. Testing

No test touches AWS or GitHub. Each collector depends on the narrow interface it
declared, so tests inject a fake.

**`walksource`** — multi-page pagination; shard fan-out at concurrency > 1; depth-2
re-split when the top level is too narrow; flat-bucket sequential fallback; one worker
erroring cancels the rest; context cancellation mid-walk; storage-class aggregation;
missing `StorageClass` defaults to `STANDARD`; and the **coverage invariant** — a
sharded walk and a forced-sequential walk over identical fake data produce identical
totals, including buckets with loose objects at every discovery level. Runs under
`-race`.

**`cwsource`** — `StorageType` to class-name mapping; overhead suffix classification;
total excludes overhead but overhead rows are still present; empty datapoints produce
`no_metrics`; `AsOf` picks the newest datapoint rather than now; per-class
`ObjectCount` stays nil; unknown `StorageType` passes through by raw name.

**`output`** — golden files for all three renderers, covering a report with null
per-class counts, one with zero storage classes, and `--no-header` CSV.

**`updater`** — `httptest.Server` serving fake release JSON, a real `.tar.gz`, and a
`checksums.txt`. Cases: happy path; **checksum mismatch aborts and leaves the original
file byte-identical**; already-current is a no-op; 403 rate limit; no asset for this
platform; unwritable target; `dev` build refuses to update. The replace target is a
temp file, never the test binary.

**`errs`** — table-driven classification from synthetic Smithy and SDK errors, asserting
both code and exit status.

**`cli`** — flag parsing plus every invalid-combination rejection from §4.

## 13. Release tooling

`.goreleaser.yaml` builds linux/darwin/windows × amd64/arm64, packages `.tar.gz`
(`.zip` for Windows), emits `checksums.txt`, and stamps `buildinfo` via ldflags.

GitHub Actions: `go vet` and `go test -race ./...` on every PR; on a `v*` tag, GoReleaser
publishes the release.

`Makefile` targets: `build`, `test`, `race`, `lint`, `snapshot`. `make snapshot` produces
a local release layout so the updater can be exercised end-to-end against a static file
server.

## 14. Dependencies

- `github.com/aws/aws-sdk-go-v2` (config, s3, cloudwatch, retry)
- `golang.org/x/sync/errgroup`
- `golang.org/x/mod/semver`
- `golang.org/x/term` (TTY detection)

Everything else is stdlib. CLI parsing, CSV, tabwriter, JSON, tar/gzip, and SHA256 all
come from the standard library.
