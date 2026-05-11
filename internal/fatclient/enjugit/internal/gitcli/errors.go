package gitcli

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Verbs document which they return; callers use
// errors.Is. String matching is forbidden above the seam.
//
// All git-CLI invocations funnel through run.go, which is the one
// place stderr-pattern matching happens to produce typed errors.
// Callers above this package see only the sentinels here.
var (
	// ErrCommitNotFound — the requested SHA isn't in the local
	// object DB and lazy-fetch couldn't find it on origin either.
	// Returned by ReadFileAtCommit, ResolveRef.
	ErrCommitNotFound = errors.New("git: commit not found")

	// ErrRefNotFound — the named ref doesn't exist locally or as
	// a remote-tracking ref. Returned by ResolveRef, Checkout
	// when the branch is unknown.
	ErrRefNotFound = errors.New("git: ref not found")

	// ErrPushNonFF — push rejected because the local tip is not
	// a fast-forward of the remote tip. Distinct from
	// ErrPushVerifyFailed which is "we tried to push and got
	// success but the ref didn't actually move."
	ErrPushNonFF = errors.New("git: push rejected (non-fast-forward)")

	// ErrPushVerifyFailed — push appeared to succeed but a
	// post-push read of the remote ref shows the SHA didn't
	// update. Most common under server-side hooks that reject
	// silently. Carries actual hashes via ErrVerifyFailed.
	ErrPushVerifyFailed = errors.New("git: push verify failed")

	// ErrMergeConflict — merge had file-level conflicts. Carries
	// the conflicting paths via ErrConflict.
	ErrMergeConflict = errors.New("git: merge conflict")

	// ErrCloneNotFound — no on-disk clone at the expected path.
	// Distinct from ErrRefNotFound (clone exists, ref doesn't).
	ErrCloneNotFound = errors.New("git: no clone at expected path")

	// ErrPreserveDirCollision is preserved for API compatibility
	// with the gitv5 / gitv6 backends. The CLI backend never
	// creates a preserve dir, so this error should not be
	// returned at runtime — but callers still match against it.
	ErrPreserveDirCollision = errors.New("git: stale preserve dir blocks operation")

	// ErrInvalidWorktreeState — the verb requires the worktree to
	// be in one of N states; current state isn't in that set.
	// Always carries the actual + expected states via
	// ErrWorktreeState.
	ErrInvalidWorktreeState = errors.New("git: worktree state incompatible with operation")

	// ErrBranchExists — caller asked to create a branch that
	// already exists. Distinct from "branch missing" so the caller
	// can decide whether to delete-and-recreate or reuse.
	ErrBranchExists = errors.New("git: branch already exists")

	// ErrNotImplemented — placeholder for Ops methods still being
	// ported. Vanishes when Phase 9 flips production to gitcli.
	ErrNotImplemented = errors.New("gitcli: not yet implemented")

	// ErrRemoteNotFound — the configured origin URL doesn't
	// resolve to a real git repository (path missing, server
	// returned 404, SSH host refused). Distinct from
	// ErrPushNonFF (remote exists, just won't accept this push)
	// and ErrRefNotFound (we tried to resolve a ref that doesn't
	// exist on either side). Wraps git's "does not appear to be
	// a git repository" / "repository not found" / "Could not
	// read from remote repository" stderr patterns.
	ErrRemoteNotFound = errors.New("git: remote repository not found")
)

// ErrConflict carries the paths that conflicted during a merge.
// Returned wrapped around ErrMergeConflict.
type ErrConflict struct {
	Paths []string
}

func (e *ErrConflict) Error() string {
	return fmt.Sprintf("git: merge conflict in %d file(s): %s",
		len(e.Paths), strings.Join(e.Paths, ", "))
}

func (e *ErrConflict) Is(target error) bool {
	return target == ErrMergeConflict
}

// ErrVerifyFailed carries the local + remote hashes when a push
// verify fails. Returned wrapped around ErrPushVerifyFailed.
type ErrVerifyFailed struct {
	Branch    string
	LocalSHA  string
	RemoteSHA string
}

func (e *ErrVerifyFailed) Error() string {
	return fmt.Sprintf("git: push verify failed for %s: local %s, remote %s",
		e.Branch, shortSHA(e.LocalSHA), shortSHA(e.RemoteSHA))
}

func (e *ErrVerifyFailed) Is(target error) bool {
	return target == ErrPushVerifyFailed
}

// ErrWorktreeState carries the actual and expected states when a
// verb's pre-state requirement is violated.
type ErrWorktreeState struct {
	Actual   WorktreeState
	Expected []WorktreeState
}

func (e *ErrWorktreeState) Error() string {
	names := make([]string, len(e.Expected))
	for i, s := range e.Expected {
		names[i] = s.String()
	}
	return fmt.Sprintf("git: worktree state %s, expected one of {%s}",
		e.Actual.String(), strings.Join(names, ", "))
}

func (e *ErrWorktreeState) Is(target error) bool {
	return target == ErrInvalidWorktreeState
}

// shortSHA truncates a hex SHA to 12 chars for log output.
// Non-hex inputs pass through unchanged.
func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
