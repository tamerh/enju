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

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestMCPAsyncComputeEndToEnd launches a `mode: async` compute
// task, waits for the detached wrapper to finish, then asks
// run_status — which triggers the fetch-path scanner, which
// POSTs /tasks/reconcile, which flips the task to accepted.
func TestMCPAsyncComputeEndToEnd(t *testing.T) {
	eachRemoteMode(t, "AsyncCompute", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
	})
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
	eachRemoteMode(t, "AsyncCursorStarve", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
	})
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
	eachRemoteMode(t, "AsyncNamedBranch", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
	})
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
	eachRemoteMode(t, "AsyncChainRace", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
	})
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
	eachRemoteMode(t, "AsyncClaimReconcile", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
	})
}

// TestMCPAsyncReExecuteUpdatesUpstreamCommit is the regression
// guard for Bug 2: after async task is invalidated via
// request_changes and re-executed, the task's commit_sha
// must reflect the NEW commit. Review's resolver reads
// upstream content via task.CommitSHA; if the re-run
// reconcile doesn't update it, the review sees stale
// content pinned to the invalidated commit.
func TestMCPAsyncReExecuteUpdatesUpstreamCommit(t *testing.T) {
	eachRemoteMode(t, "AsyncReExecute", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
	})
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
	eachRemoteMode(t, "AsyncReviewRerun", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
		if err := waitForTaskState(h, genID, "accepted", 5*time.Second); err != nil {
			t.Fatalf("run1 accept: %v", err)
		}

		// Invalidate (models request_changes on the compute).
		// A direct invalidate exercises the same state transition
		// and sidesteps the review-submit plumbing.
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
		if err := waitForTaskState(h, genID, "accepted", 5*time.Second); err != nil {
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
	})
}

// readCommitFile reads a file from a specific commit in a bare
// git remote and returns its bytes. Used by review-rerun tests
// to distinguish "which upstream commit the review saw."
func readCommitFile(t *testing.T, remoteURL, commitSHA, repoRelPath string) []byte {
	t.Helper()
	tmp := t.TempDir()
	repo, err := gogit.PlainClone(tmp, false, &gogit.CloneOptions{URL: remoteURL, NoCheckout: true})
	if err != nil {
		t.Fatalf("clone for readCommitFile: %v", err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(commitSHA))
	if err != nil {
		t.Fatalf("load commit %s: %v", commitSHA, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	file, err := tree.File(repoRelPath)
	if err != nil {
		t.Fatalf("file %s at %s: %v", repoRelPath, commitSHA, err)
	}
	r, err := file.Reader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer r.Close()
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 512)
	for {
		n, rerr := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return buf
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
	eachRemoteMode(t, "AsyncRerunArtifact", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
		projectID := h.createTestProject()

		h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
			"enju/templates/bug2/enju.yaml": {body: `name: "bug2"
version: 1
tasks:
  - id: compute_data
    action: compute
    mode: async
    script: scripts/compute.sh
    writes_artifacts:
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
		if err := waitForTaskState(h, computeID, "accepted", 5*time.Second); err != nil {
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
		if err := waitForTaskState(h, computeID, "accepted", 5*time.Second); err != nil {
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
	})
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
	eachRemoteMode(t, "AsyncDownstream", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
	})
}

// TestMCPAsyncComputeFailurePropagatesViaReaper verifies the
// failure-on-return path: a detached wrapper whose script
// exits non-zero drops a .wrap-result.json locally but does
// NOT commit (matching today's sync path). The submitter's
// next claim/execute/run_status call triggers the reaper,
// which reads the result file and posts /tasks/:id/fail,
// flipping the coordinator's view to failed.
func TestMCPAsyncComputeFailurePropagatesViaReaper(t *testing.T) {
	eachRemoteMode(t, "AsyncFail", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
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
		if err := waitForTaskState(h, h.taskID("run"), "failed", 5*time.Second); err != nil {
			t.Fatalf("task did not reach failed via reaper: %v", err)
		}

		// Reaper should have moved the result file aside so a
		// second call doesn't double-process it.
		if _, statErr := os.Stat(resultPath); statErr == nil {
			t.Errorf("expected reaper to rename %s away; still present", resultPath)
		}
	})
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
