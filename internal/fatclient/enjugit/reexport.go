package enjugit

// reexport.go — temporary re-exports of the internal git package
// for the project-package migration (#381). project.Clone wraps
// a *git.Clone to share *gogit.Repository state with enjugit
// handles, eliminating dual-handle drift. Once project retires
// (Phase 11), delete this file along with the package.

import (
	"log/slog"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// SharedSSHAuth returns an SSH auth method for the given remote URL,
// trying the SSH agent first and falling back to common key-file
// paths. Re-exported for project.Clone.CompareToRemote during the
// migration; goes away with the project package.
func SharedSSHAuth(remoteURL string) transport.AuthMethod {
	return git.SSHAuthMethod(remoteURL)
}

// SharedPreserveDirSuffix is the suffix used for the sibling
// preserve dir during a Force checkout. Re-exported for project
// package's openOrClone / OpenExisting drain paths during the
// migration; goes away with the project package.
const SharedPreserveDirSuffix = git.PreserveDirSuffix

// RecoverLeftoverSharedPreserve drains a leftover preserve dir
// from a previous crash. Re-exported for project's workspace-open
// paths; goes away with the project package.
func RecoverLeftoverSharedPreserve(workDir string, logger *slog.Logger) error {
	return git.RecoverLeftoverPreserve(workDir, logger)
}

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

// ErrSharedPushNonFF is the migration-shim alias for git.ErrPushNonFF —
// returned when a push is rejected as non-fast-forward. Callers that
// retry on non-FF (project.SubmitTaskResult / CommitFiles) check
// errors.Is(err, enjugit.ErrSharedPushNonFF) before driving a rebase.
// Goes away with the project package.
var ErrSharedPushNonFF = git.ErrPushNonFF

// SharedCommitRequest / SharedCommitResult / SharedFileWrite are the
// migration-shim aliases for the git layer's commit primitive types.
// Used by project.Clone's SubmitTaskResult / CommitFiles after they
// were refactored to delegate write+stage+commit to gitClone.CommitFiles
// (which handles file writes, mode + chmod, idempotent no-op, staging,
// and committing in one call). Goes away with the project package.
type SharedCommitRequest = git.CommitRequest
type SharedCommitResult = git.CommitResult
type SharedFileWrite = git.FileWrite

// ErrSharedNoRemote is the migration-shim alias for git.ErrNoRemote.
// project methods that delegate to gitClone.Fetch / Push (which
// surface ErrNoRemote when there's no origin) check via
// errors.Is(err, enjugit.ErrSharedNoRemote) to swallow the
// no-remote case the way project's old methods used to (return
// nil, no-op).
var ErrSharedNoRemote = git.ErrNoRemote
