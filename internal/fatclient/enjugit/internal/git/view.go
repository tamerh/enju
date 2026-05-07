package git

// View is the read-only interface a *Clone satisfies. Used by
// callers (enjugit.View) that only need to read — they take a
// View, not a *Clone, so the type itself signals "no mutation
// possible from here."
//
// Same methods as Ops's read subset. Pure subset for clarity:
// any *Clone trivially satisfies View by virtue of having all of
// Ops.
type View interface {
	ReadFileAtCommit(sha, path string) (content []byte, found bool, err error)
	ResolveRef(name string) (sha string, err error)
	Head() (sha, branch string, err error)
	LocalBranches() ([]string, error)
	State() WorktreeState
	CompareToRemote(branches []string) (*RemoteComparison, error)
}
