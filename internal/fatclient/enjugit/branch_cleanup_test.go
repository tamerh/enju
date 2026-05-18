package enjugit

import (
	"fmt"
	"testing"

	"github.com/enju-ai/enju/internal/common/wire"
)

// terminalRun builds a minimal wire.Run with the given seq, slug,
// branch, and terminal state for use in cleanup tests.
func terminalRun(seq int, slug, branch string) wire.Run {
	return wire.Run{Seq: seq, Slug: slug, Branch: branch, State: "completed"}
}

func TestCleanupRunBranches_ModeNone(t *testing.T) {
	wf, _ := makeWorkflow(t)
	runs := []wire.Run{terminalRun(1, "build", "run-1")}
	res, err := wf.CleanupRunBranches(runs, CleanupModeNone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Archived)+len(res.Pruned) != 0 {
		t.Errorf("mode=none should not touch any branches")
	}
}

func TestCleanupRunBranches_Archive_MergedRunBranch(t *testing.T) {
	wf, fake := makeWorkflow(t)
	const runBranch = "run-1"
	const runSHA = "aaaa000000000000000000000000000000000001"
	const baseSHA = "bbbb000000000000000000000000000000000001"

	fake.branches = []string{"main", runBranch}
	fake.resolveMap["refs/heads/main"] = baseSHA
	fake.resolveMap["refs/heads/"+runBranch] = runSHA
	// runSHA is an ancestor of baseSHA → merged
	fake.perAncestorResult = map[string]bool{runSHA: true}

	runs := []wire.Run{terminalRun(1, "build", runBranch)}
	res, err := wf.CleanupRunBranches(runs, CleanupModeArchive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != runBranch {
		t.Errorf("expected %q archived, got %v", runBranch, res.Archived)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("archive mode should not prune")
	}
	// Archive ref must have been created.
	if fake.callCount("CreateRef") != 1 {
		t.Errorf("expected 1 CreateRef, got %d", fake.callCount("CreateRef"))
	}
	// Head ref must be CAS-deleted (not unconditional).
	if fake.callCount("DeleteBranch") != 0 {
		t.Errorf("archive must use DeleteBranchCAS, not DeleteBranch")
	}
	if fake.callCount("DeleteBranchCAS") != 1 {
		t.Errorf("expected 1 DeleteBranchCAS, got %d", fake.callCount("DeleteBranchCAS"))
	}
}

func TestCleanupRunBranches_Prune_MergedRunBranch(t *testing.T) {
	wf, fake := makeWorkflow(t)
	const runBranch = "run-2"
	const runSHA = "cccc000000000000000000000000000000000001"
	const baseSHA = "dddd000000000000000000000000000000000001"

	fake.branches = []string{"main", runBranch}
	fake.resolveMap["refs/heads/main"] = baseSHA
	fake.resolveMap["refs/heads/"+runBranch] = runSHA
	fake.perAncestorResult = map[string]bool{runSHA: true}

	runs := []wire.Run{terminalRun(2, "myrun", runBranch)}
	res, err := wf.CleanupRunBranches(runs, CleanupModePrune)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Pruned) != 1 || res.Pruned[0] != runBranch {
		t.Errorf("expected %q pruned, got %v", runBranch, res.Pruned)
	}
	if fake.callCount("CreateRef") != 0 {
		t.Errorf("prune mode must not call CreateRef")
	}
	if fake.callCount("DeleteBranch") != 0 {
		t.Errorf("prune must use DeleteBranchCAS, not DeleteBranch")
	}
	if fake.callCount("DeleteBranchCAS") != 1 {
		t.Errorf("expected 1 DeleteBranchCAS, got %d", fake.callCount("DeleteBranchCAS"))
	}
}

func TestCleanupRunBranches_UnmergedIterBranchPreserved(t *testing.T) {
	wf, fake := makeWorkflow(t)
	const baseSHA = "base000000000000000000000000000000000001"
	const mergedSHA = "aaaa000000000000000000000000000000000001"
	const unmergedSHA = "bbbb000000000000000000000000000000000001"

	// Run 3, slug "analysis": run branch + two iter branches
	// (one merged, one not — the rejected iteration).
	const runBranch = "run-3"
	const mergedIter = "3-analysis/process/iter-1"
	const unmergedIter = "3-analysis/process/iter-2"

	fake.branches = []string{"main", runBranch, mergedIter, unmergedIter}
	fake.resolveMap["refs/heads/main"] = baseSHA
	fake.resolveMap["refs/heads/"+runBranch] = mergedSHA
	fake.resolveMap["refs/heads/"+mergedIter] = mergedSHA
	fake.resolveMap["refs/heads/"+unmergedIter] = unmergedSHA
	// mergedSHA is an ancestor of base; unmergedSHA is not.
	fake.perAncestorResult = map[string]bool{
		mergedSHA:   true,
		unmergedSHA: false,
	}

	runs := []wire.Run{terminalRun(3, "analysis", runBranch)}
	res, err := wf.CleanupRunBranches(runs, CleanupModeArchive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// run-3 + merged iter should be archived; unmerged iter skipped.
	if len(res.Archived) != 2 {
		t.Errorf("expected 2 archived (run branch + merged iter), got %d: %v", len(res.Archived), res.Archived)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != unmergedIter {
		t.Errorf("expected unmerged iter %q skipped, got %v", unmergedIter, res.Skipped)
	}
}

func TestCleanupRunBranches_NonTerminalRunSkipped(t *testing.T) {
	wf, fake := makeWorkflow(t)
	const baseSHA = "base000000000000000000000000000000000001"
	fake.branches = []string{"main", "run-1"}
	fake.resolveMap["refs/heads/main"] = baseSHA
	fake.resolveMap["refs/heads/run-1"] = "aaaa000000000000000000000000000000000001"
	// All SHAs default to ancestor=true, but the run is active.
	activeRun := wire.Run{Seq: 1, Slug: "build", Branch: "run-1", State: "active"}

	res, err := wf.CleanupRunBranches([]wire.Run{activeRun}, CleanupModeArchive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Archived)+len(res.Pruned)+len(res.Skipped) != 0 {
		t.Errorf("active run should not touch any branches")
	}
	if fake.callCount("CreateRef")+fake.callCount("DeleteBranch") != 0 {
		t.Errorf("no git mutations expected for active run")
	}
}

func TestCleanupRunBranches_MissingLocalBranchIsNoOp(t *testing.T) {
	wf, fake := makeWorkflow(t)
	const baseSHA = "base000000000000000000000000000000000001"
	fake.branches = []string{"main"} // run-4 not present locally
	fake.resolveMap["refs/heads/main"] = baseSHA
	// run-4 branch not in resolveMap (already deleted or never checked out).

	runs := []wire.Run{terminalRun(4, "gone", "run-4")}
	res, err := wf.CleanupRunBranches(runs, CleanupModeArchive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Archived)+len(res.Errors) != 0 {
		t.Errorf("missing branch should be silently skipped, got archived=%v errors=%v",
			res.Archived, res.Errors)
	}
}

func TestCleanupRunBranches_Prune_UsesCASDelete(t *testing.T) {
	// Verifies that prune calls DeleteBranchCAS (not DeleteBranch),
	// passing the exact SHA that was resolved before the ancestor check.
	wf, fake := makeWorkflow(t)
	const runBranch = "run-6"
	const runSHA = "cccc000000000000000000000000000000000002"
	const baseSHA = "eeee000000000000000000000000000000000002"

	fake.branches = []string{"main", runBranch}
	fake.resolveMap["refs/heads/main"] = baseSHA
	fake.resolveMap["refs/heads/"+runBranch] = runSHA
	fake.perAncestorResult = map[string]bool{runSHA: true}

	runs := []wire.Run{terminalRun(6, "wf", runBranch)}
	res, err := wf.CleanupRunBranches(runs, CleanupModePrune)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Pruned) != 1 {
		t.Fatalf("expected branch pruned, got %v / errors: %v", res.Pruned, res.Errors)
	}
	if fake.callCount("DeleteBranch") != 0 {
		t.Errorf("prune must not call unconditional DeleteBranch")
	}
	// Verify the CAS was called with the vetted SHA.
	cas := fake.lastCall("DeleteBranchCAS")
	if cas == nil {
		t.Fatal("expected DeleteBranchCAS call")
	}
	if cas.Args[1] != runSHA {
		t.Errorf("DeleteBranchCAS called with SHA %v, want %s", cas.Args[1], runSHA)
	}
}

func TestCleanupRunBranches_Prune_CASErrorIsSurfaced(t *testing.T) {
	// Verifies that a DeleteBranchCAS failure (e.g. concurrent advance)
	// is collected as a per-branch error, not a top-level abort.
	wf, fake := makeWorkflow(t)
	const runBranch = "run-7"
	const runSHA = "dddd000000000000000000000000000000000002"
	const baseSHA = "ffff000000000000000000000000000000000002"

	fake.branches = []string{"main", runBranch}
	fake.resolveMap["refs/heads/main"] = baseSHA
	fake.resolveMap["refs/heads/"+runBranch] = runSHA
	fake.perAncestorResult = map[string]bool{runSHA: true}
	fake.errOnCall["DeleteBranchCAS"] = fmt.Errorf("CAS: ref advanced concurrently")

	runs := []wire.Run{terminalRun(7, "wf", runBranch)}
	res, err := wf.CleanupRunBranches(runs, CleanupModePrune)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("branch must not be counted as pruned on CAS error")
	}
	if len(res.Errors) != 1 {
		t.Errorf("expected 1 error, got %v", res.Errors)
	}
}

func TestCleanupRunBranches_EmptySlugSkipsIterScan(t *testing.T) {
	wf, fake := makeWorkflow(t)
	const baseSHA = "base000000000000000000000000000000000001"
	const runSHA = "aaaa000000000000000000000000000000000001"
	// Also add a branch that looks like an iter branch for seq 5,
	// but since Slug is "", the prefix scan should be skipped.
	fake.branches = []string{"main", "run-5", "5-/iter-1"}
	fake.resolveMap["refs/heads/main"] = baseSHA
	fake.resolveMap["refs/heads/run-5"] = runSHA
	fake.perAncestorResult = map[string]bool{runSHA: true}
	fake.resolveMap["refs/heads/5-/iter-1"] = "orphan00000000000000000000000000000000001"

	noSlugRun := wire.Run{Seq: 5, Slug: "", Branch: "run-5", State: "completed"}
	res, err := wf.CleanupRunBranches([]wire.Run{noSlugRun}, CleanupModeArchive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the run branch itself should be archived; the "iter" branch
	// with empty slug prefix is not touched.
	if len(res.Archived) != 1 {
		t.Errorf("expected 1 archived (run branch only), got %v", res.Archived)
	}
}
