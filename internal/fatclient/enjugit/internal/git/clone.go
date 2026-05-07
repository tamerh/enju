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
	"github.com/go-git/go-git/v5/plumbing/object"
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

// InitLocal creates a fresh local-only repo at workDir with no
// remote configured AND a seed initial commit so refs/heads/main
// has a SHA. Used for solo / no-remote projects where the
// operator wants to commit locally and (optionally) wire a
// remote later via SetRemote.
//
// Seed contents (README + enju/templates/.gitkeep) match the
// project package's seedLocalWorkspace so the layout users see
// is identical regardless of whether they came in via project
// or enjugit. Without the seed, refs/heads/main has no SHA and
// downstream ops (fork-from-default during submit) fail with
// "default branch main not found" — same failure mode as a
// truly empty repo.
//
// Returns a *Clone whose remoteURL is "" (no origin in config).
// All push/fetch ops on this clone short-circuit via ErrNoRemote
// until SetRemote is called.
func InitLocal(workDir, lockPath string, logger *slog.Logger) (*Clone, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("git: mkdir %s: %w", workDir, err)
	}
	repo, err := gogit.PlainInitWithOptions(workDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("git: init local %s: %w", workDir, err)
	}
	if err := seedInitialCommit(repo, workDir); err != nil {
		return nil, fmt.Errorf("git: seed local init %s: %w", workDir, err)
	}
	c := &Clone{
		workDir: workDir,
		repo:    repo,
		logger:  logger,
	}
	if lockPath != "" {
		c.fileLock = flock.New(lockPath)
	}
	return c, nil
}

// seedInitialCommit writes README.md + enju/templates/.gitkeep
// and commits them so refs/heads/main has a SHA. Mirrors the
// project package's seedLocalWorkspace shape so layouts agree.
func seedInitialCommit(repo *gogit.Repository, workDir string) error {
	readme := filepath.Join(workDir, "README.md")
	readmeBody := "# Enju project\n"
	if err := os.WriteFile(readme, []byte(readmeBody), 0o644); err != nil {
		return fmt.Errorf("write README: %w", err)
	}
	templatesDir := filepath.Join(workDir, "enju", "templates")
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		return fmt.Errorf("create templates dir: %w", err)
	}
	gitkeepRel := "enju/templates/.gitkeep"
	if err := os.WriteFile(filepath.Join(workDir, gitkeepRel), []byte(""), 0o644); err != nil {
		return fmt.Errorf("write .gitkeep: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		return fmt.Errorf("add README: %w", err)
	}
	if _, err := wt.Add(gitkeepRel); err != nil {
		return fmt.Errorf("add .gitkeep: %w", err)
	}
	sig := &object.Signature{
		Name:  "Enju",
		Email: "enju@localhost",
		When:  time.Now(),
	}
	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{
		Author:    sig,
		Committer: sig,
	}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// WorkDir returns the absolute path to this clone's worktree.
// Used by callers that need to chdir or inspect non-git state
// (compute scripts, audit log readers).
func (c *Clone) WorkDir() string { return c.workDir }

// RemoteURL returns the configured origin URL, or "" when the
// clone has no remote (solo / path-only projects).
func (c *Clone) RemoteURL() string { return c.remoteURL }

// EnsureOrigin makes the on-disk .git/config carry an origin
// remote pointing at url. Idempotent: no-op when origin already
// matches; re-adds when missing; replaces when mismatched.
//
// Why this exists (band-aid for #381 dual-handle bug):
// While both project.Clone and enjugit.Workflow are in active
// use against the same on-disk dir, some code path inside the
// project package wipes the [remote "origin"] section from
// .git/config between operations (observed: between an
// export_diagram commit and a downstream submit's prepare-branch
// fetch). The cached enjugit Workflow's in-memory remoteURL
// stays correct, but go-git's repo.Fetch reads .git/config on
// every call and fails with "remote not found" once the section
// is gone. Calling EnsureOrigin before any fetch/push self-heals
// the on-disk state without us having to identify the exact
// wipe site (which proved hard to pin down — it's somewhere in
// the project package's claim/pull paths).
//
// Once the project package is fully retired (Phase 11), this
// method becomes redundant and can be deleted along with its
// callers.
func (c *Clone) EnsureOrigin(url string) error {
	defer c.lock()()
	if url == "" {
		return nil
	}
	if rem, err := c.repo.Remote("origin"); err == nil {
		if cfg := rem.Config(); cfg != nil && len(cfg.URLs) > 0 && cfg.URLs[0] == url {
			return nil
		}
		// Mismatched origin — replace so we don't fight a writer
		// that intentionally repointed it.
		if derr := c.repo.DeleteRemote("origin"); derr != nil {
			return fmt.Errorf("git: ensure-origin: delete stale: %w", derr)
		}
	}
	if _, err := c.repo.CreateRemote(&config.RemoteConfig{
		Name:  "origin",
		URLs:  []string{url},
		Fetch: []config.RefSpec{config.RefSpec("+refs/heads/*:refs/remotes/origin/*")},
	}); err != nil {
		return fmt.Errorf("git: ensure-origin: create: %w", err)
	}
	c.remoteURL = url
	return nil
}

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
