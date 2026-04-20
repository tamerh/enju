package test

// Reconcile endpoint tests (phase 2 of async compute). The
// reconcile endpoint is the coordinator's state-advancement
// path for commits produced by the compute wrapper — phase 3's
// fetch-path scanner and phase 4's async-mode kickoff both
// depend on its contract: idempotent, batchable, trust-the-
// client.
//
// Keep these tests HTTP-level. The batch handler delegates to
// engine paths already covered by submit tests; reconcile-
// specific behavior is routing, idempotency, and shape checks
// on the batch entries.

import (
	"fmt"
	"strings"
	"testing"
)

// reconcilePost issues a reconcile batch request against the
// coordinator and returns the decoded response map (error or
// results list). Mirrors the minimal shape scanners will use.
func reconcilePost(s *testServer, tasks []map[string]interface{}) map[string]interface{} {
	s.t.Helper()
	return s.post("/api/v1/tasks/reconcile", map[string]interface{}{"tasks": tasks})
}

// fakeSHA returns a 40-char lowercase-hex string that passes
// the commit_sha shape validator. The coordinator doesn't fetch
// the commit (trust-the-client), so a well-shaped fake is
// enough to exercise the endpoint's routing + state transitions
// without needing a real git commit. Phase 3's integration
// tests will reconcile real commits; here we stay focused on
// the HTTP contract.
func fakeSHA(seed string) string {
	var b strings.Builder
	for b.Len() < 40 {
		b.WriteString(fmt.Sprintf("%02x", (len(seed)*7+b.Len())&0xff))
		b.WriteString(seed)
	}
	// Keep only hex chars.
	s := b.String()
	out := make([]byte, 0, 40)
	for i := 0; i < len(s) && len(out) < 40; i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			out = append(out, c)
		}
	}
	for len(out) < 40 {
		out = append(out, '0')
	}
	return string(out)
}

// TestReconcileAcceptsClaimedTask drives the happy path: a
// task in claimed state gets a reconcile entry with exit 0, and
// the task transitions to accepted. Equivalent to a successful
// submit — just via the batch endpoint.
func TestReconcileAcceptsClaimedTask(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)

	taskID := s.taskID("task_a")
	sha := fakeSHA("t1")
	resp := reconcilePost(s, []map[string]interface{}{
		{
			"task_id":    taskID,
			"commit_sha": sha,
			"exit_code":  0,
			"username":   alice,
			"content":    "apple",
		},
	})

	results, ok := resp["results"].([]interface{})
	if !ok || len(results) != 1 {
		t.Fatalf("expected one result, got %+v", resp)
	}
	r := results[0].(map[string]interface{})
	if r["status"] != "accepted" {
		t.Fatalf("expected status=accepted, got %+v", r)
	}

	// Verify task row advanced.
	task := s.get("/api/v1/tasks/" + taskID)
	if state, _ := task["state"].(string); state != "accepted" {
		t.Errorf("expected task state=accepted, got %q", state)
	}
	if got, _ := task["commit_sha"].(string); got != sha {
		t.Errorf("expected commit_sha=%q, got %q", sha, got)
	}
}

// TestReconcileIdempotentOnSameCommit verifies re-posting the
// same (task_id, commit_sha) after acceptance returns "noop"
// without error. Required so the fetch-path scanner can retry
// after a mid-batch crash without double-processing.
func TestReconcileIdempotentOnSameCommit(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)

	taskID := s.taskID("task_a")
	sha := fakeSHA("idempot")
	entry := map[string]interface{}{
		"task_id":    taskID,
		"commit_sha": sha,
		"exit_code":  0,
		"username":   alice,
		"content":    "first",
	}
	first := reconcilePost(s, []map[string]interface{}{entry})
	if r := first["results"].([]interface{})[0].(map[string]interface{}); r["status"] != "accepted" {
		t.Fatalf("first reconcile: expected accepted, got %+v", r)
	}
	// Second call with identical entry — must be noop.
	second := reconcilePost(s, []map[string]interface{}{entry})
	r := second["results"].([]interface{})[0].(map[string]interface{})
	if r["status"] != "noop" {
		t.Errorf("second reconcile: expected noop, got %+v", r)
	}
}

// TestReconcileRejectsDifferentCommitForAcceptedTask protects
// against "reconcile trying to rewrite history": a task that's
// already accepted at commit X must not silently flip to commit
// Y. That would corrupt the artifact index + downstream reads.
func TestReconcileRejectsDifferentCommitForAcceptedTask(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)

	taskID := s.taskID("task_a")
	reconcilePost(s, []map[string]interface{}{
		{"task_id": taskID, "commit_sha": fakeSHA("first"), "exit_code": 0, "username": alice, "content": "apple"},
	})
	// Second reconcile with a DIFFERENT sha for the now-accepted task.
	resp := reconcilePost(s, []map[string]interface{}{
		{"task_id": taskID, "commit_sha": fakeSHA("second"), "exit_code": 0, "username": alice, "content": "pear"},
	})
	r := resp["results"].([]interface{})[0].(map[string]interface{})
	if r["status"] != "error" {
		t.Fatalf("expected error for divergent sha, got %+v", r)
	}
	if errMsg, _ := r["error"].(string); !strings.Contains(errMsg, "different commit") {
		t.Errorf("expected 'different commit' in error, got %q", errMsg)
	}
}

// TestReconcileFailsTaskOnNonZeroExit verifies exit != 0 routes
// the task to failed via the fail cascade — same behavior as
// the inline compute handler's script-exit-nonzero path.
func TestReconcileFailsTaskOnNonZeroExit(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)

	taskID := s.taskID("task_a")
	resp := reconcilePost(s, []map[string]interface{}{
		{
			"task_id":     taskID,
			"commit_sha":  fakeSHA("exit1"),
			"exit_code":   1,
			"username":    alice,
			"fail_reason": "script exited with code 1: permission denied",
		},
	})
	r := resp["results"].([]interface{})[0].(map[string]interface{})
	if r["status"] != "failed" {
		t.Fatalf("expected status=failed, got %+v", r)
	}
	task := s.get("/api/v1/tasks/" + taskID)
	if state, _ := task["state"].(string); state != "failed" {
		t.Errorf("expected task state=failed, got %q", state)
	}
}

// TestReconcileRejectsBadShape covers the shape-level guards:
// missing task_id, missing commit_sha, malformed commit_sha.
// Each malformed entry yields an "error" status but doesn't
// break the batch — other entries in the same batch continue.
func TestReconcileRejectsBadShape(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)
	taskID := s.taskID("task_a")

	resp := reconcilePost(s, []map[string]interface{}{
		{"commit_sha": fakeSHA("a"), "exit_code": 0},                       // missing task_id
		{"task_id": taskID, "exit_code": 0},                                // missing commit_sha
		{"task_id": taskID, "commit_sha": "not-a-sha", "exit_code": 0},     // bad shape
		{"task_id": taskID, "commit_sha": fakeSHA("g"), "exit_code": 0, "username": alice, "content": "ok"}, // good, should still process
	})
	results := resp["results"].([]interface{})
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	for i, expect := range []string{"error", "error", "error", "accepted"} {
		r := results[i].(map[string]interface{})
		if r["status"] != expect {
			t.Errorf("result[%d]: expected status=%q, got %+v", i, expect, r)
		}
	}
}

// TestReconcileRejectsEmptyBatch guards against callers sending
// a zero-length tasks array (likely a bug in the scanner).
func TestReconcileRejectsEmptyBatch(t *testing.T) {
	s := newTestServer(t)
	resp := reconcilePost(s, []map[string]interface{}{})
	if _, ok := resp["error"]; !ok {
		t.Fatalf("expected error for empty batch, got %+v", resp)
	}
}

// TestReconcileBatchResponseShapeOnMixedErrors locks in the
// write-free membership fix. Before the fix, per-entry errors
// routed through requireProjectMembershipForTask wrote an
// error payload directly to the response writer — then the
// batch handler's final writeJSON was a SECOND write to the
// same stream, corrupting the response envelope (either
// truncated mid-JSON or with concatenated garbage). The
// regression was inert in practice because scanners today
// batch within one project, but a future batcher that mixes
// projects would hit it.
//
// Contract: every entry — valid or invalid, whatever the
// error — surfaces via results[].error. The response body
// stays parseable JSON with the expected `results` array and
// no duplicate / extra payloads.
func TestReconcileBatchResponseShapeOnMixedErrors(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)
	goodTaskID := s.taskID("task_a")

	// Mixed batch: one valid (task_a, will accept), three
	// invalid (missing task_id, missing sha, bad-shape sha,
	// unknown task). Each invalid entry MUST become a
	// results[i].error without corrupting the response body.
	resp := reconcilePost(s, []map[string]interface{}{
		{"commit_sha": fakeSHA("x"), "exit_code": 0},                         // missing task_id
		{"task_id": goodTaskID, "exit_code": 0},                              // missing commit_sha
		{"task_id": goodTaskID, "commit_sha": "not-40-hex", "exit_code": 0},  // bad shape
		{"task_id": "99:99:ghost", "commit_sha": fakeSHA("g"), "exit_code": 0}, // unknown task
		{"task_id": goodTaskID, "commit_sha": fakeSHA("ok"), "exit_code": 0,
			"username": alice, "content": "done"}, // the valid one
	})

	// Envelope: single `results` key, no extras, array of 5
	// entries. Pre-fix this would have been either short-
	// circuited mid-stream or contaminated with standalone
	// error payloads.
	results, ok := resp["results"].([]interface{})
	if !ok {
		t.Fatalf("expected results[] envelope, got %+v", resp)
	}
	if _, hasError := resp["error"]; hasError {
		t.Fatalf("expected no top-level error on mixed batch, got %+v", resp)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 per-entry results, got %d: %+v", len(results), results)
	}
	// Spot-check statuses align with entry positions.
	wantStatuses := []string{"error", "error", "error", "error", "accepted"}
	for i, want := range wantStatuses {
		got := results[i].(map[string]interface{})["status"]
		if got != want {
			t.Errorf("results[%d].status = %v, want %q", i, got, want)
		}
	}
	// Error entries must carry an error message (otherwise
	// the scanner has no handle for debugging).
	for i := 0; i < 4; i++ {
		errMsg, _ := results[i].(map[string]interface{})["error"].(string)
		if errMsg == "" {
			t.Errorf("results[%d]: expected non-empty error message, got %+v", i, results[i])
		}
	}
}
