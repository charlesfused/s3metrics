package progress

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a Writer safe for concurrent use, since the ticker goroutine and
// the test goroutine both touch it.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestDisabledReporterWritesNothing(t *testing.T) {
	var buf syncBuf
	r := New(&buf, false, time.Millisecond)
	r.Start()
	r.SetShards(4)
	r.AddObjects(1000)
	time.Sleep(20 * time.Millisecond)
	r.Stop()

	if got := buf.String(); got != "" {
		t.Errorf("disabled reporter wrote %q, want nothing", got)
	}
}

func TestEnabledReporterEmitsProgress(t *testing.T) {
	var buf syncBuf
	r := New(&buf, true, time.Millisecond)
	r.Start()
	r.SetShards(4)
	r.AddObjects(1234)

	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(buf.String(), "1,234") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no progress line containing the object count appeared; got %q", buf.String())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	r.Stop()
}

func TestStopIsIdempotent(t *testing.T) {
	r := New(&syncBuf{}, true, time.Millisecond)
	r.Start()
	r.Stop()
	r.Stop() // must not panic on a closed channel
}

func TestNilReporterIsSafe(t *testing.T) {
	var r *Reporter
	r.Start()
	r.SetShards(1)
	r.AddObjects(1)
	r.ShardDone()
	r.Stop()
}

func TestConcurrentAddObjects(t *testing.T) {
	r := New(&syncBuf{}, true, time.Hour) // long interval: exercise counters, not output
	r.Start()
	defer r.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.AddObjects(1)
			}
		}()
	}
	wg.Wait()

	if got := r.Objects(); got != 5000 {
		t.Errorf("Objects() = %d, want 5000", got)
	}
}
