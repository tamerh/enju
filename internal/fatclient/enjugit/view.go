package enjugit

import (
	"fmt"
	"log/slog"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// View is the read-only handle for one project. Constructed by
// Workspace.OpenView or OpenOrLazyClone. Used by surfaces that
// only display content (webui, inbox).
//
// The type itself is the capability boundary — code that takes
// *View can't call mutating methods, full stop. That's the
// point: webui code reading "this function takes *View" knows
// at a glance it can't push, can't commit, can't switch branches.
type View struct {
	git    git.View // the read-only interface, not Ops
	convs  Conventions
	projID int64
	logger *slog.Logger
}

// ProjectID returns the project ID this View operates on.
func (v *View) ProjectID() int64 { return v.projID }

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
// Returns ErrRefNotFound when the name doesn't resolve. Callers
// that want an upstream-specific error (e.g. "this run branch
// must already be on origin") wrap explicitly.
func (v *View) ResolveRef(name string) (string, error) {
	sha, err := v.git.ResolveRef(name)
	if err != nil {
		return "", translateGitError("resolve ref", err)
	}
	return sha, nil
}

// ResultAtCommit reads the canonical result.md path for a task
// iteration. Convenience wrapper around ReadFileAtCommit that
// composes the path from a task's result dir.
func (v *View) ResultAtCommit(sha, resultDir string) (string, bool, error) {
	if resultDir == "" {
		return "", false, fmt.Errorf("enjugit: ResultAtCommit: resultDir required")
	}
	body, found, err := v.ReadFileAtCommit(sha, resultDir+"/result.md")
	return string(body), found, err
}
