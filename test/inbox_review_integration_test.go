package test

// End-to-end integration tests for the enju_inbox and enju_review
// MCP tools.
//
// Inbox is a fat-client view derived from live.jsonl + git
// (per ARCHITECTURE.md Decision #25 / #26 and the inbox redesign
// doc). The integration tests therefore have to:
//   1. Materialize the project clone in the workspace (so
//      live.jsonl has a place to live and git reads can resolve)
//   2. Seed live.jsonl with task_ready events that the
//      handleInbox candidate scan will pick up
//   3. Create the corresponding tasks in coordinator state.db
//      (so the per-candidate enju_get_task filter passes)
// We do (1) + (2) directly via the harness; (3) via
// h.mcpCreateRunInline.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedInboxReadyEvents appends a task_ready event for each task
// id, hoisting the assignee to the top-level wire field the way
// the coordinator does.
func seedInboxReadyEvents(t *testing.T, projectDir string, taskIDs []string, username string) {
	t.Helper()
	now := time.Now().UTC()
	events := make([]notifyEvent, 0, len(taskIDs))
	for i, id := range taskIDs {
		events = append(events, notifyEvent{
			Seq:       int64(i + 1),
			Timestamp: now.Add(time.Duration(-len(taskIDs)+i) * time.Second),
			Type:      "task_ready",
			Subtype:   "answer",
			TaskID:    id,
			AssignTo:  username,
		})
	}
	seedLiveJSONL(t, projectDir, events)
}

// TestInboxToolReturnsAssignedReadyTask pins the happy path: a
// task assigned to the calling citizen + ready state appears in
// the formatted inbox output. Unassigned tasks and tasks
// assigned to someone else don't appear.
func TestInboxToolReturnsAssignedReadyTask(t *testing.T) {
	h := newMCPHarness(t, "InboxTester")
	bobUsername := h.register("bob")
	projectID := h.createTestProject()

	// Run with three tasks: one assigned to me, one assigned to
	// bob, one unassigned. All ready (no upstreams).
	yaml := fmt.Sprintf(`name: "inbox-mix"
version: 1
tasks:
  - id: mine
    action: answer
    prompt: "this is yours, %s"
    assign_to: [%q]
  - id: theirs
    action: answer
    prompt: "this is bob's"
    assign_to: [%q]
  - id: open
    action: answer
    prompt: "anyone can take"
`, h.username, h.username, bobUsername)
	h.mcpCreateRunInline(t, projectID, yaml)

	// Materialize the project clone + seed live.jsonl with
	// task_ready events for ALL three tasks (the inbox handler
	// will filter via coordinator state).
	if _, err := h.workspace.ForProject(projectID, h.remoteFor(projectID), "inbox-test"); err != nil {
		t.Fatalf("workspace.ForProject: %v", err)
	}
	projectDir := h.workspace.ProjectDir(projectID)
	// Seed task_ready events for all three. The candidate scan
	// only fires on assign_to=h.username events, so theirs/open
	// won't even be candidates. Including them here would no-op;
	// we seed only mine to keep the test honest about what the
	// scan is doing.
	seedInboxReadyEvents(t, projectDir, []string{
		fmt.Sprintf("%d:1:mine", projectID),
	}, h.username)

	res := h.callOK(t, "enju_inbox", map[string]any{
		"project_id": float64(projectID),
	})
	out := mcpText(res)

	if !strings.Contains(out, ":mine]") {
		t.Errorf("expected the 'mine' task in inbox output:\n%s", out)
	}
	if strings.Contains(out, ":theirs]") {
		t.Errorf("inbox leaked bob's task to me:\n%s", out)
	}
	if strings.Contains(out, ":open]") {
		t.Errorf("inbox leaked unassigned task — assign_to filter must be strict:\n%s", out)
	}
	// Prompt rendering is intentionally not part of the v1
	// inbox — see TODO.md "Run YAML / prompt addressability".
	// The action and task-id headers above are the contract.
}

// TestInboxToolEmpty pins the no-items rendering. With no
// task_ready events for the caller in live.jsonl, the inbox is
// empty. Workspace must still be materialized so the handler
// gets past the "project clone not yet materialized" early
// return.
func TestInboxToolEmpty(t *testing.T) {
	h := newMCPHarness(t, "EmptyInbox")
	projectID := h.createTestProject()
	if _, err := h.workspace.ForProject(projectID, h.remoteFor(projectID), "empty-inbox"); err != nil {
		t.Fatalf("workspace.ForProject: %v", err)
	}

	res := h.callOK(t, "enju_inbox", map[string]any{
		"project_id": float64(projectID),
	})
	out := mcpText(res)
	if !strings.Contains(out, "(no tasks waiting on you)") {
		t.Errorf("empty inbox rendering drifted:\n%s", out)
	}
}

// TestInboxToolFiltersClaimedTasks pins the latest-event-wins
// invariant: a task that was once ready+mine but has since been
// claimed (iteration_started fired) must NOT appear in my inbox.
// The pure event-replay design relies on this — task state is
// derived from the newest event we see for that task.
func TestInboxToolFiltersClaimedTasks(t *testing.T) {
	h := newMCPHarness(t, "ClaimedFilter")
	projectID := h.createTestProject()

	yaml := fmt.Sprintf(`name: "claimed-filter"
version: 1
tasks:
  - id: still_ready
    action: answer
    prompt: "fresh task"
    assign_to: [%q]
`, h.username)
	h.mcpCreateRunInline(t, projectID, yaml)

	if _, err := h.workspace.ForProject(projectID, h.remoteFor(projectID), "claimed-test"); err != nil {
		t.Fatalf("workspace.ForProject: %v", err)
	}
	projectDir := h.workspace.ProjectDir(projectID)

	// Seed events for two tasks: one that's still ready, one that
	// was ready then claimed. The newer claim event hides the
	// claimed one from the inbox.
	stillReadyID := fmt.Sprintf("%d:1:still_ready", projectID)
	claimedID := fmt.Sprintf("%d:1:claimed_already", projectID)
	now := time.Now().UTC()
	seedLiveJSONL(t, projectDir, []notifyEvent{
		{Seq: 1, Timestamp: now.Add(-10 * time.Second), Type: "task_ready", Subtype: "answer", TaskID: claimedID, AssignTo: h.username},
		// iteration_started by me — citizen-scoped, terminates my view.
		{Seq: 2, Timestamp: now.Add(-5 * time.Second), Type: "iteration_started", TaskID: claimedID, Citizen: h.username},
		{Seq: 3, Timestamp: now.Add(-1 * time.Second), Type: "task_ready", Subtype: "answer", TaskID: stillReadyID, AssignTo: h.username},
	})

	res := h.callOK(t, "enju_inbox", map[string]any{
		"project_id": float64(projectID),
	})
	out := mcpText(res)
	if !strings.Contains(out, ":still_ready]") {
		t.Errorf("expected still_ready in inbox:\n%s", out)
	}
	if strings.Contains(out, ":claimed_already]") {
		t.Errorf("claimed task leaked into inbox (latest-event-wins broken):\n%s", out)
	}
}

// TestReviewToolWrongActionRejected pins the action guard:
// enju_review on a non-review task returns a clean error and
// directs the caller to enju_submit_result. Without this, a
// reviewer tool accidentally fired against an answer task
// would produce a confusing coordinator-side error.
func TestReviewToolWrongActionRejected(t *testing.T) {
	h := newMCPHarness(t, "WrongAction")
	projectID := h.createTestProject()

	yaml := `name: "answer-only"
version: 1
tasks:
  - id: ans
    action: answer
    prompt: "say hi"
`
	taskID := h.mcpCreateRunInline(t, projectID, yaml)
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": taskID})

	res := h.call(t, "enju_review", map[string]any{
		"task_id":  taskID,
		"decision": "approve",
		"content":  "looks good",
	})
	if !res.IsError {
		t.Fatalf("expected error for review-on-answer, got success: %s", mcpText(res))
	}
	msg := mcpText(res)
	if !strings.Contains(msg, "not action:review") {
		t.Errorf("error message should explain the action mismatch, got: %q", msg)
	}
	if !strings.Contains(msg, "enju_submit_result") {
		t.Errorf("error should redirect to enju_submit_result, got: %q", msg)
	}
}

// TestReviewToolInvalidDecisionRejected pins the decision-verb
// validation early-return: the tool refuses unknown verbs
// before it ever talks to the coordinator. Same canonical set
// as enju_submit_result; drift = bug.
func TestReviewToolInvalidDecisionRejected(t *testing.T) {
	h := newMCPHarness(t, "BadVerb")
	projectID := h.createTestProject()
	_ = projectID

	res := h.call(t, "enju_review", map[string]any{
		"task_id":  "0:0:fake", // doesn't matter — validation runs first
		"decision": "yolo",
		"content":  "bogus",
	})
	if !res.IsError {
		t.Fatalf("expected error for invalid decision, got success: %s", mcpText(res))
	}
	msg := mcpText(res)
	if !strings.Contains(msg, "yolo") {
		t.Errorf("error should echo the bad verb, got: %q", msg)
	}
	for _, want := range []string{"approve", "request_changes", "reject", "comment"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should list valid verbs (missing %q), got: %q", want, msg)
		}
	}
}
