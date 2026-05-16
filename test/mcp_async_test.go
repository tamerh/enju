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

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// TestMCPAsyncComputeEndToEnd launches a `mode: async` compute
// task, waits for the detached wrapper to finish, then asks
// run_status — which triggers the fetch-path scanner, which
// POSTs /tasks/reconcile, which flips the task to accepted.
func TestMCPAsyncComputeEndToEnd(t *testing.T) {
	h := newMCPHarness(t, "AsyncCompute")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/async-ok/enju.yaml": {body: `name: "async ok"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    mode: async
`, mode: 0o644},
		"enju/templates/async-ok/scripts/run.sh": {body: `#!/bin/bash
echo "hello from async"
`, mode: 0o755},
	}, "seed async template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/async-ok",
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
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "run/.wrap-result.json"))
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
		"enju/templates/async-starve/enju.yaml": {body: `name: "async starve"
version: 1
tasks:
  - id: job
    action: compute
    script: scripts/job.sh
    mode: async
`, mode: 0o644},
		"enju/templates/async-starve/scripts/job.sh": {body: `#!/bin/bash
echo "async payload"
`, mode: 0o755},
	}, "seed async starve template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/async-starve",
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
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "job/.wrap-result.json"))
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
		"enju/templates/simple-async/enju.yaml": {body: `name: "simple-async"
version: 1
tasks:
  - id: job
    action: compute
    script: scripts/job.sh
    mode: async
`, mode: 0o644},
		"enju/templates/simple-async/scripts/job.sh": {body: `#!/bin/bash
echo "async payload on named branch"
`, mode: 0o755},
	}, "seed simple-async template")

	// branch:"auto" → allocates slug-N (e.g. simple-async-1).
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/simple-async",
		"branch":     "auto",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:job", projectID))

	res := h.call(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("job"),
	})
	if res.IsError {
		t.Fatalf("execute_task: %s", mcpText(res))
	}

	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "job/.wrap-result.json"))
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

// TestMCPAsyncChainStage2ClaimRaceOrphan reproduces the
// tester-reported async→async chain orphan: when the user
// calls enju_execute_task on stage2 immediately after stage1
// completes, stage2 may still be PENDING on the coordinator
// side (stage1's reconcile hasn't fired yet). handleExecuteTask
// then skips the claim (gated on state == ready|collecting),
// runs pullBranchWithReconcile which promotes stage2 → READY,
// and launches the wrapper anyway. The wrapper commits with an
// Enju-Task-Complete trailer, but the coordinator still sees
// stage2 in READY, not CLAIMED. The scanner's stale-trailer
// guard correctly noop's (state != claimed|running), the
// cursor advances past the trailer, and stage2 orphans in
// READY forever.
//
// Expected fix: move pullBranchWithReconcile BEFORE the claim
// check so the state read reflects post-reconcile truth.
// Alternatively, refuse to kickoff if the task wasn't
// successfully claimed. Either way: after execute_task on
// stage2, the task must reach ACCEPTED — not orphan.
func TestMCPAsyncChainStage2ClaimRaceOrphan(t *testing.T) {
	h := newMCPHarness(t, "AsyncChainRace")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/chain3/enju.yaml": {body: `name: "chain3"
version: 1
tasks:
  - id: stage1
    action: compute
    script: scripts/s.sh
    mode: async
  - id: stage2
    action: compute
    depends_on: [stage1]
    script: scripts/s.sh
    mode: async
`, mode: 0o644},
		"enju/templates/chain3/scripts/s.sh": {body: `#!/bin/bash
sleep 0.05
echo "out-$(date +%s%N)"
`, mode: 0o755},
	}, "seed chain3 template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/chain3",
	})
	stage1ID := fmt.Sprintf("%d:1:stage1", projectID)
	stage2ID := fmt.Sprintf("%d:1:stage2", projectID)
	h.rememberRunFromTaskID(t, stage1ID)

	// Run stage1 async.
	h.callOK(t, "enju_execute_task", map[string]any{"task_id": stage1ID})
	s1Result := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "stage1/.wrap-result.json"))
	if err := waitForFile(s1Result, 20*time.Second); err != nil {
		t.Fatalf("stage1 wrap-result: %v", err)
	}

	// Call execute_task on stage2 DIRECTLY without an
	// intervening run_status/claim/list_ready — the tester's
	// exact repro. At this moment, stage1's completion
	// commit exists on origin but the coordinator's state
	// for stage1 is still "claimed" (no reconcile has
	// fired), so stage2 is still "pending" (blocked on
	// stage1). handleExecuteTask runs reconcile *inside*
	// its pullBranchWithReconcile hook, which promotes
	// stage2 → ready — but by then the claim check has
	// already been skipped.
	res := h.call(t, "enju_execute_task", map[string]any{"task_id": stage2ID})
	if res.IsError {
		// Acceptable per the tester's suggested fix: refuse
		// to launch with a clear error instead of leaking a
		// wrapper. Don't assert on a specific error wording.
		t.Logf("execute_task(stage2) refused: %s (acceptable outcome)", mcpText(res))
		return
	}

	// If execute_task returned success, the task MUST
	// eventually reach accepted — otherwise we've leaked a
	// wrapper whose commit will never advance state.
	s2Result := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "stage2/.wrap-result.json"))
	if err := waitForFile(s2Result, 20*time.Second); err != nil {
		t.Fatalf("stage2 wrap-result never appeared — wrapper may have failed: %v", err)
	}
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, stage2ID, "accepted", 5*time.Second); err != nil {
		got, _ := h.store.GetTask(stage2ID)
		state := "<nil>"
		if got != nil {
			state = string(got.State)
		}
		t.Fatalf("stage2 stuck at state=%q after async chain — wrapper launched without successful claim, commit orphaned", state)
	}

}

// TestMCPAsyncClaimTriggersReconcile is the regression guard
// for Bug 1: after an async upstream completes, claiming the
// downstream directly (without a prior run_status call)
// should still succeed. Currently handleClaimTask POSTs the
// claim BEFORE it runs pullBranchWithReconcile, so the
// coordinator sees the downstream as still blocked (upstream
// state hasn't flipped yet) and rejects. The fix: reconcile
// first, then attempt the claim.
func TestMCPAsyncClaimTriggersReconcile(t *testing.T) {
	h := newMCPHarness(t, "AsyncClaimReconcile")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/chain2/enju.yaml": {body: `name: "chain2"
version: 1
tasks:
  - id: produce
    action: compute
    script: scripts/p.sh
    mode: async
  - id: consume
    action: answer
    depends_on: [produce]
    prompt: "consume {{produce.content}}"
`, mode: 0o644},
		"enju/templates/chain2/scripts/p.sh": {body: `#!/bin/bash
echo "payload-v1"
`, mode: 0o755},
	}, "seed chain2 template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/chain2",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:produce", projectID))

	res := h.call(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("produce"),
	})
	if res.IsError {
		t.Fatalf("execute: %s", mcpText(res))
	}

	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "produce/.wrap-result.json"))
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("wrap-result.json did not appear: %v", err)
	}

	// DELIBERATELY skip run_status. Go straight to claiming
	// the downstream — handleClaimTask's reconcile hook must
	// promote the upstream to accepted before the claim
	// attempt reaches the coordinator's blocked-check gate.
	claimRes := h.call(t, "enju_claim_task", map[string]any{
		"task_id": h.taskID("consume"),
	})
	if claimRes.IsError {
		t.Fatalf("claim on downstream failed — reconcile hook not running before claim POST: %s", mcpText(claimRes))
	}
	text := mcpText(claimRes)
	if strings.Contains(text, "blocked") || strings.Contains(text, "pending") {
		t.Fatalf("claim returned blocked/pending — reconcile hook not promoting upstream before claim:\n%s", text)
	}

}

// TestMCPAsyncReExecuteUpdatesUpstreamCommit is the regression
// guard for Bug 2: after async task is invalidated via
// request_changes and re-executed, the task's commit_sha
// must reflect the NEW commit. Review's resolver reads
// upstream content via task.CommitSHA; if the re-run
// reconcile doesn't update it, the review sees stale
// content pinned to the invalidated commit.
func TestMCPAsyncReExecuteUpdatesUpstreamCommit(t *testing.T) {
	h := newMCPHarness(t, "AsyncReExecute")
	projectID := h.createTestProject()

	// Script embeds a sequence marker so we can distinguish
	// run1 vs run2 outputs.
	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/rerun/enju.yaml": {body: `name: "rerun"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/g.sh
    mode: async
`, mode: 0o644},
		"enju/templates/rerun/scripts/g.sh": {body: `#!/bin/bash
# 50ms sleep + nanosecond timestamp guarantees run2's output
# differs from run1's even on fast systems. Without this the
# commit can return "nothing to commit" when content is
# byte-identical → wrapper fails → task marked failed → test
# flakes. Nanosecond resolution is best-effort in bash; the
# sleep absorbs that jitter.
sleep 0.05
echo "run-$(date +%s%N)"
`, mode: 0o755},
	}, "seed rerun template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/rerun",
	})
	taskID := fmt.Sprintf("%d:1:gen", projectID)
	h.rememberRunFromTaskID(t, taskID)

	// First execute.
	h.callOK(t, "enju_execute_task", map[string]any{"task_id": taskID})
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "gen/.wrap-result.json"))
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("run1 wrap-result.json: %v", err)
	}
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, taskID, "accepted", 5*time.Second); err != nil {
		t.Fatalf("run1 accept: %v", err)
	}
	task1, _ := h.store.GetTask(taskID)
	sha1 := task1.CommitSHA
	if sha1 == "" {
		t.Fatalf("run1 accepted but commit_sha is empty")
	}

	// Invalidate to force re-execution.
	h.callOK(t, "enju_invalidate_task", map[string]any{
		"task_id": taskID,
		"reason":  "want fresher data",
	})

	// Remove the old wrap-result so waitForFile can tell
	// run2's apart.
	_ = os.Remove(resultPath)
	_ = os.Remove(resultPath + ".done.json") // reaper may have renamed it

	// Re-execute. SEQ env changes output content so the new
	// commit's tree differs from the first.
	h.callOK(t, "enju_execute_task", map[string]any{"task_id": taskID})
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("run2 wrap-result.json: %v", err)
	}
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, taskID, "accepted", 5*time.Second); err != nil {
		t.Fatalf("run2 accept: %v", err)
	}

	task2, _ := h.store.GetTask(taskID)
	sha2 := task2.CommitSHA
	if sha2 == "" {
		t.Fatalf("run2 accepted but commit_sha is empty")
	}
	if sha2 == sha1 {
		t.Fatalf("task.CommitSHA still points at run1 %s after re-execute — reconcile didn't update on rerun", sha1)
	}

}

// TestMCPAsyncReviewSeesLatestUpstreamAfterRerun is Bug 2's
// real user-visible scenario: async compute submits,
// reviewer calls request_changes (bouncing both tasks back),
// async compute re-executes, reviewer re-claims. The review's
// claim payload MUST show the NEW commit's content, not the
// invalidated first-run content. Walks through the
// fetchAndResolveLocally resolver path that renders
// {{upstream.content}} from the upstream task's current
// task.CommitSHA.
func TestMCPAsyncReviewSeesLatestUpstreamAfterRerun(t *testing.T) {
	h := newMCPHarness(t, "AsyncReviewRerun")
	projectID := h.createTestProject()

	// Script uses SEQ env to distinguish run1 vs run2 output
	// so the review's rendered upstream content is
	// diagnosable. SEQ defaults to "v1"; we invalidate + pass
	// nothing different and rely on the timestamp commit in
	// metadata to differ. Simpler: script reads an env the
	// caller can set via task env: block (not needed for
	// this test — just record-anything).
	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/gate/enju.yaml": {body: `name: "gate"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/g.sh
    mode: async
  - id: gate
    action: review
    reviews: gen
    prompt: "Review {{gen.content}}"
`, mode: 0o644},
		"enju/templates/gate/scripts/g.sh": {body: `#!/bin/bash
echo "content-at-$(date +%s%N)"
`, mode: 0o755},
	}, "seed gate template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/gate",
	})
	genID := fmt.Sprintf("%d:1:gen", projectID)
	gateID := fmt.Sprintf("%d:1:gate", projectID)
	h.rememberRunFromTaskID(t, genID)

	// Run1 async compute.
	h.callOK(t, "enju_execute_task", map[string]any{"task_id": genID})
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "gen/.wrap-result.json"))
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("run1: %v", err)
	}
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	// Phase 8.3: gen has a downstream review (gate), so its
	// merge is suppressed at submit time and the task stays
	// in SUBMITTED until gate approves and /merges flips
	// both. Pre-Phase-8.3 the task went directly to ACCEPTED
	// at submit; the new "honest gate" semantic keeps it in
	// SUBMITTED so downstream readiness queries don't fan
	// out before the merge confirms.
	if err := waitForTaskState(h, genID, "submitted", 5*time.Second); err != nil {
		t.Fatalf("run1 accept: %v", err)
	}

	// Invalidate (models request_changes on the compute).
	// A direct invalidate exercises the same state transition
	// and sidesteps the review-submit plumbing. Phase 8.3
	// extended applySetTaskState's clear-claim precondition
	// to allow invalidating from SUBMITTED (was ACCEPTED|FAILED
	// only) so this still works post-8.3.
	h.callOK(t, "enju_invalidate_task", map[string]any{
		"task_id": genID,
		"reason":  "want rerun",
	})

	// Clean up run1's wrap-result so the next waitForFile
	// detects run2's.
	_ = os.Remove(resultPath)
	_ = os.Remove(resultPath + ".done.json")

	// Run2 async compute.
	h.callOK(t, "enju_execute_task", map[string]any{"task_id": genID})
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("run2: %v", err)
	}
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, genID, "submitted", 5*time.Second); err != nil {
		t.Fatalf("run2 accept: %v", err)
	}

	// Gen's commit should now be run2's.
	gen2, _ := h.store.GetTask(genID)
	run2SHA := gen2.CommitSHA

	// Claim the review. The rendered prompt carries the
	// upstream's content — which should be run2's, not
	// run1's. Bug 2 surfaces as the review seeing run1's
	// content despite run2 being the accepted commit.
	claim := h.callOK(t, "enju_claim_task", map[string]any{
		"task_id": gateID,
	})
	claimText := mcpText(claim)

	// The rendered prompt should embed run2's content. The
	// script echoes a nanosecond timestamp, so run1 and run2
	// produce DIFFERENT outputs.
	if !strings.Contains(claimText, "content-at-") {
		t.Fatalf("claim text missing content-at marker; got:\n%s", claimText)
	}
	// Read gen's result.md at run2's commit to know what we
	// expect. Then assert the review claim text contains it.
	remoteURL := h.remoteFor(projectID)
	run2Result := readCommitFile(t, remoteURL, run2SHA, filepath.Join(h.runDir(1), "gen/result.md"))
	run2Content := strings.TrimSpace(string(run2Result))
	if run2Content == "" {
		t.Fatalf("run2 result.md missing at %s", run2SHA)
	}
	if !strings.Contains(claimText, run2Content) {
		t.Fatalf("review claim shows stale run1 content, not run2's (%s). Claim text:\n%s", run2Content, claimText)
	}

}

// readCommitFile reads a file from a specific commit in a bare
// git remote and returns its bytes. Used by review-rerun tests
// to distinguish "which upstream commit the review saw."
func readCommitFile(t *testing.T, remoteURL, commitSHA, repoRelPath string) []byte {
	t.Helper()
	// `git -C <bare> cat-file -p <sha>:<path>` prints the blob
	// contents directly. NoCheckout-clone-then-walk-tree is
	// equivalent but slower; cat-file is one process.
	return []byte(gittest.Run(t, remoteURL, "cat-file", "-p", commitSHA+":"+repoRelPath))
}

// TestMCPAsyncRequestChangesRerunArtifactIndex is the tester's
// corrected Bug 2 repro: async compute with writes_artifacts,
// a review that calls request_changes, then async re-execute.
// After the re-run, the compute task's stored CommitSHA in the
// coordinator MUST be the new commit, not the invalidated
// first run. The tester originally reported this as a
// functional bug ("review sees stale content") but clarified
// it's actually a display bug: the worktree and preview show
// new content, but the DB's commit_sha for the compute task
// still points at the old (invalidated) commit. enju_get_task
// on the review then renders a stale "Commit: <sha>" label
// next to the otherwise-correct Preview.
//
// Root cause: PullBranch advances the local branch ref but
// doesn't refresh `refs/remotes/origin/<branch>`. The
// scanner reads the remote-tracking ref for `tip` — stale
// ref made it re-emit the old run-1 trailer after the
// review's submit (which advanced the cursor locally but
// left origin-tracking behind). The re-emit flipped
// task.CommitSHA back to the old value, then the real new
// trailer was rejected by the "already terminal at a
// different commit" guard. Fix: explicit FetchBranch in
// pullBranchWithReconcile so origin-tracking is current
// before the scan.
func TestMCPAsyncRequestChangesRerunArtifactIndex(t *testing.T) {
	h := newMCPHarness(t, "AsyncRerunArtifact")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/bug2/enju.yaml": {body: `name: "bug2"
version: 1
tasks:
  - id: compute_data
    action: compute
    mode: async
    script: scripts/compute.sh
    writes:
      - out/data.txt
  - id: review_data
    action: review
    reviews: compute_data
    prompt: "Review"
`, mode: 0o644},
		"enju/templates/bug2/scripts/compute.sh": {body: `#!/bin/bash
mkdir -p "$ENJU_PROJECT_DIR/out"
# 50ms sleep guarantees the timestamp differs from any previous
# run's output even on fast systems — nanosecond resolution is
# best-effort in bash and two back-to-back runs could collide
# without the padding. Much shorter than the tester's 3s sleep
# but still deterministic.
sleep 0.05
echo "computed-at-$(date +%s%N)" > "$ENJU_PROJECT_DIR/out/data.txt"
`, mode: 0o755},
	}, "seed bug2 template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/bug2",
		"branch":     "auto",
	})
	computeID := fmt.Sprintf("%d:1:compute_data", projectID)
	reviewID := fmt.Sprintf("%d:1:review_data", projectID)
	h.rememberRunFromTaskID(t, computeID)

	// Run1: async compute.
	h.callOK(t, "enju_execute_task", map[string]any{"task_id": computeID})
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "compute_data/.wrap-result.json"))
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("run1 wrap-result: %v", err)
	}
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	// Phase 8.3: compute_data has a downstream review, so its
	// merge is suppressed at submit time. Stays in SUBMITTED
	// until the review approves and /merges flips it; this
	// test exercises the request_changes path so the task
	// will be invalidated before reaching ACCEPTED.
	if err := waitForTaskState(h, computeID, "submitted", 5*time.Second); err != nil {
		t.Fatalf("run1 accept: %v", err)
	}
	compute1, _ := h.store.GetTask(computeID)
	shaA := compute1.CommitSHA

	// Review run1: request_changes.
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": reviewID})
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id":  reviewID,
		"decision": "request_changes",
		"content":  "rework please",
	})

	// Compute should now be READY again, CommitSHA cleared.
	computeAfterInvalidate, _ := h.store.GetTask(computeID)
	if computeAfterInvalidate.State != "ready" {
		t.Fatalf("compute not bounced to ready after request_changes: state=%s", computeAfterInvalidate.State)
	}

	_ = os.Remove(resultPath)
	_ = os.Remove(resultPath + ".done.json")

	// Run2: re-execute. Timestamp in the content will differ,
	// so sha_A ≠ sha_B.
	h.callOK(t, "enju_execute_task", map[string]any{"task_id": computeID})
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("run2 wrap-result: %v", err)
	}
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, computeID, "submitted", 5*time.Second); err != nil {
		t.Fatalf("run2 accept: %v", err)
	}
	compute2, _ := h.store.GetTask(computeID)
	shaB := compute2.CommitSHA
	if shaB == "" || shaB == shaA {
		t.Fatalf("compute.CommitSHA did not update after re-run reconcile: shaA=%s shaB=%s — scanner is re-posting old trailers, rejecting the new one via the terminal-state guard", shaA, shaB)
	}

	// Re-claim the review. After invalidation it's READY.
	claim := h.callOK(t, "enju_claim_task", map[string]any{"task_id": reviewID})
	claimText := mcpText(claim)

	// Bug 2 manifestation: claim should show shaB's content,
	// not shaA's. If the review's target-preview reads from
	// a stale workspace state OR from an out-of-date
	// task.CommitSHA, we'd see shaA's content here.
	remoteURL := h.remoteFor(projectID)
	newArtifact := readCommitFile(t, remoteURL, shaB, "out/data.txt")
	newContent := strings.TrimSpace(string(newArtifact))
	if newContent == "" {
		t.Fatalf("shaB has no out/data.txt artifact")
	}
	oldArtifact := readCommitFile(t, remoteURL, shaA, "out/data.txt")
	oldContent := strings.TrimSpace(string(oldArtifact))
	if oldContent == newContent {
		t.Fatalf("test setup: shaA and shaB have same content, timestamps should differ")
	}

	// Assert the new content appears somewhere in the claim
	// display (result.md preview or similar). The OLD content
	// must NOT appear — that's the stale-display bug.
	if strings.Contains(claimText, oldContent) {
		t.Errorf("review claim shows STALE run1 content %q — artifact index / target preview is stale after re-run reconcile.\nClaim:\n%s", oldContent, claimText)
	}

	// Also verify the artifact index on the coordinator side
	// points at shaB, not shaA. This is the specific field
	// the tester flagged as broken. Branch comes from the
	// run's record; compute_data was created on auto-branch.
	runs, _ := h.store.ListRunsByProject(projectID)
	if len(runs) == 0 {
		t.Fatal("no runs found")
	}
	artifact, err := h.store.GetArtifact(projectID, runs[0].Branch, "out/data.txt")
	if err != nil || artifact == nil {
		t.Fatalf("artifact index missing out/data.txt entry on %q: %v", runs[0].Branch, err)
	}
	if artifact.CommitSHA != shaB {
		t.Errorf("artifact index for out/data.txt points at %s, want shaB %s (shaA %s)", artifact.CommitSHA, shaB, shaA)
	}

}

// TestMCPAsyncReconcileUnblocksDownstream is the direct
// regression guard for the tester's "downstream stays
// blocked after async reconcile" report. When a normal sync
// submit accepts a task, the handler runs
// UpdateReadyTasks(runID) to promote any downstream whose
// upstream-deps are now all satisfied. The reconcile path
// (reconcileAcceptTask) forgot this sweep, so an async
// upstream that reconciled to "accepted" left its
// downstream tasks stuck in "pending" (shown as "blocked"
// by the claim gate) forever.
//
// Minimum repro: one async compute task plus one answer
// task depending on it. Execute the compute, wait for
// reconcile, assert the downstream becomes claimable.
func TestMCPAsyncReconcileUnblocksDownstream(t *testing.T) {
	h := newMCPHarness(t, "AsyncDownstream")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/chain/enju.yaml": {body: `name: "chain"
version: 1
tasks:
  - id: produce
    action: compute
    script: scripts/p.sh
    mode: async
  - id: consume
    action: answer
    depends_on: [produce]
    prompt: "consume {{produce.content}}"
`, mode: 0o644},
		"enju/templates/chain/scripts/p.sh": {body: `#!/bin/bash
echo "payload"
`, mode: 0o755},
	}, "seed chain template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/chain",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:produce", projectID))

	// Launch async upstream.
	res := h.call(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("produce"),
	})
	if res.IsError {
		t.Fatalf("execute: %s", mcpText(res))
	}

	// Wait for wrapper to commit.
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "produce/.wrap-result.json"))
	if err := waitForFile(resultPath, 20*time.Second); err != nil {
		t.Fatalf("wrap-result.json did not appear: %v", err)
	}

	// run_status triggers reconcile. Upstream flips to accepted.
	h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	if err := waitForTaskState(h, h.taskID("produce"), "accepted", 5*time.Second); err != nil {
		t.Fatalf("produce did not reach accepted: %v", err)
	}

	// Downstream MUST now be ready — the reconcile path has to
	// run the same ready-sweep the sync submit handler does.
	// Before the fix, it stays in "pending" and the claim
	// returns a blocked error.
	if err := waitForTaskState(h, h.taskID("consume"), "ready", 5*time.Second); err != nil {
		t.Fatalf("consume not promoted to ready after upstream reconcile — ready-sweep missing from reconcile path: %v", err)
	}

}

// TestMCPAsyncComputeFailureParksRetryable verifies the
// failure-on-return path end to end: a detached wrapper whose
// script exits non-zero drops a .wrap-result.json locally but
// does NOT commit. The submitter's next run_status call triggers
// the reconcile hook, which reads the result file and posts
// /tasks/:id/fail with kind=compute_error.
//
// Contract (changed by the non-terminal-compute-failure slice):
// a compute script error is RECOVERABLE, so the task parks in
// `failed_retryable` (run stays WAITING, operator retries via
// enju_retry_task) — NOT the terminal `failed` it used to flip
// to. This also exercises the async/reconcile wire end to end
// (kind=compute_error → FailComputeTaskRetryable). Run-state
// classification (failed_retryable ⇒ WAITING, incl. the leaf
// case) is unit-pinned in store.TestApplyCompleteRun_*.
func TestMCPAsyncComputeFailureParksRetryable(t *testing.T) {
	h := newMCPHarness(t, "AsyncFail")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/async-fail/enju.yaml": {body: `name: "async fail"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    mode: async
`, mode: 0o644},
		"enju/templates/async-fail/scripts/run.sh": {body: `#!/bin/bash
echo "something went wrong" >&2
exit 7
`, mode: 0o755},
	}, "seed async fail template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/async-fail",
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
	resultPath := filepath.Join(h.workspaceDirForProject(projectID), filepath.Join(h.runDir(1), "run/.wrap-result.json"))
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
	if err := waitForTaskState(h, h.taskID("run"), "failed_retryable", 5*time.Second); err != nil {
		t.Fatalf("async compute failure must park as failed_retryable (recoverable), not terminal failed: %v", err)
	}

	// Reaper should have moved the result file aside so a
	// second call doesn't double-process it.
	if _, statErr := os.Stat(resultPath); statErr == nil {
		t.Errorf("expected reaper to rename %s away; still present", resultPath)
	}

}

// TestMCPRetryTaskRecoversFailedCompute is the end-to-end proof
// of Slice 3 composed with Slices 1–2: a sync compute task whose
// script errors parks failed_retryable (not terminal), and
// enju_retry_task re-opens AND re-runs it in one call, driving it
// to accepted. The script fails its first attempt (no marker) and
// succeeds on the second (marker present, in the stable
// ENJU_PROJECT_DIR — not per-iter scratch) — exactly the
// transient-failure case from=snapshot exists for: re-run the
// pinned script unchanged.
//
// It also pins the cross-slice trackability guarantee: the failed
// attempt and the successful retry are TWO distinct iterations
// (iter-1 failed → MarkOpenClaimsFailed closed it → retry
// re-claim advanced iter_seq), so the history shows every attempt.
func TestMCPRetryTaskRecoversFailedCompute(t *testing.T) {
	h := newMCPHarness(t, "RetryTask")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/flaky/enju.yaml": {body: `name: "flaky"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    prompt: "flaky compute"
`, mode: 0o644},
		"enju/templates/flaky/scripts/run.sh": {body: `#!/bin/bash
# ENJU_REPO_DIR is the per-RUN snapshot dir — stable across
# iterations (unlike ENJU_PROJECT_DIR, which is per-iter scratch)
# and NOT re-materialized on a from=snapshot retry, so a marker
# written on attempt 1 survives to attempt 2. This models a
# transient failure that clears on retry without any code change.
M="$ENJU_REPO_DIR/.attempt_marker"
if [ -f "$M" ]; then
  echo "recovered on retry"
  exit 0
fi
touch "$M"
echo "transient failure" >&2
exit 1
`, mode: 0o755},
	}, "seed flaky template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/flaky",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:run", projectID))

	// First attempt: exit 1 → parks failed_retryable (Slice 2),
	// not terminal failed.
	res := h.call(t, "enju_execute_task", map[string]any{"task_id": h.taskID("run")})
	if !strings.Contains(mcpText(res), "failed") {
		t.Fatalf("first attempt should report failure, got:\n%s", mcpText(res))
	}
	if err := waitForTaskState(h, h.taskID("run"), "failed_retryable", 5*time.Second); err != nil {
		t.Fatalf("first failure must park failed_retryable: %v", err)
	}

	// enju_retry_task from=snapshot: same pinned script, but the
	// marker now exists → exit 0. One call re-opens (Slice 3) and
	// re-executes.
	rres := h.call(t, "enju_retry_task", map[string]any{
		"task_id": h.taskID("run"),
		"from":    "snapshot",
	})
	if rres.IsError {
		t.Fatalf("enju_retry_task errored: %s", mcpText(rres))
	}
	if !strings.Contains(mcpText(rres), "Retrying") {
		t.Errorf("retry response missing header, got:\n%s", mcpText(rres))
	}

	// The retried attempt succeeds → single task auto-submits →
	// accepted.
	if err := waitForTaskState(h, h.taskID("run"), "accepted", 8*time.Second); err != nil {
		t.Fatalf("retry should drive the task to accepted: %v", err)
	}

	// Trackability: failed attempt and successful retry are two
	// distinct iterations.
	iters := mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
		"task_id": h.taskID("run"),
	}))
	if !strings.Contains(iters, "iter-1") || !strings.Contains(iters, "iter-2") {
		t.Errorf("expected iter-1 (failed) AND iter-2 (retry) in iteration history, got:\n%s", iters)
	}

	// Guard against terminal-failed regression: a failed_retryable
	// retry must not have left a terminal `failed` anywhere.
	if strings.Contains(iters, "iter-3") {
		t.Errorf("unexpected third iteration — retry should be exactly one re-attempt:\n%s", iters)
	}
}

// TestMCPRetryFromHeadPicksUpCommittedFix is the from=head e2e —
// the default mode and the entire point of the feature, previously
// verified only by reading RetryComputeTask. It proves the from
// axis actually BIFURCATES behavior on the *same* committed state:
//
//  1. broken script (exit 1) → create_run → execute → failed_retryable
//  2. operator commits a FIXED script to the run branch (modelled
//     by a detached worktree + update-ref on the workspace clone —
//     LocalBranchHash reads local refs and retry does not fetch,
//     so this mirrors the real single-machine flow: fix locally,
//     retry)
//  3. retry from=SNAPSHOT → re-runs the FROZEN pinned script →
//     still fails (proves snapshot ignores the new commit)
//  4. retry from=HEAD → re-materializes the run-branch tip → runs
//     the FIXED script → accepted (proves head picks it up)
//
// A regression that reverted RetryComputeTask's explicit
// MaterializeRunRepo back to materialize-if-absent would pass the
// from=snapshot test but FAIL step 4 here.
func TestMCPRetryFromHeadPicksUpCommittedFix(t *testing.T) {
	h := newMCPHarness(t, "RetryHead")
	projectID := h.createTestProject()

	const scriptRel = "enju/templates/headfix/scripts/run.sh"
	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/headfix/enju.yaml": {body: `name: "headfix"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    prompt: "headfix"
`, mode: 0o644},
		scriptRel: {body: "#!/bin/bash\necho 'broken' >&2\nexit 1\n", mode: 0o755},
	}, "seed broken script")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/headfix",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:run", projectID))

	// 1. First attempt fails → failed_retryable.
	h.call(t, "enju_execute_task", map[string]any{"task_id": h.taskID("run")})
	if err := waitForTaskState(h, h.taskID("run"), "failed_retryable", 5*time.Second); err != nil {
		t.Fatalf("first attempt must park failed_retryable: %v", err)
	}

	// 2. Operator commits a FIXED script onto the run branch in
	// the workspace clone. Detached worktree + update-ref so it
	// works regardless of which branch the live workspace has
	// checked out, and MaterializeRunRepo reads git objects (not
	// the worktree) so the moved ref is exactly what it resolves.
	runBranch, _ := h.get(fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, 1))["branch"].(string)
	if runBranch == "" {
		t.Fatal("run 1 has no branch on the coordinator record")
	}
	ws := h.workspaceDirForProject(projectID)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, ws, "worktree", "add", "--detach", wt, "refs/heads/"+runBranch)
	if err := os.WriteFile(filepath.Join(wt, scriptRel),
		[]byte("#!/bin/bash\necho 'FIXED-SCRIPT-RAN'\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fixed script: %v", err)
	}
	gittest.Run(t, wt, "add", scriptRel)
	gittest.Run(t, wt, "-c", "user.email=op@test", "-c", "user.name=op",
		"commit", "-m", "operator fixes the script on the run branch")
	newSHA := strings.TrimSpace(gittest.Run(t, wt, "rev-parse", "HEAD"))
	gittest.Run(t, ws, "update-ref", "refs/heads/"+runBranch, newSHA)
	gittest.Run(t, ws, "worktree", "remove", "--force", wt)

	// 3. from=SNAPSHOT ignores the new commit — frozen pinned
	// script still exits 1, task stays failed_retryable.
	snapRes := h.call(t, "enju_retry_task", map[string]any{
		"task_id": h.taskID("run"), "from": "snapshot",
	})
	if snapRes.IsError {
		t.Fatalf("retry from=snapshot transport error: %s", mcpText(snapRes))
	}
	if err := waitForTaskState(h, h.taskID("run"), "failed_retryable", 5*time.Second); err != nil {
		t.Fatalf("from=snapshot must re-run the FROZEN broken script and stay failed_retryable "+
			"(if this passed, snapshot wrongly picked up the branch fix): %v", err)
	}

	// 4. from=HEAD re-materializes the run-branch tip → the FIXED
	// script runs → accepted.
	headRes := h.call(t, "enju_retry_task", map[string]any{
		"task_id": h.taskID("run"), "from": "head",
	})
	if headRes.IsError {
		t.Fatalf("retry from=head transport error: %s", mcpText(headRes))
	}
	if err := waitForTaskState(h, h.taskID("run"), "accepted", 8*time.Second); err != nil {
		t.Fatalf("from=head must re-materialize the committed fix and succeed — "+
			"if this fails, RetryComputeTask is not refreshing the snapshot from the branch tip: %v", err)
	}
}

// TestMCPFailedComputePreservesScratchAndTailsStderr pins Slice 4
// ("don't fly blind"). Two regressions, one test:
//
//	(a) the wrapper used to wipe the task scratch dir on a script
//	    failure (it only preserved when a git submit ALSO failed),
//	    so the very state needed to debug was deleted. Now any
//	    non-clean run preserves scratch (the 24h startup sweep is
//	    the TTL). The sentinel the failing script writes into its
//	    CWD (= scratch) must survive.
//
//	(b) fail_reason captured stderr[:N] — the HEAD (startup noise)
//	    — and threw away the tail where the actual error is. Now
//	    it keeps the tail. The script emits 300 noise lines then
//	    the real error LAST.
func TestMCPFailedComputePreservesScratchAndTailsStderr(t *testing.T) {
	h := newMCPHarness(t, "FailScratch")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju/templates/boom/enju.yaml": {body: `name: "boom"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    prompt: "boom"
`, mode: 0o644},
		"enju/templates/boom/scripts/run.sh": {body: `#!/bin/bash
# CWD is the task scratch dir — drop a sentinel so the test can
# prove scratch survived the failure.
touch sentinel-marker.txt
for i in $(seq 1 300); do
  echo "noise-line-$i: routine progress chatter, not the error" >&2
done
echo "FATAL: the actual root cause is right here" >&2
exit 1
`, mode: 0o755},
	}, "seed boom template")

	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju/templates/boom",
	})
	h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:run", projectID))

	res := h.call(t, "enju_execute_task", map[string]any{"task_id": h.taskID("run")})
	txt := mcpText(res)
	if !strings.Contains(txt, "Script failed") {
		t.Fatalf("expected failure report, got:\n%s", txt)
	}
	if !strings.Contains(txt, "Scratch (preserved for inspection") {
		t.Errorf("failure output must point at the preserved scratch dir, got:\n%s", txt)
	}
	if err := waitForTaskState(h, h.taskID("run"), "failed_retryable", 5*time.Second); err != nil {
		t.Fatalf("script failure must park failed_retryable: %v", err)
	}

	// (a) Scratch preserved: the sentinel the failing script wrote
	// into its CWD must still be on disk.
	root := h.workspaceDirForProject(projectID)
	var sentinel string
	filepath.Walk(filepath.Join(root, ".enju", "bots"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi != nil && !fi.IsDir() && filepath.Base(p) == "sentinel-marker.txt" {
			sentinel = p
		}
		return nil
	})
	if sentinel == "" {
		t.Errorf("scratch wiped on script failure — sentinel-marker.txt missing under %s/.enju/bots (Slice 4 regression)", root)
	}

	// (b) fail_reason carries the TAIL of stderr, not the head.
	task := h.get("/api/v1/tasks/" + h.taskID("run"))
	fr, _ := task["fail_reason"].(string)
	if !strings.Contains(fr, "FATAL: the actual root cause is right here") {
		t.Errorf("fail_reason must retain the final stderr line (the real error), got:\n%s", fr)
	}
	if !strings.Contains(fr, "...(truncated)") {
		t.Errorf("a long stderr must be marked truncated, got:\n%s", fr)
	}
	if strings.Contains(fr, "noise-line-1:") {
		t.Errorf("fail_reason kept the HEAD noise instead of the tail (pre-Slice-4 behavior):\n%s", fr)
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
	proj, err := h.enjugit.ForProject(projectID, h.remoteFor(projectID))
	if err != nil {
		h.t.Fatalf("opening project %d workspace: %v", projectID, err)
	}
	return proj.WorkDir()
}

// Keep context.Context import live; helpers may gain ctx-aware
// variants in follow-up.
var _ = context.Background
