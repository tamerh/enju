package service

// Compute-task execution. FatClient.ExecuteComputeTask is the
// per-task worker that backs both enju_execute_task (single
// task) and enju_execute_run (batch cascade). Returns the
// structured ExecuteOutcome so the batch caller can stop /
// continue per entry without re-parsing formatted text;
// handlers translate the outcome to MCP text.
//
// Async kickoff lives on FatClient too — same dependencies
// (workspace + compute.Spec assembly), and keeping it here
// means the per-tool flows pull from one place.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/format"
	corelayout "github.com/enju-ai/enju/internal/common/layout"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/executor"
)

// ExecuteOutcome is the structured result of one compute-task
// execution. Exactly one of Status ∈ {completed, failed,
// async_started, git_failed} is set per call. All display /
// formatting happens at the call site.
type ExecuteOutcome struct {
	TaskID           string
	Script           string
	Status           string
	ExitCode         int
	ElapsedMS        int64
	CommitSHA        string
	ContentLen       int
	ArtifactsWritten []string
	MissingArtifacts []string
	ScriptLogPath    string
	ScratchDir       string // preserved on failure for inspection
	ErrorMessage     string
	Stderr           string
	ContribNum       int
	NewlyReady       int
	Async            *AsyncKickoffResult
	Branch           string
}

// AsyncKickoffResult captures the launch outcome of a detached
// `enju wrap-task` subprocess so callers can tell the user
// exactly what was spawned and where its logs go.
type AsyncKickoffResult struct {
	// Executor is the launcher kind ("local" | "slurm") so the
	// formatter can render the right "what was spawned" text
	// (a PID + log for local, a SLURM job id + sacct hint for
	// slurm).
	Executor   string
	PID        int    // local
	JobID      string // slurm
	SpecPath   string
	OutputPath string
	WrapperLog string
}

// ResolvedMode returns the effective execution mode for a
// compute task, applying the default-to-sync rule. Non-compute
// tasks return "" — callers check `Action == "compute"` before
// branching on mode.
func ResolvedMode(meta *TaskMeta) string {
	if meta == nil {
		return ""
	}
	return enjuYaml.ResolvedModeFields(meta.Action, meta.Mode, meta.Executor)
}

// computeClassification is the post-compute.Run decision. Outcome
// nil means "script succeeded — caller continues to commit/report".
// A non-nil Outcome is returned from ExecuteComputeTask as-is;
// PostFail (only with a "failed" Outcome) tells the caller to POST
// /tasks/{id}/fail kind=compute_error first so the coordinator
// parks the task failed_retryable.
type computeClassification struct {
	Outcome  *ExecuteOutcome
	PostFail bool
	Reason   string
}

// classifyComputeResult is the post-compute.Run truth table,
// extracted as a pure function (cf. failTaskOwnershipOK) so each
// arm is unit-pinned independent of the live workspace +
// coordinator. The arms, in order:
//
//  1. git_failed — script exited 0 but the post-script git op
//     failed (commit/push exhausted, "object not found" on a fresh
//     remote). Distinct recovery: the work product is on disk, fix
//     the git state, don't re-run. Checked FIRST so it is not
//     shadowed by (2); it formerly sat after an `if res.Error != ""
//     { return }` early-out, which both made the HasPrefix fallback
//     dead code AND stranded the wrapper-abort case (see (2)).
//  2. failed — script exited non-zero, OR a wrapper-level abort
//     (required writes_artifacts not produced, undeclared-path
//     rejection, work_dir / reads-materialization error,
//     spec/script-not-found) which sets res.Error with ExitCode==0.
//     BOTH are recoverable and BOTH must POST /fail
//     kind=compute_error (PostFail=true) so the task parks
//     failed_retryable and enju_retry_task can recover it. The
//     res.Error!="" half previously returned a raw error WITHOUT
//     the POST → task stuck RUNNING, claim held, retry refused,
//     reaper re-ran it forever (the bughunt A1 livelock). Unifying
//     it with the verified-correct ExitCode!=0 path removes the
//     asymmetry.
//  3. success — Outcome nil; caller falls through to commit/report.
func classifyComputeResult(res compute.Result, taskID, script, branch, scratchDir string) computeClassification {
	gitErrMsg := res.GitError
	if gitErrMsg == "" && res.Error != "" && strings.HasPrefix(res.Error, compute.GitSubmitFailedPrefix) {
		gitErrMsg = res.Error
	}
	if gitErrMsg != "" {
		return computeClassification{Outcome: &ExecuteOutcome{
			TaskID:        taskID,
			Script:        script,
			Status:        "git_failed",
			ElapsedMS:     res.ElapsedMS,
			ContentLen:    len(res.Content),
			ScriptLogPath: res.ScriptLogPath,
			ErrorMessage:  gitErrMsg,
			Branch:        branch,
		}}
	}

	if res.ExitCode != 0 || res.Error != "" {
		stderr := compute.StderrTail(res.Stderr, 1000)
		var reason string
		if res.ExitCode != 0 {
			// Tail, not head: the error is at the bottom of stderr.
			reason = fmt.Sprintf("script %s exited with code %d", script, res.ExitCode)
			if stderr != "" {
				reason += ": " + stderr
			}
		} else {
			// Wrapper-level abort, script exit 0 (e.g.
			// "required writes_artifacts not produced: [...]").
			reason = res.Error
		}
		return computeClassification{
			Outcome: &ExecuteOutcome{
				TaskID:        taskID,
				Script:        script,
				Status:        "failed",
				ExitCode:      res.ExitCode,
				ElapsedMS:     res.ElapsedMS,
				ScriptLogPath: res.ScriptLogPath,
				// Scratch is preserved on failure (the wrapper only
				// wipes on a clean run) — surface it so the operator
				// knows where to look instead of flying blind.
				ScratchDir:   scratchDir,
				ErrorMessage: reason,
				Stderr:       stderr,
				Branch:       branch,
			},
			PostFail: true,
			Reason:   reason,
		}
	}

	return computeClassification{} // success — caller continues
}

// ExecuteComputeTask runs one action:compute task end-to-end:
// reconcile → claim (if needed) → script execution → result
// report.
//
// The error return is reserved for "cannot even attempt"
// conditions (task not found, wrong action, no workspace,
// script missing, claim gate closed). A script that runs and
// exits non-zero is NOT an error — it returns
// (outcome{Status:"failed"}, nil) so the batch caller can
// record it and stop the cascade gracefully without unwinding
// as a generic failure.
func (s *FatClient) ExecuteComputeTask(ctx context.Context, taskID string) (*ExecuteOutcome, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("enju_execute_task requires a local workspace")
	}

	meta, err := s.FetchTaskMeta(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if meta.Action != "compute" {
		return nil, fmt.Errorf("task %q is action=%q, not compute — use enju_submit_result", taskID, meta.Action)
	}
	if meta.Script == "" {
		return nil, fmt.Errorf("task %q has no script field declared", taskID)
	}

	wf, remoteURL, projName, _, err := s.OpenWorkflow(ctx, meta.ProjectID)
	if err != nil {
		return nil, err
	}

	// Reconcile before the claim gate — an async→async chain
	// otherwise orphans downstream tasks because the coordinator
	// hasn't yet seen the upstream completion commit. Best-effort;
	// a reconcile failure here just means slightly stale state.
	// No-checkout: compute reads the snapshot + commits via plumbing,
	// so this must not move the operator's worktree onto the run branch.
	s.reconcileBranchWF(ctx, wf, meta.ProjectID, meta.Branch)

	// Re-fetch so the claim decision sees the post-reconcile
	// state (esp. for chained async tasks that just transitioned
	// to ready as a side effect of the pull).
	if fresh, ferr := s.FetchTaskMeta(ctx, taskID); ferr == nil && fresh != nil {
		meta = fresh
	}

	switch meta.State {
	case "ready", "collecting":
		// Claim with transient-error retry. Parallel cascades can
		// stack short-lived BUSY / 5xx / network blips on the
		// coordinator side; without a retry, the cascade aborts
		// with stop_reason=compute_errored on what should be a
		// recoverable hiccup.
		//
		// The claim response carries the post-claim TaskMeta
		// (IterationBranch, IterSeq, refreshed state) — adopt it
		// directly so SubmitComputeTaskResult sees the per-claim
		// fields without a second GET round-trip.
		postClaim, err := s.claimWithTransientRetry(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if postClaim != nil {
			meta = postClaim
		}
	case "claimed", "running":
		// Already ours (e.g. retry after transient wrapper
		// failure) — proceed without a fresh claim.
	default:
		return nil, fmt.Errorf(
			"task %q is not claimable (state: %s) — run enju_run_status or wait for upstream to complete before retrying",
			taskID, meta.State,
		)
	}

	workDir := wf.WorkDir()
	resultDir := meta.ResultDir

	taskScratchDir := compute.ResolveTaskScratchDir(wf.ProjectRoot(), s.coord.Username(), taskID, meta.IterSeq)

	repoSnapshotDir, _ := resolveSnapshotDirs(wf, meta)
	if repoSnapshotDir != "" {
		if _, statErr := os.Stat(repoSnapshotDir); os.IsNotExist(statErr) {
			if _, merr := wf.MaterializeRunRepo(meta.Branch, repoSnapshotDir); merr != nil {
				return nil, fmt.Errorf("materializing run snapshot from branch %q: %w", meta.Branch, merr)
			}
		}
	}
	bigfilesDir := enjugit.ResolveBigfilesDir(wf.ProjectRoot(), meta.ProjectID, projName, meta.Branch)
	if bigfilesDir != "" {
		_ = os.MkdirAll(bigfilesDir, 0755)
	}

	// Script paths are PROJECT-ROOT-relative — same addressing as
	// writes:/reads:, resolved off the snapshot root (which IS the
	// frozen project tree at create-run, since the snapshot already
	// contains the whole repo). The pre-2026 workflow-dir-relative
	// rule was dropped: one shared src/ across many workflows is the
	// typical pattern, and forcing per-workflow scripts/ subdirectories
	// was friction with no real payoff. ENJU_TEMPLATE_DIR now also
	// points at the snapshot root.
	// Two run shapes, two scriptRoots:
	//   - Templated run (RunSourcePath != ""): the workflow source —
	//     including any committed scripts — is frozen into the per-
	//     run snapshot at create-run. Resolve against repoSnapshotDir
	//     so reads are reproducible and immune to later commits on
	//     the run branch.
	//   - Inline-YAML run: there is no frozen source. Scripts that an
	//     upstream task `writes:` only exist on the live run-branch
	//     clone (workDir), which is pulled before each claim — the
	//     snapshot was materialized at run-start, before any upstream
	//     commit, so it's stale for this case.
	var scriptPath, templateDir, scriptRoot string
	if meta.RunSourcePath != "" {
		templateDir = repoSnapshotDir
		scriptRoot = repoSnapshotDir
	} else {
		scriptRoot = workDir
	}
	scriptPath = filepath.Join(scriptRoot, meta.Script)
	// Traversal guard against the same root we resolved off, so the
	// convention change ("project-root-relative, whole tree
	// addressable") doesn't open up `../` escapes.
	if cleaned, err := filepath.Rel(scriptRoot, scriptPath); err != nil || strings.HasPrefix(cleaned, "..") {
		return nil, fmt.Errorf("script %q resolves outside the project root %q — script: paths are project-root-relative; remove any leading slash or '..' segments", meta.Script, scriptRoot)
	}
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("script %q not found at %s — script: is project-root-relative (e.g. src/foo.py for a script at <project>/src/foo.py)", meta.Script, scriptPath)
	}

	// Declared reads from the task record — resolved before the
	// env is built so buildComputeEnv can export ENJU_READS
	// alongside ENJU_WRITES (one place, symmetric, unit-testable);
	// also feeds the reads materializer below.
	var readsArtifacts []string
	if rawTask, rerr := s.coord.Get(ctx, "/api/v1/tasks/"+taskID); rerr == nil {
		var tm map[string]interface{}
		if json.Unmarshal(rawTask, &tm) == nil {
			if r, ok := tm["reads_artifacts"].([]interface{}); ok {
				for _, p := range r {
					if str, ok := p.(string); ok {
						readsArtifacts = append(readsArtifacts, str)
					}
				}
			}
		}
	}

	env := buildComputeEnv(taskID, workDir, resultDir, templateDir, repoSnapshotDir, bigfilesDir, taskScratchDir, readsArtifacts, meta)
	// Phase 2.2 — resolve the run-branch tip so the wrapper can
	// materialize each declared reads_artifacts entry from a
	// pinned commit. Reading from the run-branch (not the
	// task's own iteration branch) is correct here: upstream
	// task outputs auto-merge to the run-branch on accept, so
	// the run-branch tip carries every dep's content by the
	// time this task claims.
	//
	// Empty hash → branch absent locally + on origin. We let
	// the materializer no-op (it skips when ReadsSourceSHA is
	// empty + ReadsArtifacts is empty) by leaving readsSourceSHA
	// "" — combined with the ReadsArtifacts-non-empty guard in
	// the wrapper, the task fails loud with a "caller bug" if
	// it actually has declared reads but no resolvable source.
	var readsSourceSHA string
	if len(readsArtifacts) > 0 && wf != nil {
		if sha, herr := wf.LocalBranchHash(meta.Branch); herr == nil {
			readsSourceSHA = sha
		}
	}

	// writes_artifacts carries the per-entry track flag (object
	// form) so a script can tell which of its declared outputs is
	// untracked — it can't otherwise, and tracked vs track:false
	// have different destinations. Non-nil → marshals as [] not
	// null. These are the declared entries (glob/dir expansion
	// against the worktree happens post-script in the wrapper).
	writesCtx := make([]map[string]interface{}, 0, len(meta.WritesArtifacts))
	for _, w := range meta.WritesArtifacts {
		writesCtx = append(writesCtx, map[string]interface{}{
			"path":  w.Path,
			"track": w.Track,
		})
	}
	contextPayload := map[string]interface{}{
		"task_id":          taskID,
		"task_def_id":      meta.TaskDefID,
		"instance_key":     meta.InstanceKey,
		"iteration":        meta.InstanceParams,
		"params":           meta.RunParams,
		"reads_artifacts":  stringSliceNonNil(readsArtifacts),
		"writes_artifacts": writesCtx,
	}
	contextBytes, _ := json.MarshalIndent(contextPayload, "", "  ")
	// Phase 2.6 — context.json lives in scratch (production) so the
	// worktree never sees it as an untracked file. Without this,
	// a non-FF MergeAcceptedTopic's post-merge Checkout(target)
	// refuses to overwrite the untracked context.json/script.log
	// the wrapper had left under enju/runs/<run>/<task>/, stalling
	// parallel-merge fan-out at the second sibling. The wrapper's
	// context.json read site honors the same placement.
	//
	// Legacy callers without scratch (older test fixtures) still
	// land in workDir/<resultDir> so the existing contract holds.
	contextFullPath := filepath.Join(workDir, resultDir, "context.json")
	if taskScratchDir != "" {
		contextFullPath = filepath.Join(taskScratchDir, "context.json")
	}
	if err := os.MkdirAll(filepath.Dir(contextFullPath), 0755); err != nil {
		return nil, fmt.Errorf("creating run dir for context.json: %w", err)
	}
	if err := os.WriteFile(contextFullPath, contextBytes, 0644); err != nil {
		return nil, fmt.Errorf("writing context.json: %w", err)
	}

	// Post-NDW.5: pre-resolve the operator's clone path + flock
	// path here and thread them through Spec. The wrapper opens
	// the clone directly without any Workspace/Registry re-
	// resolution — no risk of landing on a divergent path.
	spec := compute.Spec{
		TaskID:    taskID,
		ProjectID: meta.ProjectID,
		RemoteURL: remoteURL,
		WorkDir:   workDir,
		LockPath:  enjugit.LockPathFor(wf.ProjectRoot()),
		Branch:    meta.Branch,
		// IterationBranch is the per-task topic branch coord
		// populated at claim time. compute.Run uses it as
		// BranchOverride for SubmitComputeTaskResult so each
		// parallel sibling commits to its own ref, isolated from
		// the others. Coord's auto-merge then FFs the topic onto
		// meta.Branch (the run branch) on accept.
		IterationBranch: meta.IterationBranch,
		ResultDir:       resultDir,
		BigfilesDir:     bigfilesDir,
		ScriptPath:      scriptPath,
		ScriptLabel:     meta.Script,
		WritesArtifacts: meta.WritesArtifacts,
		ReadsArtifacts:  readsArtifacts,
		ReadsSourceSHA:  readsSourceSHA,
		TaskScratchDir:  taskScratchDir,
		// SnapshotDir is the script's read-only working directory
		// when the task came from a template run (RunSourcePath
		// non-empty). Inline-YAML runs leave it empty — they don't
		// have a snapshot to mount; the script lives in the
		// project clone and CWD falls back to scratch or workDir
		// via ScriptCwdFor.
		SnapshotDir: templateDir,
		AuthorName:  s.coord.CitizenName(),
		AuthorEmail: s.coord.CitizenEmail(),
		Username:    s.coord.Username(),
		// A compute task is script-produced — no LLM ran — so the
		// model attribution is empty. `model` answers "what
		// produced the words", not "who launched it" (the operator
		// citizen already records who). Stamping the session model
		// here is a false credit: an `echo` step is not Opus
		// output. Empty regardless of who ran it (human or agent).
		Model:            "",
		Container:        meta.Container,
		ContainerRuntime: meta.ContainerRuntime,
		Volumes:          meta.Volumes,
		Env:              meta.Env,
	}

	// Phase 8.2 — signal CLAIMED → RUNNING just before kicking
	// off compute (sync) or the wrapper subprocess (async). The
	// coord-side handler validates state==CLAIMED so a duplicate
	// POST on a resume retry returns a benign 400; we log and
	// proceed since this transition is observability, not
	// correctness. Human-action tasks skip RUNNING entirely (the
	// brief's "no exec phase" path) and never reach this code.
	if _, perr := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/started", nil); perr != nil {
		s.logger.Debug("mark task started failed; observability only",
			"task_id", taskID, "error", perr)
	}

	if ResolvedMode(meta) == "async" {
		kick, err := s.kickoffWrapTask(ctx, meta, spec, env, resultDir, workDir)
		if err != nil {
			return nil, fmt.Errorf("async kickoff: %w", err)
		}
		return &ExecuteOutcome{
			TaskID: taskID,
			Script: meta.Script,
			Status: "async_started",
			Async:  kick,
			Branch: meta.Branch,
		}, nil
	}

	// Pass the already-opened Workflow so compute.Run doesn't
	// re-open one per task — concurrent goroutines share the
	// in-process Mutex rather than falling through to the
	// slower cross-process flock.
	res := compute.Run(ctx, wf, spec, env, s.logger)

	// Post-run classification is a three-way truth table
	// (git_failed / failed / continue-to-success) extracted as a
	// pure function so every arm is unit-pinned independent of the
	// live workspace + coordinator. A missing arm here is exactly
	// how the wrapper-abort livelock shipped: the old inline code
	// returned a raw error on res.Error!="" *before* the /fail POST,
	// so a script that exited 0 but didn't produce its declared
	// writes stranded the task in RUNNING (claim held, never
	// failed_retryable, enju_retry_task refused it, reaper re-ran
	// it forever).
	if cls := classifyComputeResult(res, taskID, meta.Script, meta.Branch, taskScratchDir); cls.Outcome != nil {
		if cls.PostFail {
			// Recoverable compute failure (script non-zero exit OR
			// wrapper-level abort). POST kind=compute_error so the
			// coordinator parks it failed_retryable (run stays
			// alive, descendants PENDING) — operator fixes +
			// enju_retry_task.
			s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/fail", map[string]string{
				"reason": cls.Reason,
				"kind":   "compute_error",
			})
		}
		return cls.Outcome, nil
	}

	// Exit 0 → wrapper committed + pushed. Report the landed
	// commit to the coordinator so the state machine advances.
	reportBody := map[string]interface{}{
		"commit_sha":  res.CommitSHA,
		"result_path": resultDir,
		// A compute task is script-produced — no LLM ran — so the
		// model is empty (NULL), never the triggering session's
		// model. `model` reflects what produced the work, not who
		// launched it (the operator citizen records who).
		"model":    "",
		"username": s.coord.Username(),
		"content":  res.Content,
	}
	if len(res.ArtifactsWritten) > 0 {
		reportBody["artifacts_written"] = res.ArtifactsWritten
	}
	reportData, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/result", reportBody)
	if err != nil {
		return nil, fmt.Errorf("coordinator report for %s: %w", taskID, err)
	}

	// Apply any FF auto-merges the coordinator emitted in the
	// report response. Compute submits land on a per-task topic
	// branch (SubmitComputeTaskResult); the run branch only
	// catches up when the topic is FF-merged in. Server-side
	// collectAcceptedMerges enumerates the eligible merges and
	// returns them in the response — applyAcceptedMerges executes
	// each one locally and reports back via report_merge so coord
	// stamps a branch_merged event.
	//
	// Phase 8.4 — non-conflict merge failures (push rejected,
	// transport timeout, etc.) are now terminal. applyAcceptedMerges
	// posts /merges/failed before returning the error, which
	// drives the underlying task to FAILED + fires the fail-cascade
	// on coord. We surface the failure as a git_failed
	// ExecuteOutcome so enju_execute_run renders it as a hard stop
	// rather than silently logging a Warn — pre-Phase-8.4 the
	// silent-stall let the task look ACCEPTED while downstream
	// tasks fanned out against an artifact whose commit never
	// reached the run branch.
	if mergeErr := s.applyAcceptedMerges(ctx, wf, reportData); mergeErr != nil {
		s.logger.Error("post-compute auto-merge failed", "task_id", taskID, "error", mergeErr)
		return &ExecuteOutcome{
			TaskID:        taskID,
			Script:        meta.Script,
			Status:        "git_failed",
			ElapsedMS:     res.ElapsedMS,
			CommitSHA:     res.CommitSHA,
			ContentLen:    len(res.Content),
			ScriptLogPath: res.ScriptLogPath,
			ErrorMessage:  fmt.Sprintf("post-merge failed: %s", mergeErr.Error()),
			Branch:        meta.Branch,
		}, nil
	}

	// Run-completion sync (FF-merge the run branch into base, then
	// push when sync:push) must fire on whichever path reports the
	// final task result. The citizen single-submit path does this
	// after its accepted-merges step; the compute path is reached
	// by enju_execute_task and the enju_execute_run cascade, so
	// without this a compute-final pipeline would complete but its
	// branch would silently never merge into the default branch,
	// regardless of the workflow's sync: setting. applyRunCompletion
	// no-ops unless the report response carries run_completed.
	s.applyRunCompletion(ctx, mergeWorkflowOrNil(wf), meta, reportData)

	out := &ExecuteOutcome{
		TaskID:           taskID,
		Script:           meta.Script,
		Status:           "completed",
		ExitCode:         0,
		ElapsedMS:        res.ElapsedMS,
		CommitSHA:        res.CommitSHA,
		ContentLen:       len(res.Content),
		ArtifactsWritten: res.ArtifactsWritten,
		MissingArtifacts: res.MissingArtifacts,
		Branch:           meta.Branch,
	}
	var report map[string]interface{}
	if json.Unmarshal(reportData, &report) == nil {
		if n := format.JsonFloat(report["contribution_number"]); n > 0 {
			out.ContribNum = int(n)
		}
		if r := format.JsonFloat(report["newly_ready"]); r > 0 {
			out.NewlyReady = int(r)
		}
	}
	return out, nil
}

// RetryComputeTask is the client half of enju_retry_task. The
// coordinator has already flipped the task failed_retryable→READY
// (see service.RetryTask); this re-runs it, honoring the from
// axis:
//
//   - "head": re-materialize the run snapshot from the run
//     branch's current tip so the operator's committed fix is the
//     script that runs. ExecuteComputeTask only materializes when
//     the snapshot dir is absent (it persists across attempts), so
//     without this explicit refresh the retry would silently
//     re-run the unfixed script. The refresh is overwrite-in-
//     place, not a clean checkout: MaterializeRunRepo rewrites
//     the branch tip's blobs but never visits a path deleted or
//     renamed on the branch since the last materialize, so such
//     a path lingers in the snapshot. Modifying a script (the
//     dominant fix) is correct; a delete/rename needs a fresh
//     run. The fix must be on the RUN BRANCH — a commit to the
//     default branch is invisible to the run.
//   - "snapshot": leave the existing snapshot untouched —
//     ExecuteComputeTask reuses it and re-runs the pinned script
//     verbatim (transient-failure retry).
//
// Then it delegates to ExecuteComputeTask, which claims the now
// READY task — a fresh claim, so iter_seq advances and this retry
// is its own auditable iteration.
func (s *FatClient) RetryComputeTask(ctx context.Context, taskID, from string) (*ExecuteOutcome, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("enju_retry_task requires a local workspace")
	}
	if from == "" {
		from = "head"
	}
	if from == "head" {
		meta, err := s.FetchTaskMeta(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("task %q not found: %w", taskID, err)
		}
		wf, _, _, _, err := s.OpenWorkflow(ctx, meta.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("opening workspace to re-materialize fixed script: %w", err)
		}
		repoSnapshotDir, _ := resolveSnapshotDirs(wf, meta)
		// Empty for inline-YAML runs with no project root —
		// there's no branch snapshot to refresh from a commit,
		// so head and snapshot collapse to the same thing.
		if repoSnapshotDir != "" {
			if _, merr := wf.MaterializeRunRepo(meta.Branch, repoSnapshotDir); merr != nil {
				return nil, fmt.Errorf(
					"re-materializing run snapshot from branch %q (retry from=head): %w",
					meta.Branch, merr)
			}
		}
	}
	return s.ExecuteComputeTask(ctx, taskID)
}

// resolveSnapshotDirs derives the on-disk snapshot paths for a run
// from the task meta and workflow. Returns (repoSnapshotDir,
// templateSnapshotDir); both may be empty for inline-YAML runs
// without a project root.
func resolveSnapshotDirs(wf *enjugit.Workflow, meta *TaskMeta) (repoSnapshotDir, templateSnapshotDir string) {
	if wf.ProjectRoot() != "" && meta.Branch != "" {
		repoSnapshotDir = filepath.Join(wf.ProjectRoot(), corelayout.RunSnapshotOnDiskDir(meta.RunSeq, meta.RunSlug))
		if meta.RunSourcePath != "" {
			templateRel := meta.RunSourcePath
			if strings.HasSuffix(templateRel, ".yaml") || strings.HasSuffix(templateRel, ".yml") {
				templateRel = filepath.Dir(templateRel)
			}
			templateSnapshotDir = filepath.Join(repoSnapshotDir, templateRel)
		} else {
			templateSnapshotDir = filepath.Join(repoSnapshotDir, corelayout.RunTemplateSnapshotDir(meta.RunSeq, meta.RunSlug))
		}
	} else {
		templateSnapshotDir = filepath.Join(wf.WorkDir(), corelayout.RunTemplateSnapshotDir(meta.RunSeq, meta.RunSlug))
	}
	return
}

// buildComputeEnv assembles the env slice passed to the wrapper
// process. Split out so both execute entry points (single-task
// and batch) produce byte-identical environments for the same
// task — any divergence would show up as reproducibility drift
// between a manual execute and a batched one.
//
// repoSnapshotDir is the absolute path to the per-run on-disk
// snapshot of the run branch's whole tree. Exposed as
// ENJU_REPO_DIR so scripts can read arbitrary repo files frozen
// at the run's base SHA — `cat $ENJU_REPO_DIR/src/main.go`
// always returns the bytes that were there when create_run
// fired, regardless of subsequent operator edits to the
// working tree. Empty string suppresses the export (legacy /
// test paths without a workspace root).
//
// bigfilesDir is the absolute path the script's track:false
// outputs go into; exposed as ENJU_BIGFILES so recipes can
// write directly via "$ENJU_BIGFILES/<path>". Empty string
// suppresses the export — only happens in legacy / test paths
// that haven't resolved a project root yet.
//
// taskScratchDir is the absolute path the wrapper creates +
// cleans up around the script run (Phase 2.1). Exposed as
// ENJU_TASK_DIR so scripts can opt in to writing under a
// per-task isolated location. Empty string suppresses the
// export.
func buildComputeEnv(taskID, workDir, resultDir, templateDir, repoSnapshotDir, bigfilesDir, taskScratchDir string, reads []string, meta *TaskMeta) []string {
	// Phase 2.3 / 2.5 — point ENJU_PROJECT_DIR at the scratch
	// dir whenever scratch is set, regardless of execution mode.
	// Direct-exec scripts run with cmd.Dir = scratch and see the
	// host path here. Container scripts get the host path
	// translated to ContainerScratchDir (/scratch) by the docker
	// arg builder's env-forwarding loop, so the value the
	// container actually sees is /scratch.
	//
	// ENJU_RUN_DIR points at the per-task scratch dir when scratch
	// is set (Phase 2.6 — context.json + script.log live in scratch
	// to keep the worktree clean of untracked task-metadata files
	// that would otherwise block parallel-merge fan-out at
	// MergeAcceptedTopic's post-merge Checkout step). Legacy callers
	// without scratch keep workDir/<resultDir> so the documented
	// `$ENJU_RUN_DIR/context.json` contract still resolves to the
	// place the handler wrote the file.
	//
	// Container mode: buildDockerArgs translates host paths in env
	// values, so a scratch host path here becomes ContainerScratchDir
	// (/scratch) inside the container — same value scripts see for
	// ENJU_TASK_DIR. The two are aliases inside the container, which
	// is fine: both name the script's working directory.
	projectDir := workDir
	if taskScratchDir != "" {
		projectDir = taskScratchDir
	}
	runDir := filepath.Join(workDir, resultDir)
	if taskScratchDir != "" {
		runDir = taskScratchDir
	}
	env := os.Environ()
	env = append(env,
		"ENJU_TASK_ID="+taskID,
		"ENJU_PROJECT_DIR="+projectDir,
		"ENJU_RUN_DIR="+runDir,
	)
	if bigfilesDir != "" {
		env = append(env, enjugit.BigfilesEnv+"="+bigfilesDir)
	}
	if taskScratchDir != "" {
		// ENJU_TASK_DIR points at the per-task scratch dir
		// (host path, or in-container ContainerScratchDir after
		// the docker arg builder rewrites it). This is the
		// script's CWD: declared reads_artifacts are
		// materialized here before the script starts; declared
		// writes_artifacts get picked up from here after exit.
		// Scripts that only read/write under "$ENJU_TASK_DIR/"
		// or "$ENJU_PROJECT_DIR/" (which equals task dir under
		// 2.5) are guaranteed parallel-safe across siblings.
		env = append(env, "ENJU_TASK_DIR="+taskScratchDir)
	}
	if templateDir != "" {
		env = append(env, "ENJU_TEMPLATE_DIR="+templateDir)
	}
	// ENJU_REPO_DIR is the per-run on-disk snapshot root —
	// the frozen tree of the run branch's tip. Scripts read
	// arbitrary repo content from here ($ENJU_REPO_DIR/src/main.go,
	// $ENJU_REPO_DIR/data/...). Distinct from ENJU_TEMPLATE_DIR,
	// which points at a SUBPATH of this dir (the workflow's
	// committed bundle).
	if repoSnapshotDir != "" {
		env = append(env, "ENJU_REPO_DIR="+repoSnapshotDir)
	}
	// ENJU_SCRATCH points at the writable per-iter sandbox. With
	// the snapshot-as-CWD shape, scripts that need to write
	// intermediate files do so via $ENJU_SCRATCH rather than
	// against the (read-only) CWD. Container path translates this
	// to /scratch via the env-forwarding loop in container_args.go.
	if taskScratchDir != "" {
		env = append(env, "ENJU_SCRATCH="+taskScratchDir)
	}
	// ENJU_WRITES — the task's declared output paths, one per
	// line, in declaration order. enju has already resolved these
	// ({{param}}/{{item}} substituted); handing them over saves
	// the script from re-deriving or parsing context.json for the
	// common "where do I write" case. Single-output degenerates to
	// a bare path. Per-entry track:false lives in context.json
	// (env can't carry the flag).
	env = append(env, "ENJU_WRITES="+strings.Join(meta.WritesArtifacts.Paths(), "\n"))
	// ENJU_READS — the resolved declared input paths, the
	// materialized-into-CWD counterpart of ENJU_WRITES. Same
	// always-set contract (empty when none declared).
	env = append(env, "ENJU_READS="+strings.Join(reads, "\n"))
	for k, v := range meta.RunParams {
		env = append(env, "ENJU_PARAM_"+k+"="+encodeParamEnv(v))
	}
	for k, v := range meta.InstanceParams {
		env = append(env, "ENJU_PARAM_"+k+"="+encodeParamEnv(v))
	}
	for k, v := range meta.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// claimWithTransientRetry posts the claim with a small retry
// loop on transient errors only. Substantive errors (state
// reasons, role mismatch, validation) bypass the retry and
// return immediately so the caller doesn't waste attempts on
// genuinely-failed claims.
//
// 3 attempts with 50/100/200ms backoff. Total max wait ~350ms,
// short enough that callers don't notice the retries unless
// the network is really down.
//
// Returns the parsed post-claim TaskMeta so the caller has the
// per-claim fields (IterationBranch, IterSeq, refreshed State)
// without a second GET. Returns nil meta when the response
// envelope doesn't carry a "task" subobject (older coord builds);
// callers that need the meta unconditionally must FetchTaskMeta.
func (s *FatClient) claimWithTransientRetry(ctx context.Context, taskID string) (*TaskMeta, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// A compute task is script-produced — claim with an empty
		// model so task_claims.model is NULL from the start.
		// (Submit COALESCEs, so a model stamped here would survive
		// an empty submit — this is the load-bearing site.)
		claimData, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
			"username": s.coord.Username(),
			"model":    "",
		})
		if err != nil {
			if !isTransientTransportError(err) {
				return nil, fmt.Errorf("claim %s: %w", taskID, err)
			}
			lastErr = fmt.Errorf("claim %s: %w", taskID, err)
			sleepBackoff(attempt)
			continue
		}
		var claimResp map[string]interface{}
		if json.Unmarshal(claimData, &claimResp) == nil {
			if errMsg, _ := claimResp["error"].(string); errMsg != "" {
				if !isTransientCoordinatorError(errMsg) {
					return nil, fmt.Errorf("claim %s: %s", taskID, errMsg)
				}
				lastErr = fmt.Errorf("claim %s: %s", taskID, errMsg)
				sleepBackoff(attempt)
				continue
			}
			if taskMap, ok := claimResp["task"].(map[string]interface{}); ok {
				return s.parseTaskMetaFromMap(taskID, taskMap), nil
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("after %d retries: %w", maxAttempts, lastErr)
}

// isTransientTransportError reports whether a transport-level
// error is the sort that's worth retrying.
func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pat := range []string{
		"coordinator unreachable",
		"connection refused",
		"connection reset",
		"EOF",
		"i/o timeout",
		"broken pipe",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

// isTransientCoordinatorError reports whether a coordinator-
// returned error string indicates a transient condition worth
// retrying. The current patterns are SQLITE_BUSY-family.
func isTransientCoordinatorError(msg string) bool {
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}

// sleepBackoff is the exponential backoff between retry
// attempts. Caps at 200ms so the worst case is ~350ms total
// across 3 attempts.
func sleepBackoff(attempt int) {
	d := time.Duration(50<<attempt) * time.Millisecond
	if d > 200*time.Millisecond {
		d = 200 * time.Millisecond
	}
	time.Sleep(d)
}

// kickoffWrapTask serializes the spec and launches the wrapper
// through the executor seam, returning without waiting. Spec
// serialization + run-dir creation stay HERE — the seam's
// contract is that Submit only launches. The launcher is chosen
// from the task's executor: local/"" forks a detached process
// on this host (unchanged behavior, now via
// executor.LocalExecutor); slurm sbatches a job.
//
// DeferCommit is set from the executor, not a launcher flag: a
// remote node must not touch git / credentials / network, so
// for any non-local executor the wrapper produces the result
// and the host-side reaper replays the commit (byte-identical
// — same DeferredCommit, same SubmitComputeTaskResult). Local
// async still commits inline (it runs on a git-capable host).
//
// Submit also writes the .wrap-job.json sidecar, which is what
// lets the reaper discover a still-queued SLURM job (no
// .wrap-result.json yet) and lets enju_terminate_run Cancel an
// in-flight job (the handle — PID or job id — is now persisted).
func (s *FatClient) kickoffWrapTask(ctx context.Context, meta *TaskMeta, spec compute.Spec, env []string, resultDir, workDir string) (*AsyncKickoffResult, error) {
	runSubdir := filepath.Join(workDir, resultDir)
	if err := os.MkdirAll(runSubdir, 0o755); err != nil {
		return nil, fmt.Errorf("creating run dir: %w", err)
	}
	specPath := filepath.Join(runSubdir, ".wrap-spec.json")
	outputPath := filepath.Join(runSubdir, ".wrap-result.json")
	wrapperLogPath := filepath.Join(runSubdir, "wrapper.log")

	exKind := ""
	if meta != nil {
		exKind = meta.Executor
	}
	spec.DeferCommit = exKind != "" && exKind != executor.KindLocal

	specBytes, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding spec: %w", err)
	}
	if err := os.WriteFile(specPath, specBytes, 0o600); err != nil {
		return nil, fmt.Errorf("writing spec: %w", err)
	}

	impl, err := s.pickExecutor(exKind)
	if err != nil {
		return nil, err
	}
	var res enjuYaml.Resources
	if meta != nil && meta.Resources != nil {
		res = *meta.Resources
	}
	h, err := impl.Submit(ctx, specPath, outputPath, env, res)
	if err != nil {
		return nil, fmt.Errorf("executor submit: %w", err)
	}
	return &AsyncKickoffResult{
		Executor:   h.Executor,
		PID:        h.PID,
		JobID:      h.JobID,
		SpecPath:   specPath,
		OutputPath: outputPath,
		WrapperLog: wrapperLogPath,
	}, nil
}

// stringSliceNonNil normalizes a possibly-nil []string to an
// empty slice. context.json consumers expect `reads_artifacts`
// / `writes_artifacts` to always be JSON arrays — `null` forces
// every script to special-case absent keys.
func stringSliceNonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// encodeParamEnv renders a run param or for_each iteration
// value as a shell-safe env var string. Scalars → fmt.Sprint;
// []interface{} → comma-joined. Nested structures fall back to
// JSON.
func encodeParamEnv(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []interface{}:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, encodeParamEnv(e))
		}
		return strings.Join(parts, ",")
	default:
		if b, err := json.Marshal(x); err == nil {
			return string(b)
		}
		return fmt.Sprint(x)
	}
}
