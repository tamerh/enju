package service

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/dagcache"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

func newCVFStore(t *testing.T) (*store.Store, *Coordinator) {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return st, NewCoordinator(st, dagcache.New(st), logger)
}

type cvfFixtureOpts struct {
	citizens int
	cap      int // per-task VerifyRetryCap (0 = coordinator default)
}

type cvfFixture struct {
	projectID int64
	runID     int64
	runSeq    int
	taskID    string
	citizenID int64
	caller    *store.CitizenRecord
}

// newCVFFixture builds project + active run + human citizen + a
// single-citizen READY answer task, then claims it (one open claim,
// iter_seq=1). The citizen is a HUMAN so failTaskOwnershipOK does
// not gate it (the claimant-ownership rule only restricts agents).
func newCVFFixture(t *testing.T, st *store.Store, opts cvfFixtureOpts) cvfFixture {
	t.Helper()
	now := time.Now()

	res, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateProject{Project: store.ProjectRecord{Name: "p", CreatedAt: now, UpdatedAt: now}},
	}})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID := res.ProjectID

	res, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateRun{Run: store.RunRecord{
			ProjectID: projectID, Name: "r", YAMLData: "name: r",
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

	citizens := opts.citizens
	if citizens == 0 {
		citizens = 1
	}
	tid := fmt.Sprintf("%d:%d:task-a", projectID, runSeq)
	if _, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateTask{Task: store.TaskRecord{
			ID: tid, RunID: runID, Seq: 1, TaskDefID: "task-a",
			Action: "answer", ResultType: "text",
			State: store.TaskReady, Citizens: citizens,
			VerifyRetryCap: opts.cap, CreatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.SetClaim{TaskID: tid, CitizenID: citizenID, Deadline: now.Add(30 * time.Minute)},
	}}); err != nil {
		t.Fatalf("claim task: %v", err)
	}
	c, err := st.GetCitizen(citizenID)
	if err != nil || c == nil {
		t.Fatalf("get citizen: %v", err)
	}
	return cvfFixture{projectID, runID, runSeq, tid, citizenID, c}
}

// reclaim simulates the daemon's real between-attempts cycle: the
// "counted" verdict releases the claim and the task is re-claimed
// for the next attempt. ExpireClaim closes the open claim
// (outcome=timed_out, terminal) and flips the task READY WITHOUT
// touching verify_fail_count — only delivery/retry/invalidate reset
// it. SetClaim then re-claims, and applySetClaim advances iter_seq
// (MAX(iter_seq WHERE outcome NOT NULL)+1). This is what makes the
// counter measure CONSECUTIVE *iterations*, charged once each.
func reclaim(t *testing.T, st *store.Store, f cvfFixture) {
	t.Helper()
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.ExpireClaim{TaskID: f.taskID, CitizenID: f.citizenID},
	}}); err != nil {
		t.Fatalf("reclaim/expire: %v", err)
	}
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.SetClaim{TaskID: f.taskID, CitizenID: f.citizenID, Deadline: time.Now().Add(30 * time.Minute)},
	}}); err != nil {
		t.Fatalf("reclaim/setclaim: %v", err)
	}
}

func mustState(t *testing.T, st *store.Store, taskID string, want store.TaskState) *store.TaskRecord {
	t.Helper()
	task, err := st.GetTask(taskID)
	if err != nil || task == nil {
		t.Fatalf("get task %q: %v", taskID, err)
	}
	if store.TaskState(task.State) != want {
		t.Fatalf("task %q: state = %s, want %s", taskID, task.State, want)
	}
	return task
}

// Acceptance: a citizen answer task that fails verify cap times —
// across cap distinct iterations — parks failed_retryable with the
// reason set, NOT terminal failed.
func TestCVF_EscalatesAfterCapConsecutiveIterations(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{})

	for i := 1; i < defaultVerifyFailCap; i++ {
		resp, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "writes missing", false)
		if err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
		if resp.Status != "counted" || resp.FailCount != i {
			t.Fatalf("report %d: got status=%s count=%d, want counted/%d", i, resp.Status, resp.FailCount, i)
		}
		mustState(t, st, f.taskID, store.TaskClaimed) // not yet escalated
		reclaim(t, st, f)
	}

	resp, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "still missing", false)
	if err != nil {
		t.Fatalf("cap report: %v", err)
	}
	if resp.Status != "escalated" || resp.FailCount != defaultVerifyFailCap {
		t.Fatalf("cap report: got status=%s count=%d, want escalated/%d", resp.Status, resp.FailCount, defaultVerifyFailCap)
	}
	task := mustState(t, st, f.taskID, store.TaskFailedRetryable)
	if task.FailReason == "" {
		t.Error("failed_retryable task must carry a fail_reason (operator must not fly blind)")
	}
}

// Idempotency on (task_id, iter_seq): repeated reports WITHIN one
// iteration (no reclaim) count exactly once and never escalate —
// the increment is gated on the claim's iter_seq.
func TestCVF_IdempotentWithinIteration(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{})

	for i := 0; i < defaultVerifyFailCap+2; i++ {
		resp, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false)
		if err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
		if resp.Status != "counted" {
			t.Fatalf("report %d: status=%s, want counted (same iteration must never escalate)", i, resp.Status)
		}
		if resp.FailCount != 1 {
			t.Fatalf("report %d: count=%d, want 1 (idempotent on iter_seq)", i, resp.FailCount)
		}
	}
	mustState(t, st, f.taskID, store.TaskClaimed)
}

// Descendants stay PENDING (NOT cascade-SKIPPED) after escalation —
// a skipped descendant would make the run un-retryable.
func TestCVF_DescendantsStayPending(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{cap: 1})
	now := time.Now()

	childID := fmt.Sprintf("%d:%d:child", f.projectID, f.runSeq)
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateTask{Task: store.TaskRecord{
			ID: childID, RunID: f.runID, Seq: 2, TaskDefID: "child",
			Action: "answer", ResultType: "text",
			State: store.TaskPending, DependsOn: f.taskID, CreatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("create child: %v", err)
	}

	if _, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false); err != nil {
		t.Fatalf("report: %v", err)
	}
	mustState(t, st, f.taskID, store.TaskFailedRetryable)
	mustState(t, st, childID, store.TaskPending) // NOT skipped
}

// Acceptance: escalated → retry_task (Script=="" no longer blocks)
// → fresh iteration; the counter resets to 0 so the retry starts a
// new consecutive count; descendants still PENDING/promotable.
func TestCVF_RetryRecoversAndResetsCounter(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{cap: 1})

	if _, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	parked := mustState(t, st, f.taskID, store.TaskFailedRetryable)
	if parked.VerifyFailCount != 0 {
		t.Errorf("park (ClearClaim) must reset verify_fail_count; got %d", parked.VerifyFailCount)
	}

	// retry_task must accept a citizen task in failed_retryable
	// (the Script=="" rejection was dropped; failed_retryable gate
	// kept).
	if _, err := coord.RetryTask(f.caller, f.taskID, RetryFromSnapshot); err != nil {
		t.Fatalf("RetryTask on citizen task: %v", err)
	}
	reopened := mustState(t, st, f.taskID, store.TaskReady)
	if reopened.VerifyFailCount != 0 || reopened.VerifyFailCountedIter != 0 {
		t.Errorf("retry must start a fresh consecutive count; got count=%d countedIter=%d",
			reopened.VerifyFailCount, reopened.VerifyFailCountedIter)
	}
}

// THE HEADLINE (spec D3): the coordinator reaper escalates a
// livelocked citizen task with NO client report ever arriving —
// the run-#3 livelock must be impossible even with a crashed/silent
// daemon. Each ReapExpiredClaim is one lease cycle / iteration.
func TestCVF_ReaperEscalatesWithoutAnyClientReport(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{})

	for i := 1; i < defaultVerifyFailCap; i++ {
		if err := coord.ReapExpiredClaim(f.taskID, f.citizenID); err != nil {
			t.Fatalf("reap %d: %v", i, err)
		}
		// Under cap → plain expire: task READY, counter advanced.
		task := mustState(t, st, f.taskID, store.TaskReady)
		if task.VerifyFailCount != i {
			t.Fatalf("reap %d: count=%d, want %d", i, task.VerifyFailCount, i)
		}
		// Reclaim for the next lease cycle (daemon re-claims).
		if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
			store.SetClaim{TaskID: f.taskID, CitizenID: f.citizenID, Deadline: time.Now().Add(time.Minute)},
		}}); err != nil {
			t.Fatalf("reclaim %d: %v", i, err)
		}
	}
	if err := coord.ReapExpiredClaim(f.taskID, f.citizenID); err != nil {
		t.Fatalf("cap reap: %v", err)
	}
	mustState(t, st, f.taskID, store.TaskFailedRetryable)
}

// Client report + reaper sweep of the SAME iteration must not
// double-count (idempotency across the two independent producers).
func TestCVF_ClientAndReaperNoDoubleCount(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{})

	resp, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false)
	if err != nil || resp.FailCount != 1 {
		t.Fatalf("client report: count=%d err=%v, want 1", resp.FailCount, err)
	}
	// Reaper sweeps the SAME still-open claim/iteration.
	if err := coord.ReapExpiredClaim(f.taskID, f.citizenID); err != nil {
		t.Fatalf("reap: %v", err)
	}
	task, _ := st.GetTask(f.taskID)
	if task.VerifyFailCount != 1 {
		t.Errorf("same iteration counted by both producers must stay 1; got %d", task.VerifyFailCount)
	}
}

// Durability: the count survives a lease-reclaim by a DIFFERENT
// claimant mid-loop; escalation still fires at the cap.
func TestCVF_CountSurvivesReclaimByDifferentClaimant(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{})
	now := time.Now()

	res, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateCitizen{Citizen: store.CitizenRecord{
			Username: "bob", Name: "Bob", Email: "b@t.local",
			Kind: store.CitizenKindHuman, RegisteredAt: now, LastSeen: now,
		}, Token: "tok-bob"},
	}})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	bobID := res.CitizenID
	bob, _ := st.GetCitizen(bobID)

	// iter 1 by alice → counted.
	if _, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false); err != nil {
		t.Fatalf("alice report: %v", err)
	}
	// Reclaim by bob (different claimant, advances iter_seq).
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.ExpireClaim{TaskID: f.taskID, CitizenID: f.citizenID},
	}}); err != nil {
		t.Fatalf("expire alice: %v", err)
	}
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.SetClaim{TaskID: f.taskID, CitizenID: bobID, Deadline: now.Add(30 * time.Minute)},
	}}); err != nil {
		t.Fatalf("bob claim: %v", err)
	}
	if _, err := coord.ReportCitizenVerifyFail(bob, f.taskID, "miss", false); err != nil {
		t.Fatalf("bob report: %v", err)
	}
	// cap=3: alice(1) + bob(2), reclaim, bob(3) → escalate.
	reclaim(t, st, cvfFixture{taskID: f.taskID, citizenID: bobID})
	resp, err := coord.ReportCitizenVerifyFail(bob, f.taskID, "miss", false)
	if err != nil {
		t.Fatalf("bob cap report: %v", err)
	}
	if resp.Status != "escalated" || resp.FailCount != defaultVerifyFailCap {
		t.Fatalf("durability: got status=%s count=%d, want escalated/%d", resp.Status, resp.FailCount, defaultVerifyFailCap)
	}
}

// Layer separation (D4): a recorded submission (layer-① delivery)
// resets the counter, so a verify-fail accrued before delivery does
// NOT carry forward — and request_changes, being
// submitted→accepted→review, can therefore never feed the
// layer-① livelock counter. A future reader most wants THIS proven:
// healthy review iteration must never trigger a verify-fail park.
func TestCVF_DeliveryResetsCounter_RequestChangesCannotAccumulate(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{})

	// Two layer-① misses on two iterations (count → 2).
	if _, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false); err != nil {
		t.Fatalf("miss 1: %v", err)
	}
	reclaim(t, st, f)
	if _, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false); err != nil {
		t.Fatalf("miss 2: %v", err)
	}
	if task, _ := st.GetTask(f.taskID); task.VerifyFailCount != 2 {
		t.Fatalf("pre-delivery count = %d, want 2", task.VerifyFailCount)
	}

	// The agent delivers: RecordSubmission flips the task to
	// SUBMITTED. This is the layer-① delivery point — the counter
	// MUST reset to 0. (request_changes would only come AFTER this,
	// against a 0 counter, which is exactly why a healthy
	// develop→review→request_changes loop can never accumulate a
	// verify-fail escalation: D4 separation, enforced at delivery.)
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.RecordSubmission{TaskID: f.taskID, CitizenID: f.citizenID, CommitSHA: "abc123"},
	}}); err != nil {
		t.Fatalf("record submission: %v", err)
	}
	task, _ := st.GetTask(f.taskID)
	if task.VerifyFailCount != 0 || task.VerifyFailCountedIter != 0 {
		t.Fatalf("delivery must reset the counter; got count=%d countedIter=%d",
			task.VerifyFailCount, task.VerifyFailCountedIter)
	}
}

// Multi-claimant (v1): citizens>1 → clear error, not silent
// mis-handling.
func TestCVF_MultiClaimantRejected(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{citizens: 2})

	_, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("multi-claimant: want ErrInvalidArgument, got %v", err)
	}
}

// State gate: a report on a task that is not CLAIMED/RUNNING (late
// / duplicate — e.g. the reaper already escalated) is refused, not
// re-counted on top of the final escalation.
func TestCVF_WrongStateRejected(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{cap: 1})

	if _, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	mustState(t, st, f.taskID, store.TaskFailedRetryable)
	_, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "late dup", false)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("late report on parked task: want ErrInvalidArgument, got %v", err)
	}
}

// Config hierarchy (D6): per-task verify_retry_cap overrides the
// const. cap=1 → escalate on the very first iteration.
func TestCVF_PerTaskCapOverridesDefault(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{cap: 1})

	resp, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "miss", false)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if resp.Status != "escalated" || resp.Cap != 1 {
		t.Fatalf("per-task cap=1: got status=%s cap=%d, want escalated/1", resp.Status, resp.Cap)
	}
}

// Operator escape hatch (D5): force=true escalates NOW, bypassing
// the counter — but into the SAME recoverable state, never terminal
// failed.
func TestCVF_OperatorForceEscalatesToRetryable(t *testing.T) {
	st, coord := newCVFStore(t)
	f := newCVFFixture(t, st, cvfFixtureOpts{})

	resp, err := coord.ReportCitizenVerifyFail(f.caller, f.taskID, "operator bail", true)
	if err != nil {
		t.Fatalf("force report: %v", err)
	}
	if resp.Status != "escalated" {
		t.Fatalf("force: status=%s, want escalated", resp.Status)
	}
	mustState(t, st, f.taskID, store.TaskFailedRetryable) // recoverable, NOT terminal failed
}

// Regression: compute retry_task is unchanged — a compute task in
// failed_retryable still retries (the dropped guard was Script=="",
// not the failed_retryable gate; the multi-claimant guard added to
// retry.go must not affect single-claimant compute tasks).
func TestCVF_ComputeRetryStillWorks(t *testing.T) {
	st, coord := newCVFStore(t)
	now := time.Now()

	res, _ := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateProject{Project: store.ProjectRecord{Name: "p", CreatedAt: now, UpdatedAt: now}},
	}})
	projectID := res.ProjectID
	res, _ = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateRun{Run: store.RunRecord{ProjectID: projectID, Name: "r", YAMLData: "name: r", State: store.RunActive, CreatedAt: now, UpdatedAt: now}},
	}})
	runID, runSeq := res.RunID, res.RunSeq
	res, _ = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateCitizen{Citizen: store.CitizenRecord{Username: "op", Name: "Op", Email: "o@t.local", Kind: store.CitizenKindHuman, RegisteredAt: now, LastSeen: now}, Token: "tok-op"},
	}})
	op, _ := st.GetCitizen(res.CitizenID)

	cid := fmt.Sprintf("%d:%d:compute", projectID, runSeq)
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateTask{Task: store.TaskRecord{
			ID: cid, RunID: runID, Seq: 1, TaskDefID: "compute",
			Action: "compute", Script: "run.sh", ResultType: "text",
			State: store.TaskFailedRetryable, CreatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("create compute task: %v", err)
	}
	if _, err := coord.RetryTask(op, cid, RetryFromSnapshot); err != nil {
		t.Fatalf("compute RetryTask regression: %v", err)
	}
	mustState(t, st, cid, store.TaskReady)
}
