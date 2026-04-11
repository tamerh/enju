// Package git handles git operations for result storage.
package git

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Writer handles git operations for the results repository.
type Writer struct {
	repo    *gogit.Repository
	workDir string
	logger  *slog.Logger
}

// NewWriter initializes a git writer. If the workDir is already a git repo, it opens it.
// Otherwise it initializes a new repo. If remoteURL is provided, it sets the origin remote.
func NewWriter(workDir, remoteURL string, logger *slog.Logger) (*Writer, error) {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("creating work dir: %w", err)
	}

	var repo *gogit.Repository
	var err error

	// Try to open existing repo
	repo, err = gogit.PlainOpen(workDir)
	if err != nil {
		// Initialize new repo
		repo, err = gogit.PlainInit(workDir, false)
		if err != nil {
			return nil, fmt.Errorf("initializing git repo: %w", err)
		}
		logger.Info("initialized new git repo", "path", workDir)
	}

	// Set remote if provided and not already set
	if remoteURL != "" {
		_, err := repo.Remote("origin")
		if err != nil {
			_, err = repo.CreateRemote(&config.RemoteConfig{
				Name: "origin",
				URLs: []string{remoteURL},
			})
			if err != nil {
				return nil, fmt.Errorf("creating remote: %w", err)
			}
			logger.Info("set git remote", "url", remoteURL)
		}
	}

	return &Writer{
		repo:    repo,
		workDir: workDir,
		logger:  logger,
	}, nil
}

// Commit stages all changes and creates a commit.
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

// CommitAndPush stages all changes, commits, and pushes.
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
