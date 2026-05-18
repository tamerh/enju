package test

// Coverage for enju_terminate_run — the human-pulled-the-plug
// operation. Pins the cascade contract (run → terminated, tasks
// → skipped, claims → abandoned) and the late-submit refusal
// gate. Late-arriving claims are also refused for symmetry.

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

func TestMCPTerminateRunCascade(t *testing.T) {
	h := newMCPHarness(t, "Terminator")
	projectID := h.createTestProject()

	yaml := `name: "terminate-cascade"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write something."
  - id: review
    action: review
    reviews: draft
    prompt: "Approve or request changes."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	draftID := h.taskID("draft")
	reviewID := h.taskID("review")

	// Open a claim on draft so we can pin the abandoned-claims
	// count later. Don't submit — leave the claim open across
	// the terminate.
	h.mcpClaimOK(t, "draft")

	// Sanity: pre-terminate the run is alive and the draft is
	// claimed.
	if state := mustGetTaskState(t, h.store, draftID); state != store.TaskClaimed {
		t.Fatalf("pre-terminate draft state: got %s, want claimed", state)
	}

	// Terminate.
	res := h.callOK(t, "enju_terminate_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
		"reason":     "design flaw — restarting with the new template",
	})
	text := mcpText(res)
	for _, want := range []string{"terminated", "design flaw"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q: %s", want, text)
		}
	}

	// Run state → terminated.
	run, err := h.store.GetRunByProjectSeq(projectID, int(h.lastRunSeq))
	if err != nil || run == nil {
		t.Fatalf("GetRun after terminate: %v", err)
	}
	if run.State != store.RunTerminated {
		t.Errorf("run state: got %s, want terminated", run.State)
	}

	// Draft (was claimed): cascade flips to skipped with
	// run_terminated reason.
	draft, _ := h.store.GetTask(draftID)
	if draft == nil || draft.State != store.TaskSkipped {
		t.Errorf("draft state: got %v, want skipped", draft)
	}
	if draft.SkipReason != "run_terminated" {
		t.Errorf("draft skip_reason: got %q, want run_terminated", draft.SkipReason)
	}

	// Review (was pending): same.
	review, _ := h.store.GetTask(reviewID)
	if review == nil || review.State != store.TaskSkipped {
		t.Errorf("review state: got %v, want skipped", review)
	}

	// Open claim on draft → outcome=abandoned. Read the most
	// recent iteration row directly from store for the
	// invariant check.
	iters, err := h.store.ListTaskIterations(draftID)
	if err != nil || len(iters) == 0 {
		t.Fatalf("ListTaskIterations: %v (got %d rows)", err, len(iters))
	}
	last := iters[len(iters)-1]
	if string(last.Outcome) != "abandoned" {
		t.Errorf("draft claim outcome: got %q, want abandoned", last.Outcome)
	}

	// Late-arriving submit on the now-terminated run is refused.
	// The cascade has already flipped the task to skipped, so
	// the fat-client's terminal-task pre-check fires before the
	// coord-side run-terminated guard — the user-facing message
	// mentions "skipped" rather than "terminated", but the
	// effect is the same: submission rejected, no commit lands
	// on main. Substring match on either signal.
	errMsg := h.callExpectError(t, "enju_submit_result", map[string]any{
		"task_id": draftID,
		"content": "too late, run is dead",
	})
	low := strings.ToLower(errMsg)
	if !strings.Contains(low, "skipped") && !strings.Contains(low, "terminated") {
		t.Errorf("late submit error didn't refuse coherently: %s", errMsg)
	}

	// Late-arriving claim on the same run. The fat-client claim
	// handler renders coord 4xx errors as a successful tool
	// result with formatted "✗ Failed to claim:" text — that's
	// the existing pattern. Read the text and confirm
	// "terminated" surfaces so the LLM knows the run is dead.
	res = h.call(t, "enju_claim_task", map[string]any{
		"task_id": reviewID,
	})
	if !strings.Contains(strings.ToLower(mcpText(res)), "terminated") {
		t.Errorf("late claim didn't surface terminated: %s", mcpText(res))
	}

	// Re-terminate is refused — terminate is irreversible.
	errMsg = h.callExpectError(t, "enju_terminate_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	if !strings.Contains(strings.ToLower(errMsg), "terminal") {
		t.Errorf("re-terminate error didn't refuse: %s", errMsg)
	}
}

func TestMCPTerminateRunFromPaused(t *testing.T) {
	// Pause→terminate is a real workflow: "I paused thinking
	// I'd come back, then realized this run is bad."
	h := newMCPHarness(t, "Pauser")
	projectID := h.createTestProject()
	yaml := `name: "pause-then-terminate"
version: 1
tasks:
  - id: only
    action: answer
    prompt: "Anything."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.callOK(t, "enju_pause_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})

	run, _ := h.store.GetRunByProjectSeq(projectID, int(h.lastRunSeq))
	if run == nil || run.State != store.RunPaused {
		t.Fatalf("pre-terminate state: got %v, want paused", run)
	}

	h.callOK(t, "enju_terminate_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})

	run, _ = h.store.GetRunByProjectSeq(projectID, int(h.lastRunSeq))
	if run == nil || run.State != store.RunTerminated {
		t.Errorf("post-terminate state: got %v, want terminated", run)
	}
}

// TestMCPTerminateRunIsTerminal pins the apply-time guards that
// keep the terminated state coherent against stale follow-on
// mutations + later operator commands. Spawn, resume, and the
// implicit auto-CompleteRun must all refuse to touch a
// terminated run; without the guards a later CompleteRun could
// silently flip the state back to completed when the cascade
// counts settle.
func TestMCPTerminateRunIsTerminal(t *testing.T) {
	h := newMCPHarness(t, "Gravedigger")
	projectID := h.createTestProject()
	yaml := `name: "post-terminate-guards"
version: 1
tasks:
  - id: anchor
    action: answer
    prompt: "Anchor task."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.callOK(t, "enju_terminate_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
		"reason":     "pinning the guards",
	})

	// Spawn into terminated → refused with terminal-run message.
	errMsg := h.callExpectError(t, "enju_spawn_task", map[string]any{
		"project_id":  float64(projectID),
		"run_id":      float64(h.lastRunSeq),
		"task_def_id": "post_terminate_spawn",
		"action":      "answer",
		"prompt":      "should never run",
	})
	if !strings.Contains(strings.ToLower(errMsg), "terminal") {
		t.Errorf("spawn into terminated didn't mention terminal: %s", errMsg)
	}

	// Resume of terminated → refused with terminal-run message.
	errMsg = h.callExpectError(t, "enju_resume_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	if !strings.Contains(strings.ToLower(errMsg), "terminal") {
		t.Errorf("resume of terminated didn't mention terminal: %s", errMsg)
	}

	// Run state should still be terminated after both refusals
	// (no silent state-machine drift from a partially-applied plan).
	run, _ := h.store.GetRunByProjectSeq(projectID, int(h.lastRunSeq))
	if run == nil || run.State != store.RunTerminated {
		t.Errorf("after refused mutations, state: got %v, want still terminated", run)
	}

	// Apply a bare CompleteRun directly through the store. This
	// is the stale-plan scenario: a CompleteRun mutation
	// composed before the terminate, fired afterward. Without
	// the guard in applyCompleteRun, the count walk would flip
	// "all tasks terminal" → completed and silently overwrite
	// terminated. Driving the mutation through ApplyPlan
	// (rather than only via service.PauseRun-style wrappers)
	// pins the apply-time guard directly.
	if _, err := h.store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CompleteRun{RunID: run.ID},
		},
	}); err != nil {
		t.Fatalf("CompleteRun on terminated run: got error %v, want silent no-op", err)
	}
	run, _ = h.store.GetRunByProjectSeq(projectID, int(h.lastRunSeq))
	if run == nil || run.State != store.RunTerminated {
		t.Errorf("after stale CompleteRun, state: got %v, want still terminated", run)
	}
}

// TestMCPTerminateRunSkipsClaimedReviewTask reproduces the
// production defect (mustache-engine project #9, run #1). Event-log
// reconstruction:
//
//	develop_renderer SUBMITTED → review_renderer READY
//	review_renderer CLAIMED (iter1, active iteration)
//	run_terminated
//	~2 min later the NEW run's project-scoped reviewer daemon
//	  iteration_started/REOPEN-ed 9:1:review_renderer (a terminated
//	  run's task) and the coordinator ACCEPTED the submit (✅).
//
// TestMCPTerminateRunCascade already covers a *pending* review
// (never claimed) — that path refuses correctly. The uncovered hole
// is a review that was CLAIMED in an active iteration at terminate
// (its target already submitted): it is left claimable, a fresh
// claim re-opens it, and the submit is accepted on a dead run.
//
// The coordinator is the authoritative gate (your point: a daemon
// can't close the terminate-vs-claim race). Two invariants:
//  1. terminate durably skips a CLAIMED review task.
//  2. a fresh claim of any task whose run is terminal is refused.
func TestMCPTerminateRunSkipsClaimedReviewTask(t *testing.T) {
	h := newMCPHarness(t, "ClaimedReview")
	reviewer := h.newMCPClientAs(t, "LeftoverClaimer")
	projectID := h.createTestProject()
	yaml := `name: "claimed-review-after-terminate"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write something."
  - id: gate
    action: review
    reviews: draft
    prompt: "Approve or request changes."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	gateID := h.taskID("gate")

	// Drive draft to SUBMITTED so the review gate becomes READY —
	// exactly the state develop_renderer/review_renderer were in.
	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "draft content")
	if st := mustGetTaskState(t, h.store, gateID); st != store.TaskReady {
		t.Fatalf("pre-claim gate state: got %s, want ready", st)
	}

	// Reviewer CLAIMS the gate → active iteration open at terminate
	// (review_renderer iter1).
	h.mcpClaimAs(t, reviewer, "gate")
	if st := mustGetTaskState(t, h.store, gateID); st != store.TaskClaimed {
		t.Fatalf("pre-terminate gate state: got %s, want claimed", st)
	}

	h.callOK(t, "enju_terminate_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
		"reason":     "repro: claimed review task at terminate",
	})

	// Invariant 1: a claimed review task must be durably skipped by
	// terminate (its sibling develop_renderer was; review_renderer
	// was NOT — this is the bug).
	gate, _ := h.store.GetTask(gateID)
	if gate == nil || gate.State != store.TaskSkipped {
		t.Errorf("claimed review left un-skipped by terminate: got %v, want skipped", gate)
	}

	// Invariant 2: a fresh claim on the leftover (what the new
	// run's project-scoped reviewer daemon did) must be refused
	// because the run is terminal — not reopen the task.
	res := h.mcpCallVia(t, reviewer, "enju_claim_task", map[string]any{"task_id": gateID})
	if !strings.Contains(strings.ToLower(mcpText(res)), "terminal") &&
		!strings.Contains(strings.ToLower(mcpText(res)), "terminated") {
		t.Errorf("fresh claim of a terminated run's review task wasn't refused: %s", mcpText(res))
	}
	gate, _ = h.store.GetTask(gateID)
	if gate == nil || gate.State != store.TaskSkipped {
		t.Errorf("fresh claim resurrected the review off a terminated run: got %v, want still skipped", gate)
	}
}
