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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/format"
	corelayout "github.com/enju-ai/enju/internal/common/layout"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
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
	PID        int
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
	return enjuYaml.ResolvedModeFields(meta.Action, meta.Mode)
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
	_ = s.PullBranchWithReconcileWF(ctx, wf, meta.ProjectID, meta.Branch)

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

	taskScratchDir := compute.ResolveTaskScratchDir(s.enjugit.RootDir(), s.coord.Username(), taskID, meta.IterSeq)

	// Per-run-snapshot redesign: a single materialization of the
	// run branch's whole tree lives at <project>/.enju/runs/<N>/snapshot/
	// (RunSnapshotOnDiskDir). Every task in the run reads from
	// there — task defs, scripts, sibling files, and the rest of
	// the frozen repo via $ENJU_REPO_DIR.
	//
	// create_run does the materialization once and the directory
	// survives until the run sweep. If it's missing at claim time
	// (operator wiped .enju/, or the sweep already ran while
	// retrying a stale task) we re-materialize from .git/objects/
	// — cheap and idempotent.
	//
	// Fallback path: when no workspace is set (legacy/test paths
	// without a real project root) we still read from the
	// worktree's enju/runs/<N>/template-snapshot/ directly. Those
	// paths only ever exercise one run at a time so the
	// concurrency hazard the per-run materialization addresses
	// doesn't bite.
	repoSnapshotDir := ""
	templateSnapshotDir := ""
	if wf.ProjectRoot() != "" && meta.Branch != "" {
		repoSnapshotDir = filepath.Join(wf.ProjectRoot(), corelayout.RunSnapshotOnDiskDir(meta.RunSeq, meta.RunSlug))
		// path= runs: the workflow YAML and its neighbors live at
		// their authored paths inside the materialized snapshot.
		// The "template dir" is the workflow YAML's containing
		// directory:
		//   - YAML file path ("workflows/scan-deps/enju.yaml") →
		//     dirname → "workflows/scan-deps"
		//   - Directory path ("enju/templates/variant-calling") →
		//     the dir itself
		// No more enju/runs/N/template-snapshot/ subdir nesting.
		//
		// Inline-YAML runs (no RunSourcePath): legacy committed
		// template-snapshot/ path still applies — the inline
		// YAML doesn't exist anywhere else.
		if meta.RunSourcePath != "" {
			templateRel := meta.RunSourcePath
			if strings.HasSuffix(templateRel, ".yaml") || strings.HasSuffix(templateRel, ".yml") {
				templateRel = filepath.Dir(templateRel)
			}
			templateSnapshotDir = filepath.Join(repoSnapshotDir, templateRel)
		} else {
			templateSnapshotDir = filepath.Join(repoSnapshotDir, corelayout.RunTemplateSnapshotDir(meta.RunSeq, meta.RunSlug))
		}
		if _, statErr := os.Stat(repoSnapshotDir); os.IsNotExist(statErr) {
			if _, merr := wf.MaterializeRunRepo(meta.Branch, repoSnapshotDir); merr != nil {
				return nil, fmt.Errorf("materializing run snapshot from branch %q: %w", meta.Branch, merr)
			}
		}
	} else {
		templateSnapshotDir = filepath.Join(workDir, corelayout.RunTemplateSnapshotDir(meta.RunSeq, meta.RunSlug))
	}
	taskDef, err := enjuYaml.LoadTaskDefFromSnapshot(templateSnapshotDir, meta.TaskDefID)
	if err != nil {
		return nil, fmt.Errorf("loading task def from snapshot: %w", err)
	}

	// Script resolution: template runs pin scripts to the per-run
	// template snapshot directory; inline-YAML runs use
	// project-relative paths as declared.
	var scriptPath, templateDir string
	if meta.RunSourcePath != "" {
		templateDir = templateSnapshotDir
		scriptPath = filepath.Join(templateDir, meta.Script)
	} else {
		scriptPath = filepath.Join(workDir, meta.Script)
	}
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("script %q not found at %s", meta.Script, scriptPath)
	}

	// Read-only is now convention, not host-side enforcement.
	// Scripts that write to $ENJU_REPO_DIR or $ENJU_TEMPLATE_DIR
	// are buggy and should target $ENJU_SCRATCH instead.
	// Container path still gets a kernel-side :ro bind for the
	// strong guarantee inside the sandbox.

	// Resolve + pre-create the bigfiles dir for this branch so
	// the script can write track:false outputs into it without
	// "no such file or directory". Best-effort: if MkdirAll fails
	// (permission, full disk), let the script error path surface
	// it — the resolver always returns a non-empty path in the
	// production layout, so an mkdir failure here is genuine IO
	// trouble worth seeing.
	bigfilesDir := enjugit.ResolveBigfilesDir(wf.ProjectRoot(), meta.ProjectID, projName, meta.Branch)
	if bigfilesDir != "" {
		_ = os.MkdirAll(bigfilesDir, 0755)
	}

	// taskScratchDir was resolved earlier (also used for snapshot
	// materialization above). Empty workspace root → "" — the
	// wrapper's lifecycle is a no-op in that case, preserving
	// legacy/test behavior.

	env := buildComputeEnv(taskID, workDir, resultDir, templateDir, repoSnapshotDir, bigfilesDir, taskScratchDir, meta)

	// context.json — structured companion to the env vars.
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

	contextPayload := map[string]interface{}{
		"task_id":          taskID,
		"task_def_id":      meta.TaskDefID,
		"instance_key":     meta.InstanceKey,
		"iteration":        meta.InstanceParams,
		"params":           meta.RunParams,
		"reads_artifacts":  stringSliceNonNil(readsArtifacts),
		"writes_artifacts": stringSliceNonNil(meta.WritesArtifacts.Paths()),
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

	spec := compute.Spec{
		TaskID:             taskID,
		ProjectID:          meta.ProjectID,
		RemoteURL:          remoteURL,
		WorkspaceRoot:      s.enjugit.RootDir(),
		ProjectName:        projName,
		Branch:             meta.Branch,
		// IterationBranch is the per-task topic branch coord
		// populated at claim time. compute.Run uses it as
		// BranchOverride for SubmitComputeTaskResult so each
		// parallel sibling commits to its own ref, isolated from
		// the others. Coord's auto-merge then FFs the topic onto
		// meta.Branch (the run branch) on accept.
		IterationBranch:    meta.IterationBranch,
		ResultDir:          resultDir,
		BigfilesDir:        bigfilesDir,
		ScriptPath:         scriptPath,
		ScriptLabel:        meta.Script,
		WritesArtifacts:    meta.WritesArtifacts,
		ReadsArtifacts:     readsArtifacts,
		ReadsSourceSHA:     readsSourceSHA,
		TaskScratchDir:     taskScratchDir,
		// SnapshotDir is the script's read-only working directory
		// when the task came from a template run (RunSourcePath
		// non-empty). Inline-YAML runs leave it empty — they don't
		// have a snapshot to mount; the script lives in the
		// project clone and CWD falls back to scratch or workDir
		// via ScriptCwdFor.
		SnapshotDir:        templateDir,
		AuthorName:         s.coord.CitizenName(),
		AuthorEmail:        s.coord.CitizenEmail(),
		Username:           s.coord.Username(),
		// Compute attribution uses the session model unconditionally,
		// no per-call override path. Rationale: a citizen-action
		// submit attributes the LLM that produced the words, which
		// genuinely varies per turn. A compute submit attributes the
		// citizen who LAUNCHED THE SCRIPT — the script itself
		// produces deterministic bytes from code + inputs, not LLM
		// output. The "who initiated" answer doesn't change mid-run.
		Model:            s.modelName,
		Container:        taskDef.Container,
		ContainerRuntime: taskDef.ContainerRuntime,
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
		kick, err := s.kickoffAsyncWrapTask(spec, env, resultDir, workDir)
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
	if res.Error != "" {
		return nil, fmt.Errorf("%s", res.Error)
	}

	// Script ran fine but the post-script git operation failed
	// (commit/push retry exhausted, "object not found" on a
	// freshly-added remote, etc.). Surface as Status="git_failed"
	// so batch callers route to a distinct stop_reason instead
	// of conflating with script-failed — the work product is
	// still on disk in spec.ResultDir; recovery is fix-the-git-
	// state, not re-run-the-script.
	gitErrMsg := res.GitError
	if gitErrMsg == "" && res.Error != "" && strings.HasPrefix(res.Error, compute.GitSubmitFailedPrefix) {
		gitErrMsg = res.Error
	}
	if gitErrMsg != "" {
		return &ExecuteOutcome{
			TaskID:        taskID,
			Script:        meta.Script,
			Status:        "git_failed",
			ElapsedMS:     res.ElapsedMS,
			ContentLen:    len(res.Content),
			ScriptLogPath: res.ScriptLogPath,
			ErrorMessage:  gitErrMsg,
			Branch:        meta.Branch,
		}, nil
	}

	if res.ExitCode != 0 {
		stderr := res.Stderr
		if len(stderr) > 1000 {
			stderr = stderr[:1000] + "...(truncated)"
		}
		reason := fmt.Sprintf("script %s exited with code %d", meta.Script, res.ExitCode)
		if stderr != "" {
			reason += ": " + stderr
		}
		s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/fail", map[string]string{
			"reason": reason,
		})
		return &ExecuteOutcome{
			TaskID:        taskID,
			Script:        meta.Script,
			Status:        "failed",
			ExitCode:      res.ExitCode,
			ElapsedMS:     res.ElapsedMS,
			ScriptLogPath: res.ScriptLogPath,
			ErrorMessage:  reason,
			Stderr:        stderr,
			Branch:        meta.Branch,
		}, nil
	}

	// Exit 0 → wrapper committed + pushed. Report the landed
	// commit to the coordinator so the state machine advances.
	reportBody := map[string]interface{}{
		"commit_sha":  res.CommitSHA,
		"result_path": resultDir,
		"model":       s.modelName,
		"username":    s.coord.Username(),
		"content":     res.Content,
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
func buildComputeEnv(taskID, workDir, resultDir, templateDir, repoSnapshotDir, bigfilesDir, taskScratchDir string, meta *TaskMeta) []string {
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
		claimData, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
			"username": s.coord.Username(),
			"model":    s.modelName,
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

// kickoffAsyncWrapTask spawns a detached `enju wrap-task`
// subprocess and returns without waiting. The subprocess
// inherits `env` (so ENJU_PARAM_* + task env vars reach the
// script). Its stdin is /dev/null, stdout/stderr redirect to
// a log file alongside the task's result dir so post-mortem
// debugging works even when the MCP session is long gone.
//
// Detach mechanism: Setsid puts the child in a new session /
// process group / no controlling terminal, so SIGHUP from the
// user's shell doesn't propagate, and when the parent MCP
// process exits, the child is adopted by init rather than
// killed. The parent doesn't Wait() — a background goroutine
// reaps to avoid a zombie.
func (s *FatClient) kickoffAsyncWrapTask(spec compute.Spec, env []string, resultDir, workDir string) (*AsyncKickoffResult, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating enju binary: %w", err)
	}

	runSubdir := filepath.Join(workDir, resultDir)
	if err := os.MkdirAll(runSubdir, 0755); err != nil {
		return nil, fmt.Errorf("creating run dir: %w", err)
	}
	specPath := filepath.Join(runSubdir, ".wrap-spec.json")
	outputPath := filepath.Join(runSubdir, ".wrap-result.json")
	wrapperLogPath := filepath.Join(runSubdir, "wrapper.log")

	specBytes, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding spec: %w", err)
	}
	if err := os.WriteFile(specPath, specBytes, 0600); err != nil {
		return nil, fmt.Errorf("writing spec: %w", err)
	}

	logFile, err := os.Create(wrapperLogPath)
	if err != nil {
		return nil, fmt.Errorf("opening wrapper log: %w", err)
	}
	// We do NOT defer logFile.Close(). The subprocess inherits
	// the fd; closing here would yank it mid-write. The OS
	// closes it when the subprocess exits.

	cmd := exec.Command(self, "wrap-task", "--spec", specPath, "--output", outputPath)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("starting wrap-task: %w", err)
	}
	pid := cmd.Process.Pid

	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return &AsyncKickoffResult{
		PID:        pid,
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
