package enjugit

import (
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// AggregateRemoteStatus enumerates the possible relationships between
// the local HEAD and the remote ref. Used by sync-preflight UX
// (project_remote_status / project_sync MCP tools) to render
// actionable guidance: ahead is fast-forwardable, diverged requires
// force, etc.
//
// Distinct from git.BranchState (the per-branch shape on
// View.CompareToRemote) — this aggregate compares one branch and
// adds counts + an unreachable signal when ls-remote fails.
type AggregateRemoteStatus string

const (
	AggregateInSync      AggregateRemoteStatus = "in_sync"
	AggregateAhead       AggregateRemoteStatus = "ahead"
	AggregateBehind      AggregateRemoteStatus = "behind"
	AggregateDiverged    AggregateRemoteStatus = "diverged"
	AggregateUnrelated   AggregateRemoteStatus = "unrelated"
	AggregateRemoteEmpty AggregateRemoteStatus = "remote_empty"
	AggregateLocalEmpty  AggregateRemoteStatus = "local_empty"
	AggregateUnreachable AggregateRemoteStatus = "unreachable"
)

// AggregateComparison is the single-branch result of
// CompareDefaultBranch. Carries the relationship (ahead/behind/etc.)
// plus head SHAs and ahead/behind counts so the caller can render a
// full sync-status response in one shot.
type AggregateComparison struct {
	Status     AggregateRemoteStatus
	LocalHead  string
	RemoteHead string
	AheadBy    int
	BehindBy   int
	// Unreachable carries the underlying error when Status ==
	// AggregateUnreachable so the caller can surface it in UX.
	Unreachable string
}

// CompareDefaultBranch performs an ls-remote against origin and
// classifies the local default branch's relationship to the remote
// ref. Used as a sync preflight: the distinction between Ahead and
// Diverged decides whether a non-force push is safe.
//
// branch is the branch name to compare (typically the project's
// default). Empty falls back to the workflow's default branch.
//
// Origin is assumed configured — every project gets one at
// creation. A genuinely missing origin (corrupt project state)
// surfaces as AggregateUnreachable with the underlying error.
func (w *Workflow) CompareDefaultBranch(branch string) (*AggregateComparison, error) {
	r := &AggregateComparison{}
	if branch == "" {
		branch = w.DefaultBranch()
	}
	// Production always backs Workflow with *git.Clone; the
	// Ops-only fakeOps test path does not exercise this verb.
	clone, ok := w.git.(*git.Clone)
	if !ok {
		return nil, fmt.Errorf("enjugit: CompareDefaultBranch requires concrete *git.Clone, got %T", w.git)
	}

	localHash, _, _ := clone.Head()
	r.LocalHead = localHash

	remoteHash, err := clone.RemoteBranchHash(branch)
	if err != nil {
		r.Status = AggregateUnreachable
		r.Unreachable = err.Error()
		return r, nil
	}
	r.RemoteHead = remoteHash

	switch {
	case localHash == "" && remoteHash == "":
		r.Status = AggregateInSync
		return r, nil
	case localHash == "":
		r.Status = AggregateLocalEmpty
		return r, nil
	case remoteHash == "":
		r.Status = AggregateRemoteEmpty
		return r, nil
	case localHash == remoteHash:
		r.Status = AggregateInSync
		return r, nil
	}

	repo := clone.Repo()
	localCommit, err := repo.CommitObject(plumbing.NewHash(localHash))
	if err != nil {
		return nil, fmt.Errorf("loading local commit: %w", err)
	}
	// Remote commit may not be in the local object DB (we only did
	// ls-remote). Try once; on miss, fetch and retry.
	remoteCommit, err := repo.CommitObject(plumbing.NewHash(remoteHash))
	if err != nil {
		if fetchErr := repo.Fetch(&gogit.FetchOptions{
			RemoteName: "origin",
			Auth:       git.SSHAuthMethod(clone.RemoteURL()),
		}); fetchErr != nil && fetchErr != gogit.NoErrAlreadyUpToDate {
			r.Status = AggregateDiverged
			return r, nil
		}
		remoteCommit, err = repo.CommitObject(plumbing.NewHash(remoteHash))
		if err != nil {
			r.Status = AggregateDiverged
			return r, nil
		}
	}

	if remoteIsAncestor, aerr := remoteCommit.IsAncestor(localCommit); aerr == nil && remoteIsAncestor {
		r.Status = AggregateAhead
		r.AheadBy = countCommitsBetween(localCommit, remoteHash)
		return r, nil
	}
	if localIsAncestor, aerr := localCommit.IsAncestor(remoteCommit); aerr == nil && localIsAncestor {
		r.Status = AggregateBehind
		r.BehindBy = countCommitsBetween(remoteCommit, localHash)
		return r, nil
	}

	bases, err := localCommit.MergeBase(remoteCommit)
	if err != nil || len(bases) == 0 {
		r.Status = AggregateUnrelated
		return r, nil
	}
	baseHash := bases[0].Hash.String()
	r.Status = AggregateDiverged
	r.AheadBy = countCommitsBetween(localCommit, baseHash)
	r.BehindBy = countCommitsBetween(remoteCommit, baseHash)
	return r, nil
}

// countCommitsBetween walks first-parent history starting at `from`
// and returns the count of commits before reaching `until`
// (exclusive). Diagnostic only — returns 0 on any walk error.
func countCommitsBetween(from *object.Commit, until string) int {
	count := 0
	current := from
	for current != nil {
		if current.Hash.String() == until {
			return count
		}
		count++
		if current.NumParents() == 0 {
			return count
		}
		parent, err := current.Parent(0)
		if err != nil {
			return count
		}
		current = parent
	}
	return count
}
