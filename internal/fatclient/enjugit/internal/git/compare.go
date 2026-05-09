package git

import (
	"errors"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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

// CompareCommits classifies the relationship between two commits
// in the local object DB and returns ahead/behind counts. See the
// Ops docstring for the contract. localSHA and remoteSHA must be
// non-empty.
//
// Tries once to load both commits; if remoteSHA is missing,
// fetches origin and retries. On any unresolved-load after that
// (e.g., remote is genuinely unrelated history), returns
// RemoteDiverged with zero counts.
func (c *Clone) CompareCommits(localSHA, remoteSHA string) (CommitCompareResult, error) {
	out := CommitCompareResult{}
	if localSHA == remoteSHA {
		out.State = RemoteInSync
		return out, nil
	}
	localHash := plumbing.NewHash(localSHA)
	remoteHash := plumbing.NewHash(remoteSHA)
	localCommit, err := c.repo.CommitObject(localHash)
	if err != nil {
		return out, err
	}
	remoteCommit, err := c.repo.CommitObject(remoteHash)
	if err != nil {
		fetchErr := c.repo.Fetch(&gogit.FetchOptions{
			RemoteName: "origin",
			Auth:       SSHAuthMethod(c.RemoteURL()),
		})
		if fetchErr != nil && !errors.Is(fetchErr, gogit.NoErrAlreadyUpToDate) {
			out.State = RemoteDiverged
			return out, nil
		}
		remoteCommit, err = c.repo.CommitObject(remoteHash)
		if err != nil {
			out.State = RemoteDiverged
			return out, nil
		}
	}
	if anc, aerr := remoteCommit.IsAncestor(localCommit); aerr == nil && anc {
		out.State = RemoteAhead
		out.AheadBy = countFirstParentBetween(localCommit, remoteSHA)
		return out, nil
	}
	if anc, aerr := localCommit.IsAncestor(remoteCommit); aerr == nil && anc {
		out.State = RemoteBehind
		out.BehindBy = countFirstParentBetween(remoteCommit, localSHA)
		return out, nil
	}
	bases, err := localCommit.MergeBase(remoteCommit)
	if err != nil || len(bases) == 0 {
		out.State = RemoteUnrelated
		return out, nil
	}
	baseHash := bases[0].Hash.String()
	out.State = RemoteDiverged
	out.AheadBy = countFirstParentBetween(localCommit, baseHash)
	out.BehindBy = countFirstParentBetween(remoteCommit, baseHash)
	return out, nil
}

// countFirstParentBetween walks first-parent history starting at
// `from` and returns the number of commits before reaching
// `until` (exclusive). Diagnostic only — returns 0 on any walk
// error so a count failure never aborts the comparison.
func countFirstParentBetween(from *object.Commit, until string) int {
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

// silence unused import when we don't reference gogit types in
// this file directly.
var _ = errors.New
var _ = gogit.NoErrAlreadyUpToDate
