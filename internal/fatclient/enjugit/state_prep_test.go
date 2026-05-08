package enjugit

import (
	"errors"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// makeWorkflow returns a Workflow wired to a fake git.Ops with
// production conventions and the standard test defaults
// (default branch "main", templates root "enju/templates").
// Tests that need other state can override after construction.
func makeWorkflow(t *testing.T) (*Workflow, *fakeOps) {
	t.Helper()
	fake := newFakeOps()
	wf := &Workflow{
		git:           fake,
		convs:         NewProductionConventions(),
		projID:        7,
		defaultBranch: "main",
		logger:        nullLogger(),
	}
	return wf, fake
}

func TestMaterializeUpstreamForReview_HappyPath(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["1-build/develop_a/iter-1"] = "abcdef0123456789abcdef0123456789abcdef01"

	if err := wf.MaterializeUpstreamForReview("1-build/develop_a/iter-1"); err != nil {
		t.Fatalf("MaterializeUpstreamForReview: %v", err)
	}
	// Must call Fetch (so origin is current) then ResolveRef
	// then CheckoutCommit.
	if fake.callCount("Fetch") != 1 {
		t.Errorf("expected 1 Fetch, got %d", fake.callCount("Fetch"))
	}
	if fake.callCount("CheckoutCommit") != 1 {
		t.Errorf("expected 1 CheckoutCommit, got %d", fake.callCount("CheckoutCommit"))
	}
	last := fake.lastCall("CheckoutCommit")
	if last.Args[0] != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("CheckoutCommit got SHA %v, want the resolved one", last.Args[0])
	}
}

func TestMaterializeUpstreamForReview_UpstreamMissing(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// resolveMap empty → ResolveRef returns ErrRefNotFound.
	_ = fake

	err := wf.MaterializeUpstreamForReview("nonexistent/topic")
	if !errors.Is(err, ErrUpstreamNotFound) {
		t.Errorf("expected ErrUpstreamNotFound, got %v", err)
	}
}

func TestMaterializeUpstreamForReview_EmptyBranch(t *testing.T) {
	wf, _ := makeWorkflow(t)
	if err := wf.MaterializeUpstreamForReview(""); err == nil {
		t.Error("expected error for empty upstreamBranch")
	}
}

func TestStartIterationBranch_FromRunBranch(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["main"] = "runbranch00000000000000000000000000000000"

	branchName, err := wf.StartIterationBranch(
		"7:1:dev_a", 2,
		ForkFromRunBranch,
		"dev_a", "",
		1, "build",
		"main", "",
	)
	if err != nil {
		t.Fatalf("StartIterationBranch: %v", err)
	}
	if branchName != "1-build/dev_a/iter-2" {
		t.Errorf("branch name: got %q, want 1-build/dev_a/iter-2", branchName)
	}
	// Verify atomicity: WithLock invoked, then CreateBranchAt + Checkout.
	if fake.callCount("WithLock") != 1 {
		t.Errorf("expected 1 WithLock, got %d", fake.callCount("WithLock"))
	}
	createCall := fake.lastCall("CreateBranchAt")
	if createCall == nil {
		t.Fatal("CreateBranchAt not called")
	}
	if createCall.Args[0] != "1-build/dev_a/iter-2" {
		t.Errorf("CreateBranchAt branch: got %v, want 1-build/dev_a/iter-2", createCall.Args[0])
	}
	if createCall.Args[1] != "runbranch00000000000000000000000000000000" {
		t.Errorf("CreateBranchAt forkSHA: got %v, want runbranch's SHA", createCall.Args[1])
	}
}

func TestStartIterationBranch_FromUpstreamTopic(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["1-build/develop_a/iter-1"] = "upstreamtopic000000000000000000000000000"

	branchName, err := wf.StartIterationBranch(
		"7:1:review_a", 1,
		ForkFromUpstreamTopic,
		"review_a", "",
		1, "build",
		"main", "1-build/develop_a/iter-1",
	)
	if err != nil {
		t.Fatalf("StartIterationBranch: %v", err)
	}
	if branchName != "1-build/review_a/iter-1" {
		t.Errorf("branch name: got %q", branchName)
	}
	createCall := fake.lastCall("CreateBranchAt")
	if createCall.Args[1] != "upstreamtopic000000000000000000000000000" {
		t.Errorf("review iter must fork from upstream's topic SHA, got %v", createCall.Args[1])
	}
}

func TestStartIterationBranch_UnknownForkPoint(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.StartIterationBranch("7:1:x", 1, ForkUnknown,
		"x", "", 1, "build", "main", "")
	if !errors.Is(err, ErrInvalidForkPoint) {
		t.Errorf("expected ErrInvalidForkPoint, got %v", err)
	}
}

func TestStartIterationBranch_ForkBaseMissing(t *testing.T) {
	wf, _ := makeWorkflow(t)
	// resolveMap empty → ResolveRef("main") returns ErrRefNotFound.
	_, err := wf.StartIterationBranch("7:1:x", 1, ForkFromRunBranch,
		"x", "", 1, "build", "main", "")
	if !errors.Is(err, ErrForkBaseNotFound) {
		t.Errorf("expected ErrForkBaseNotFound, got %v", err)
	}
}

func TestStartIterationBranch_ExistingBranchErrors(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["main"] = "runtip00000000000000000000000000000000000"
	// Pre-existing branch.
	fake.resolveMap["refs/heads/1-build/dev_a/iter-2"] = "oldsha"

	_, err := wf.StartIterationBranch("7:1:dev_a", 2, ForkFromRunBranch,
		"dev_a", "", 1, "build", "main", "")
	if !errors.Is(err, ErrIterationBranchExists) {
		t.Errorf("expected ErrIterationBranchExists, got %v", err)
	}
}

func TestResumeIterationBranch_HappyPath(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/1-build/dev_a/iter-1"] = "matchingsha0000000000000000000000000000"
	fake.resolveMap["refs/remotes/origin/1-build/dev_a/iter-1"] = "matchingsha0000000000000000000000000000"

	branchName, err := wf.ResumeIterationBranch("7:1:dev_a", 1,
		"dev_a", "", 1, "build")
	if err != nil {
		t.Fatalf("ResumeIterationBranch: %v", err)
	}
	if branchName != "1-build/dev_a/iter-1" {
		t.Errorf("branch name: got %q", branchName)
	}
	if fake.callCount("Checkout") != 1 {
		t.Errorf("expected 1 Checkout, got %d", fake.callCount("Checkout"))
	}
	// Local matched origin → no SetBranchTo.
	if fake.callCount("SetBranchTo") != 0 {
		t.Errorf("expected 0 SetBranchTo (local matched origin), got %d", fake.callCount("SetBranchTo"))
	}
}

func TestResumeIterationBranch_AutoHealsStaleLocal(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/1-build/dev_a/iter-1"] = "stalelocal0000000000000000000000000000"
	fake.resolveMap["refs/remotes/origin/1-build/dev_a/iter-1"] = "originfresh000000000000000000000000000"

	if _, err := wf.ResumeIterationBranch("7:1:dev_a", 1,
		"dev_a", "", 1, "build"); err != nil {
		t.Fatalf("ResumeIterationBranch: %v", err)
	}
	// Auto-heal: local disagreed with origin → SetBranchTo
	// resets local to origin's hash.
	setCall := fake.lastCall("SetBranchTo")
	if setCall == nil {
		t.Fatal("SetBranchTo not called for stale-ref auto-heal")
	}
	if setCall.Args[1] != "originfresh000000000000000000000000000" {
		t.Errorf("SetBranchTo target: got %v, want origin's SHA", setCall.Args[1])
	}
}

func TestResumeIterationBranch_BranchMissing(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.ResumeIterationBranch("7:1:dev_a", 1,
		"dev_a", "", 1, "build")
	if !errors.Is(err, ErrIterationBranchMissing) {
		t.Errorf("expected ErrIterationBranchMissing, got %v", err)
	}
}

func TestResetCleanWorktree(t *testing.T) {
	wf, fake := makeWorkflow(t)
	if err := wf.ResetCleanWorktree(); err != nil {
		t.Fatalf("ResetCleanWorktree: %v", err)
	}
	if fake.callCount("ResetClean") != 1 {
		t.Errorf("expected 1 ResetClean, got %d", fake.callCount("ResetClean"))
	}
}

// TestStartIterationBranch_TraceNarrates: when a fork-base ref
// is missing, the trace tells the operator which step decided
// the fork ref, that fetch was attempted, and that resolve-fork-base
// was the failure point. No log archaeology to figure out
// "was it the wrong runBranch arg, or was main truly absent?"
func TestStartIterationBranch_TraceNarrates(t *testing.T) {
	wf, _ := makeWorkflow(t)
	// resolveMap is empty → ResolveRef("main") fails.
	_, err := wf.StartIterationBranch("7:1:x", 1, ForkFromRunBranch,
		"x", "", 1, "build", "main", "")
	if err == nil {
		t.Fatal("expected error when fork base missing")
	}
	var opErr *WorkflowOpError
	if !errors.As(err, &opErr) {
		t.Fatalf("expected *WorkflowOpError, got %T: %v", err, err)
	}
	if opErr.Op != "StartIterationBranch" {
		t.Errorf("Op: got %q", opErr.Op)
	}
	stepStatus := map[string]string{}
	for _, s := range opErr.Steps {
		stepStatus[s.Name] = s.Status
	}
	// pick-fork-ref should be ok (we picked "main"),
	// resolve-fork-base should be the failure point.
	if stepStatus["pick-fork-ref"] != "ok" {
		t.Errorf("pick-fork-ref should be ok, got %q", stepStatus["pick-fork-ref"])
	}
	if stepStatus["resolve-fork-base"] != "failed" {
		t.Errorf("resolve-fork-base should be failed, got %q", stepStatus["resolve-fork-base"])
	}
	if !errors.Is(err, ErrForkBaseNotFound) {
		t.Error("errors.Is(err, ErrForkBaseNotFound) should still be true")
	}
}

// TestResumeIterationBranch_TraceShowsAutoHeal: the auto-heal
// step is exactly the kind of "silent helpfulness" that needs
// to be visible. When local disagreed with origin and was
// reset, the trace records that fact for forensic recovery.
func TestResumeIterationBranch_TraceShowsAutoHeal(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/1-build/dev_a/iter-1"] = "stalelocal0000000000000000000000000000"
	fake.resolveMap["refs/remotes/origin/1-build/dev_a/iter-1"] = "originfresh000000000000000000000000000"

	_, err := wf.ResumeIterationBranch("7:1:dev_a", 1,
		"dev_a", "", 1, "build")
	if err != nil {
		t.Fatalf("ResumeIterationBranch: %v", err)
	}
	// Success — no error to inspect, but the test value of the
	// trace is in the failure case. Inject a Checkout failure to
	// surface the success-path trace via the error.
	fake2 := newFakeOps()
	fake2.resolveMap["refs/heads/1-build/dev_a/iter-1"] = "stalelocal"
	fake2.resolveMap["refs/remotes/origin/1-build/dev_a/iter-1"] = "originfresh"
	fake2.inject("Checkout", errors.New("worktree dirty"))
	wf2 := &Workflow{
		git:           fake2,
		convs:         NewProductionConventions(),
		projID:        7,
		defaultBranch: "main",
		logger:        nullLogger(),
	}
	_, err = wf2.ResumeIterationBranch("7:1:dev_a", 1,
		"dev_a", "", 1, "build")
	if err == nil {
		t.Fatal("expected checkout failure")
	}
	var opErr *WorkflowOpError
	if !errors.As(err, &opErr) {
		t.Fatalf("expected *WorkflowOpError")
	}
	stepStatus := map[string]string{}
	stepDetail := map[string]string{}
	for _, s := range opErr.Steps {
		stepStatus[s.Name] = s.Status
		stepDetail[s.Name] = s.Detail
	}
	// Auto-heal should be recorded as ok (it ran) with the SHA
	// transition in detail.
	if stepStatus["auto-heal-stale-local"] != "ok" {
		t.Errorf("auto-heal-stale-local should be ok, got %q", stepStatus["auto-heal-stale-local"])
	}
	if !strings.Contains(stepDetail["auto-heal-stale-local"], "→") {
		t.Errorf("auto-heal detail should show SHA transition, got %q", stepDetail["auto-heal-stale-local"])
	}
}

func TestStartIterationBranch_PriorIterationFork(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["1-build/dev_a/iter-1"] = "prior_iter_sha000000000000000000000000"

	branchName, err := wf.StartIterationBranch("7:1:dev_a", 2,
		ForkFromPriorIteration,
		"dev_a", "", 1, "build", "main", "")
	if err != nil {
		t.Fatalf("StartIterationBranch: %v", err)
	}
	if branchName != "1-build/dev_a/iter-2" {
		t.Errorf("branch name: got %q", branchName)
	}
	createCall := fake.lastCall("CreateBranchAt")
	if createCall.Args[1] != "prior_iter_sha000000000000000000000000" {
		t.Errorf("ForkFromPriorIteration must fork from iter-1's SHA, got %v", createCall.Args[1])
	}
}

// ensure injection helper compiles + exports the right shapes
var _ = git.ErrRefNotFound
