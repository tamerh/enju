package enjugit

import (
	"errors"
	"testing"
)

// Tests for Workflow.SubmitBatch. Each test exercises one slice
// of the contract — happy path, mid-loop rollback, push-failure
// handling, single-branch enforcement.

func TestSubmitBatch_HappyPath_ResolvesSHAsFromTrailers(t *testing.T) {
	wf, fake := makeWorkflow(t)
	branch := "1-probe/vote/iter-1"
	// The branch needs to exist locally so prepareBranchForCommit
	// hits the fast-path (checkout-local).
	fake.resolveMap["refs/heads/"+branch] = "preheadsha000000"
	fake.headSHA = "preheadsha000000"
	// Post-push HEAD scan returns 3 commits, each carrying its
	// own Enju-Task-Complete trailer. The walk is newest-first.
	fake.recentCommits = []struct{ SHA, Message string }{
		{SHA: "shaC", Message: "Task t-c\n\nEnju-Task-Complete: t-c"},
		{SHA: "shaB", Message: "Task t-b\n\nEnju-Task-Complete: t-b"},
		{SHA: "shaA", Message: "Task t-a\n\nEnju-Task-Complete: t-a"},
	}

	reqs := []SubmitRequest{
		newBatchReq("t-a", branch),
		newBatchReq("t-b", branch),
		newBatchReq("t-c", branch),
	}
	res, err := wf.SubmitBatch(reqs)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got, want := len(res.Branches), 1; got != want {
		t.Fatalf("Branches: got %d, want 1 (single-branch happy path)", got)
	}
	if res.Branches[0].Name != branch {
		t.Errorf("Branches[0].Name: got %q, want %q", res.Branches[0].Name, branch)
	}
	if got, want := len(res.Entries), 3; got != want {
		t.Fatalf("Entries: got %d, want %d", got, want)
	}
	for i, want := range []string{"shaA", "shaB", "shaC"} {
		e := res.Entries[i]
		if !e.Attempted {
			t.Errorf("entry[%d] Attempted: got false", i)
		}
		if e.Err != nil {
			t.Errorf("entry[%d] Err: got %v", i, e.Err)
		}
		if e.CommitSHA != want {
			t.Errorf("entry[%d] CommitSHA: got %q, want %q (post-trailer-scan)", i, e.CommitSHA, want)
		}
	}
	// CommitFiles called once per entry.
	if got, want := fake.callCount("CommitFiles"), 3; got != want {
		t.Errorf("CommitFiles count: got %d, want %d", got, want)
	}
	// Push called exactly once (the coalesced push).
	if got, want := fake.callCount("PushWithVerify"), 1; got != want {
		t.Errorf("Push count: got %d, want %d (single coalesced push)", got, want)
	}
}

func TestSubmitBatch_MidLoopFailure_RollsBackToPreBatchHead(t *testing.T) {
	wf, fake := makeWorkflow(t)
	branch := "1-probe/vote/iter-1"
	fake.resolveMap["refs/heads/"+branch] = "preheadsha000000"
	fake.headSHA = "preheadsha000000"
	// Make CommitFiles fail starting on the 2nd call so entry[0]
	// commits successfully and entry[1] triggers rollback.
	fake.commitFailAfter = 1
	fake.commitFailErr = errors.New("disk full")

	reqs := []SubmitRequest{
		newBatchReq("t-a", branch),
		newBatchReq("t-b", branch),
		newBatchReq("t-c", branch),
	}
	res, err := wf.SubmitBatch(reqs)
	if err == nil {
		t.Fatal("expected error from mid-loop commit failure")
	}
	if !res.Entries[0].Attempted {
		t.Errorf("entry[0] should be Attempted (committed before fail)")
	}
	if res.Entries[0].Err == nil {
		t.Errorf("entry[0] Err should be set (committed but rolled back)")
	}
	if res.Entries[1].Err == nil {
		t.Errorf("entry[1] should carry the commit error")
	}
	if res.Entries[2].Attempted {
		t.Errorf("entry[2] should NOT be Attempted (rolled back without trying)")
	}
	// SetBranchTo called for rollback.
	if got := fake.callCount("SetBranchTo"); got != 1 {
		t.Errorf("SetBranchTo (rollback): got %d, want 1", got)
	}
	// Trace mentions rollback step for the touched branch.
	var opErr *WorkflowOpError
	if !errors.As(err, &opErr) {
		t.Fatal("expected *WorkflowOpError")
	}
	stepStatuses := map[string]string{}
	for _, s := range opErr.Steps {
		stepStatuses[s.Name] = s.Status
	}
	if stepStatuses["rollback:"+branch] != "ok" {
		t.Errorf("rollback:%s step status: got %q, want ok", branch, stepStatuses["rollback:"+branch])
	}
}

func TestSubmitBatch_PushFailure_FlagsAllEntries(t *testing.T) {
	wf, fake := makeWorkflow(t)
	branch := "1-probe/vote/iter-1"
	fake.resolveMap["refs/heads/"+branch] = "preheadsha000000"
	fake.headSHA = "preheadsha000000"
	pushErr := errors.New("ssh: connection refused")
	fake.inject("PushWithVerify", pushErr)

	reqs := []SubmitRequest{
		newBatchReq("t-a", branch),
		newBatchReq("t-b", branch),
	}
	res, err := wf.SubmitBatch(reqs)
	if err == nil {
		t.Fatal("expected push failure")
	}
	for i, e := range res.Entries {
		if e.Err == nil {
			t.Errorf("entry[%d] Err should be set on push failure", i)
		}
		if !e.Attempted {
			t.Errorf("entry[%d] Attempted should be true (commits succeeded)", i)
		}
	}
	// No rollback — local commits stay for operator inspection.
	if got := fake.callCount("SetBranchTo"); got != 0 {
		t.Errorf("SetBranchTo: got %d, want 0 (no rollback on push fail)", got)
	}
}

func TestSubmitBatch_MultiBranch_GroupsAndPushesEach(t *testing.T) {
	wf, fake := makeWorkflow(t)
	branchA := "1-r/task_a/iter-1"
	branchB := "1-r/task_b/iter-1"
	fake.resolveMap["refs/heads/"+branchA] = "preheadA00000000"
	fake.resolveMap["refs/heads/"+branchB] = "preheadB00000000"
	fake.headSHA = "preheadA00000000"
	// Trailer scan after each branch's push must find that
	// branch's task ids. We swap the recentCommits per group
	// since the fake returns the same list to every walk; for
	// this test we leave a superset and let the wantSet filter.
	fake.recentCommits = []struct{ SHA, Message string }{
		{SHA: "shaA1", Message: "Task t-a1\n\nEnju-Task-Complete: t-a1"},
		{SHA: "shaA2", Message: "Task t-a2\n\nEnju-Task-Complete: t-a2"},
		{SHA: "shaB1", Message: "Task t-b1\n\nEnju-Task-Complete: t-b1"},
		{SHA: "shaB2", Message: "Task t-b2\n\nEnju-Task-Complete: t-b2"},
	}

	// Interleave entries across branches to confirm grouping
	// preserves per-branch order independent of input order.
	reqs := []SubmitRequest{
		newBatchReq("t-a1", branchA),
		newBatchReq("t-b1", branchB),
		newBatchReq("t-a2", branchA),
		newBatchReq("t-b2", branchB),
	}
	res, err := wf.SubmitBatch(reqs)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got, want := len(res.Branches), 2; got != want {
		t.Fatalf("Branches: got %d, want %d", got, want)
	}
	// Branch order matches first-seen-in-input order: branchA
	// first (reqs[0]), branchB second (reqs[1]).
	if res.Branches[0].Name != branchA {
		t.Errorf("Branches[0]: got %q, want %q", res.Branches[0].Name, branchA)
	}
	if res.Branches[1].Name != branchB {
		t.Errorf("Branches[1]: got %q, want %q", res.Branches[1].Name, branchB)
	}
	// Two pushes — one per branch.
	if got := fake.callCount("PushWithVerify"); got != 2 {
		t.Errorf("Push count: got %d, want 2 (one per branch)", got)
	}
	// All four commits made it.
	for i, e := range res.Entries {
		if e.Err != nil {
			t.Errorf("entry[%d] Err: got %v", i, e.Err)
		}
		if !e.Attempted {
			t.Errorf("entry[%d] Attempted: got false", i)
		}
		if e.CommitSHA == "" {
			t.Errorf("entry[%d] CommitSHA: empty (trailer scan didn't find it)", i)
		}
	}
}

func TestSubmitBatch_EmptyReqs_Errors(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.SubmitBatch(nil)
	if err == nil {
		t.Fatal("expected error for empty reqs")
	}
}

func TestSubmitBatch_TraceNarratesAllSteps(t *testing.T) {
	wf, fake := makeWorkflow(t)
	branch := "1-probe/vote/iter-1"
	fake.resolveMap["refs/heads/"+branch] = "preheadsha000000"
	fake.headSHA = "preheadsha000000"
	fake.recentCommits = []struct{ SHA, Message string }{
		{SHA: "shaA", Message: "Task t-a\n\nEnju-Task-Complete: t-a"},
	}

	reqs := []SubmitRequest{newBatchReq("t-a", branch)}
	if _, err := wf.SubmitBatch(reqs); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	// On success there's no error to read steps from. The proof
	// the trace ran is the call graph: prepare-branch (Fetch +
	// Checkout), Head, CommitFiles, Push, Head, WalkRecentCommits.
	for _, expected := range []string{"Fetch", "Checkout", "Head", "CommitFiles", "PushWithVerify", "WalkRecentCommits"} {
		if fake.callCount(expected) == 0 {
			t.Errorf("expected %s to be called at least once", expected)
		}
	}
}

// newBatchReq builds a minimal SubmitRequest with a single file
// for batch tests. BranchOverride lets tests pin entries to a
// specific branch without exercising Conventions.BranchName.
func newBatchReq(taskID, branch string) SubmitRequest {
	return SubmitRequest{
		TaskID:         taskID,
		BranchOverride: branch,
		Files: []FileWrite{
			{RepoRelPath: "out/" + taskID + ".txt", Content: []byte(taskID)},
		},
		Citizen: Identity{Name: "tester", Email: "t@example.com"},
	}
}
