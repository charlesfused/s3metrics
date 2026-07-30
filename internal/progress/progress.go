// Package progress reports long-walk progress to a terminal.
//
// Output goes to stderr and only when stderr is a terminal, so piped and CI
// output stay clean. Every method is nil-safe, letting callers hold a *Reporter
// that may be absent without branching at each call site.
package progress

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// Reporter accumulates counters and prints them on a ticker.
type Reporter struct {
	w        io.Writer
	enabled  bool
	interval time.Duration

	objects   atomic.Int64
	shards    atomic.Int64
	shardsRem atomic.Int64

	stopOnce sync.Once
	done     chan struct{}
	finished chan struct{}
}

// New returns a Reporter writing to w. When enabled is false every method is a
// no-op and nothing is ever written.
func New(w io.Writer, enabled bool, interval time.Duration) *Reporter {
	return &Reporter{
		w:        w,
		enabled:  enabled,
		interval: interval,
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
}

// IsTTY reports whether f is a terminal.
func IsTTY(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// Start begins the ticker. Safe to call on a nil or disabled Reporter.
func (r *Reporter) Start() {
	if r == nil || !r.enabled {
		return
	}
	go func() {
		defer close(r.finished)
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-r.done:
				return
			case <-t.C:
				r.emit()
			}
		}
	}()
}

// Stop halts the ticker and waits for the goroutine to exit. Idempotent.
func (r *Reporter) Stop() {
	if r == nil || !r.enabled {
		return
	}
	r.stopOnce.Do(func() {
		close(r.done)
		<-r.finished
	})
}

// AddObjects records n more scanned objects.
func (r *Reporter) AddObjects(n int64) {
	if r == nil {
		return
	}
	r.objects.Add(n)
}

// SetShards records the total shard count.
func (r *Reporter) SetShards(n int64) {
	if r == nil {
		return
	}
	r.shards.Store(n)
	r.shardsRem.Store(n)
}

// ShardDone records one completed shard.
func (r *Reporter) ShardDone() {
	if r == nil {
		return
	}
	r.shardsRem.Add(-1)
}

// Objects returns the running object count. Exported for tests.
func (r *Reporter) Objects() int64 {
	if r == nil {
		return 0
	}
	return r.objects.Load()
}

func (r *Reporter) emit() {
	objects := r.objects.Load()
	total := r.shards.Load()
	if total == 0 {
		fmt.Fprintf(r.w, "scanned %s objects\n", comma(objects))
		return
	}
	fmt.Fprintf(r.w, "scanned %s objects · %d/%d shards remaining\n",
		comma(objects), r.shardsRem.Load(), total)
}

// comma groups digits so a seven-figure object count is readable at a glance.
func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
