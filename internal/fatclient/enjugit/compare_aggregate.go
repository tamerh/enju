package enjugit

import (
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
//
// Goes entirely through the git.Ops surface (RemoteBranchHash +
// CompareCommits). No direct go-git imports — the seam stays
// clean so the underlying backend can be swapped without
// touching this file.
func (w *Workflow) CompareDefaultBranch(branch string) (*AggregateComparison, error) {
	r := &AggregateComparison{}
	if branch == "" {
		branch = w.DefaultBranch()
	}

	localHash, _, _ := w.git.Head()
	r.LocalHead = localHash

	remoteHash, err := w.git.RemoteBranchHash(branch)
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

	cmp, err := w.git.CompareCommits(localHash, remoteHash)
	if err != nil {
		r.Status = AggregateUnreachable
		r.Unreachable = err.Error()
		return r, nil
	}
	r.AheadBy = cmp.AheadBy
	r.BehindBy = cmp.BehindBy
	r.Status = aggregateFromRemoteState(cmp.State)
	return r, nil
}

// aggregateFromRemoteState maps git.RemoteState (the backend-level
// enum) to AggregateRemoteStatus (the UX-level enum). Empty-side
// cases (RemoteEmpty / LocalEmpty) are handled by the caller
// before invoking CompareCommits, so they don't appear here.
func aggregateFromRemoteState(s git.RemoteState) AggregateRemoteStatus {
	switch s {
	case git.RemoteInSync:
		return AggregateInSync
	case git.RemoteAhead:
		return AggregateAhead
	case git.RemoteBehind:
		return AggregateBehind
	case git.RemoteDiverged:
		return AggregateDiverged
	case git.RemoteUnrelated:
		return AggregateUnrelated
	case git.RemoteUnreachable:
		return AggregateUnreachable
	default:
		return AggregateUnreachable
	}
}
