package enjugit

// reexport.go — temporary re-exports of the internal git package
// for the project-package migration (#381). project.Clone wraps
// a *git.Clone to share *gogit.Repository state with enjugit
// handles, eliminating dual-handle drift. Once project retires
// (Phase 11), delete this file along with the package.

import (
	"log/slog"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// SharedClone is the migration-shim alias for *git.Clone. Project
// package holds one of these in project.Clone.gitClone so its
// repo pointer matches whatever an enjugit Workflow opened on the
// same dir would hold.
type SharedClone = git.Clone

// OpenSharedClone opens an existing on-disk clone. Mirrors
// git.OpenClone for the project package's openOrClone path.
func OpenSharedClone(workDir, lockPath string, logger *slog.Logger) (*SharedClone, error) {
	return git.OpenClone(workDir, lockPath, logger)
}

// CloneOrInitShared mirrors git.CloneOrInit for the project
// package's openOrClone fresh-clone path.
func CloneOrInitShared(workDir, remoteURL, lockPath string, logger *slog.Logger) (*SharedClone, error) {
	return git.CloneOrInit(workDir, remoteURL, lockPath, logger)
}

// InitLocalShared mirrors git.InitLocal for the project package's
// no-remote local-init path.
func InitLocalShared(workDir, lockPath string, logger *slog.Logger) (*SharedClone, error) {
	return git.InitLocal(workDir, lockPath, logger)
}
