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
