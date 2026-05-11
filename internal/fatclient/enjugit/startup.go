package enjugit

// startup.go — public re-exports of gitcli's startup-time
// checks. The gitcli package itself is module-internal
// (under internal/fatclient/enjugit/internal/), so callers
// outside enjugit (cmd/enju, internal/fatclient/service, etc.)
// can't import it directly. These thin forwards give them
// access to the small set of things they need before opening
// any Workspace.

import (
	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

// MinGitMajor / MinGitMinor — the minimum git version every
// enjugit-using binary requires on PATH. Mirrors gitcli's
// constants of the same name; re-exported so callers can show
// the requirement in --help output without crossing the
// internal boundary.
const (
	MinGitMajor = git.MinGitMajor
	MinGitMinor = git.MinGitMinor
)

// CheckGitMinVersion runs `git --version` and verifies the
// binary is at least MinGitMajor.MinGitMinor. Returns a
// human-readable error suitable for stderr — names the minimum
// and the verb that requires it.
//
// Call once at startup from binaries that exercise enjugit
// verbs. Cheap (one subprocess) and fail-fast: produces a
// clear error before the operator hits cryptic "unknown
// option" failures mid-workflow.
func CheckGitMinVersion() error {
	return git.CheckMinVersion()
}

// GitDirInfo is the result of InspectGitDir: origin URL and
// HEAD branch name (both empty when not configured /
// detached).
type GitDirInfo = git.GitDirInfo

// InspectGitDir reads the on-disk state of a non-bare git
// repo at path: origin URL + HEAD branch name. Forwarder to
// gitcli — keeps the gitcli package internal to enjugit while
// project-adoption callers (service/project_ops.go) get the
// primitives they need without importing go-git directly.
//
// Returns an error only when the path can't be opened as a
// git repo. Missing origin or detached HEAD are non-errors;
// the struct fields are just empty.
func InspectGitDir(path string) (GitDirInfo, error) {
	return git.InspectGitDir(path)
}

// HeadResolvesToCommit reports whether HEAD in the repo at
// path resolves to a real commit. Used by adoption's
// "populated unrelated repo" detection. Forwarder to gitcli.
func HeadResolvesToCommit(path string) bool {
	return git.HeadResolvesToCommit(path)
}

// InitLocalAdoptExisting initializes a non-bare git repo in a
// directory that already has files, stages everything, and
// creates an initial commit with the Enju identity. Ensures
// templatesRelDir/.gitkeep exists (the enju/ scaffold) before
// staging.
//
// Returns the short HEAD branch name after the commit (e.g.
// "main"). Used by the adoption path when an operator runs
// enju_create_project against a directory full of pre-existing
// content.
func InitLocalAdoptExisting(workDir, templatesRelDir string) (string, error) {
	return git.InitLocalAdoptExisting(workDir, templatesRelDir)
}
