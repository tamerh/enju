package gitcli

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
)

// Clone is the handle on one project's local git checkout, backed
// by the system `git` CLI. Mirrors gitv5 / gitv6's shape exactly
// (same fields above the library seam, same behavioural contracts
// on each method) so the Ops interface is identical across
// backends.
type Clone struct {
	workDir   string
	remoteURL string
	logger    *slog.Logger

	mu sync.Mutex

	fileLock *flock.Flock

	// holder is the goroutine ID that currently holds mu, or 0
	// when the lock is free. Set inside mu.Lock(), cleared
	// before mu.Unlock(). Read without the mutex during the
	// reentrancy fast-path: a goroutine that observes its own
	// id here is guaranteed to already be the holder, so it can
	// safely skip re-acquisition.
	//
	// Same per-goroutine reentrancy as gitv6 — process-global
	// `reentrant bool` was the bug behind #381 (parallel-merge
	// race), don't reintroduce it.
	holder atomic.Uint64

	lastPushAt    time.Time
	lastPushError string
}

// OpenClone opens an existing git clone at workDir. If workDir
// doesn't contain a .git, returns ErrCloneNotFound. Hydrates
// remoteURL from `git remote get-url origin` so lazy-fetch and
// push paths know where to talk to.
//
// lockPath is the file used for the cross-process flock. Pass
// "" to skip cross-process locking (test fixtures).
//
// logger may be nil — falls back to slog.Default().
//
// Note on tmp_pack_* sweeping: gitv6's OpenClone proactively
// removed orphan `tmp_pack_<n>` files because go-git's pack
// reader would scan them and trip on the partial content.
// Real `git` doesn't scan tmp_pack_* during fetch / read paths
// (it's git's own private staging prefix), so orphans are
// harmless to operations — verified empirically. We don't
// sweep here; old orphans accumulate as minor disk-space waste
// that `git gc` reaps. See TestFetchTolerantOfLeftoverTmpPack
// for the regression pin.
func OpenClone(workDir, lockPath string, logger *slog.Logger) (*Clone, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrCloneNotFound, workDir)
		}
		return nil, fmt.Errorf("git: stat %s/.git: %w", workDir, err)
	}
	c := &Clone{
		workDir: workDir,
		logger:  logger,
	}
	// Hydrate remoteURL from origin if present. A missing origin
	// is fine — solo single-machine projects stay local until the
	// operator wires a remote.
	if out, err := runGit(workDir, []string{"remote", "get-url", "origin"}, runOpts{}); err == nil {
		c.remoteURL = strings.TrimSpace(string(out))
	}
	if lockPath != "" {
		// gofrs/flock won't auto-create the parent dir. Ensure
		// <projectPath>/.enju/locks/ exists so the first
		// fileLock.Lock() call (lazy create) doesn't fail with
		// "no such file or directory". Cheap on subsequent
		// OpenClone calls (MkdirAll is a stat-then-noop when
		// the dir already exists).
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
			return nil, fmt.Errorf("git: prep lock dir %s: %w", filepath.Dir(lockPath), err)
		}
		c.fileLock = flock.New(lockPath)
	}
	return c, nil
}

// CloneOrInit ensures a clone exists at workDir. If one exists,
// behaves like OpenClone. If not, performs `git clone` from
// remoteURL into workDir. When remoteURL is empty AND workDir is
// missing, returns an error.
func CloneOrInit(workDir, remoteURL, lockPath string, logger *slog.Logger) (*Clone, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		return OpenClone(workDir, lockPath, logger)
	}
	if remoteURL == "" {
		return nil, fmt.Errorf("git: no clone at %s and no remoteURL given", workDir)
	}
	if err := os.MkdirAll(filepath.Dir(workDir), 0o755); err != nil {
		return nil, fmt.Errorf("git: mkdir parent %s: %w", workDir, err)
	}
	// `git clone <url> <dir>` from the parent dir is fine — git
	// will create <dir>. Use empty workDir for runGit so no -C is
	// passed (the target dir doesn't exist yet).
	args := []string{"clone", remoteURL, workDir}
	if _, err := runGit("", args, runOpts{network: true}); err != nil {
		return nil, fmt.Errorf("git: clone %s into %s: %w", remoteURL, workDir, err)
	}
	return OpenClone(workDir, lockPath, logger)
}

// InitLocal initializes a non-bare git repo at workDir with a
// `main` default branch AND seeds an initial commit (README.md)
// so refs/heads/main has a SHA. Bootstrap step for path-mode
// projects + test fixtures.
//
// No enju/templates/ scaffold — Phase 8 dropped the required
// directory structure; templates live wherever the user puts
// them, identified by path at run-creation time.
func InitLocal(workDir, lockPath string, logger *slog.Logger) (*Clone, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("git: mkdir %s: %w", workDir, err)
	}
	if _, err := runGit(workDir, []string{"init", "-b", "main"}, runOpts{}); err != nil {
		return nil, fmt.Errorf("git: init %s: %w", workDir, err)
	}
	if err := seedInitialCommit(workDir); err != nil {
		return nil, fmt.Errorf("git: seed local init %s: %w", workDir, err)
	}
	return OpenClone(workDir, lockPath, logger)
}

// seedInitialCommit writes README.md and commits it so
// refs/heads/main has a SHA. Authorship is the placeholder Enju
// identity (callers that want their own author go through
// CommitFiles afterward).
func seedInitialCommit(workDir string) error {
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Enju project\n"), 0o644); err != nil {
		return fmt.Errorf("write README: %w", err)
	}
	if _, err := runGit(workDir, []string{"add", "README.md"}, runOpts{}); err != nil {
		return fmt.Errorf("git add seed files: %w", err)
	}
	env := authorEnvVars("Enju", "enju@localhost", time.Now())
	if _, err := runGit(workDir, []string{"commit", "-m", "initial commit"}, runOpts{extraEnv: env}); err != nil {
		return fmt.Errorf("git commit seed: %w", err)
	}
	return nil
}

// WorkDir returns the absolute path to this clone's worktree.
func (c *Clone) WorkDir() string { return c.workDir }

// RemoteURL returns the cached origin URL, or "" when origin is
// not configured. Refreshed by EnsureOrigin / RemoveOrigin.
func (c *Clone) RemoteURL() string { return c.remoteURL }

// LastPushAt returns the timestamp of the last push attempt
// (success or failure). Zero value when no push has happened.
func (c *Clone) LastPushAt() time.Time { return c.lastPushAt }

// LastPushError returns the error string from the most recent
// push, or "" when the last push succeeded.
func (c *Clone) LastPushError() string { return c.lastPushError }

// RecordPush sets the cached LastPushAt/LastPushError fields.
func (c *Clone) RecordPush(t time.Time, errMsg string) {
	defer c.lock()()
	c.lastPushAt = t
	c.lastPushError = errMsg
}

// HeadCommitTime returns the commit time of HEAD. Zero time on
// any error (no commits yet, detached HEAD pointing at nothing,
// etc.) — callers treat the zero value as "unknown" rather than
// branching on a separate error.
func (c *Clone) HeadCommitTime() time.Time {
	out, err := runGit(c.workDir, []string{"log", "-1", "--format=%ct", "HEAD"}, runOpts{})
	if err != nil {
		return time.Time{}
	}
	secs, parseErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if parseErr != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// Project-lock contention cadence. acquireFileLock NEVER proceeds
// without the flock (fail-open would let two processes mutate one
// git repo concurrently), so a held lock is waited out — but the
// wait is now observable rather than the old silent forever-block.
const (
	lockContentionRetryDelay    = 100 * time.Millisecond
	lockContentionReLogInterval = 15 * time.Second
)

// acquireFileLock takes the cross-process project flock, or no-ops
// when cross-process locking is disabled (lockPath==""; test
// fixtures). It is the safety boundary for "one writer per git
// repo across processes": it must not return until it holds the
// lock.
//
// Previously this was `_ = c.fileLock.Lock()` — a blocking acquire
// with the error discarded and no diagnostic. A lingering stale
// process holding the flock (a wedged daemon/mcp that didn't exit)
// turned every other process's first git op into an INVISIBLE,
// unbounded, uninterruptible hang: no log, no timeout, not even
// ctx-cancellable (the agent-lifecycle bug seen as prisma run #4 —
// a fresh daemon "started, then never claimed for 9+ min"). This
// keeps the exact safety (still waits as long as it takes; never
// fail-open) but makes the wait LOUD: a WARN naming the lock path
// the moment contention is detected, re-logged periodically with
// the elapsed wait, and a close-out line on acquisition. The
// operator now sees "blocked on the project lock held by another
// enju process" and can kill the stale holder, instead of staring
// at a silent process.
func (c *Clone) acquireFileLock() {
	if c.fileLock == nil {
		return
	}
	if ok, err := c.fileLock.TryLock(); ok && err == nil {
		return
	}
	start := time.Now()
	c.logger.Warn("blocked acquiring project lock; another enju process holds it — waiting (if this persists, verify holder_pid is a live enju process and kill it)",
		"lock_path", c.fileLock.Path(),
		"holder_pid", c.lockOwnerPID())
	lastLog := start
	for {
		ok, err := c.fileLock.TryLock()
		if ok && err == nil {
			c.logger.Warn("project lock acquired after contention",
				"lock_path", c.fileLock.Path(),
				"waited", time.Since(start).Round(time.Second).String())
			return
		}
		if time.Since(lastLog) >= lockContentionReLogInterval {
			c.logger.Warn("still blocked acquiring project lock",
				"lock_path", c.fileLock.Path(),
				"waited", time.Since(start).Round(time.Second).String(),
				"holder_pid", c.lockOwnerPID(),
				"try_err", err)
			lastLog = time.Now()
		}
		time.Sleep(lockContentionRetryDelay)
	}
}

// lockOwnerPath is the sidecar file naming the pid that currently
// holds the project flock — written on acquire, removed on release.
// flock itself doesn't expose the holder, so this is how a blocked
// process can name the pid to kill in its contention WARN.
func (c *Clone) lockOwnerPath() string {
	return c.fileLock.Path() + ".owner"
}

// writeLockOwner records this process as the lock holder. Best-effort:
// the pid is a diagnostic hint, never load-bearing for correctness.
func (c *Clone) writeLockOwner() {
	if c.fileLock == nil {
		return
	}
	_ = os.WriteFile(c.lockOwnerPath(), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// clearLockOwner removes the holder sidecar on release. A holder that
// crashes leaves a stale file; the next acquirer overwrites it, and a
// waiter reading the dead pid in the meantime is actually a useful
// "this is the wedged process" pointer.
func (c *Clone) clearLockOwner() {
	if c.fileLock == nil {
		return
	}
	_ = os.Remove(c.lockOwnerPath())
}

// lockOwnerPID reads the holder sidecar; "" when absent/unreadable.
func (c *Clone) lockOwnerPID() string {
	if c.fileLock == nil {
		return ""
	}
	b, err := os.ReadFile(c.lockOwnerPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// lock acquires both mu and (when configured) the flock. No-op
// when the calling goroutine already holds the lock (reentrant
// fast path). Returns a function the caller must call to release.
// Use as: defer c.lock()()
func (c *Clone) lock() func() {
	me := goroutineID()
	if c.holder.Load() == me {
		return func() {}
	}
	c.mu.Lock()
	c.acquireFileLock()
	c.holder.Store(me)
	c.writeLockOwner()
	return func() {
		c.clearLockOwner()
		c.holder.Store(0)
		if c.fileLock != nil {
			_ = c.fileLock.Unlock()
		}
		c.mu.Unlock()
	}
}

// WithLock holds the project lock across the closure. Inside fn,
// the passed Ops is the same Clone; nested mutating method calls
// from the SAME goroutine see themselves as the lock holder and
// skip re-acquisition (the non-recursive sync.Mutex would
// deadlock otherwise). Goroutine semantics keyed on the holder
// goroutine ID — same approach as gitv6's #381 fix.
func (c *Clone) WithLock(fn func(Ops) error) error {
	me := goroutineID()
	if c.holder.Load() == me {
		return fn(c)
	}
	c.mu.Lock()
	c.acquireFileLock()
	c.holder.Store(me)
	defer func() {
		c.holder.Store(0)
		if c.fileLock != nil {
			_ = c.fileLock.Unlock()
		}
		c.mu.Unlock()
	}()
	return fn(c)
}

// goroutineID returns the current goroutine's ID by parsing the
// header of runtime.Stack. Used as a per-goroutine reentrancy
// key for the project lock. ~100ns cost paid only on lock
// acquisition, not on every method inside a held lock.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s := buf[:n]
	rest, ok := bytes.CutPrefix(s, []byte("goroutine "))
	if !ok {
		return 0
	}
	digits, _, ok := bytes.Cut(rest, []byte(" "))
	if !ok {
		return 0
	}
	id, err := strconv.ParseUint(string(digits), 10, 64)
	if err != nil {
		return 0
	}
	return id
}
