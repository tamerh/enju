package test

// Integration tests for enju_execute_run — the batch tool that
// drains ready compute tasks in a run until it hits the next
// citizen gate, a failure, or the safety cap.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMCPExecuteRunAllComputeCascades: a run where every task
// is action:compute should drain to terminal in one call. No
// human intervention; stop_reason=no_ready_compute.
func TestMCPExecuteRunAllComputeCascades(t *testing.T) {
	h := newMCPHarness(t, "ExecRunAll")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"scripts/say.sh": {body: `#!/bin/bash
echo "did $ENJU_TASK_ID"
`, mode: 0o755},
	}, "seed script")

	yaml := `name: "all-compute"
version: 1
tasks:
  - id: seed
    action: compute
    script: scripts/say.sh
  - id: mid
    action: compute
    script: scripts/say.sh
    depends_on: [seed]
  - id: tail
    action: compute
    script: scripts/say.sh
    depends_on: [mid]
`
	h.mcpCreateRunInline(t, projectID, yaml)

	res := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text := mcpText(res)
	if !strings.Contains(text, "Stop reason: no_ready_compute") {
		t.Fatalf("expected stop_reason=no_ready_compute, got:\n%s", text)
	}
	// All three tasks should have completed — look for the
	// per-entry lines.
	for _, id := range []string{"seed", "mid", "tail"} {
		full := fmt.Sprintf("%d:1:%s", projectID, id)
		if !strings.Contains(text, "✓ "+full) {
			t.Errorf("expected completion entry for %s, got:\n%s", id, text)
		}
	}
}

// TestMCPExecuteRunStopsAtCitizenTask: a compute→answer→compute
// pipeline pauses at the answer task with stop_reason=
// citizen_task_ready and names the blocker. After the citizen
// submits, a second execute_run call resumes the cascade.
func TestMCPExecuteRunStopsAtCitizenTask(t *testing.T) {
	h := newMCPHarness(t, "ExecRunStopAtCitizen")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"scripts/say.sh": {body: `#!/bin/bash
echo "did $ENJU_TASK_ID"
`, mode: 0o755},
	}, "seed script")

	yaml := `name: "mixed"
version: 1
tasks:
  - id: prep
    action: compute
    script: scripts/say.sh
  - id: humgate
    action: answer
    prompt: "decide"
    depends_on: [prep]
  - id: wrap
    action: compute
    script: scripts/say.sh
    depends_on: [humgate]
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// First call: should execute prep, then stop at humgate.
	res := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text := mcpText(res)
	if !strings.Contains(text, "Stop reason: citizen_task_ready") {
		t.Fatalf("expected stop_reason=citizen_task_ready, got:\n%s", text)
	}
	humgateID := fmt.Sprintf("%d:1:humgate", projectID)
	if !strings.Contains(text, "next_blocker="+humgateID) {
		t.Errorf("expected next_blocker=%s, got:\n%s", humgateID, text)
	}
	prepID := fmt.Sprintf("%d:1:prep", projectID)
	if !strings.Contains(text, "✓ "+prepID) {
		t.Errorf("expected prep to have executed, got:\n%s", text)
	}
	if strings.Contains(text, fmt.Sprintf("%d:1:wrap", projectID)) {
		t.Errorf("wrap should not have executed before human gate, got:\n%s", text)
	}

	// Human submits. Then a second execute_run resumes the
	// cascade through wrap.
	h.mcpClaimOK(t, "humgate")
	h.mcpSubmitText(t, "humgate", "decided")

	res2 := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text2 := mcpText(res2)
	wrapID := fmt.Sprintf("%d:1:wrap", projectID)
	if !strings.Contains(text2, "✓ "+wrapID) {
		t.Fatalf("expected wrap to execute after humgate submit, got:\n%s", text2)
	}
	if !strings.Contains(text2, "Stop reason: no_ready_compute") {
		t.Errorf("expected stop_reason=no_ready_compute after wrap, got:\n%s", text2)
	}
}

// TestMCPExecuteRunStopsOnFailure: a compute task that exits
// non-zero halts the cascade. Downstream tasks stay PENDING;
// stop_reason=compute_failed and the failing task is in the
// entries list with status=failed.
func TestMCPExecuteRunStopsOnFailure(t *testing.T) {
	h := newMCPHarness(t, "ExecRunFailStops")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"scripts/ok.sh": {body: `#!/bin/bash
echo ok
`, mode: 0o755},
		"scripts/boom.sh": {body: `#!/bin/bash
echo "script went wrong" >&2
exit 7
`, mode: 0o755},
	}, "seed scripts")

	yaml := `name: "fail-mid"
version: 1
tasks:
  - id: pre
    action: compute
    script: scripts/ok.sh
  - id: mid
    action: compute
    script: scripts/boom.sh
    depends_on: [pre]
  - id: post
    action: compute
    script: scripts/ok.sh
    depends_on: [mid]
`
	h.mcpCreateRunInline(t, projectID, yaml)

	res := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text := mcpText(res)
	if !strings.Contains(text, "Stop reason: compute_failed") {
		t.Fatalf("expected stop_reason=compute_failed, got:\n%s", text)
	}
	midID := fmt.Sprintf("%d:1:mid", projectID)
	if !strings.Contains(text, "✗ "+midID) {
		t.Errorf("expected failed entry for mid, got:\n%s", text)
	}
	postID := fmt.Sprintf("%d:1:post", projectID)
	if strings.Contains(text, postID) {
		t.Errorf("post should not have been attempted after mid failed, got:\n%s", text)
	}
}

// TestMCPExecuteRunMaxTasksCap: max_tasks=1 on a 3-compute run
// stops after one execution with stop_reason=max_tasks. Calling
// again picks up where it left off.
func TestMCPExecuteRunMaxTasksCap(t *testing.T) {
	h := newMCPHarness(t, "ExecRunMaxCap")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"scripts/say.sh": {body: `#!/bin/bash
echo did
`, mode: 0o755},
	}, "seed script")

	yaml := `name: "cap"
version: 1
tasks:
  - id: a
    action: compute
    script: scripts/say.sh
  - id: b
    action: compute
    script: scripts/say.sh
    depends_on: [a]
  - id: c
    action: compute
    script: scripts/say.sh
    depends_on: [b]
`
	h.mcpCreateRunInline(t, projectID, yaml)

	res := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
		"max_tasks":  float64(1),
	})
	text := mcpText(res)
	if !strings.Contains(text, "Stop reason: max_tasks") {
		t.Fatalf("expected stop_reason=max_tasks, got:\n%s", text)
	}
	aID := fmt.Sprintf("%d:1:a", projectID)
	if !strings.Contains(text, "✓ "+aID) {
		t.Errorf("expected a to complete, got:\n%s", text)
	}
	if strings.Contains(text, fmt.Sprintf("%d:1:b", projectID)) {
		t.Errorf("b should not have executed under cap=1, got:\n%s", text)
	}

	// Second call — should pick up b, and drain through c.
	res2 := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text2 := mcpText(res2)
	if !strings.Contains(text2, fmt.Sprintf("✓ %d:1:b", projectID)) {
		t.Errorf("expected b to complete on second call, got:\n%s", text2)
	}
	if !strings.Contains(text2, fmt.Sprintf("✓ %d:1:c", projectID)) {
		t.Errorf("expected c to complete on second call, got:\n%s", text2)
	}
}

// TestMCPExecuteRunSkipsAssignedElsewhere: a compute task with
// assign_to set to another citizen is treated as a blocker —
// stop_reason=compute_assigned_elsewhere — so the caller
// doesn't silently execute work that was explicitly scoped to
// someone else.
func TestMCPExecuteRunSkipsAssignedElsewhere(t *testing.T) {
	h := newMCPHarness(t, "Alice")
	// Register a second citizen so the assign_to validator
	// accepts the reference. Keep the name slug-clean so the
	// server's username matches the display name byte-for-byte
	// (the registration endpoint may slug-transform mixed-case
	// names, which would make the assign_to lookup miss).
	bobUsername := h.register("bob")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"scripts/say.sh": {body: `#!/bin/bash
echo did
`, mode: 0o755},
	}, "seed script")

	yaml := fmt.Sprintf(`name: "assigned"
version: 1
tasks:
  - id: only_bob
    action: compute
    script: scripts/say.sh
    assign_to: [%q]
`, bobUsername)
	h.mcpCreateRunInline(t, projectID, yaml)

	// Caller is ExecRunAssignSkip, not bob. Task is eligible
	// only for bob, so execute_run can't advance it.
	res := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text := mcpText(res)
	if !strings.Contains(text, "Stop reason: compute_assigned_elsewhere") {
		t.Fatalf("expected stop_reason=compute_assigned_elsewhere, got:\n%s", text)
	}
	if !strings.Contains(text, "next_blocker="+fmt.Sprintf("%d:1:only_bob", projectID)) {
		t.Errorf("expected blocker to name the assigned task, got:\n%s", text)
	}
}

// TestMCPExecuteRunNonexistentRun: calling with a run_id that
// doesn't exist surfaces "run not found" rather than the
// misleading "idle run" the main loop would produce for an
// empty /ready on a bogus id.
func TestMCPExecuteRunNonexistentRun(t *testing.T) {
	h := newMCPHarness(t, "ExecRunBogus")
	projectID := h.createTestProject()

	res, err := h.client.Call(context.Background(), "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(9999),
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected tool error for nonexistent run, got success:\n%s", mcpText(res))
	}
	msg := mcpText(res)
	if !strings.Contains(msg, "not found") {
		t.Errorf("expected 'not found' in error, got: %s", msg)
	}
}

// TestMCPExecuteRunEmpty: a run with no ready tasks (e.g. all
// accepted) reports no_ready_compute with zero entries. The
// tool is safe to call on an idle run.
func TestMCPExecuteRunEmpty(t *testing.T) {
	h := newMCPHarness(t, "ExecRunIdle")
	projectID := h.createTestProject()

	yaml := `name: "single"
version: 1
tasks:
  - id: only
    action: answer
    prompt: "x"
`
	h.mcpCreateRunInline(t, projectID, yaml)
	// Handle the one answer task so nothing remains ready.
	h.mcpClaimOK(t, "only")
	h.mcpSubmitText(t, "only", "done")

	res, err := h.client.Call(context.Background(), "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	if err != nil || res.IsError {
		t.Fatalf("execute_run on idle run: err=%v body=%s", err, mcpText(res))
	}
	text := mcpText(res)
	if !strings.Contains(text, "0 executed") {
		t.Errorf("expected 0 executed on idle run, got:\n%s", text)
	}
	if !strings.Contains(text, "Stop reason: no_ready_compute") {
		t.Errorf("expected stop_reason=no_ready_compute on idle run, got:\n%s", text)
	}
}

// TestMCPExecuteRunStopsOnGitFailure is the regression for TP53
// Bug 3a: when the script runs successfully but the post-script
// git push fails (e.g. "object not found" on a freshly-added
// remote), the cascade must surface stop_reason=
// git_operation_failed — distinct from compute_failed (script
// non-zero) and compute_errored (wrapper-level failure) — so the
// user knows the work product is on disk and the recovery is
// fix-the-git-state, not re-run-the-script.
//
// We provoke the failure by removing the project's bare remote
// directory between create_run and execute_run, so the push
// inside the wrapper hits a non-existent remote.
func TestMCPExecuteRunStopsOnGitFailure(t *testing.T) {
	h := newMCPHarness(t, "ExecRunGitFail")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"scripts/say.sh": {body: `#!/bin/bash
echo "ok"
`, mode: 0o755},
	}, "seed script")

	yaml := `name: "git-fail"
version: 1
tasks:
  - id: only
    action: compute
    script: scripts/say.sh
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// Touch the workspace via run_status so the project is
	// cloned into c.workspace before we sabotage the bare. If
	// we delete the bare too early, the first ForProject call
	// inside execute_run hits "clone failed" — that's a
	// legitimate failure mode but a different one than the
	// post-script push failure we're reproducing here.
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})

	// Sabotage the remote: nuke the bare repo directory so the
	// post-script push fails. Script will still run fine — it
	// only writes to the local workspace clone — but the push
	// to origin won't find the remote.
	bareURL := h.remoteFor(projectID)
	if bareURL == "" {
		t.Fatalf("no remote URL for project %d", projectID)
	}
	if err := os.RemoveAll(bareURL); err != nil {
		t.Fatalf("removing bare to force git failure: %v", err)
	}

	res := h.callOK(t, "enju_execute_run", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text := mcpText(res)
	if !strings.Contains(text, "Stop reason: git_operation_failed") {
		t.Fatalf("expected stop_reason=git_operation_failed, got:\n%s", text)
	}
	// The git_failed icon distinguishes this from script failure.
	onlyID := fmt.Sprintf("%d:1:only", projectID)
	if !strings.Contains(text, "✗git "+onlyID) {
		t.Errorf("expected ✗git entry for %s, got:\n%s", onlyID, text)
	}
	// Recovery hint should mention the git layer + project_remote_status.
	if !strings.Contains(text, "git layer") {
		t.Errorf("expected recovery hint to cite the git layer, got:\n%s", text)
	}
	// Hint must NOT suggest enju_invalidate_task — the task is
	// still in `claimed`, and invalidate only operates on
	// accepted tasks. Following that suggestion would dead-end
	// the user.
	if strings.Contains(text, "enju_invalidate_task") {
		t.Errorf("recovery hint should not point at enju_invalidate_task (task is in claimed state, invalidate would error); got:\n%s", text)
	}
}
