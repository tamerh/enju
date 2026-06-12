package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestComputeHeartbeatTask_AcceptsClaimedAndRunning pins the two
// legal from-states for a lease renewal — the heartbeat loop wraps
// the whole execution window, so it must work both before and
// after the /started transition — and that the plan is EXACTLY one
// SetClaimDeadline (no state flip: a heartbeat observes, it doesn't
// advance the lifecycle).
func TestComputeHeartbeatTask_AcceptsClaimedAndRunning(t *testing.T) {
	for _, s := range []store.TaskState{store.TaskClaimed, store.TaskRunning} {
		t.Run(string(s), func(t *testing.T) {
			ms := &mockStore{
				tasks: map[string]*store.TaskRecord{
					"t1": {ID: "t1", State: s, Citizens: 1},
				},
			}
			dl := time.Now().Add(2 * time.Hour)
			plan, err := New(ms, nil).ComputeHeartbeatTask("t1", dl)
			if err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if len(plan.Mutations) != 1 {
				t.Fatalf("expected 1 mutation (SetClaimDeadline), got %d", len(plan.Mutations))
			}
			scd, ok := plan.Mutations[0].(store.SetClaimDeadline)
			if !ok {
				t.Fatalf("mutation[0]: expected SetClaimDeadline, got %T", plan.Mutations[0])
			}
			if scd.TaskID != "t1" || !scd.Deadline.Equal(dl) {
				t.Errorf("re-anchor mutation wrong: got %+v want TaskID=t1 Deadline=%v", scd, dl)
			}
		})
	}
}

// TestComputeHeartbeatTask_RejectsOtherStates walks every state
// outside {claimed, running} and asserts the renewal is refused.
// A reaped/released/resolved task has no open claim — refusing
// (instead of silently no-opping) is what lets the fat-client log
// that its lease was lost.
func TestComputeHeartbeatTask_RejectsOtherStates(t *testing.T) {
	disallowed := []store.TaskState{
		store.TaskPending,
		store.TaskReady, // reaped mid-script — the desync trigger
		store.TaskCollecting,
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
			_, err := New(ms, nil).ComputeHeartbeatTask("t1", time.Now())
			if err == nil {
				t.Fatalf("expected rejection for state=%s, got nil", s)
			}
			if !strings.Contains(err.Error(), "must be claimed or running") {
				t.Errorf("error should name the required from-states; got: %v", err)
			}
		})
	}
}

// TestComputeHeartbeatTask_RejectsMissingTask: a stale taskID
// surfaces not-found rather than a silent renewal of nothing.
func TestComputeHeartbeatTask_RejectsMissingTask(t *testing.T) {
	ms := &mockStore{tasks: map[string]*store.TaskRecord{}}
	_, err := New(ms, nil).ComputeHeartbeatTask("ghost", time.Now())
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("wrong error: %v", err)
	}
}
