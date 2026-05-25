package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// autoRetryFixture builds project + active run + human citizen + a
// claimable compute task with the given retries budget, and returns
// (coord, taskID, caller, claim helper). The claim helper re-claims
// the task (READY → CLAIMED, advancing iter_seq) so each call models a
// fresh attempt.
func autoRetryFixture(t *testing.T, retries int) (*Coordinator, *store.Store, string, *store.CitizenRecord, func()) {
	t.Helper()
	st, coord := newCVFStore(t)
	now := time.Now()

	res, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateProject{Project: store.ProjectRecord{Name: "p", CreatedAt: now, UpdatedAt: now}},
	}})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID := res.ProjectID

	runYAML := "name: r\ntasks:\n  - id: c\n    action: compute\n    script: run.sh\n"
	res, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateRun{Run: store.RunRecord{
			ProjectID: projectID, Name: "r", YAMLData: runYAML,
			State: store.RunActive, CreatedAt: now, UpdatedAt: now,
		}},
	}})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID, runSeq := res.RunID, res.RunSeq

	res, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateCitizen{Citizen: store.CitizenRecord{
			Username: "alice", Name: "Alice", Email: "a@t.local",
			Kind: store.CitizenKindHuman, RegisteredAt: now, LastSeen: now,
		}, Token: "tok-alice"},
	}})
	if err != nil {
		t.Fatalf("create citizen: %v", err)
	}
	citizenID := res.CitizenID

	tid := fmt.Sprintf("%d:%d:c", projectID, runSeq)
	if _, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateTask{Task: store.TaskRecord{
			ID: tid, RunID: runID, Seq: 1, TaskDefID: "c",
			Action: "compute", Script: "run.sh", ResultType: "text",
			State: store.TaskReady, Citizens: 1, Retries: retries, CreatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	caller, err := st.GetCitizen(citizenID)
	if err != nil || caller == nil {
		t.Fatalf("get citizen: %v", err)
	}
	claim := func() {
		if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
			store.SetClaim{TaskID: tid, CitizenID: citizenID, Deadline: time.Now().Add(30 * time.Minute)},
		}}); err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	return coord, st, tid, caller, claim
}

// retries: 2 → attempts 1 and 2 (iter_seq 1,2) auto-retry to READY;
// attempt 3 (iter_seq 3) exhausts the budget and parks failed_retryable.
func TestFailComputeTaskRetryable_AutoRetryBudget(t *testing.T) {
	coord, st, tid, caller, claim := autoRetryFixture(t, 2)

	for attempt := 1; attempt <= 2; attempt++ {
		claim() // READY → CLAIMED, iter_seq = attempt
		resp, err := coord.FailComputeTaskRetryable(caller, tid, "error_max_turns")
		if err != nil {
			t.Fatalf("attempt %d: FailComputeTaskRetryable: %v", attempt, err)
		}
		if resp.Status != "retrying" {
			t.Fatalf("attempt %d: status = %q, want retrying", attempt, resp.Status)
		}
		mustState(t, st, tid, store.TaskReady)
	}

	// Attempt 3: iter_seq 3 > retries 2 → park.
	claim()
	resp, err := coord.FailComputeTaskRetryable(caller, tid, "error_max_turns")
	if err != nil {
		t.Fatalf("attempt 3: %v", err)
	}
	if resp.Status != "failed_retryable" {
		t.Fatalf("attempt 3: status = %q, want failed_retryable (budget exhausted)", resp.Status)
	}
	mustState(t, st, tid, store.TaskFailedRetryable)
}

// retries: 0 (the default) → the first failure parks failed_retryable
// immediately, with no auto-retry (current behavior preserved).
func TestFailComputeTaskRetryable_ZeroRetriesParksImmediately(t *testing.T) {
	coord, st, tid, caller, claim := autoRetryFixture(t, 0)
	claim()
	resp, err := coord.FailComputeTaskRetryable(caller, tid, "boom")
	if err != nil {
		t.Fatalf("FailComputeTaskRetryable: %v", err)
	}
	if resp.Status != "failed_retryable" {
		t.Errorf("retries:0 status = %q, want failed_retryable", resp.Status)
	}
	mustState(t, st, tid, store.TaskFailedRetryable)
}
