package store

import (
	"testing"
	"time"
)

// TestSetClaimDeadline_ReanchorsAwayFromReaper is the behavioral
// pin for the ISSUE-3 / long-compute fix: the reaper kills any
// claimed-or-running task whose claim deadline has passed. The
// CLAIMED → RUNNING plan re-anchors that deadline so a task that
// is actually executing (a multi-hour assembly, a first-run
// multi-GB image pull) is no longer indistinguishable from a dead
// worker holding a stale claim-time deadline.
func TestSetClaimDeadline_ReanchorsAwayFromReaper(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-tst")
	taskID := makeTaskWithAction(t, s, runID, "1:1:t", "compute", TaskReady)

	// Claim with an ALREADY-expired deadline (simulates: claim
	// happened, then a slow pull/long compute blew the original
	// claim-time budget).
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: alice, Deadline: time.Now().Add(-time.Hour)},
	}}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	expired, err := s.GetExpiredClaims()
	if err != nil {
		t.Fatalf("GetExpiredClaims: %v", err)
	}
	if !containsTask(expired, taskID) {
		t.Fatalf("precondition: an expired-deadline claim must be reapable, got %v", expired)
	}

	// The CLAIMED → RUNNING plan ComputeStartTask produces:
	// flip to RUNNING + re-anchor the lease into the future.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: taskID, NewState: TaskRunning},
		SetClaimDeadline{TaskID: taskID, Deadline: time.Now().Add(2 * time.Hour)},
	}}); err != nil {
		t.Fatalf("start+reanchor: %v", err)
	}
	expired, err = s.GetExpiredClaims()
	if err != nil {
		t.Fatalf("GetExpiredClaims (post): %v", err)
	}
	if containsTask(expired, taskID) {
		t.Fatalf("re-anchored RUNNING task must NOT be reapable, got %v", expired)
	}
}

// TestSetClaimDeadline_NoOpWithoutOpenClaim guards the race where
// the claim closed between plan-build and apply: re-anchoring a
// task with no open claim must be a clean no-op, not an error.
func TestSetClaimDeadline_NoOpWithoutOpenClaim(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	taskID := makeTaskWithAction(t, s, runID, "1:1:t", "compute", TaskReady)

	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaimDeadline{TaskID: taskID, Deadline: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatalf("SetClaimDeadline with no open claim must be a no-op, got %v", err)
	}
}

func containsTask(claims []TaskClaimRecord, taskID string) bool {
	for _, c := range claims {
		if c.TaskID == taskID {
			return true
		}
	}
	return false
}
