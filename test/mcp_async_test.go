package test

// End-to-end async compute test. Drives the full phase 4 loop:
// YAML declares mode: async → enju_execute_task forks a detached
// wrapper → wrapper commits + pushes → enju_run_status fires the
// fetch-path scanner → /tasks/reconcile advances the task to
// accepted. The test exercises every new surface introduced by
// phase 4a–4c simultaneously; if any piece breaks the loop, this
// is the test that catches it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCPAsyncComputeEndToEnd launches a `mode: async` compute
// task, waits for the detached wrapper to finish, then asks
// run_status — which triggers the fetch-path scanner, which
// POSTs /tasks/reconcile, which flips the task to accepted.
func TestMCPAsyncComputeEndToEnd(t *testing.T) {
	h := newMCPHarness(t, "AsyncCompute")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju_templates/async-ok/template.yaml": {body: `name: "async ok"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    mode: async
`, mode: 0o644},
		"enju_templates/async-ok/scripts/run.sh": {body: `#!/bin/bash
echo "hello from async"
`, mode: 0o755},
	}, "seed async template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/async-ok",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:run", projectID))

	// Execute must return immediately (not block until the
	// script finishes) and its response must say "background."
	execStart := time.Now()
	res := h.call(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("run"),
	})
	elapsed := time.Since(execStart)
	if res.IsError {
		t.Fatalf("execute: %s", mcpText(res))
	}
	text := mcpText(res)
	if !strings.Contains(text, "background") {
		t.Errorf("async execute text missing 'background' marker; got:\n%s", text)
	}
	// Sanity: async kickoff should be fast (fork + metadata
	// write, no waiting on script). A >5s elapsed here would
	// mean the handler accidentally took the sync path.
	if elapsed > 5*time.Second {
		t.Errorf("async execute took %v — expected fast fork-and-return", elapsed)
	}

	// Poll until the wrapper's .wrap-result.json exists in the
	// project clone. Its presence signals the detached
	// subprocess has written its result and (for exit 0)
	// committed + pushed. We poll the workspace the harness
	// uses to issue MCP calls — the same clone the MCP server
	// and wrapper share for this project.
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), ".enju/runs/1/run/.wrap-result.json")
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("wrapper result file did not appear: %v", err)
	}

	// run_status triggers reconcileRunBranch: fetch origin,
	// scan for Enju-Task-Complete trailers since the cursor,
	// POST /tasks/reconcile, advance cursor.
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})

	// The run_status call above already fired reconcile via
	// reconcileRunBranch. Poll briefly — if the HTTP round-
	// trip is mid-flight when we check state, give it a beat.
	if err := waitForTaskState(h, h.taskID("run"), "accepted", 5*time.Second); err != nil {
		t.Fatalf("task did not reach accepted: %v", err)
	}
}

// TestMCPAsyncComputeFailurePropagatesViaReaper verifies the
// failure-on-return path: a detached wrapper whose script
// exits non-zero drops a .wrap-result.json locally but does
// NOT commit (matching today's sync path). The submitter's
// next claim/execute/run_status call triggers the reaper,
// which reads the result file and posts /tasks/:id/fail,
// flipping the coordinator's view to failed.
func TestMCPAsyncComputeFailurePropagatesViaReaper(t *testing.T) {
	h := newMCPHarness(t, "AsyncFail")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju_templates/async-fail/template.yaml": {body: `name: "async fail"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    mode: async
`, mode: 0o644},
		"enju_templates/async-fail/scripts/run.sh": {body: `#!/bin/bash
echo "something went wrong" >&2
exit 7
`, mode: 0o755},
	}, "seed async fail template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/async-fail",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:run", projectID))

	// Launch async task. Returns immediately.
	res := h.callOK(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("run"),
	})
	if !strings.Contains(mcpText(res), "background") {
		t.Fatalf("expected background kickoff, got:\n%s", mcpText(res))
	}

	// Wait for the wrapper to finish (.wrap-result.json written
	// with exit_code=7). Before the reaper runs, coordinator
	// still sees claimed.
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), ".enju/runs/1/run/.wrap-result.json")
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("wrap-result.json did not appear: %v", err)
	}

	// Trigger reaper via run_status. The handler's reconcile
	// hook walks the tree and posts /tasks/:id/fail for the
	// exit!=0 entry.
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, h.taskID("run"), "failed", 5*time.Second); err != nil {
		t.Fatalf("task did not reach failed via reaper: %v", err)
	}

	// Reaper should have moved the result file aside so a
	// second call doesn't double-process it.
	if _, statErr := os.Stat(resultPath); statErr == nil {
		t.Errorf("expected reaper to rename %s away; still present", resultPath)
	}
}

// waitForFile polls a filesystem path until it exists or the
// timeout elapses. Used to await the detached wrapper's result
// file without tying the test to a specific process-signaling
// mechanism.
func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("file %q did not appear within %s", path, timeout)
}

// waitForTaskState polls /tasks/:id until state == want or the
// timeout elapses. Tolerates the tiny window between reconcile
// POST returning and the DB commit being visible.
func waitForTaskState(h *mcpHarness, taskID, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Fetch via the MCP test client's HTTP path.
		t, err := h.store.GetTask(taskID)
		if err == nil && t != nil && string(t.State) == want {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	t, _ := h.store.GetTask(taskID)
	cur := ""
	if t != nil {
		cur = string(t.State)
	}
	return fmt.Errorf("task %q state never reached %q (current: %q)", taskID, want, cur)
}

// workspaceDirForProject returns the absolute path to the
// fat-client's local clone of the given project. Shared by the
// harness and the MCP server under test (both use the same
// Workspace root via the mcpHarness wiring). Extracted here so
// async-compute tests can poll wrapper output without reaching
// into harness internals.
func (h *mcpHarness) workspaceDirForProject(projectID int64) string {
	proj, err := h.workspace.ForProject(projectID, h.remoteFor(projectID))
	if err != nil {
		h.t.Fatalf("opening project %d workspace: %v", projectID, err)
	}
	return proj.WorkDir()
}

// Keep context.Context import live; helpers may gain ctx-aware
// variants in follow-up.
var _ = context.Background
