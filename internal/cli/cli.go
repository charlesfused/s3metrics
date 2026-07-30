// Package cli defines the command's switches and the rules for combining them.
//
// Every invalid combination is rejected rather than ignored: silently dropping a
// flag the user passed is how a tool ends up reporting the wrong bucket.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/output"
)

// Collection modes.
const (
	ModeMetrics = "metrics"
	ModeWalk    = "walk"
)

// Config is the fully validated result of parsing the command line.
type Config struct {
	Bucket  string
	Region  string
	Profile string
	Mode    string
	Format  string

	Prefix      string
	Concurrency int
	Timeout     time.Duration
	NoHeader    bool

	// IncludeVersions makes a walk count noncurrent versions and delete markers
	// as well as current objects, which is what metrics mode already counts.
	IncludeVersions bool

	CheckUpdate   bool
	SelfUpdate    bool
	NoUpdateCheck bool
	ShowVersion   bool
}

// IsUpdateAction reports whether the invocation is about the binary itself
// rather than about a bucket.
func (c *Config) IsUpdateAction() bool {
	return c.ShowVersion || c.CheckUpdate || c.SelfUpdate
}

// UpdateCheckEnabled reports whether the background availability check should
// run. It is suppressed by the flag, by the environment, and whenever stderr is
// not a terminal — a nag line has no audience in a pipe or in CI.
func (c *Config) UpdateCheckEnabled(stderrIsTTY bool) bool {
	if c.NoUpdateCheck || !stderrIsTTY {
		return false
	}
	return os.Getenv("S3METRICS_NO_UPDATE_CHECK") == ""
}

// Parse reads args (excluding argv[0]). It returns flag.ErrHelp unchanged when
// help was requested, so the caller can exit 0 for that case.
func Parse(args []string, out io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("s3metrics", flag.ContinueOnError)

	// Silence the flag package's own output. On a parse error it would print
	// both its one-line message and the full usage banner, while a
	// validation error prints only a one-liner — inconsistent, and the banner
	// buries the actual problem. Parse returns a categorised error for the
	// caller to render instead, and --help is handled explicitly below.
	fs.SetOutput(io.Discard)

	c := &Config{}
	fs.StringVar(&c.Bucket, "bucket", "", "S3 bucket name (required)")
	fs.StringVar(&c.Region, "region", "", "AWS region (default: resolved from the bucket)")
	fs.StringVar(&c.Profile, "profile", "", "AWS shared-config profile")
	fs.StringVar(&c.Mode, "mode", ModeMetrics, "collection mode: metrics or walk")
	fs.StringVar(&c.Format, "format", output.FormatJSON, "output format: json, csv, or table")
	fs.StringVar(&c.Prefix, "prefix", "", "limit a walk to this key prefix (walk mode only)")
	fs.IntVar(&c.Concurrency, "concurrency", 8, "parallel shard walkers (walk mode only)")
	fs.BoolVar(&c.IncludeVersions, "include-versions", false,
		"count noncurrent versions and delete markers too (walk mode only)")
	fs.DurationVar(&c.Timeout, "timeout", 0, "ceiling on the whole run, e.g. 5m (default: none)")
	fs.BoolVar(&c.NoHeader, "no-header", false, "omit the CSV header row (csv format only)")
	fs.BoolVar(&c.CheckUpdate, "check-update", false, "report whether a newer release exists, then exit")
	fs.BoolVar(&c.SelfUpdate, "self-update", false, "download and install the latest release, then exit")
	fs.BoolVar(&c.NoUpdateCheck, "no-update-check", false, "suppress the background update notice")
	fs.BoolVar(&c.ShowVersion, "version", false, "print version information, then exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// PrintDefaults writes to the flag set's own configured output, not
			// to the out parameter — and fs.SetOutput above points that at
			// io.Discard so a parse error doesn't print twice. Point it at out
			// for this one call so the switch list actually reaches the
			// banner; parsing is already finished by the time usage runs, so
			// there is nothing left for the discard to protect.
			fs.SetOutput(out)
			usage(out, fs) // --help is a request for the banner, not an error
			return nil, flag.ErrHelp
		}
		return nil, errs.New(errs.CodeUsage, err.Error(), "run s3metrics --help for the full switch list")
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if err := c.validate(set, fs.Args()); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate(set map[string]bool, positional []string) error {
	if len(positional) > 0 {
		return errs.New(errs.CodeUsage,
			"unexpected argument: "+positional[0],
			"this command takes switches only — did you mean --bucket "+positional[0]+"?")
	}

	actions := 0
	for _, on := range []bool{c.ShowVersion, c.CheckUpdate, c.SelfUpdate} {
		if on {
			actions++
		}
	}
	if actions > 1 {
		return errs.New(errs.CodeUsage,
			"--version, --check-update, and --self-update are mutually exclusive",
			"pass exactly one of them")
	}
	if actions == 1 {
		return nil // these actions are about the binary; bucket flags do not apply
	}

	if c.Bucket == "" {
		return errs.New(errs.CodeUsage, "missing required flag: --bucket",
			"pass the bucket name, e.g. --bucket my-bucket")
	}

	switch c.Mode {
	case ModeMetrics, ModeWalk:
	default:
		return errs.New(errs.CodeUsage, "unknown --mode value: "+c.Mode,
			"valid values are metrics and walk")
	}

	switch c.Format {
	case output.FormatJSON, output.FormatCSV, output.FormatTable:
	default:
		return errs.New(errs.CodeUsage, "unknown --format value: "+c.Format,
			"valid values are json, csv, and table")
	}

	if c.Mode == ModeMetrics {
		// --include-versions belongs here rather than being quietly accepted:
		// CloudWatch's NumberOfObjects already counts every version and delete
		// marker, so the switch asks metrics mode for something it cannot change.
		for _, f := range []string{"prefix", "concurrency", "include-versions"} {
			if set[f] {
				return errs.New(errs.CodeUsage,
					"--"+f+" applies to walk mode only",
					"add --mode walk, or drop --"+f)
			}
		}
	}

	if c.Concurrency < 1 {
		return errs.New(errs.CodeUsage,
			fmt.Sprintf("--concurrency must be at least 1, got %d", c.Concurrency),
			"try --concurrency 8")
	}

	if c.Timeout < 0 {
		return errs.New(errs.CodeUsage,
			"--timeout must not be negative",
			"pass a positive duration such as --timeout 5m, or omit it for no ceiling")
	}

	if c.NoHeader && c.Format != output.FormatCSV {
		return errs.New(errs.CodeUsage,
			"--no-header applies to the csv format only",
			"add --format csv, or drop --no-header")
	}

	return nil
}

func usage(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(out, `s3metrics — report size and object-count metrics for an S3 bucket

Usage:
  s3metrics --bucket NAME [switches]
  s3metrics --version | --check-update | --self-update

Switches:
`)
	fs.PrintDefaults()
	fmt.Fprint(out, `
Modes:
  metrics  (default) read AWS's free daily CloudWatch storage metrics. Fast, but
           the data is published once a day and can trail by 24-48h — see the
           as_of field. Requires cloudwatch:ListMetrics and cloudwatch:GetMetricData.
  walk     list every object and compute the totals directly. Exact and current,
           but costs one LIST request per 1000 objects. Requires s3:ListBucket.

On a versioned bucket the two modes count different things: CloudWatch counts
noncurrent versions and delete markers, a plain listing does not. Add
--include-versions to make a walk count them too and the two comparable.

Required IAM permissions:
  s3:GetBucketLocation                              (both modes)
  s3:ListBucket                                     (walk mode)
  s3:ListBucketVersions                             (walk mode, --include-versions)
  cloudwatch:ListMetrics, cloudwatch:GetMetricData  (metrics mode)
  s3:GetBucketVersioning                            (optional; reports the
                                                     versioned field, and is
                                                     skipped silently if denied)

Exit codes:
  0 success    3 no credentials     6 bucket not found   9 network
  1 internal   4 expired creds      7 no metrics        10 canceled
  2 usage      5 access denied      8 throttled         11 update failed
`)
}
