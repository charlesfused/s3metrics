// Command s3metrics reports size and object-count metrics for an S3 bucket.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/charlesfused/s3metrics/internal/awsx"
	"github.com/charlesfused/s3metrics/internal/buildinfo"
	"github.com/charlesfused/s3metrics/internal/cli"
	"github.com/charlesfused/s3metrics/internal/errs"
	"github.com/charlesfused/s3metrics/internal/metrics"
	"github.com/charlesfused/s3metrics/internal/metrics/cwsource"
	"github.com/charlesfused/s3metrics/internal/metrics/walksource"
	"github.com/charlesfused/s3metrics/internal/output"
	"github.com/charlesfused/s3metrics/internal/progress"
	"github.com/charlesfused/s3metrics/internal/updater"
)

// progressInterval is how often a running walk reports itself to a terminal.
const progressInterval = 2 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole program, parameterised over its streams so it can be tested
// without a subprocess. It returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := cli.Parse(args, stdout)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		// Parsing failed, so --format was never validated; rendering this as
		// JSON would assume a format the user may not have asked for.
		errs.Render(stderr, err, false)
		return errs.ExitCode(err)
	}

	asJSON := cfg.Format == output.FormatJSON

	if cfg.IsUpdateAction() {
		if err := runUpdateAction(cfg, stdout); err != nil {
			errs.Render(stderr, err, asJSON)
			return errs.ExitCode(err)
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	// Started before collection so the check overlaps the real work; drained
	// afterwards without ever blocking on it.
	stderrIsTTY := progress.IsTTY(osFile(stderr))
	notice := updater.StartBackgroundCheck(updater.New(), cfg.UpdateCheckEnabled(stderrIsTTY))

	rendered, err := collectAndRender(ctx, cfg, stderr, stderrIsTTY)
	if err != nil {
		errs.Render(stderr, err, asJSON)
		return errs.ExitCode(err)
	}

	// stdout gets the complete report or nothing; a partial JSON document would
	// be worse than no output at all.
	if _, err := stdout.Write(rendered); err != nil {
		errs.Render(stderr, errs.Wrap(err, errs.CodeInternal, "could not write the report", ""), false)
		return 1
	}

	drainNotice(notice, stderr)
	return 0
}

// collectAndRender builds the clients, runs the chosen collector, and renders
// the result into a buffer.
func collectAndRender(ctx context.Context, cfg *cli.Config, stderr io.Writer, stderrIsTTY bool) ([]byte, error) {
	renderer, err := output.New(cfg.Format, cfg.NoHeader)
	if err != nil {
		return nil, err
	}

	awsCfg, err := awsx.Load(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return nil, err
	}

	// A client for the lookup only; the real clients are pinned to the region
	// it returns, because CloudWatch publishes a bucket's metrics in the
	// bucket's own region.
	region, err := awsx.ResolveRegion(ctx, awsx.NewS3(awsCfg, awsCfg.Region),
		cfg.Bucket, cfg.Region, awsCfg.Region)
	if err != nil {
		return nil, err
	}

	collector, cleanup := newCollector(cfg, awsCfg, region, stderr, stderrIsTTY)
	defer cleanup()

	report, err := collector.Collect(ctx, cfg.Bucket)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := renderer.Render(&buf, report); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not render the report", "")
	}
	return buf.Bytes(), nil
}

// newCollector builds the collector for the selected mode, plus a cleanup that
// stops any progress reporter it started.
func newCollector(cfg *cli.Config, awsCfg aws.Config, region string,
	stderr io.Writer, stderrIsTTY bool) (metrics.Collector, func()) {

	if cfg.Mode == cli.ModeWalk {
		reporter := progress.New(stderr, stderrIsTTY, progressInterval)
		reporter.Start()

		return walksource.New(awsx.NewS3(awsCfg, region), region, walksource.Options{
			Prefix:      cfg.Prefix,
			Concurrency: cfg.Concurrency,
			Progress:    reporter,
		}), reporter.Stop
	}

	return cwsource.New(awsx.NewCloudWatch(awsCfg, region), region), func() {}
}

// runUpdateAction handles the three invocations that are about the binary
// itself rather than about a bucket.
func runUpdateAction(cfg *cli.Config, stdout io.Writer) error {
	if cfg.ShowVersion {
		fmt.Fprintln(stdout, buildinfo.String())
		return nil
	}

	client := updater.New()
	ctx := context.Background()

	if cfg.CheckUpdate {
		if updater.IsDevBuild() {
			fmt.Fprintln(stdout, "this is an unstamped development build; no update check performed")
			return nil
		}
		rel, err := client.Latest(ctx)
		if err != nil {
			return err
		}
		if updater.IsNewer(rel.TagName, client.Version) {
			fmt.Fprintf(stdout, "a newer version %s is available (running %s)\n", rel.TagName, client.Version)
			return nil
		}
		fmt.Fprintf(stdout, "%s is the latest version\n", client.Version)
		return nil
	}

	installed, err := client.SelfUpdate(ctx)
	if err != nil {
		return err
	}
	if installed == client.Version {
		fmt.Fprintf(stdout, "%s is already the latest version\n", client.Version)
		return nil
	}
	fmt.Fprintf(stdout, "updated to %s\n", installed)
	return nil
}

// drainNotice prints an update notice if one is already waiting. The select's
// default is what enforces the hard constraint: the check may never delay a run.
func drainNotice(notice <-chan string, stderr io.Writer) {
	select {
	case msg, ok := <-notice:
		if ok && msg != "" {
			fmt.Fprintln(stderr, msg)
		}
	default:
	}
}

// osFile recovers the underlying *os.File so TTY detection works, returning nil
// for the buffers tests pass in — which correctly reports "not a terminal".
func osFile(w io.Writer) *os.File {
	f, ok := w.(*os.File)
	if !ok {
		return nil
	}
	return f
}
