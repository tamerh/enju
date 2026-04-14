// Package git handles git operations for result storage.
//
// As of Phase C, each project has its own git repository. The Registry
// type owns a base directory containing many per-project repos and
// hands out a *Writer for any given projectID. A Writer is scoped to one
// project repo and is the only thing that should touch its files.
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
)

// Writer handles git operations for a single project's repository.
// All paths passed to its methods are relative to the project repo root.
type Writer struct {
	repo      *gogit.Repository
	workDir   string
	projectID int64
	logger    *slog.Logger
	mu        sync.Mutex // serializes commits within this project
}

// openOrInitRepo opens an existing git repo at workDir or initializes a
// new one with the default branch set to main. If a remote URL is given
// and not already configured, it's added as origin.
func openOrInitRepo(workDir, remoteURL string, logger *slog.Logger) (*gogit.Repository, bool, error) {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, false, fmt.Errorf("creating work dir: %w", err)
	}

	created := false
	repo, err := gogit.PlainOpen(workDir)
	if err != nil {
		repo, err = gogit.PlainInitWithOptions(workDir, &gogit.PlainInitOptions{
			InitOptions: gogit.InitOptions{
				DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
			},
		})
		if err != nil {
			return nil, false, fmt.Errorf("initializing git repo: %w", err)
		}
		created = true
		logger.Info("initialized new git repo", "path", workDir)
	}

	if remoteURL != "" {
		if _, err := repo.Remote("origin"); err != nil {
			if _, err := repo.CreateRemote(&config.RemoteConfig{
				Name: "origin",
				URLs: []string{remoteURL},
			}); err != nil {
				return nil, false, fmt.Errorf("creating remote: %w", err)
			}
			logger.Info("set git remote", "url", remoteURL)
		}
	}

	return repo, created, nil
}

// NewWriter initializes a git writer for a single repository at workDir.
// If projectID is zero, the writer is treated as legacy/unscoped — used by
// the test helpers and any caller that hasn't migrated to the registry.
func NewWriter(workDir, remoteURL string, logger *slog.Logger) (*Writer, error) {
	repo, _, err := openOrInitRepo(workDir, remoteURL, logger)
	if err != nil {
		return nil, err
	}
	return &Writer{
		repo:    repo,
		workDir: workDir,
		logger:  logger,
	}, nil
}

// ProjectID returns the project this writer is scoped to (0 if unscoped).
func (w *Writer) ProjectID() int64 {
	return w.projectID
}

// Lock acquires the writer's mutex. Callers that perform a sequence of
// WriteFile + Commit + Push operations should hold this across the whole
// sequence so the commit is atomic from the perspective of other writers.
func (w *Writer) Lock()   { w.mu.Lock() }
func (w *Writer) Unlock() { w.mu.Unlock() }

// Commit stages all changes and creates a commit. The caller MUST hold
// the writer's lock across WriteFile + Commit + Push to keep the commit
// atomic.
func (w *Writer) Commit(message string) error {
	wt, err := w.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	// Stage all changes
	if err := wt.AddGlob("."); err != nil {
		return fmt.Errorf("staging changes: %w", err)
	}

	// Check if there's anything to commit
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("getting status: %w", err)
	}
	if status.IsClean() {
		w.logger.Debug("nothing to commit")
		return nil
	}

	_, err = wt.Commit(message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Enju Coordinator",
			Email: "enju@localhost",
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("committing: %w", err)
	}

	w.logger.Debug("committed", "message", message)
	return nil
}

// Push pushes to the origin remote. Returns nil if no remote is configured.
// The caller MUST hold the writer's lock.
func (w *Writer) Push() error {
	_, err := w.repo.Remote("origin")
	if err != nil {
		// No remote configured — skip push
		w.logger.Debug("no remote configured, skipping push")
		return nil
	}

	err = w.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return fmt.Errorf("pushing: %w", err)
	}

	w.logger.Debug("pushed to remote")
	return nil
}

// CommitAndPush stages all changes, commits, and pushes. The caller MUST
// hold the writer's lock.
func (w *Writer) CommitAndPush(message string) error {
	if err := w.Commit(message); err != nil {
		return err
	}
	return w.Push()
}

// WorkDir returns the path to the git working directory.
func (w *Writer) WorkDir() string {
	return w.workDir
}

// WriteFile writes content to a file in the working directory.
func (w *Writer) WriteFile(relPath string, data []byte) error {
	fullPath := filepath.Join(w.workDir, relPath)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	return nil
}

// ReadFile reads a file from the working directory.
func (w *Writer) ReadFile(relPath string) ([]byte, error) {
	fullPath := filepath.Join(w.workDir, relPath)
	return os.ReadFile(fullPath)
}

// RemoveFile deletes a file from the working directory. Used by the
// rollback path when an invalidated task was the file's first writer
// and there's no prior version to restore to. The caller MUST hold the
// writer's lock and is responsible for committing the deletion.
func (w *Writer) RemoveFile(relPath string) error {
	fullPath := filepath.Join(w.workDir, relPath)
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing file: %w", err)
	}
	return nil
}

// CommitInfo describes a single commit for history-walking purposes.
// Returned by LogFile in reverse-chronological order (newest first).
type CommitInfo struct {
	Hash    string
	Message string
	Author  string
	Time    time.Time
}

// LogFile returns the commits that touched a specific file, in
// reverse-chronological order. Used by the rollback path to find the
// previous state of an artifact that was written by a now-invalidated
// task.
func (w *Writer) LogFile(relPath string) ([]CommitInfo, error) {
	iter, err := w.repo.Log(&gogit.LogOptions{
		FileName: &relPath,
	})
	if err != nil {
		return nil, fmt.Errorf("opening log for %s: %w", relPath, err)
	}
	defer iter.Close()

	var out []CommitInfo
	err = iter.ForEach(func(c *object.Commit) error {
		out = append(out, CommitInfo{
			Hash:    c.Hash.String(),
			Message: c.Message,
			Author:  c.Author.Name,
			Time:    c.Author.When,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterating log: %w", err)
	}
	return out, nil
}

// ReadFileAtCommit reads a file's content as of a specific commit.
// Returns (content, exists, err). Used by the rollback path to fetch
// the earlier version of an artifact.
func (w *Writer) ReadFileAtCommit(commitHash, relPath string) ([]byte, bool, error) {
	hash := plumbing.NewHash(commitHash)
	commit, err := w.repo.CommitObject(hash)
	if err != nil {
		return nil, false, fmt.Errorf("loading commit %s: %w", commitHash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, false, fmt.Errorf("loading tree: %w", err)
	}
	file, err := tree.File(relPath)
	if err == object.ErrFileNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("looking up file: %w", err)
	}
	content, err := file.Contents()
	if err != nil {
		return nil, false, fmt.Errorf("reading file contents: %w", err)
	}
	return []byte(content), true, nil
}

// Registry manages a base directory containing many per-project git
// repositories. Each project has its own repo at {baseDir}/{projectID}/.
// Writers are opened lazily on first access and cached.
type Registry struct {
	baseDir string
	logger  *slog.Logger

	mu      sync.Mutex
	writers map[int64]*Writer
}

// NewRegistry creates a registry rooted at baseDir. The directory is
// created if it doesn't exist. Existing project repos are not opened
// here — they're opened lazily on first For() call.
func NewRegistry(baseDir string, logger *slog.Logger) (*Registry, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("creating base dir: %w", err)
	}
	return &Registry{
		baseDir: baseDir,
		logger:  logger,
		writers: make(map[int64]*Writer),
	}, nil
}

// BaseDir returns the directory containing all project repos.
func (r *Registry) BaseDir() string {
	return r.baseDir
}

// projectDir returns the on-disk directory for a project's repo.
func (r *Registry) projectDir(projectID int64) string {
	return filepath.Join(r.baseDir, fmt.Sprintf("%d", projectID))
}

// For returns the writer for a project, opening (or self-healing) the
// repo on disk if needed. The writer is cached for subsequent calls.
//
// If the directory exists but isn't a valid git repo, or doesn't exist
// at all, this method recreates it and returns a writer over the fresh
// repo. This is the self-heal path for the case where the DB has a
// project but the on-disk repo went missing.
func (r *Registry) For(projectID int64) (*Writer, error) {
	if projectID == 0 {
		return nil, fmt.Errorf("registry: projectID is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if w, ok := r.writers[projectID]; ok {
		return w, nil
	}

	workDir := r.projectDir(projectID)
	repo, _, err := openOrInitRepo(workDir, "", r.logger)
	if err != nil {
		return nil, fmt.Errorf("opening project %d repo: %w", projectID, err)
	}

	w := &Writer{
		repo:      repo,
		workDir:   workDir,
		projectID: projectID,
		logger:    r.logger,
	}
	r.writers[projectID] = w
	return w, nil
}

// CreateProjectRepo initializes a fresh repo for a new project, writes
// an initial README.md committing the project name, and registers the
// writer. Returns the writer for the freshly-created project.
//
// If the repo directory already exists (e.g. from a previous failed
// creation), this method opens the existing repo rather than failing,
// so it's idempotent for crash-recovery scenarios.
func (r *Registry) CreateProjectRepo(projectID int64, projectName string) (*Writer, error) {
	if projectID == 0 {
		return nil, fmt.Errorf("registry: projectID is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if w, ok := r.writers[projectID]; ok {
		return w, nil
	}

	workDir := r.projectDir(projectID)
	repo, created, err := openOrInitRepo(workDir, "", r.logger)
	if err != nil {
		return nil, fmt.Errorf("creating project %d repo: %w", projectID, err)
	}

	w := &Writer{
		repo:      repo,
		workDir:   workDir,
		projectID: projectID,
		logger:    r.logger,
	}
	r.writers[projectID] = w

	// Only write the initial README + commit on a fresh repo.
	if created {
		w.Lock()
		defer w.Unlock()

		readme := fmt.Sprintf("# %s\n\nProject %d managed by Enju.\n", projectName, projectID)
		if err := w.WriteFile("README.md", []byte(readme)); err != nil {
			return nil, fmt.Errorf("writing initial README: %w", err)
		}
		if err := w.Commit(fmt.Sprintf("Initialize project: %s", projectName)); err != nil {
			return nil, fmt.Errorf("initial commit: %w", err)
		}
	}

	return w, nil
}

// HealthCheck verifies all known projects on disk look reasonable.
// Called at startup. Currently it just stats each project subdirectory
// the registry has been told about; missing dirs are logged as warnings
// and self-healed lazily on first For() call.
func (r *Registry) HealthCheck(projectIDs []int64) {
	for _, pid := range projectIDs {
		dir := r.projectDir(pid)
		if _, err := os.Stat(dir); err != nil {
			r.logger.Warn("project repo missing on disk, will self-heal on first access",
				"project_id", pid, "path", dir)
		}
	}
}
