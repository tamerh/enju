package enjugit

import (
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// ReadFileAtCommit reads a file's content at a specific commit.
// Pass-through to git.View.ReadFileAtCommit.
//
// Returns:
//   - (content, true, nil) on hit.
//   - (nil, false, nil) when the file isn't in the commit's tree.
//   - (nil, false, ErrCommitNotFound) when the SHA can't be resolved.
func (v *View) ReadFileAtCommit(sha, path string) ([]byte, bool, error) {
	body, found, err := v.git.ReadFileAtCommit(sha, path)
	return body, found, translateGitError("read at commit", err)
}

// Head returns HEAD's commit SHA + branch name. Branch is "" when
// HEAD is detached.
func (v *View) Head() (sha, branch string, err error) {
	sha, branch, err = v.git.Head()
	return sha, branch, translateGitError("head", err)
}

// CompareToRemote returns local-vs-remote sync state for the
// listed branches (or all local branches when nil).
func (v *View) CompareToRemote(branches []string) (*git.RemoteComparison, error) {
	cmp, err := v.git.CompareToRemote(branches)
	return cmp, translateGitError("compare to remote", err)
}

// ResolveRef resolves a ref name (or SHA) to a commit SHA.
// Returns ErrUpstreamNotFound when the name doesn't resolve —
// the View-side translation of git.ErrRefNotFound.
func (v *View) ResolveRef(name string) (string, error) {
	sha, err := v.git.ResolveRef(name)
	if err != nil {
		return "", translateGitError("resolve ref", err)
	}
	return sha, nil
}

// ResultAtCommit reads the canonical result.md path for a task
// iteration. Convenience wrapper around ReadFileAtCommit that
// composes the path from task ID + iter seq using Conventions.
//
// Currently delegates to ReadFileAtCommit with a path the caller
// composes; we keep this as a stub so service can call a
// task-named verb. Full implementation in a later phase once
// service decides whether ResultDir lives in coord state or
// gets composed from conventions.
func (v *View) ResultAtCommit(sha, resultDir string) (string, bool, error) {
	if resultDir == "" {
		return "", false, fmt.Errorf("enjugit: ResultAtCommit: resultDir required")
	}
	body, found, err := v.ReadFileAtCommit(sha, resultDir+"/result.md")
	return string(body), found, err
}
