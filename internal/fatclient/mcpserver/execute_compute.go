package mcpserver

// Extracted compute-task execution core. executeComputeTask
// is the per-task worker for both enju_execute_task (single
// task, renders free-text response) and enju_execute_run
// (batch cascade, consumes the structured outcome). Keeping
// the structured shape lets the batch caller stop/continue
// per entry without re-parsing formatted text.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/compute"
	corelayout "github.com/enju-ai/enju/internal/core/layout"
)

// executeOutcome is the structured result of one compute-task
// execution. Exactly one of Status ∈ {completed, failed,
// async_started} is set per call. All display/formatting happens
// at the call site.
type executeOutcome struct {
	TaskID           string
	Script           string
	Status           string // "completed" | "failed" | "async_started" | "git_failed"
	ExitCode         int
	ElapsedMS        int64
	CommitSHA        string
	ContentLen       int
	ArtifactsWritten []string
	MissingArtifacts []string
	ScriptLogPath    string
	ErrorMessage     string // populated when Status == "failed" or "git_failed"
	Stderr           string // captured stderr on failure (truncated at source)
	ContribNum       int    // coordinator's contribution counter (0 if unknown)
	NewlyReady       int    // count of tasks that transitioned to READY after this commit
	Async            *asyncKickoffResult
	// Branch is the run branch the task executed on — useful to
	// callers that batch multiple tasks and want to pass it on.
	Branch string
}

// executeComputeTask runs one action:compute task end-to-end:
// reconcile → claim (if needed) → script execution → result
// report. Mirrors the shape of handleExecuteTask but returns
// structured data instead of an MCP text response.
//
// The error return is reserved for "cannot even attempt" conditions
// (task not found, wrong action, no workspace, script missing,
// claim gate closed). A script that runs and exits non-zero is
// NOT an error — it returns (outcome{Status:"failed"}, nil) so
// the batch caller can record it and stop the cascade
// gracefully without unwinding as a generic failure.
func (c *apiClient) executeComputeTask(ctx context.Context, taskID string) (*executeOutcome, error) {
	if c.workspace == nil {
		return nil, fmt.Errorf("enju_execute_task requires a local workspace")
	}

	meta, err := c.fetchTaskMeta(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if meta.Action != "compute" {
		return nil, fmt.Errorf("task %q is action=%q, not compute — use enju_submit_result", taskID, meta.Action)
	}
	if meta.Script == "" {
		return nil, fmt.Errorf("task %q has no script field declared", taskID)
	}

	proj, remoteURL, projName, _, err := c.openProject(ctx, meta.ProjectID)
	if err != nil {
		return nil, err
	}

	// Reconcile before the claim gate — an async→async chain
	// otherwise orphans downstream tasks because the coordinator
	// hasn't yet seen the upstream completion commit. Best-effort;
	// a reconcile failure here just means slightly stale state.
	_ = c.pullBranchWithReconcile(ctx, proj, meta.ProjectID, meta.Branch)

	// Re-fetch so the claim decision sees the post-reconcile
	// state (esp. for chained async tasks that just transitioned
	// to ready as a side effect of the pull).
	if fresh, ferr := c.fetchTaskMeta(ctx, taskID); ferr == nil && fresh != nil {
		meta = fresh
	}

	switch meta.State {
	case "ready", "collecting":
		// Claim with transient-error retry. Parallel cascades can
		// stack short-lived BUSY / 5xx / network blips on the
		// coordinator side; without a retry, the cascade aborts
		// with stop_reason=compute_errored on what should be a
		// recoverable hiccup. The store layer's _txlock=immediate
		// + ApplyPlan retry already cover SQLITE_BUSY end-to-end,
		// so this loop's main job is mopping up coordinator-side
		// transient HTTP/network errors.
		if err := c.claimWithTransientRetry(ctx, taskID); err != nil {
			return nil, err
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

	workDir := proj.WorkDir()
	resultDir := meta.ResultDir

	// Script resolution: template runs pin scripts to the per-run
	// snapshot directory; inline-YAML runs use project-relative
	// paths as declared. See handleExecuteTask for the full
	// rationale — this block mirrors it exactly.
	var scriptPath, templateDir string
	if meta.RunSourcePath != "" {
		templateDir = filepath.Join(workDir, corelayout.RunTemplateSnapshotDir(meta.RunSeq, meta.RunSlug))
		scriptPath = filepath.Join(templateDir, meta.Script)
	} else {
		scriptPath = filepath.Join(workDir, meta.Script)
	}
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("script %q not found at %s", meta.Script, scriptPath)
	}

	env := buildComputeEnv(taskID, workDir, resultDir, templateDir, meta)

	// context.json — structured companion to the env vars.
	// See handleExecuteTask for the motivation; this block
	// stays identical so both entry points produce the same
	// on-disk payload.
	var readsArtifacts []string
	if rawTask, rerr := c.get(ctx, "/api/v1/tasks/"+taskID); rerr == nil {
		var tm map[string]interface{}
		if json.Unmarshal(rawTask, &tm) == nil {
			if r, ok := tm["reads_artifacts"].([]interface{}); ok {
				for _, p := range r {
					if s, ok := p.(string); ok {
						readsArtifacts = append(readsArtifacts, s)
					}
				}
			}
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
	contextFullPath := filepath.Join(workDir, resultDir, "context.json")
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
		WorkspaceRoot:      c.workspace.RootDir(),
		ProjectName:        projName,
		Branch:             meta.Branch,
		ResultDir:          resultDir,
		ScriptPath:         scriptPath,
		ScriptLabel:        meta.Script,
		WritesArtifacts:    meta.WritesArtifacts.TrackedPaths(),
		UntrackedArtifacts: meta.WritesArtifacts.UntrackedPaths(),
		AuthorName:         c.citizenName,
		AuthorEmail:        c.citizenEmail,
		Username:           c.username,
		// Compute attribution uses the session model unconditionally,
		// no per-call override path. Rationale: an answer/review/vote
		// submit attributes the LLM that produced the words, which
		// genuinely varies per turn (caller might draft with Opus
		// then ratify with Sonnet). A compute submit attributes the
		// citizen who LAUNCHED THE SCRIPT — the script itself
		// produces deterministic bytes from code + inputs, not LLM
		// output. The "who initiated" answer doesn't change mid-run.
		// If an operator wants attribution for a different model,
		// they restart MCP with -model X and re-execute.
		Model:              c.modelName,
		Container:          meta.Container,
		Env:                meta.Env,
	}

	if resolvedMode(meta) == "async" {
		kick, err := c.kickoffAsyncWrapTask(spec, env, resultDir, workDir)
		if err != nil {
			return nil, fmt.Errorf("async kickoff: %w", err)
		}
		return &executeOutcome{
			TaskID: taskID,
			Script: meta.Script,
			Status: "async_started",
			Async:  kick,
			Branch: meta.Branch,
		}, nil
	}

	res := compute.Run(ctx, spec, env, c.logger)
	if res.Error != "" {
		return nil, fmt.Errorf("%s", res.Error)
	}

	// Script ran fine but the post-script git operation failed
	// (commit/push retry exhausted, "object not found" on a
	// freshly-added remote, etc.). Surface as Status="git_failed"
	// so batch callers (enju_execute_run) can route to a distinct
	// stop_reason instead of conflating with script-failed —
	// the work product is still on disk in spec.ResultDir; the
	// recovery is fix-the-git-state, not re-run-the-script.
	//
	// Backwards-compat fallback: old wrappers that didn't set
	// GitError put the same message in Error with a "git submit
	// failed:" prefix, so we treat that path identically. New
	// wrappers populate GitError directly.
	gitErrMsg := res.GitError
	if gitErrMsg == "" && res.Error != "" && strings.HasPrefix(res.Error, compute.GitSubmitFailedPrefix) {
		gitErrMsg = res.Error
	}
	if gitErrMsg != "" {
		return &executeOutcome{
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
		c.post(ctx, "/api/v1/tasks/"+taskID+"/fail", map[string]string{
			"reason": reason,
		})
		return &executeOutcome{
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
		"model":       c.modelName,
		"username":    c.username,
		"content":     res.Content,
	}
	if len(res.ArtifactsWritten) > 0 {
		reportBody["artifacts_written"] = res.ArtifactsWritten
	}
	reportData, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", reportBody)
	if err != nil {
		return nil, fmt.Errorf("coordinator report for %s: %w", taskID, err)
	}

	out := &executeOutcome{
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
		if n := jsonFloat(report["contribution_number"]); n > 0 {
			out.ContribNum = int(n)
		}
		if r := jsonFloat(report["newly_ready"]); r > 0 {
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
func buildComputeEnv(taskID, workDir, resultDir, templateDir string, meta *taskMeta) []string {
	env := os.Environ()
	env = append(env,
		"ENJU_TASK_ID="+taskID,
		"ENJU_PROJECT_DIR="+workDir,
		"ENJU_RUN_DIR="+filepath.Join(workDir, resultDir),
	)
	if templateDir != "" {
		env = append(env, "ENJU_TEMPLATE_DIR="+templateDir)
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
// Transient classes covered:
//
//   - Coordinator unreachable / network blip (transport-level
//     error from c.post).
//   - SQLITE_BUSY / "database is locked" surfacing in the
//     response body's error field. The store-layer retry in
//     ApplyPlan should normally catch this before it reaches
//     here, but a particularly nasty contention pile-up could
//     exhaust those 5 attempts; this is the second tier.
//
// 3 attempts with 50/100/200ms backoff. Total max wait ~350ms,
// short enough that callers don't notice the retries unless
// the network is really down.
func (c *apiClient) claimWithTransientRetry(ctx context.Context, taskID string) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		claimData, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
			"username": c.username,
			"model":    c.modelName, // operator/model design — empty for unaided humans
		})
		if err != nil {
			// Transport-level error. Check if it's transient
			// — connection refused, EOF, timeout, etc. — and
			// retry; otherwise give up.
			if !isTransientTransportError(err) {
				return fmt.Errorf("claim %s: %w", taskID, err)
			}
			lastErr = fmt.Errorf("claim %s: %w", taskID, err)
			sleepBackoff(attempt)
			continue
		}
		// HTTP-level success — but the coordinator may still
		// have surfaced an error in the body.
		var claimResp map[string]interface{}
		if json.Unmarshal(claimData, &claimResp) == nil {
			if errMsg, _ := claimResp["error"].(string); errMsg != "" {
				if !isTransientCoordinatorError(errMsg) {
					return fmt.Errorf("claim %s: %s", taskID, errMsg)
				}
				lastErr = fmt.Errorf("claim %s: %s", taskID, errMsg)
				sleepBackoff(attempt)
				continue
			}
		}
		// Success.
		return nil
	}
	return fmt.Errorf("after %d retries: %w", maxAttempts, lastErr)
}

// isTransientTransportError reports whether a transport-level
// error from c.post is the sort that's worth retrying.
// Pattern-matches against the friendly wrappers c.post emits
// ("coordinator unreachable: ...") plus net-package error text
// for connection refused / EOF / timeout.
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
// returned error string (from response body's `error` field)
// indicates a transient condition worth retrying. The current
// patterns are SQLITE_BUSY-family — the store layer retries
// these first, so seeing one here means contention exhausted
// those retries too. One more shot with backoff.
func isTransientCoordinatorError(msg string) bool {
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}

// sleepBackoff is the exponential backoff between retry
// attempts. Caps at 200ms so the worst case is ~350ms total
// across 3 attempts — invisible to humans, generous to the
// coordinator.
func sleepBackoff(attempt int) {
	d := time.Duration(50<<attempt) * time.Millisecond
	if d > 200*time.Millisecond {
		d = 200 * time.Millisecond
	}
	time.Sleep(d)
}
