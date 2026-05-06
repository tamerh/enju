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
	"syscall"
	"time"

	"github.com/enju-ai/enju/internal/common/format"
	corelayout "github.com/enju-ai/enju/internal/common/layout"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/compute"
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
	if s.project == nil {
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

	proj, remoteURL, projName, _, err := s.OpenProject(ctx, meta.ProjectID)
	if err != nil {
		return nil, err
	}

	// Reconcile before the claim gate — an async→async chain
	// otherwise orphans downstream tasks because the coordinator
	// hasn't yet seen the upstream completion commit. Best-effort;
	// a reconcile failure here just means slightly stale state.
	_ = s.PullBranchWithReconcile(ctx, proj, meta.ProjectID, meta.Branch)

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
		if err := s.claimWithTransientRetry(ctx, taskID); err != nil {
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
	// paths as declared.
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
		WorkspaceRoot:      s.project.RootDir(),
		ProjectName:        projName,
		Branch:             meta.Branch,
		ResultDir:          resultDir,
		ScriptPath:         scriptPath,
		ScriptLabel:        meta.Script,
		WritesArtifacts:    meta.WritesArtifacts,
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
		Model:     s.modelName,
		Container: meta.Container,
		Env:       meta.Env,
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

	res := compute.Run(ctx, spec, env, s.logger)
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
func buildComputeEnv(taskID, workDir, resultDir, templateDir string, meta *TaskMeta) []string {
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
// 3 attempts with 50/100/200ms backoff. Total max wait ~350ms,
// short enough that callers don't notice the retries unless
// the network is really down.
func (s *FatClient) claimWithTransientRetry(ctx context.Context, taskID string) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		claimData, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
			"username": s.coord.Username(),
			"model":    s.modelName,
		})
		if err != nil {
			if !isTransientTransportError(err) {
				return fmt.Errorf("claim %s: %w", taskID, err)
			}
			lastErr = fmt.Errorf("claim %s: %w", taskID, err)
			sleepBackoff(attempt)
			continue
		}
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
		return nil
	}
	return fmt.Errorf("after %d retries: %w", maxAttempts, lastErr)
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

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
