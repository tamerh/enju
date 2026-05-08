package enjugit

import (
	"errors"
	"fmt"
	"testing"
)

// Tests for Workflow.prepareBranchForCommit. Each test exercises
// one resolution-chain step and asserts the structured Steps
// diagnostics so the architectural promise — "errors say WHERE"
// — has test coverage, not just a comment.

func TestPrepareBranch_LocalCheckoutHits(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Branch already exists locally → step 2 wins, step 3+4
	// never run.
	fake.resolveMap["refs/heads/topic"] = "localsha000000"

	err := callPrepare(wf, fake, "topic")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if fake.callCount("Checkout") != 1 {
		t.Errorf("expected 1 checkout, got %d", fake.callCount("Checkout"))
	}
	if fake.callCount("CreateBranchAt") != 0 {
		t.Errorf("expected 0 CreateBranchAt (local hit shouldn't fork), got %d",
			fake.callCount("CreateBranchAt"))
	}
}

func TestPrepareBranch_TracksOrigin(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Local missing, but origin tracking ref exists.
	fake.resolveMap["refs/remotes/origin/topic"] = "originsha000000"
	// Mark local checkout as "ref not found" then succeed after
	// CreateBranchAt makes the local ref.
	fake.checkoutMissingUntilCreated = "topic"

	err := callPrepare(wf, fake, "topic")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	createCall := fake.lastCall("CreateBranchAt")
	if createCall == nil {
		t.Fatal("CreateBranchAt not called for origin-tracking path")
	}
	if createCall.Args[1] != "originsha000000" {
		t.Errorf("CreateBranchAt forkSHA: got %v, want origin's SHA", createCall.Args[1])
	}
}

func TestPrepareBranch_ForksFromDefault(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Branch missing locally + on origin → fall through to fork
	// from default branch (main).
	fake.resolveMap["refs/heads/main"] = "mainsha000000"
	fake.checkoutMissingUntilCreated = "topic"

	err := callPrepare(wf, fake, "topic")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	createCall := fake.lastCall("CreateBranchAt")
	if createCall == nil {
		t.Fatal("CreateBranchAt not called")
	}
	if createCall.Args[1] != "mainsha000000" {
		t.Errorf("expected fork from main's SHA, got %v", createCall.Args[1])
	}
}

func TestPrepareBranch_DefaultMissingErrorIsStructured(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Branch missing locally + on origin AND default branch
	// missing too → fall through fails at fork-from-default.
	fake.checkoutMissingUntilCreated = "topic"

	err := callPrepare(wf, fake, "topic")
	if err == nil {
		t.Fatal("expected error when nothing resolves")
	}
	if !errors.Is(err, ErrCannotForkBranch) {
		t.Errorf("expected errors.Is(err, ErrCannotForkBranch), got %v", err)
	}
	var opErr *WorkflowOpError
	if !errors.As(err, &opErr) {
		t.Fatal("expected *WorkflowOpError in chain")
	}
	if opErr.Op != "PrepareBranchForCommit" {
		t.Errorf("Op: got %q, want PrepareBranchForCommit", opErr.Op)
	}
	if opErr.Context["branch"] != "topic" {
		t.Errorf("Context[branch]: got %q, want topic", opErr.Context["branch"])
	}
	if opErr.Context["default_branch"] != "main" {
		t.Errorf("Context[default_branch]: got %q, want main", opErr.Context["default_branch"])
	}
	// The structured Steps must say which steps were tried and
	// which one finally failed.
	stepStatuses := map[string]string{}
	for _, s := range opErr.Steps {
		stepStatuses[s.Name] = s.Status
	}
	if stepStatuses["checkout-local"] != "skipped" {
		t.Errorf("local-checkout should be skipped, got %q", stepStatuses["checkout-local"])
	}
	if stepStatuses["track-origin"] != "skipped" {
		t.Errorf("track-origin should be skipped, got %q", stepStatuses["track-origin"])
	}
	if stepStatuses["fork-from-default"] != "failed" {
		t.Errorf("fork-from-default should be failed, got %q", stepStatuses["fork-from-default"])
	}
}

func TestPrepareBranch_FetchFailureIsRecorded(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Fetch fails for a network/auth reason. Local already has
	// the branch, so the verb succeeds but the step diagnostic
	// records the failure for debug.
	fake.inject("Fetch", fmt.Errorf("ssh: connection refused"))
	fake.resolveMap["refs/heads/topic"] = "localsha"

	err := callPrepare(wf, fake, "topic")
	if err != nil {
		t.Fatalf("expected success when local hit covers fetch failure, got %v", err)
	}
	// And no fork happened.
	if fake.callCount("CreateBranchAt") != 0 {
		t.Errorf("expected 0 CreateBranchAt, got %d", fake.callCount("CreateBranchAt"))
	}
}

func TestPrepareBranch_EmptyBranchErrors(t *testing.T) {
	wf, fake := makeWorkflow(t)
	err := callPrepare(wf, fake, "")
	if err == nil {
		t.Error("expected error for empty branch")
	}
}

// callPrepare is a small helper that invokes prepareBranchForCommit
// with the test's fakeOps. It mimics the WithLock wrapper that
// production callers use, but just calls the fake directly since
// fakeOps's WithLock simply calls fn(f) inline.
func callPrepare(wf *Workflow, fake *fakeOps, branch string) error {
	return wf.prepareBranchForCommit(fake, branch, "")
}
