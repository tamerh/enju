package gitv6

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/gofrs/flock"
)

// Clone is the handle on one project's local git checkout, backed
// by go-git v6. Ports the v5 sibling's shape exactly (same fields,
// same behavioural contracts on each method) so the Ops interface
// is identical above the seam.
type Clone struct {
	workDir   string
	repo      *gogit.Repository
	remoteURL string
	logger    *slog.Logger

	mu sync.Mutex

	fileLock *flock.Flock

	reentrant bool

	lastPushAt    time.Time
	lastPushError string
}

// OpenClone opens an existing git clone at workDir. If workDir
// doesn't contain a .git, returns ErrCloneNotFound. Hydrates
// remoteURL from the on-disk origin so lazy-fetch and push paths
// know where to talk to.
//
// lockPath is the file used for the cross-process flock. Pass
// "" to skip cross-process locking (test fixtures).
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
// missing, returns an error.
//
// v6 PlainClone signature dropped the isBare bool — Bare moved
// into CloneOptions. We default to non-bare (worktree).
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
	repo, err := gogit.PlainClone(workDir, &gogit.CloneOptions{
		URL:           remoteURL,
		ClientOptions: clientOptionsFor(remoteURL),
	})
	if err != nil {
		return nil, fmt.Errorf("git: clone %s into %s: %w", remoteURL, workDir, err)
	}
	// Pin the remote refspec to the full form so subsequent
	// fetches don't accidentally narrow it.
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
// origin configured AND a seed initial commit so refs/heads/main
// has a SHA. Bootstrap step for path-mode projects.
//
// v6 PlainInit takes variadic InitOption (PlainInitWithOptions
// is gone). DefaultBranch is set via WithDefaultBranch.
func InitLocal(workDir, lockPath string, logger *slog.Logger) (*Clone, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("git: mkdir %s: %w", workDir, err)
	}
	repo, err := gogit.PlainInit(workDir, false,
		gogit.WithDefaultBranch(plumbing.ReferenceName("refs/heads/main")),
	)
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
// and commits them so refs/heads/main has a SHA.
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
func (c *Clone) WorkDir() string { return c.workDir }

// Repo returns the underlying go-git repository handle. Exposed
// so the enjugit package can wrap *Clone during the migration;
// drop once Phase G retires the v5 backend.
func (c *Clone) Repo() *gogit.Repository { return c.repo }

// EnsureOrigin makes the on-disk .git/config carry an origin
// remote pointing at url. Idempotent. See v5 sibling for the
// full rationale (band-aid for the dual-handle bug #381).
func (c *Clone) EnsureOrigin(url string) error {
	defer c.lock()()
	if url == "" {
		return nil
	}
	if rem, err := c.repo.Remote("origin"); err == nil {
		if cfg := rem.Config(); cfg != nil && len(cfg.URLs) > 0 && cfg.URLs[0] == url {
			return nil
		}
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

// RemoveOrigin deletes the origin remote when present. Idempotent.
func (c *Clone) RemoveOrigin() error {
	defer c.lock()()
	if _, err := c.repo.Remote("origin"); err != nil {
		return nil
	}
	if err := c.repo.DeleteRemote("origin"); err != nil {
		return fmt.Errorf("git: remove-origin: delete: %w", err)
	}
	c.remoteURL = ""
	return nil
}

// RecordPush sets the cached LastPushAt/LastPushError fields.
func (c *Clone) RecordPush(t time.Time, errMsg string) {
	defer c.lock()()
	c.lastPushAt = t
	c.lastPushError = errMsg
}

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
