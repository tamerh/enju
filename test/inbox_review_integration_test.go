package test

// End-to-end integration tests for the enju_inbox and enju_review
// MCP tools. Pin the request → coordinator → JSON-decode → format
// pipeline that the unit tests don't cover (those test the
// formatter and validator separately).

import (
	"fmt"
	"strings"
	"testing"
)

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

	res := h.callOK(t, "enju_inbox", map[string]any{
		"project_id": float64(projectID),
	})
	out := mcpText(res)

	if !strings.Contains(out, "[") || !strings.Contains(out, ":mine]") {
		t.Errorf("expected the 'mine' task in inbox output:\n%s", out)
	}
	if strings.Contains(out, ":theirs]") {
		t.Errorf("inbox leaked bob's task to me:\n%s", out)
	}
	if strings.Contains(out, ":open]") {
		t.Errorf("inbox leaked unassigned task — assign_to filter must be strict:\n%s", out)
	}
	if !strings.Contains(out, "this is yours") {
		t.Errorf("expected my task's prompt in output:\n%s", out)
	}
}

// TestInboxToolEmpty pins the no-items rendering. The exact
// text matters because the assistant pattern-matches it to skip
// rendering an empty list as "you have nothing waiting."
func TestInboxToolEmpty(t *testing.T) {
	h := newMCPHarness(t, "EmptyInbox")
	projectID := h.createTestProject()

	res := h.callOK(t, "enju_inbox", map[string]any{
		"project_id": float64(projectID),
	})
	out := mcpText(res)
	if !strings.Contains(out, "(no tasks waiting on you)") {
		t.Errorf("empty inbox rendering drifted:\n%s", out)
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
