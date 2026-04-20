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

// TestMCPAsyncCursorAdvanceDoesNotStarveScanner is the direct
// regression guard for the tester-reported "async cursor race"
// bug. Claim: in async mode the wrapper's SubmitTaskResult
// auto-advances the reconcile cursor past its own commit, so
// when the submitter later calls enju_run_status the scanner
// sees no new commits (cursor already past) and never posts
// /tasks/reconcile — task stuck in claimed/running forever.
//
// Fix rule (already in compute.Run): wrapper MUST NOT pass
// ProjectID/StateDir to SubmitTaskResult, because the scanner
// needs to see that trailer on the submitter's next sweep.
// Only sync-path callers (answer/review/vote submit, sync
// compute report) advance the cursor — those already
// reported to the coordinator directly.
//
// Scenario exercised:
//  1. Seed a prior trailer-free state via create_run.
//  2. Launch an async compute task.
//  3. Wait for wrapper to finish and commit.
//  4. Call run_status — scanner MUST find + reconcile the
//     wrapper's commit.
//  5. Task must reach "accepted".
//
// If the wrapper auto-advanced the cursor in step 3, step 4
// would see no new commits and the task would be stuck.
func TestMCPAsyncCursorAdvanceDoesNotStarveScanner(t *testing.T) {
	h := newMCPHarness(t, "AsyncCursorStarve")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju_templates/async-starve/template.yaml": {body: `name: "async starve"
version: 1
tasks:
  - id: job
    action: compute
    script: scripts/job.sh
    mode: async
`, mode: 0o644},
		"enju_templates/async-starve/scripts/job.sh": {body: `#!/bin/bash
echo "async payload"
`, mode: 0o755},
	}, "seed async starve template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/async-starve",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:job", projectID))

	// Launch async. Handler's pullBranchWithReconcile fires
	// at this point (cursor saves to pre-wrapper tip).
	res := h.call(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("job"),
	})
	if res.IsError {
		t.Fatalf("execute_task: %s", mcpText(res))
	}

	// Wait for wrapper to land its .wrap-result.json.
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), ".enju/runs/1/job/.wrap-result.json")
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("wrap-result.json did not appear: %v", err)
	}

	// run_status triggers reconcileRunBranch. If the wrapper
	// had auto-advanced the cursor, scanner would find no
	// new commits and never post — task state stays claimed.
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, h.taskID("job"), "accepted", 5*time.Second); err != nil {
		t.Fatalf("task did not reach accepted — cursor likely advanced past wrapper commit: %v", err)
	}
}

// TestMCPAsyncCursorRaceOnNamedBranch replicates the tester's
// exact repro scenario: async task on a non-default named
// branch (via branch:"auto" → slug-N). Their session showed
// task state stuck in "claimed" even after run_status, with
// the cursor advanced past the completion commit. The
// baseline TestMCPAsyncComputeEndToEnd only exercised the
// default branch, so any branch-specific regression would
// slip through.
func TestMCPAsyncCursorRaceOnNamedBranch(t *testing.T) {
	h := newMCPHarness(t, "AsyncNamedBranch")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju_templates/simple-async/template.yaml": {body: `name: "simple-async"
version: 1
tasks:
  - id: job
    action: compute
    script: scripts/job.sh
    mode: async
`, mode: 0o644},
		"enju_templates/simple-async/scripts/job.sh": {body: `#!/bin/bash
echo "async payload on named branch"
`, mode: 0o755},
	}, "seed simple-async template")

	// branch:"auto" → allocates slug-N (e.g. simple-async-1).
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/simple-async",
		"branch":     "auto",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:job", projectID))

	res := h.call(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("job"),
	})
	if res.IsError {
		t.Fatalf("execute_task: %s", mcpText(res))
	}

	resultPath := filepath.Join(h.workspaceDirForProject(projectID), ".enju/runs/1/job/.wrap-result.json")
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("wrap-result.json did not appear: %v", err)
	}

	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, h.taskID("job"), "accepted", 5*time.Second); err != nil {
		t.Fatalf("task did not reach accepted on named branch: %v", err)
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
