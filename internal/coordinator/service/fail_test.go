package service

import (
	"errors"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestFailTaskOwnershipOK pins the load-bearing ownership
// invariant for FailTask as a standalone truth table. The
// bot-side daemon tests assert the bot doesn't *call* FailTask
// on the claim path; this asserts the coord *rejects* a
// non-claimant bot that does — the durable, path-independent
// guarantee. A regression in the guard (auth-check reorder, a
// "simplified" Kind condition during an unrelated FailTask
// refactor) passes every bot-side test but fails here.
// TestFailedRetryableIsNotTerminal pins the load-bearing Slice-1
// invariant: failed_retryable must NOT classify as terminal.
// isTerminalTaskState gates whether the fail cascade may (re)touch
// a descendant; treating failed_retryable as terminal would make
// it un-retryable and let a run with one wrongly count as
// settled. The terminal trio (accepted/failed/skipped) is
// asserted alongside so a future edit that "tidies" the set is
// caught.
func TestFailedRetryableIsNotTerminal(t *testing.T) {
	if isTerminalTaskState(store.TaskFailedRetryable) {
		t.Fatal("failed_retryable must be NON-terminal (live blocker, retryable)")
	}
	for _, s := range []store.TaskState{store.TaskAccepted, store.TaskFailed, store.TaskSkipped} {
		if !isTerminalTaskState(s) {
			t.Errorf("%s must remain terminal", s)
		}
	}
	for _, s := range []store.TaskState{
		store.TaskPending, store.TaskReady, store.TaskClaimed,
		store.TaskRunning, store.TaskCollecting, store.TaskParked,
	} {
		if isTerminalTaskState(s) {
			t.Errorf("%s must remain non-terminal", s)
		}
	}
}

func TestFailTaskOwnershipOK(t *testing.T) {
	const botID, otherID = int64(7), int64(99)

	cases := []struct {
		name    string
		caller  *store.CitizenRecord
		task    *store.TaskRecord
		wantErr bool
	}{
		{
			name:    "bot is the claimant → allowed (process+submit budget path)",
			caller:  &store.CitizenRecord{ID: botID, Kind: store.CitizenKindBot, Username: "dev-bot"},
			task:    &store.TaskRecord{ID: "p:1:t", ClaimedBy: botID},
			wantErr: false,
		},
		{
			name:    "bot is NOT the claimant → ErrForbidden (claim-path cascade blocked)",
			caller:  &store.CitizenRecord{ID: botID, Kind: store.CitizenKindBot, Username: "lint-bot"},
			task:    &store.TaskRecord{ID: "p:1:t", ClaimedBy: otherID},
			wantErr: true,
		},
		{
			name:    "bot, task unclaimed (the real lint-bot scenario) → ErrForbidden",
			caller:  &store.CitizenRecord{ID: botID, Kind: store.CitizenKindBot, Username: "lint-bot"},
			task:    &store.TaskRecord{ID: "p:1:t", ClaimedBy: 0},
			wantErr: true,
		},
		{
			name:    "human non-claimant → allowed (operator-override preserved)",
			caller:  &store.CitizenRecord{ID: otherID, Kind: store.CitizenKindHuman, Username: "tamer"},
			task:    &store.TaskRecord{ID: "p:1:t", ClaimedBy: botID},
			wantErr: false,
		},
		{
			name:    "human, task unclaimed → allowed (kill a wedged task they don't own)",
			caller:  &store.CitizenRecord{ID: otherID, Kind: store.CitizenKindHuman, Username: "tamer"},
			task:    &store.TaskRecord{ID: "p:1:t", ClaimedBy: 0},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := failTaskOwnershipOK(tc.caller, tc.task)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got nil")
				}
				if !errors.Is(err, ErrForbidden) {
					t.Errorf("error should wrap ErrForbidden, got %v", err)
				}
			} else if err != nil {
				t.Errorf("expected allowed, got %v", err)
			}
		})
	}
}
