package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestComputeStartTask_AcceptsClaimed pins the only legal
// from-state for CLAIMED → RUNNING, and that the plan both flips
// to RUNNING (claim stays open) AND re-anchors the claim lease to
// the supplied deadline — the half that stops the reaper from
// killing long legitimate work on a stale claim-time deadline.
func TestComputeStartTask_AcceptsClaimed(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskClaimed, Citizens: 1},
		},
	}
	dl := time.Now().Add(2 * time.Hour)
	plan, err := New(ms, nil).ComputeStartTask("t1", dl)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if len(plan.Mutations) != 2 {
		t.Fatalf("expected 2 mutations (SetTaskState + SetClaimDeadline), got %d", len(plan.Mutations))
	}
	st, ok := plan.Mutations[0].(store.SetTaskState)
	if !ok {
		t.Fatalf("mutation[0]: expected SetTaskState, got %T", plan.Mutations[0])
	}
	if st.TaskID != "t1" || st.NewState != store.TaskRunning {
		t.Errorf("wrong state mutation: %+v", st)
	}
	if st.ClearClaim {
		t.Error("ClearClaim must be false for start transition (claim stays open)")
	}
	scd, ok := plan.Mutations[1].(store.SetClaimDeadline)
	if !ok {
		t.Fatalf("mutation[1]: expected SetClaimDeadline, got %T", plan.Mutations[1])
	}
	if scd.TaskID != "t1" || !scd.Deadline.Equal(dl) {
		t.Errorf("re-anchor mutation wrong: got %+v want TaskID=t1 Deadline=%v", scd, dl)
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
			_, err := New(ms, nil).ComputeStartTask("t1", time.Now())
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
	_, err := New(ms, nil).ComputeStartTask("ghost", time.Now())
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("wrong error: %v", err)
	}
}
