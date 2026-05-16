package store

import "testing"

func crRunState(t *testing.T, s *Store, runID int64) RunState {
	t.Helper()
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{CompleteRun{RunID: runID}}}); err != nil {
		t.Fatalf("ApplyPlan(CompleteRun): %v", err)
	}
	r, err := s.GetRun(runID)
	if err != nil || r == nil {
		t.Fatalf("GetRun: %v", err)
	}
	return RunState(r.State)
}

// TestApplyCompleteRun_FailedRetryableKeepsRunAlive pins the
// load-bearing Slice-2 classification: a failed_retryable task is
// a live blocker, so the run lands on WAITING — never Completed.
// The leaf case (failed_retryable with no pending descendants) is
// the one that would silently regress: without failed_retryable
// in the holding bucket it counts as neither active nor holding
// and falls through to RunCompleted, wrongly settling a run that
// actually needs a retry. The terminal-`failed` contrast proves
// the new state is genuinely distinct from "stop, dead".
func TestApplyCompleteRun_FailedRetryableKeepsRunAlive(t *testing.T) {
	// A — failed_retryable + a pending descendant.
	s := newTestStore(t)
	runID := createTestRun(t, s)
	makeTaskWithAction(t, s, runID, "t1", "compute", TaskFailedRetryable)
	makeTaskWithAction(t, s, runID, "t2", "compute", TaskPending)
	if got := crRunState(t, s, runID); got != RunWaiting {
		t.Errorf("failed_retryable + pending descendant: run = %q, want waiting", got)
	}

	// B — LEAF: failed_retryable is the only non-terminal task.
	// This is the regression the holding-bucket fix addresses.
	s2 := newTestStore(t)
	r2 := createTestRun(t, s2)
	makeTaskWithAction(t, s2, r2, "done", "compute", TaskAccepted)
	makeTaskWithAction(t, s2, r2, "leaf", "compute", TaskFailedRetryable)
	if got := crRunState(t, s2, r2); got != RunWaiting {
		t.Errorf("leaf failed_retryable: run = %q, want waiting (holding-bucket fix)", got)
	}

	// C — contrast: a terminal `failed` leaf settles the run
	// (unchanged behavior). Proves failed_retryable ≠ failed.
	s3 := newTestStore(t)
	r3 := createTestRun(t, s3)
	makeTaskWithAction(t, s3, r3, "done", "compute", TaskAccepted)
	makeTaskWithAction(t, s3, r3, "deadleaf", "compute", TaskFailed)
	if got := crRunState(t, s3, r3); got != RunCompleted {
		t.Errorf("terminal failed leaf: run = %q, want completed (contrast — must differ from failed_retryable)", got)
	}
}

// TestApplySetTaskState_RetryReopensFailedRetryable pins the FSM
// gate Slice 3 opened: the ClearClaim→READY precondition must now
// admit failed_retryable (the enju_retry_task transition: a
// compute task that errored on its own merits is sent back for a
// fresh attempt). Before Slice 3 the gate admitted only
// accepted/submitted/failed, so a retry was rejected with "cannot
// be invalidated". The contrast case proves the gate widened by
// exactly one state — an ineligible state (pending) is still
// rejected, so this is not a wholesale loosening.
func TestApplySetTaskState_RetryReopensFailedRetryable(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	taskID := makeTaskWithAction(t, s, runID, "1:1:t", "compute", TaskFailedRetryable)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: taskID, NewState: TaskReady, ClearClaim: true},
	}}); err != nil {
		t.Fatalf("failed_retryable→ready (retry) must be allowed: %v", err)
	}
	if tk, _ := s.GetTask(taskID); tk == nil || tk.State != TaskReady {
		t.Fatalf("after retry transition, state = %v, want ready", tk)
	}

	// Contrast: pending→ready via ClearClaim is still rejected.
	s2 := newTestStore(t)
	r2 := createTestRun(t, s2)
	pid := makeTaskWithAction(t, s2, r2, "1:1:p", "compute", TaskPending)
	if _, err := s2.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: pid, NewState: TaskReady, ClearClaim: true},
	}}); err == nil {
		t.Fatal("pending→ready via ClearClaim must still be rejected — Slice 3 widened the gate by one state, not opened it")
	}
}

// TestApplySetTaskState_ClearClaimPreservesFailReason pins the
// load-bearing Slice-4 fix. performComputeFailure parks a task in
// failed_retryable via the ClearClaim path (it must drop the claim
// pointer) WITH a reason. That UPDATE used to hardcode
// fail_reason=” and silently discard the reason, so every
// failed_retryable task showed an empty fail_reason and the
// operator flew blind. The fix must (1) persist a non-empty
// FailReason through ClearClaim, while (2) still CLEARING it for
// re-ready callers (request_changes / unfail / cascade) that pass
// no reason — those depend on the wipe.
func TestApplySetTaskState_ClearClaimPreservesFailReason(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)

	// (1) carries a reason → persisted through ClearClaim.
	t1 := makeTaskWithAction(t, s, runID, "1:1:fail", "compute", TaskRunning)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: t1, NewState: TaskFailedRetryable, FailReason: "boom: exit 7", ClearClaim: true},
	}}); err != nil {
		t.Fatalf("park ApplyPlan: %v", err)
	}
	if tk, _ := s.GetTask(t1); tk == nil || tk.FailReason != "boom: exit 7" {
		t.Fatalf("fail_reason dropped by ClearClaim — operator would fly blind: %+v", tk)
	}

	// (2) re-ready with no reason → stale fail_reason cleared.
	t2 := makeTaskWithAction(t, s, runID, "1:1:rr", "compute", TaskRunning)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: t2, NewState: TaskFailedRetryable, FailReason: "stale", ClearClaim: true},
	}}); err != nil {
		t.Fatalf("seed ApplyPlan: %v", err)
	}
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: t2, NewState: TaskReady, ClearClaim: true},
	}}); err != nil {
		t.Fatalf("re-ready ApplyPlan: %v", err)
	}
	if tk, _ := s.GetTask(t2); tk == nil || tk.FailReason != "" {
		t.Fatalf("re-ready must clear stale fail_reason (request_changes/unfail rely on this), got %q", tk.FailReason)
	}
}
