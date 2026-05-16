package service

// Fetch-path reconciliation: turns wrap-task outcomes into
// coordinator state transitions. Two parallel paths:
//
//   - Trailer scan: walks new commits on the run branch via
//     enjugit's FetchBranch + ScanBranchSince + Cursors and
//     posts /tasks/reconcile for the trailers it finds. Catches
//     commits that landed directly on the run branch (legacy
//     and review/vote paths).
//   - Wrapper-result reaper: walks `.wrap-result.json` files that
//     async wrap-task subprocesses leave behind. On success
//     POSTs /tasks/:id/result + applies the auto-merge plan so
//     the run branch FFs to include the topic-branch commit
//     (the trailer scanner can't see topic-only commits on its
//     own); on failure POSTs /tasks/:id/fail.
//
// Lives on FatClient because every per-tool service call that
// touches a project's branch wants the same "freshen + sweep"
// semantics. Hook points: places the fat client naturally touches
// git + talks to the coordinator already, so reconciliation adds
// no new round trips on the fast path.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/executor"
)

// StateDir returns the directory used for per-project cursor
// files. Derived from the workspace root so production
// installs put state next to other fat-client housekeeping
// (post-NDW.6 the workspace root defaults to ~/.enju/, so
// state lives at ~/.enju/.state/). Tests get isolated state
// per t.TempDir() run.
//
// Falls back to ~/.enju/state/ only when no workspace is
// configured (local-only / legacy callers).
func (s *FatClient) StateDir() string {
	if s.enjugit != nil {
		return filepath.Join(s.enjugit.RootDir(), ".state")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".enju", "state")
}

// PullBranchWithReconcileWF is the enjugit-Workflow flavored
// counterpart to PullBranchWithReconcile. Same semantics:
// checkout + pull + fetch + scan + reconcile-post + cursor
// advance, plus wrapper-failure reap. Used by claim.go's
// fat-client paths after the port to enjugit.
//
// Mirrors PullBranchWithReconcile step-for-step but on a
// *enjugit.Workflow handle, using enjugit's CursorMutexFor /
// LoadCursors and the Workflow's PullBranch / FetchBranch /
// ScanBranchSince / LocalBranchHash.
//
// Errors: same contract as the project version. Pull error is
// the only thing surfaced; scanner / reconcile-post / reaper
// failures are logged at Debug.
func (s *FatClient) PullBranchWithReconcileWF(ctx context.Context, wf *enjugit.Workflow, projectID int64, branch string) error {
	if wf == nil {
		return nil
	}
	if branch != "" {
		if err := wf.CheckoutBranch(branch); err != nil {
			return fmt.Errorf("switching workspace to branch %q: %w", branch, err)
		}
	}
	pullErr := wf.PullBranch(branch)
	if branch != "" {
		_ = wf.FetchBranch(branch)
	}
	var trailers []enjugit.CommitTrailer
	var newTip string
	var preCursor string
	if branch != "" {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := enjugit.LoadCursors(stateDir, projectID)
		preCursor = cursors.Get(branch)
		// Persist-on-first-touch — same baseline-seeding rationale
		// as the project-flavored version. See its comment block.
		if preCursor == "" {
			if h, herr := wf.LocalBranchHash(branch); herr == nil && h != "" {
				preCursor = h
				cursors.Set(branch, h)
				_ = cursors.Save()
			}
		}
		cursorMu.Unlock()
		res, serr := wf.ScanBranchSince(branch, preCursor)
		if serr != nil {
			s.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
		} else {
			newTip = res.NewTip
			trailers = res.Trailers
		}
	}

	// Network phase: POST /tasks/reconcile.
	if len(trailers) > 0 {
		body := buildReconcileBodyWF(trailers)
		if _, perr := s.coord.Post(ctx, "/api/v1/tasks/reconcile", body); perr != nil {
			s.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			s.ReapWrapperFailuresWF(ctx, wf)
			return pullErr
		}
	}

	// Cursor-save phase.
	if newTip != "" && newTip != preCursor {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		latest, _ := enjugit.LoadCursors(stateDir, projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}

	s.ReapWrapperFailuresWF(ctx, wf)
	return pullErr
}

// buildReconcileBodyWF mirrors BuildReconcileBody but on
// enjugit.CommitTrailer (vs project.CommitTrailer). Same
// payload shape — keep in sync if either side changes.
func buildReconcileBodyWF(trailers []enjugit.CommitTrailer) map[string]interface{} {
	entries := make([]map[string]interface{}, 0, len(trailers))
	for _, t := range trailers {
		entry := map[string]interface{}{
			"task_id":    t.Trailers.TaskID,
			"commit_sha": t.CommitSHA,
		}
		if t.Trailers.ExitSet {
			entry["exit_code"] = t.Trailers.ExitCode
		}
		combined := append([]string(nil), t.Trailers.Artifacts...)
		combined = append(combined, t.Trailers.UntrackedArtifacts...)
		if len(combined) > 0 {
			entry["artifacts_written"] = combined
		}
		entries = append(entries, entry)
	}
	return map[string]interface{}{"tasks": entries}
}

// ReapWrapperFailuresWF is the enjugit-Workflow flavored
// counterpart to ReapWrapperFailures — walks the workflow's
// worktree for .wrap-result.json files and posts /tasks/:id/fail
// for non-zero exits, /tasks/:id/result + applyAcceptedMerges
// for successes.
//
// The success branch handles the parallel-compute topic-branch
// flow: wrap-task pushed the commit to a per-task topic ref,
// but nothing on the local clone has FF'd the run branch up to
// it yet (sync compute does that via the /result response).
// We re-run the same dance here from the parent: POST /result
// with the wrap-task's commit_sha → coord returns
// accepted_merges → applyAcceptedMerges advances the run branch
// locally and pushes it. Without this step the trailer scanner
// would walk the run branch and never see the topic-branch
// commit.
func (s *FatClient) ReapWrapperFailuresWF(ctx context.Context, wf *enjugit.Workflow) {
	if wf == nil {
		return
	}
	root := filepath.Join(wf.WorkDir(), corelayout.RunStateRunsRoot())
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) != ".wrap-result.json" {
			return nil
		}
		s.handleOneWrapperResult(ctx, path, wf)
		return nil
	})
	// Same sweep, second pass: SLURM jobs whose .wrap-job.json
	// exists but whose .wrap-result.json doesn't yet — the walk
	// above can't see a still-queued job. Pull-based, no daemon.
	s.reapSlurmSidecars(ctx, wf)
}

// reapSlurmSidecars polls sacct for every outstanding SLURM
// job sidecar and drives terminal ones to /result or /fail.
// Local sidecars are skipped here — the .wrap-result.json walk
// already reaps them; the local sidecar exists only so
// enju_terminate_run can Cancel an in-flight PID.
//
// Discovery keys off .wrap-job.json because a queued job has no
// .wrap-result.json yet. For each, gated by the
// .wrap-result.done.json marker handleOneWrapperResult writes:
//
//   - queued / running        → skip, next sweep retries
//   - terminal, result on disk → handleOneWrapperResult (does the
//     host-side commit for the deferred result, posts, renames)
//   - terminal, NO result      → the node died before writing one:
//     /fail kind=compute_error with the SLURM state as the reason,
//     so it parks failed_retryable and composes with
//     enju_retry_task (from=snapshot for a transient infra state)
//
// On any terminal outcome the sidecar is renamed
// .wrap-job.done.json so the next sweep doesn't re-poll sacct.
func (s *FatClient) reapSlurmSidecars(ctx context.Context, wf *enjugit.Workflow) {
	if wf == nil {
		return
	}
	root := filepath.Join(wf.WorkDir(), corelayout.RunStateRunsRoot())
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) != executor.JobSidecarName {
			return nil
		}
		dir := filepath.Dir(path)
		donePath := strings.TrimSuffix(path, ".json") + ".done.json"

		h, herr := executor.ReadJobSidecar(path)
		if herr != nil || h.Executor != executor.KindSlurm {
			return nil // local sidecar (or unreadable) — not ours to poll
		}

		// Spec §5 gate: ".wrap-job.json without an adjacent
		// .wrap-result.done.json". The .wrap-result.json walk
		// (first pass of this same sweep) renames the result to
		// .done after it commits + posts; if that already
		// happened, the job is fully reaped — just retire the
		// sidecar so we stop polling sacct. Without this check
		// the pass below would find no .wrap-result.json and
		// spuriously /fail an already-accepted task.
		if _, doneErr := os.Stat(filepath.Join(dir, ".wrap-result.done.json")); doneErr == nil {
			_ = os.Rename(path, donePath)
			return nil
		}

		slurm, perr := s.pickExecutor(h.Executor)
		if perr != nil {
			return nil
		}
		st, perr := slurm.Poll(ctx, h)
		if perr != nil {
			// sacct itself failed (not on a submit host, slurmdbd
			// down). Poll already returned StateRunning; just wait
			// for the next sweep rather than mis-failing the task.
			return nil
		}
		switch st.State {
		case executor.StateQueued, executor.StateRunning:
			return nil // still going
		}

		// Terminal. Prefer the result the node wrote to the shared
		// FS — handleOneWrapperResult performs the host-side commit
		// and posts /result|/fail exactly as for local async.
		resultPath := filepath.Join(dir, ".wrap-result.json")
		if _, statErr := os.Stat(resultPath); statErr == nil {
			s.handleOneWrapperResult(ctx, resultPath, wf)
			_ = os.Rename(path, donePath)
			return nil
		}

		// No result: the job ended (lost / crashed / OOM / timeout)
		// without producing one. Name the task from the spec and
		// fail it through the standard recoverable contract.
		taskID := ""
		if sb, serr := os.ReadFile(filepath.Join(dir, ".wrap-spec.json")); serr == nil {
			var sp compute.Spec
			if json.Unmarshal(sb, &sp) == nil {
				taskID = sp.TaskID
			}
		}
		if taskID == "" {
			// Can't name the task — leave the sidecar for a human
			// to inspect rather than renaming it away silently.
			return nil
		}
		why := st.Reason
		if why == "" {
			why = "no result produced"
		}
		_, postErr := s.coord.Post(ctx, fmt.Sprintf("/api/v1/tasks/%s/fail", taskID), map[string]string{
			"reason": fmt.Sprintf("slurm job %s ended (%s) with no result on the shared filesystem", h.JobID, why),
			"kind":   "compute_error",
		})
		if postErr != nil && !strings.Contains(postErr.Error(), "terminal") {
			s.logger.Debug("reap slurm fail post", "task_id", taskID, "job", h.JobID, "error", postErr)
			return nil // network blip — retry next sweep
		}
		_ = os.Rename(path, donePath)
		return nil
	})
}

// ReconcileRunBranch is the read-only reconcile path used by
// handlers (run_status) that have a run payload in hand and
// don't want a full pull — fetch + scan only, then post any
// new trailers. Cheaper than PullBranchWithReconcile and safe
// to call on every render of run_status.
//
// Pulls the branch from runData (a coordinator run-detail
// payload), opens the project workspace, fetches + scans for
// trailers, posts to /tasks/reconcile, advances the cursor,
// reaps wrapper failures. Best-effort throughout — fetch /
// scan / post errors are logged at Debug and the call returns
// without surfacing them.
func (s *FatClient) ReconcileRunBranch(ctx context.Context, projectID int64, runData []byte) {
	if s.enjugit == nil {
		return
	}
	branch := RunBranchFromData(runData)
	if branch == "" {
		return
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return
	}
	ferr := wf.FetchBranch(branch)
	var trailers []enjugit.CommitTrailer
	var newTip, preCursor string
	if ferr == nil {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := enjugit.LoadCursors(stateDir, projectID)
		preCursor = cursors.Get(branch)
		// Persist-on-first-touch — same rationale as
		// PullBranchWithReconcileWF's identical block.
		if preCursor == "" {
			if h, herr := wf.LocalBranchHash(branch); herr == nil && h != "" {
				preCursor = h
				cursors.Set(branch, h)
				_ = cursors.Save()
			}
		}
		cursorMu.Unlock()
		if res, serr := wf.ScanBranchSince(branch, preCursor); serr == nil {
			newTip = res.NewTip
			trailers = res.Trailers
		} else {
			s.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
		}
	}

	if len(trailers) > 0 {
		if _, perr := s.coord.Post(ctx, "/api/v1/tasks/reconcile", buildReconcileBodyWF(trailers)); perr != nil {
			s.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			s.ReapWrapperFailuresWF(ctx, wf)
			return
		}
	}
	if newTip != "" && newTip != preCursor {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		latest, _ := enjugit.LoadCursors(stateDir, projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}
	s.ReapWrapperFailuresWF(ctx, wf)
}

// handleOneWrapperResult processes one wrapper result file.
// Reads result + corresponding spec; routes by outcome:
//
//   - Success (exit 0, no error): POST /tasks/:id/result with the
//     wrap-task's commit_sha, then applyAcceptedMerges so the
//     run branch FFs to include the topic-branch commit. Without
//     this the trailer scanner walks the run branch and never
//     sees the async commit (it's on a topic ref).
//   - Failure: POST /tasks/:id/fail with a human-readable reason.
//
// On both paths the file is renamed to .wrap-result.done.json so
// a later reap doesn't revisit it. Idempotent — the coordinator
// rejects state changes on already-terminal tasks, which we
// treat as "already handled, move on."
func (s *FatClient) handleOneWrapperResult(ctx context.Context, resultPath string, wf *enjugit.Workflow) {
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return
	}
	var res compute.Result
	if err := json.Unmarshal(data, &res); err != nil {
		// Malformed file — don't keep retrying forever; rename
		// so the reaper skips it. A human can inspect after.
		_ = os.Rename(resultPath, resultPath+".malformed")
		return
	}

	// Both branches need the spec — read once.
	specPath := filepath.Join(filepath.Dir(resultPath), ".wrap-spec.json")
	specBytes, specErr := os.ReadFile(specPath)
	var spec compute.Spec
	if specErr == nil {
		_ = json.Unmarshal(specBytes, &spec)
	}

	if res.ExitCode == 0 && res.Error == "" {
		// Success case — wrap-task already committed + pushed
		// the topic branch. We still need to (a) tell coord the
		// commit landed so it can advance state and emit the
		// auto-merge plan, and (b) execute that merge plan
		// locally so the run branch catches up.
		if specErr != nil || spec.TaskID == "" {
			// No spec → can't name the task. Mark done; a
			// human can inspect after if a downstream stays
			// stuck. The trailer-scanner fallback may still
			// catch it on a later sweep if the run branch is
			// somehow advanced by other means.
			_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
			return
		}

		// Produce-vs-commit split (executor: slurm). The compute
		// node ran the script and captured the would-be commit but
		// did NOT touch git. Replay it host-side now — same
		// DeferredCommit through the same SubmitComputeTaskResult,
		// so the commit is byte-identical to the local inline path.
		// On git failure, route through the same compute_error /
		// failed_retryable contract (no new failure UX): the work
		// product is intact on the shared FS, the operator fixes +
		// enju_retry_task. We rename .done rather than leaving the
		// result for the next sweep because SubmitComputeTaskResult
		// already exhausted its own internal commit/rebase retry —
		// retrying the bare CommitDeferred here wouldn't add a new
		// attempt. The likeliest real cause is NOT a permanent
		// error but a transient run-branch push race; the cost of
		// that is a full SLURM re-run via enju_retry_task
		// from=snapshot (the code was fine — only the host-side
		// push lost). Accepted for v1: rare, and the alternative
		// (an un-bounded reaper retry loop on a genuinely broken
		// remote) is worse. Revisit if push races show up in
		// practice — a bounded host-side re-commit before failing
		// is the natural follow-up.
		if res.CommitSHA == "" && res.DeferredCommit != nil {
			submitRes, cerr := compute.CommitDeferred(wf, *res.DeferredCommit)
			if cerr != nil {
				s.logger.Error("host-side deferred commit failed",
					"task_id", spec.TaskID, "error", cerr)
				_, _ = s.coord.Post(ctx, fmt.Sprintf("/api/v1/tasks/%s/fail", spec.TaskID), map[string]string{
					"reason": fmt.Sprintf("host-side commit failed for slurm job result: %v", cerr),
					"kind":   "compute_error",
				})
				_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
				return
			}
			res.CommitSHA = submitRes.CommitSHA
		}

		reportBody := map[string]interface{}{
			"commit_sha":  res.CommitSHA,
			"result_path": spec.ResultDir,
			"model":       spec.Model,
			"username":    spec.Username,
			"content":     res.Content,
		}
		if len(res.ArtifactsWritten) > 0 {
			reportBody["artifacts_written"] = res.ArtifactsWritten
		}
		reportData, perr := s.coord.Post(ctx, fmt.Sprintf("/api/v1/tasks/%s/result", spec.TaskID), reportBody)
		if perr != nil {
			// Network blip or coord-side rejection (e.g. task
			// already terminal — repeat reap after re-attach).
			// Leave the file in place so the next reap retries
			// unless the rejection is terminal.
			if !strings.Contains(perr.Error(), "terminal") {
				s.logger.Debug("reap post result", "task_id", spec.TaskID, "error", perr)
				return
			}
		} else if mergeErr := s.applyAcceptedMerges(ctx, wf, reportData); mergeErr != nil {
			// Phase 8.4 — applyAcceptedMerges posted
			// /merges/failed before returning, so the
			// underlying task is already in FAILED with the
			// fail-cascade fired on coord. We bump severity
			// to Error so this surfaces in operator log
			// scans (the silent-stall class of bugs Phase
			// 8.4 closes was hidden under Warn). The .json
			// is still renamed to .done.json below so the
			// next reap doesn't double-process.
			s.logger.Error("post-async auto-merge failed; task driven to FAILED via /merges/failed",
				"task_id", spec.TaskID, "error", mergeErr)
		}
		_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
		return
	}

	if specErr != nil || spec.TaskID == "" {
		// Failure path needs the task id; without it we can't
		// name what to fail. Skip silently — the next reap will
		// retry once spec arrives.
		return
	}

	reason := buildFailReason(spec, res)
	_, postErr := s.coord.Post(ctx, fmt.Sprintf("/api/v1/tasks/%s/fail", spec.TaskID), map[string]string{
		"reason": reason,
		// Async-compute outcome failure is the same recoverable
		// class as the sync path: park failed_retryable, don't
		// terminally cascade. Coord re-checks the precondition.
		"kind": "compute_error",
	})
	if postErr != nil {
		// Coordinator-side refusal (already terminal,
		// membership, etc) is fine — treat as "handled,
		// move on." Network errors also leave the file
		// alone; next reap will retry.
		if !strings.Contains(postErr.Error(), "terminal") {
			s.logger.Debug("reap post fail", "task_id", spec.TaskID, "error", postErr)
			return
		}
	}
	_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
}

// buildFailReason renders a short human-readable failure
// message from the wrapper's result. Prefers a wrapper-level
// Error (spec parse / fork failures) over script exit code +
// stderr tail, since the former is the truer root cause. Caps
// the stderr tail to keep the coordinator payload reasonable.
func buildFailReason(spec compute.Spec, res compute.Result) string {
	if res.Error != "" {
		return fmt.Sprintf("async wrap-task failed: %s", res.Error)
	}
	label := spec.ScriptLabel
	if label == "" {
		label = "script"
	}
	msg := fmt.Sprintf("async %s exited with code %d", label, res.ExitCode)
	if res.Stderr != "" {
		// Tail, not head — the failing command's error is at the
		// end of stderr, not buried under startup noise.
		msg += ": " + compute.StderrTail(res.Stderr, 800)
	}
	return msg
}
