package enjugit

// Pins the invariant the `enju go` no-checkout reconcile rests on: the
// compute cascade reconciles the run branch via FetchBranch (origin-
// tracking only), which must NOT move the operator's worktree onto that
// branch. If a future change routed the compute reconcile back through a
// checkout/pull, this would catch the operator's HEAD jumping to the run
// branch. (See service.reconcileBranchWF.)

import (
	"testing"
)

func TestFetchBranch_DoesNotMoveWorktreeHEAD(t *testing.T) {
	bare := initBareForWorkspaceTest(t) // origin/main = seed

	// Operator's clone, sitting on the default branch.
	wsOp, _ := newWorkspaceForIDs(t, 91)
	wfOp, err := wsOp.ForProject(91, bare)
	if err != nil {
		t.Fatalf("ForProject op: %v", err)
	}
	_, startBranch, err := wfOp.git.Head()
	if err != nil || startBranch == "" {
		t.Fatalf("read starting HEAD branch: %q err=%v", startBranch, err)
	}

	// A second clone creates the run branch with a commit and pushes it
	// to origin — modelling commits that landed on the run branch the
	// operator's clone hasn't seen yet.
	wsRun, _ := newWorkspaceForIDs(t, 92)
	wfRun, err := wsRun.ForProject(92, bare)
	if err != nil {
		t.Fatalf("ForProject run: %v", err)
	}
	if _, err := wfRun.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "run-1",
		Subject:     "run work",
		AuthorName:  "R",
		AuthorEmail: "r@example.com",
		Files:       []FileWrite{{RepoRelPath: "out/r.md", Content: []byte("r")}},
	}); err != nil {
		t.Fatalf("commit+push run-1: %v", err)
	}

	// The no-checkout reconcile primitive.
	if err := wfOp.git.FetchBranch("run-1"); err != nil {
		t.Fatalf("FetchBranch run-1: %v", err)
	}

	// HEAD must still be on the operator's original branch — fetch is
	// ref-only, never a checkout.
	_, afterBranch, err := wfOp.git.Head()
	if err != nil {
		t.Fatalf("read HEAD after fetch: %v", err)
	}
	if afterBranch != startBranch {
		t.Errorf("FetchBranch moved the worktree HEAD: was on %q, now on %q — the compute reconcile must never check out the run branch",
			startBranch, afterBranch)
	}

	// And origin/run-1 is now resolvable locally (the fetch did update
	// the remote-tracking ref).
	if sha, rerr := wfOp.git.RemoteBranchHash("run-1"); rerr != nil || sha == "" {
		t.Errorf("origin should carry run-1 after the fetch: sha=%q err=%v", sha, rerr)
	}
}
