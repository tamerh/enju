package test

import (
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestSmokeReviewCycleFullPipeline is the cross-layer end-to-end
// smoke test for the canonical review-revision-approve cycle:
//
//	create_run → claim → submit V1 → review request_changes
//	          → resubmit V2 → review approve → terminal
//
// Existing tests cover slices of this flow:
//   - TestMCPRequestChangesKeepsClaimOpenForRevision — iter_seq stability
//   - TestMCPApprovedReviewMergesUpstreamAndVerdict   — git-side merge behavior
//   - TestMCPRejectedReviewLeavesMainUntouched       — git-side reject behavior
//   - TestMCPTaskRequestChangesEmitsEvent             — single-event emission
//   - TestMCPRequestChangesCascadesDownstreamOfReviewGate — cascade reach
//
// Each focuses on one layer. This test is the *seam* test: it
// exercises the full pipeline once and asserts on every layer
// (task state machine, claim/iteration row state, audit-event
// emission, run-state evolution) in one shot. If any single
// layer drifts — apply handler, cascade orchestration, event
// emit, run-state evaluator — the slice tests can stay green
// while the full pipeline subtly breaks. This test fires
// across layers in one shot so that class of regression shows
// up here.
//
// Kept lean on git assertions (those are the slice tests'
// responsibility); focuses on coordinator-side state
// invariants that the recent ApplyPlan + Mutation refactor
// touched.
func TestSmokeReviewCycleFullPipeline(t *testing.T) {
	h := newMCPHarness(t, "Author")
	reviewer := h.newMCPClientAs(t, "Reviewer")
	projectID := h.createTestProject()

	yaml := `name: "smoke review cycle"
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

	draftID := h.taskID("draft")
	gateID := h.taskID("gate")

	// ---------------------------------------------------------
	// Phase 1: Author claims + submits V1.
	//
	// After single-citizen submit, the draft task's claim row
	// closes [completed] and the task surfaces as accepted at
	// the state-machine level — the review gate evaluates the
	// already-accepted upstream and either leaves it (approve)
	// or invalidates it (request_changes). The review task is
	// now ready for a reviewer to claim.
	// ---------------------------------------------------------
	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "VERSION-1-CONTENT")

	if state := mustGetTaskState(t, h.store, gateID); state != store.TaskReady {
		t.Errorf("gate after V1 submit: state=%s, expected ready", state)
	}

	// ---------------------------------------------------------
	// Phase 2: Reviewer requests changes.
	//
	// Phase 6c contract: request_changes does NOT close the
	// draft's claim row. The draft stays in iter-1 with the
	// claim open for revision. The review task itself
	// terminates (ACCEPTED), and a task_request_changes event
	// fires with the draft as TaskID.
	// ---------------------------------------------------------
	h.mcpClaimAs(t, reviewer, "gate")
	h.mcpSubmitReviewAs(t, reviewer, "gate", "needs more detail", "request_changes")

	iters, err := h.store.ListTaskIterations(draftID)
	if err != nil {
		t.Fatalf("ListTaskIterations(draft): %v", err)
	}
	if len(iters) != 1 {
		t.Errorf("draft iterations after request_changes: %d, expected 1 (claim stays open for revision)", len(iters))
	}
	// The draft must NOT be in terminal-accepted: the review's
	// request_changes cascade reopens the upstream so the
	// author can revise. Specific state (ready/claimed/etc)
	// is implementation detail; what matters is the gate
	// allows reclaim.
	if state := mustGetTaskState(t, h.store, draftID); state == store.TaskAccepted {
		t.Errorf("draft after request_changes: state=%s, expected non-terminal so author can revise", state)
	}

	if !hasEventForTask(t, h, projectID, "task_request_changes", draftID) {
		t.Error("expected task_request_changes event with draft as TaskID after request_changes verdict")
	}

	// ---------------------------------------------------------
	// Phase 3: Author resubmits V2 on the reopened claim.
	//
	// Reuse-on-reopen: the same iter row stays. Two submission
	// attempts now exist on iter-1 (one V1 + one V2), but
	// ListTaskIterations still reports a single iteration.
	// ---------------------------------------------------------
	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "VERSION-2-CONTENT")

	iters, err = h.store.ListTaskIterations(draftID)
	if err != nil {
		t.Fatalf("ListTaskIterations(draft) after resubmit: %v", err)
	}
	if len(iters) != 1 {
		t.Errorf("draft iterations after V2 resubmit: %d, expected 1 (revision stays in same iter)", len(iters))
	}

	// ---------------------------------------------------------
	// Phase 4: Reviewer approves V2.
	//
	// Both tasks reach terminal-success; the run completes.
	// task_completed fires for the draft (terminal-ACCEPTED
	// transition is the universal "task done" emit per Phase
	// 7d.4).
	// ---------------------------------------------------------
	h.mcpClaimAs(t, reviewer, "gate")
	h.mcpSubmitReviewAs(t, reviewer, "gate", "looks good now", "approve")

	if state := mustGetTaskState(t, h.store, draftID); state != store.TaskAccepted {
		t.Errorf("draft after approve: state=%s, expected accepted", state)
	}
	if state := mustGetTaskState(t, h.store, gateID); state != store.TaskAccepted {
		t.Errorf("gate after approve: state=%s, expected accepted", state)
	}

	if !hasEventForTask(t, h, projectID, "task_completed", draftID) {
		t.Error("expected task_completed event with draft as TaskID after terminal accept")
	}

	runs, err := h.store.ListRunsByProject(projectID)
	if err != nil {
		t.Fatalf("ListRunsByProject: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].State != store.RunCompleted {
		t.Errorf("run state after approve: %s, expected completed", runs[0].State)
	}

	// Final iteration row should be marked completed (claim
	// outcome=completed). Surface as a clear failure if the
	// terminal label didn't propagate.
	iters, _ = h.store.ListTaskIterations(draftID)
	if len(iters) != 1 || iters[0].Outcome != store.ClaimOutcomeCompleted {
		got := make([]store.ClaimOutcome, 0, len(iters))
		for _, it := range iters {
			got = append(got, it.Outcome)
		}
		t.Errorf("draft final iteration: outcomes=%v, expected single [%s]",
			got, store.ClaimOutcomeCompleted)
	}

	// ---------------------------------------------------------
	// Audit-channel assertions: verify the events landed in
	// chronological order with the right relative positions.
	// Catches a class of bug where individual events fire
	// (slice tests pass) but ordering / count is wrong (e.g.,
	// task_completed fires BEFORE task_request_changes due to
	// a misordered cascade emit).
	// ---------------------------------------------------------
	h.store.Events().WaitForDrain(2 * time.Second)
	allEvents, err := h.store.ListEvents(store.EventQuery{ProjectID: projectID, Limit: 200})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	// Loose count guard — a fresh run with one rejection +
	// resubmission emits ~30 events. Anything below 20 means
	// emissions were dropped; above 50 means a regression is
	// firing duplicates.
	if len(allEvents) < 20 || len(allEvents) > 50 {
		t.Errorf("event count = %d, expected ~30 (between 20-50). Possible drop or duplication.",
			len(allEvents))
	}

	// Per-task event ordering: for the draft, request_changes
	// MUST precede task_completed (Phase 7d.4 says
	// task_completed is the terminal-accepted transition; if
	// it fires before the rejection cascade, the cascade is
	// observing post-terminal state — a real bug).
	rcSeq := firstEventSeq(allEvents, draftID, "task_request_changes")
	tcSeq := firstEventSeq(allEvents, draftID, "task_completed")
	if rcSeq == 0 || tcSeq == 0 {
		t.Errorf("draft event ordering: missing request_changes (seq=%d) or task_completed (seq=%d)",
			rcSeq, tcSeq)
	} else if rcSeq >= tcSeq {
		t.Errorf("draft event ordering: task_request_changes (seq=%d) must precede task_completed (seq=%d)",
			rcSeq, tcSeq)
	}

	// Cascade origin event fires for the request_changes.
	// The cascade_fired event is what feeds the audit "what
	// triggered this" view; missing means a regression in the
	// cascade-emission seam (Phase 7d.6).
	if !hasEventForTask(t, h, projectID, "cascade_fired", draftID) {
		t.Error("expected cascade_fired event with draft as TaskID after request_changes (Phase 7d.6 cascade origin)")
	}

	// Reopen contract: the SECOND iteration_started for the
	// gate task must carry subtype=reopen (Phase 6c reuse-on-
	// reopen surfacing). Catches a regression where the
	// reopen flag is dropped at emit time, leaving downstream
	// audit consumers unable to distinguish a fresh claim
	// from a revision reclaim.
	gateIterStarts := eventsForTaskByType(allEvents, gateID, "iteration_started")
	if len(gateIterStarts) != 2 {
		t.Errorf("gate iteration_started count = %d, expected 2 (fresh + reopen)", len(gateIterStarts))
	} else if gateIterStarts[1].Subtype != "reopen" {
		t.Errorf("gate second iteration_started subtype = %q, expected %q (reopen flag)",
			gateIterStarts[1].Subtype, "reopen")
	}

	// Both review_given events fire with the right verdict
	// subtype — request_changes first, then approve.
	reviewGivens := eventsForTaskByType(allEvents, gateID, "review_given")
	if len(reviewGivens) != 2 {
		t.Errorf("review_given count = %d, expected 2 (request_changes + approve)", len(reviewGivens))
	} else {
		if reviewGivens[0].Subtype != "request_changes" {
			t.Errorf("first review_given subtype = %q, expected request_changes", reviewGivens[0].Subtype)
		}
		if reviewGivens[1].Subtype != "approve" {
			t.Errorf("second review_given subtype = %q, expected approve", reviewGivens[1].Subtype)
		}
	}

	// Run lifecycle: must see run_created and run_completed,
	// in that order. The intermediate run_idle/run_active
	// transitions are detail; what's load-bearing is start
	// and end.
	runCreatedSeq := firstEventSeq(allEvents, "", "run_created")
	runCompletedSeq := firstEventSeq(allEvents, "", "run_completed")
	if runCreatedSeq == 0 || runCompletedSeq == 0 {
		t.Errorf("run lifecycle: missing run_created (seq=%d) or run_completed (seq=%d)",
			runCreatedSeq, runCompletedSeq)
	} else if runCreatedSeq >= runCompletedSeq {
		t.Errorf("run lifecycle: run_created (seq=%d) must precede run_completed (seq=%d)",
			runCreatedSeq, runCompletedSeq)
	}
}

// firstEventSeq returns the per-project Seq of the first event
// matching (taskID, eventType), or 0 if none found. ListEvents
// returns newest-first; this scans backward to find the
// chronological-first event.
func firstEventSeq(events []store.RunEventRecord, taskID, eventType string) int64 {
	var first int64
	for _, e := range events {
		if e.Type != eventType {
			continue
		}
		if taskID != "" && e.TaskID != taskID {
			continue
		}
		if first == 0 || e.Seq < first {
			first = e.Seq
		}
	}
	return first
}

// eventsForTaskByType filters events for a (taskID, eventType)
// pair and returns them in chronological order (oldest-first).
// ListEvents is newest-first; this reverses for natural
// "first happened, then second" assertions.
func eventsForTaskByType(events []store.RunEventRecord, taskID, eventType string) []store.RunEventRecord {
	var out []store.RunEventRecord
	for _, e := range events {
		if e.Type == eventType && e.TaskID == taskID {
			out = append(out, e)
		}
	}
	// Reverse so chronological order matches the order asserted
	// in tests (oldest first).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// mustGetTaskState fetches a task's state or fails the test.
func mustGetTaskState(t *testing.T, s *store.Store, taskID string) store.TaskState {
	t.Helper()
	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask(%q): %v", taskID, err)
	}
	if task == nil {
		t.Fatalf("task %q not found", taskID)
	}
	return task.State
}

// hasEventForTask drains the event store and returns whether
// any event of the given type targets the given taskID.
func hasEventForTask(t *testing.T, h *mcpHarness, projectID int64, eventType, taskID string) bool {
	t.Helper()
	h.store.Events().WaitForDrain(2 * time.Second)
	events, err := h.store.ListEvents(store.EventQuery{
		ProjectID:  projectID,
		EventTypes: []string{eventType},
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("ListEvents(%s): %v", eventType, err)
	}
	for _, e := range events {
		if e.TaskID == taskID {
			return true
		}
	}
	if len(events) > 0 {
		var got []string
		for _, e := range events {
			got = append(got, e.TaskID)
		}
		t.Logf("%s events found but none target %s; targets=%v", eventType, taskID, strings.Join(got, ","))
	}
	return false
}
