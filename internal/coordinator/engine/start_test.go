package engine

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestComputeStartTask_AcceptsClaimed pins the only legal
// from-state for the CLAIMED → RUNNING transition. The plan
// must carry exactly one SetTaskState mutation targeting
// TaskRunning so apply.go's emission branch fires.
func TestComputeStartTask_AcceptsClaimed(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskClaimed, Citizens: 1},
		},
	}
	plan, err := New(ms, nil).ComputeStartTask("t1")
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if len(plan.Mutations) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(plan.Mutations))
	}
	m, ok := plan.Mutations[0].(store.SetTaskState)
	if !ok {
		t.Fatalf("expected SetTaskState, got %T", plan.Mutations[0])
	}
	if m.TaskID != "t1" || m.NewState != store.TaskRunning {
		t.Errorf("wrong mutation: %+v", m)
	}
	if m.ClearClaim {
		t.Error("ClearClaim must be false for start transition (claim stays open)")
	}
}

// TestComputeStartTask_RejectsNonClaimed walks every other
// state and asserts it's rejected. Without this guard the
// fat-client could double-flip on retries OR a bot could mark
// something running before claiming it (the auto-register-then-
// start pattern that bypasses the claim is explicitly out of
// scope for 8.2).
func TestComputeStartTask_RejectsNonClaimed(t *testing.T) {
	disallowed := []store.TaskState{
		store.TaskPending,
		store.TaskReady,
		store.TaskCollecting,
		store.TaskRunning, // already running — no double-start
		store.TaskSubmitted,
		store.TaskAccepted,
		store.TaskFailed,
		store.TaskSkipped,
		store.TaskParked,
	}
	for _, s := range disallowed {
		t.Run(string(s), func(t *testing.T) {
			ms := &mockStore{
				tasks: map[string]*store.TaskRecord{
					"t1": {ID: "t1", State: s, Citizens: 1},
				},
			}
			_, err := New(ms, nil).ComputeStartTask("t1")
			if err == nil {
				t.Fatalf("expected rejection for state=%s, got nil", s)
			}
			if !strings.Contains(err.Error(), "must be claimed") {
				t.Errorf("error should name the required from-state; got: %v", err)
			}
		})
	}
}

// TestComputeStartTask_RejectsMissingTask makes sure a stale
// taskID (e.g. cleaned up between claim and POST) surfaces a
// not-found error rather than the fat-client retrying forever.
func TestComputeStartTask_RejectsMissingTask(t *testing.T) {
	ms := &mockStore{tasks: map[string]*store.TaskRecord{}}
	_, err := New(ms, nil).ComputeStartTask("ghost")
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("wrong error: %v", err)
	}
}
