# s3metrics

Report size and object-count metrics for an S3 bucket. Single Go binary, no runtime dependencies, updates itself.

## Install

Download a release archive for your platform from the [releases page](https://github.com/charlesfused/s3metrics/releases), or build from source:

```bash
make build
```

Once installed, the binary keeps itself current:

```bash
s3metrics --check-update   # report whether a newer release exists
s3metrics --self-update    # download, verify, and install it
```

Updates are verified against the SHA256 published in the release's `checksums.txt` before anything replaces the running binary. A mismatch aborts with the installed binary untouched.

## Usage

```bash
s3metrics --bucket my-bucket                          # JSON from CloudWatch (default)
s3metrics --bucket my-bucket --format table           # human-readable
s3metrics --bucket my-bucket --format csv             # one ALL row plus one row per class
s3metrics --bucket my-bucket --mode walk              # list every object and count directly
s3metrics --bucket my-bucket --mode walk --prefix logs/ --concurrency 16
```

Run `s3metrics --help` for the full switch list.

## The two modes

**`--mode metrics`** (default) reads AWS's daily CloudWatch storage metrics. These are enabled on every bucket automatically, cost nothing, and need no setup — but they publish once a day, so a reading can trail wall-clock time by 24-48 hours. The `as_of` field carries the datapoint's own timestamp so you always know how stale the answer is.

**`--mode walk`** lists every object and computes the totals directly. Exact and current, at the cost of one LIST request per 1000 objects. The walk splits the bucket into disjoint prefixes and scans them in parallel.

> The walk's speedup depends entirely on prefix shape. A bucket with a million keys in one flat namespace walks sequentially no matter what `--concurrency` says, because `ListObjectsV2` continuation tokens are inherently sequential within a prefix.

### Why the two modes can disagree

The two modes do not always measure the same thing. On a versioned bucket they measure genuinely different things, and the gap can be a multiple rather than a percentage — so do not read a disagreement as one mode being wrong.

- **Versioning (the dominant cause).** CloudWatch's `BucketSizeBytes` and `NumberOfObjects` count every noncurrent version and every delete marker. `ListObjectsV2` returns current versions only, so `--mode walk` reports **current-version bytes only**. On a bucket with heavy overwrite traffic and no expiration lifecycle rule, metrics mode can be many times larger.
- **Incomplete multipart uploads.** Parts uploaded but never completed are counted by the storage metrics and are invisible to a listing, so walk mode never sees them. An abort-incomplete-multipart lifecycle rule is the usual fix.
- **Glacier overhead.** `total_size_bytes` counts object data only. CloudWatch also reports per-object bookkeeping — Glacier metadata, for instance — under storage types like `GlacierObjectOverhead`. Those appear as their own rows with `"overhead": true` and are **excluded** from the total, so the two modes stay comparable. It also means the total is lower than the S3 console's figure, which includes overhead; add the overhead rows back to reconcile against your bill.

## Output

JSON (default) and CSV carry raw byte counts; the table format humanizes sizes because it is the only one meant for eyes. stdout carries the report or nothing — diagnostics, progress, and errors all go to stderr, so `s3metrics --bucket b | jq` is always safe.

In metrics mode, per-class `object_count` is `null`: CloudWatch's `NumberOfObjects` metric only publishes for `AllStorageTypes`, so a per-class count does not exist to report. The top-level `object_count` is real in both modes.

## Required IAM permissions

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetBucketLocation", "s3:ListBucket"],
      "Resource": "arn:aws:s3:::BUCKET"
    },
    {
      "Effect": "Allow",
      "Action": ["cloudwatch:ListMetrics", "cloudwatch:GetMetricData"],
      "Resource": "*"
    }
  ]
}
```

`s3:ListBucket` is needed only for walk mode; the CloudWatch actions only for metrics mode.

## Exit codes

| Code | Meaning | Code | Meaning |
|---|---|---|---|
| 0 | success | 6 | bucket not found |
| 1 | internal error | 7 | no metrics available |
| 2 | usage error | 8 | throttled by AWS |
| 3 | no credentials | 9 | network error |
| 4 | expired credentials | 10 | canceled or timed out |
| 5 | access denied | 11 | update failed |

With `--format json`, errors are written to stderr as `{"error":{"code":…,"message":…,"hint":…}}`, so a script can parse a failure the same way it parses a success.

## Environment

| Variable | Effect |
|---|---|
| `AWS_PROFILE`, `AWS_REGION`, … | standard AWS SDK configuration |
| `S3METRICS_UPDATE_REPO` | repoint self-update at another `owner/repo` |
| `S3METRICS_NO_UPDATE_CHECK` | set to any non-empty value to silence the background update notice |
| `GITHUB_TOKEN` | lifts GitHub's 60-per-hour anonymous API rate limit |

## Development

```bash
make test    # go test ./...
make race    # go test -race ./...
make vet
make build
make snapshot  # local release layout under dist/
```

No test touches AWS or GitHub: collectors take narrow interfaces that tests fill with fakes, and the updater is exercised against an `httptest.Server`.
