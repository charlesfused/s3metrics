// Package buildinfo carries release identity stamped in at link time.
//
// The three vars below are the -ldflags -X targets. A build that does not stamp
// them reports "dev", which the updater treats as un-upgradeable: it has no way
// to compare an unstamped build against a release tag.
package buildinfo

import (
	"fmt"
	"runtime"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// IsDev reports whether this binary was built without release stamping.
func IsDev() bool {
	return Version == "" || Version == "dev"
}

// String renders the one-line identity printed by --version.
func String() string {
	return fmt.Sprintf("s3metrics %s (commit %s, built %s, %s/%s)",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH)
}
