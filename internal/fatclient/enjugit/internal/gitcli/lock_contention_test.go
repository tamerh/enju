package gitcli

// Reproduction for the agent-lifecycle hang (prisma run #4): a fresh
// daemon's first WithLock blocked forever on the cross-process
// project flock held by a lingering stale process — silently, with
// no diagnostic, not even ctx-interruptible. clone.go's lock() /
// WithLock() did `_ = c.fileLock.Lock()` (blocking, error discarded,
// no log).
//
// Two OpenClone handles on the same lockPath contend via flock(2)
// even in one process (separate fds → LOCK_EX conflicts), so this
// exercises the exact cross-process path without spawning a second
// process or needing the LLM/prisma setup.
//
// The contract pinned here ("bounded + loud, never fail-open"):
//   - blocked acquisition emits a WARN naming the lock path within
//     a generous bound (pre-fix: silent forever → RED);
//   - it NEVER proceeds while another holder has the lock (safety:
//     no fail-open, both before and after the fix);
//   - once the holder releases, the contender acquires.

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is an io.Writer safe for the slog handler (written from
// the blocked contender goroutine) + the test reader.
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

func TestProjectLock_ContentionIsObservableNeverFailOpen(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	lockPath := filepath.Join(dir, ".enju", "locks", "proj.lock")

	// Holder: a second handle holds the cross-process flock.
	holder, err := OpenClone(dir, lockPath, nullLogger())
	if err != nil {
		t.Fatalf("OpenClone holder: %v", err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer doRelease() // ensure the holder goroutine can always exit

	go func() {
		_ = holder.WithLock(func(Ops) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held // holder now owns the flock

	// Contender with a WARN-capturing logger.
	logs := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	contender, err := OpenClone(dir, lockPath, logger)
	if err != nil {
		t.Fatalf("OpenClone contender: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		rel := contender.lock() // blocks while holder holds the flock
		defer rel()
		close(acquired)
	}()

	// Within a generous bound the block must be OBSERVABLE (a WARN
	// naming the lock path) and must NOT have proceeded unlocked.
	select {
	case <-acquired:
		t.Fatal("contender acquired the lock while the holder still held it — fail-open / lock not honored (must never happen)")
	case <-time.After(3 * time.Second):
		got := logs.String()
		if !strings.Contains(got, lockPath) {
			t.Fatalf("blocked on the project lock for 3s with NO diagnostic naming %q — "+
				"silent unbounded hang (the run #4 defect). Captured WARN+ logs:\n%s",
				lockPath, got)
		}
	}

	// Never fail-open: it only proceeds AFTER the holder releases.
	doRelease()
	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		t.Fatal("contender never acquired even after the holder released the lock")
	}
}
