package git

import (
	"errors"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// CompareToRemote returns local-vs-remote sync state for the
// named branches. When branches is nil/empty, compares all local
// branches that have a corresponding remote-tracking ref.
//
// Read-only — does not acquire the lock. Does NOT fetch first;
// caller should Fetch() if they want fresh data.
func (c *Clone) CompareToRemote(branches []string) (*RemoteComparison, error) {
	out := &RemoteComparison{}
	if len(branches) == 0 {
		// Default: all local branches.
		all, err := c.LocalBranches()
		if err != nil {
			return nil, err
		}
		branches = all
	}
	for _, br := range branches {
		bs := BranchStatus{Name: br}
		// Local SHA
		if ref, err := c.repo.Reference(plumbing.NewBranchReferenceName(br), true); err == nil {
			bs.LocalSHA = ref.Hash().String()
		}
		// Remote-tracking SHA
		if ref, err := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", br), true); err == nil {
			bs.RemoteSHA = ref.Hash().String()
		}
		bs.State = c.classifyState(bs.LocalSHA, bs.RemoteSHA)
		out.Branches = append(out.Branches, bs)
	}
	return out, nil
}

// classifyState picks a RemoteState for one branch given its
// local + remote SHAs.
func (c *Clone) classifyState(localSHA, remoteSHA string) RemoteState {
	switch {
	case localSHA == "":
		// Local missing — nothing to compare against. Caller can
		// inspect BranchStatus.RemoteSHA directly if it cares.
		return RemoteUnknown
	case remoteSHA == "":
		// Branch exists locally but not yet pushed. Treat as Ahead
		// — push will create the remote ref.
		return RemoteAhead
	case localSHA == remoteSHA:
		return RemoteInSync
	}
	localHash := plumbing.NewHash(localSHA)
	remoteHash := plumbing.NewHash(remoteSHA)
	localCommit, lerr := c.repo.CommitObject(localHash)
	remoteCommit, rerr := c.repo.CommitObject(remoteHash)
	if lerr != nil || rerr != nil {
		return RemoteUnreachable
	}
	// Local ancestor of remote → behind.
	if isAnc, err := localCommit.IsAncestor(remoteCommit); err == nil && isAnc {
		return RemoteBehind
	}
	// Remote ancestor of local → ahead.
	if isAnc, err := remoteCommit.IsAncestor(localCommit); err == nil && isAnc {
		return RemoteAhead
	}
	// Neither way: check for any common ancestor at all.
	bases, err := localCommit.MergeBase(remoteCommit)
	if err != nil || len(bases) == 0 {
		return RemoteUnrelated
	}
	return RemoteDiverged
}

// silence unused import when we don't reference gogit types in
// this file directly.
var _ = errors.New
var _ = gogit.NoErrAlreadyUpToDate
