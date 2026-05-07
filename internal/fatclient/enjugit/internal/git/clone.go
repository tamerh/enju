package git

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/gofrs/flock"
)

// Clone is the handle on one project's local git checkout. It owns
// the worktree path, the go-git repo handle, and both layers of
// the lock model (in-process mutex + cross-process flock).
//
// Construct via OpenClone. Methods on *Clone are listed in
// separate files by category (read.go, branch.go, push.go, etc.).
// All mutating methods acquire the lock internally except when
// called from inside WithLock (which sets a re-entrant flag).
//
// Clone satisfies the Ops interface. Workflow code holds an Ops
// reference, not a *Clone reference, so it can be mocked in tests.
type Clone struct {
	workDir   string
	repo      *gogit.Repository
	remoteURL string // hydrated from origin on open; "" when no remote
	logger    *slog.Logger

	// In-process serialization. Acquired by every mutating method
	// except when reentrant is set.
	mu sync.Mutex

	// Cross-process serialization. nil when no lockPath was
	// supplied (test fixtures that don't share the workdir across
	// processes). When non-nil, acquired alongside mu.
	fileLock *flock.Flock

	// reentrant is set by WithLock to suppress lock re-acquisition
	// in nested calls. The Clone caller never sets this directly.
	reentrant bool

	// Push observability — pure bookkeeping for diagnostic tools.
	// Not part of the Ops surface.
	lastPushAt    time.Time
	lastPushError string
}

// OpenClone opens an existing git clone at workDir. If workDir
// doesn't contain a .git, returns ErrCloneNotFound. The remoteURL
// argument is informational only — the canonical source of truth
// is the on-disk origin remote, which OpenClone reads to populate
// Clone.remoteURL.
//
// lockPath is the file used for the cross-process flock. Pass an
// empty string to skip cross-process locking (test fixtures).
//
// logger may be nil — falls back to slog.Default().
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
	repo, err := gogit.PlainOpen(workDir)
	if err != nil {
		return nil, fmt.Errorf("git: open %s: %w", workDir, err)
	}
	c := &Clone{
		workDir: workDir,
		repo:    repo,
		logger:  logger,
	}
	// Hydrate remoteURL from the on-disk origin so lazy-fetch and
	// push paths know where to talk to. Without this, an
	// OpenClone'd clone has remoteURL == "" even when origin is
	// configured — the silent-source-of-truth bug we hit in the
	// per-bot-clones rollout.
	if rem, err := repo.Remote("origin"); err == nil {
		if cfg := rem.Config(); cfg != nil && len(cfg.URLs) > 0 {
			c.remoteURL = cfg.URLs[0]
		}
	}
	if lockPath != "" {
		c.fileLock = flock.New(lockPath)
	}
	return c, nil
}

// CloneOrInit ensures a clone exists at workDir. If one exists,
// behaves like OpenClone. If not, performs `git clone` from
// remoteURL into workDir. When remoteURL is empty AND workDir is
// missing, returns an error — this layer does NOT silently init
// orphan empty repos. Enjugit handles "no remote, init empty"
// explicitly via a separate path.
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
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("git: mkdir %s: %w", workDir, err)
	}
	// SingleBranch defaults to false → all branches come down.
	// Avoids the narrow-refspec silent cross-bot-read bug.
	repo, err := gogit.PlainClone(workDir, false, &gogit.CloneOptions{
		URL: remoteURL,
	})
	if err != nil {
		return nil, fmt.Errorf("git: clone %s into %s: %w", remoteURL, workDir, err)
	}
	// Pin the remote refspec to the full form so subsequent
	// fetches don't accidentally narrow it. Done by re-creating
	// the remote (DeleteRemote + CreateRemote) since CloneOptions
	// has no RefSpecs field.
	_ = repo.DeleteRemote("origin")
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name:  "origin",
		URLs:  []string{remoteURL},
		Fetch: []config.RefSpec{config.RefSpec("+refs/heads/*:refs/remotes/origin/*")},
	}); err != nil {
		return nil, fmt.Errorf("git: re-create origin with full refspec: %w", err)
	}
	c := &Clone{
		workDir:   workDir,
		repo:      repo,
		remoteURL: remoteURL,
		logger:    logger,
	}
	if lockPath != "" {
		c.fileLock = flock.New(lockPath)
	}
	return c, nil
}

// WorkDir returns the absolute path to this clone's worktree.
// Used by callers that need to chdir or inspect non-git state
// (compute scripts, audit log readers).
func (c *Clone) WorkDir() string { return c.workDir }

// RemoteURL returns the configured origin URL, or "" when the
// clone has no remote (solo / path-only projects).
func (c *Clone) RemoteURL() string { return c.remoteURL }

// LastPushAt returns the timestamp of the most recent successful
// push. Zero value when no push has happened in this process.
func (c *Clone) LastPushAt() time.Time { return c.lastPushAt }

// LastPushError returns the most recent push error message, or
// "" when the most recent push succeeded.
func (c *Clone) LastPushError() string { return c.lastPushError }

// lock acquires both mu and (when configured) the flock. No-op
// when reentrant is set. Returns a function the caller must call
// to release. Use as: defer c.lock()()
func (c *Clone) lock() func() {
	if c.reentrant {
		return func() {}
	}
	c.mu.Lock()
	if c.fileLock != nil {
		_ = c.fileLock.Lock()
	}
	return func() {
		if c.fileLock != nil {
			_ = c.fileLock.Unlock()
		}
		c.mu.Unlock()
	}
}

// silence unused-import warnings for things later phases will use.
var (
	_ = plumbing.ZeroHash
)
