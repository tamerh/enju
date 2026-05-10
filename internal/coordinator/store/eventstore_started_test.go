package store

import (
	"testing"
	"time"
)

// TestEventEmission_TaskStartedFires verifies CLAIMED → RUNNING
// produces a task_started event with prior_state=claimed and
// the task's action as subtype. Phase 8.2 observability hook;
// the fat-client posts /tasks/:id/started right before the
// script / LLM call kicks off, and downstream consumers
// (run_status, future "stuck claiming vs actually running"
// dashboards) need this event to bracket the work-execution
// window with task_completed.
func TestEventEmission_TaskStartedFires(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-tst")
	taskID := makeTaskWithAction(t, s, runID, "1:1:t", "compute", TaskReady)

	// CLAIMED is the only legal entry state; SetClaim flips
	// READY → CLAIMED via task_claims insertion + tasks.state
	// update.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: alice, Deadline: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: taskID, NewState: TaskRunning},
	}}); err != nil {
		t.Fatalf("set running: %v", err)
	}

	got := hasEventWithMetadata(t, s, runID, "task_started", `"prior_state":"claimed"`)
	if got == nil {
		t.Fatal("no task_started event with prior_state=claimed found")
	}
	if got.Subtype != "compute" {
		t.Errorf("subtype = %q, want compute (event-subtype carries task action)", got.Subtype)
	}
	if got.TaskID != taskID {
		t.Errorf("TaskID = %q, want %q", got.TaskID, taskID)
	}
}

// TestEventEmission_TaskStartedSkippedOnDoubleFlip verifies
// that re-applying SetTaskState{NewState=Running} when the
// task is ALREADY in RUNNING does not emit a duplicate
// task_started. The from-state guard inside applySetTaskState
// (prior_state must be CLAIMED) is what protects against the
// fat-client retry loop double-emitting on resume.
func TestEventEmission_TaskStartedSkippedOnDoubleFlip(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-dbl")
	taskID := makeTaskWithAction(t, s, runID, "1:1:t", "compute", TaskReady)

	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: alice, Deadline: time.Now().Add(time.Hour)},
		SetTaskState{TaskID: taskID, NewState: TaskRunning},
	}}); err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Second flip — already in RUNNING. The state column
	// re-writes (no-op) but the emission branch's guard
	// (currentState == TaskClaimed) is false, so no second
	// event lands.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: taskID, NewState: TaskRunning},
	}}); err != nil {
		t.Fatalf("second start: %v", err)
	}

	waitForEventsDrained(t, s)
	events, err := s.ListEvents(EventQuery{
		RunID:      runID,
		EventTypes: []string{"task_started"},
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	count := 0
	for _, e := range events {
		if e.TaskID == taskID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 task_started event for double-flip, got %d", count)
	}
}
