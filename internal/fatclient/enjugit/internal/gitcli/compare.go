package gitcli

// compare.go — Phase 7 diagnostic verbs. Both are read-only and
// do not acquire the project lock.
//
// CompareToRemote and CompareCommits classify the relationship
// between two commits (or "local + origin/" tip pairs) into a
// RemoteState (in-sync / ahead / behind / diverged / unrelated)
// with ahead/behind counts for the divergent cases. Used by
// sync-preflight diagnostics and the project_remote_status
// reporter.

import (
	"fmt"
	"strconv"
	"strings"
)

// CompareToRemote returns local-vs-remote sync state for the
// named branches. When branches is nil/empty, compares every
// local branch.
//
// Does NOT fetch first — caller runs Fetch() if they want
// fresh data. (Fetching here would surprise callers running
// CompareToRemote on a cadence to display status.)
func (c *Clone) CompareToRemote(branches []string) (*RemoteComparison, error) {
	out := &RemoteComparison{}
	if len(branches) == 0 {
		all, err := c.LocalBranches()
		if err != nil {
			return nil, err
		}
		branches = all
	}
	for _, br := range branches {
		bs := BranchStatus{Name: br}
		// Local SHA — refs/heads/<br>.
		if sha, err := c.localRefHashLocked(br); err == nil {
			bs.LocalSHA = sha
		}
		// Remote-tracking SHA — refs/remotes/origin/<br>.
		if sha, ok := c.resolveOriginRefHashLocked(br); ok {
			bs.RemoteSHA = sha
		}
		bs.State = c.classifyState(bs.LocalSHA, bs.RemoteSHA)
		out.Branches = append(out.Branches, bs)
	}
	return out, nil
}

// classifyState picks a RemoteState for one branch given its
// local + remote SHAs. Same precedence as gitv6:
//   - local missing → RemoteUnknown
//   - remote missing → RemoteAhead (local exists; push would
//     create remote)
//   - equal → RemoteInSync
//   - one is ancestor of the other → Behind / Ahead
//   - no common ancestor → RemoteUnrelated
//   - else → RemoteDiverged
//
// Either commit unresolvable locally (both should be — refs
// resolved at the caller) → RemoteUnreachable.
func (c *Clone) classifyState(localSHA, remoteSHA string) RemoteState {
	switch {
	case localSHA == "":
		return RemoteUnknown
	case remoteSHA == "":
		return RemoteAhead
	case localSHA == remoteSHA:
		return RemoteInSync
	}
	// Both SHAs present and different. Make sure both objects
	// exist locally before walking — a remote-tracking ref can
	// occasionally outlive its pack (gc shenanigans) and we'd
	// surface that as Unreachable.
	if !c.commitExistsLocally(localSHA) || !c.commitExistsLocally(remoteSHA) {
		return RemoteUnreachable
	}
	if isAnc, _ := c.isAncestorRaw(localSHA, remoteSHA); isAnc {
		return RemoteBehind
	}
	if isAnc, _ := c.isAncestorRaw(remoteSHA, localSHA); isAnc {
		return RemoteAhead
	}
	if base, ok := c.mergeBaseLocked(localSHA, remoteSHA); ok && base != "" {
		return RemoteDiverged
	}
	return RemoteUnrelated
}

// CompareCommits classifies the relationship between two
// commits in the local object DB. localSHA and remoteSHA must
// be non-empty; callers handle missing-side cases before
// calling.
//
// When remoteSHA can't be resolved locally, attempts one fetch
// from origin and retries. Still unresolvable → returns
// RemoteDiverged with zero counts (best effort).
func (c *Clone) CompareCommits(localSHA, remoteSHA string) (CommitCompareResult, error) {
	out := CommitCompareResult{}
	if localSHA == "" || remoteSHA == "" {
		return out, fmt.Errorf("git: CompareCommits: both SHAs required")
	}
	if localSHA == remoteSHA {
		out.State = RemoteInSync
		return out, nil
	}
	if !c.commitExistsLocally(localSHA) {
		// Local missing — caller is asking about a commit we've
		// never seen. Surface as error so caller distinguishes
		// from "I know the local but not the remote."
		return out, fmt.Errorf("%w: %s", ErrCommitNotFound, localSHA)
	}
	if !c.commitExistsLocally(remoteSHA) {
		// One fetch attempt to recover (peer might have pushed
		// since our last fetch). Caller-friendly: a stale
		// cursor shouldn't require an explicit pre-fetch step.
		if c.remoteURL != "" {
			_, _ = runGit(c.workDir,
				[]string{"fetch", "origin", "+refs/heads/*:refs/remotes/origin/*"},
				runOpts{network: true})
		}
		if !c.commitExistsLocally(remoteSHA) {
			out.State = RemoteDiverged
			return out, nil
		}
	}

	// Both commits local. Classify.
	if isAnc, _ := c.isAncestorRaw(remoteSHA, localSHA); isAnc {
		out.State = RemoteAhead
		out.AheadBy = c.firstParentCount(remoteSHA, localSHA)
		return out, nil
	}
	if isAnc, _ := c.isAncestorRaw(localSHA, remoteSHA); isAnc {
		out.State = RemoteBehind
		out.BehindBy = c.firstParentCount(localSHA, remoteSHA)
		return out, nil
	}
	base, ok := c.mergeBaseLocked(localSHA, remoteSHA)
	if !ok || base == "" {
		out.State = RemoteUnrelated
		return out, nil
	}
	out.State = RemoteDiverged
	out.AheadBy = c.firstParentCount(base, localSHA)
	out.BehindBy = c.firstParentCount(base, remoteSHA)
	return out, nil
}

// --- helpers ---

// commitExistsLocally returns true when sha resolves to a
// commit object in the local DB. Read-only; doesn't fetch.
func (c *Clone) commitExistsLocally(sha string) bool {
	_, err := runGit(c.workDir, []string{"cat-file", "-e", sha + "^{commit}"}, runOpts{})
	return err == nil
}

// isAncestorRaw is a lock-free, exit-code-only ancestor check.
// Distinct from the public IsAncestor: that one is a method
// on Ops and returns (false, nil) for unknown SHAs to match
// gitv6's hash-zero contract; here we know both SHAs exist
// (caller already validated via commitExistsLocally) and just
// need the boolean.
func (c *Clone) isAncestorRaw(ancestor, descendant string) (bool, error) {
	_, err := runGit(c.workDir,
		[]string{"merge-base", "--is-ancestor", ancestor, descendant},
		runOpts{})
	if err == nil {
		return true, nil
	}
	return false, nil
}

// mergeBaseLocked returns the merge base of two commits, or
// ("", false) when no common ancestor exists. Read-only,
// lock-free (suffix matches our convention for verbs that
// don't actually acquire the lock — name signals "safe to call
// while holding").
func (c *Clone) mergeBaseLocked(a, b string) (string, bool) {
	out, err := runGit(c.workDir, []string{"merge-base", a, b}, runOpts{})
	if err != nil {
		return "", false
	}
	base := strings.TrimSpace(string(out))
	if base == "" {
		return "", false
	}
	return base, true
}

// firstParentCount returns the number of commits reachable
// from `to` but not from `from`, walking first-parent only.
// Diagnostic — returns 0 on any walk error so a count failure
// never aborts the comparison.
//
// Maps to gitv6's countFirstParentBetween(from=to, until=from)
// — same numeric result via git's native rev-list.
func (c *Clone) firstParentCount(from, to string) int {
	out, err := runGit(c.workDir,
		[]string{"rev-list", "--first-parent", "--count", from + ".." + to},
		runOpts{})
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}
