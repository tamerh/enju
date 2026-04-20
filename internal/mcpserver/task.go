package mcpserver

// Task-lifecycle handlers (excluding claim and submit, which
// live in their own files because of their size + density).
// Covers the discovery + state-management tools: list ready,
// get task detail, release, invalidate, fail, tally (force a
// multi-citizen resolution), execute (action:compute).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/compute"
	"github.com/enju-ai/enju/internal/mcpgit"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleInvalidateTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	reason := req.GetString("reason", "")

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/invalidate", map[string]string{
		"reason": reason,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatInvalidateResult(data, taskID)), nil
}
func (c *apiClient) handleTallyTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/tally", map[string]interface{}{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if errMsg := extractErrorString(data); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText(formatTallyResult(data, taskID)), nil
}
func (c *apiClient) handleListReadyTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := "/api/v1/tasks/ready"
	pid := req.GetInt("project_id", 0)
	rid := req.GetInt("run_id", 0)
	if pid > 0 && rid > 0 {
		path += fmt.Sprintf("?project_id=%d&run_id=%d", pid, rid)
	}
	data, err := c.get(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatReadyTasks(data)), nil
}
func (c *apiClient) handleReleaseTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/release", map[string]string{
		"username": c.username,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if errMsg, ok := result["error"].(string); ok {
			return mcp.NewToolResultError(errMsg), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Released task: %s", taskID)), nil
}
func (c *apiClient) handleGetTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.get(ctx, "/api/v1/tasks/"+taskID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Also fetch inputs if task has dependencies
	inputs, _ := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")

	// For review tasks, enrich with the target's result_path and
	// commit_sha so the reviewer can see what they'd be reviewing
	// before claiming.
	var taskMap map[string]interface{}
	if json.Unmarshal(data, &taskMap) == nil {
		if reviewsTarget, _ := taskMap["reviews_target"].(string); reviewsTarget != "" {
			if projectID, _ := taskMap["project_id"].(float64); projectID > 0 {
				if runSeq, _ := taskMap["run_seq"].(float64); runSeq > 0 {
					targetFullID := fmt.Sprintf("%d:%d:%s", int(projectID), int(runSeq), reviewsTarget)
					if targetData, terr := c.get(ctx, "/api/v1/tasks/"+targetFullID); terr == nil {
						var target map[string]interface{}
						if json.Unmarshal(targetData, &target) == nil {
							resultPath, _ := target["result_path"].(string)
							commitSHA, _ := target["commit_sha"].(string)
							taskMap["_review_target_path"] = resultPath
							taskMap["_review_target_commit"] = commitSHA
							taskMap["_review_target_claimed_by"] = target["claimed_by"]

							// Read preview from local workspace if available.
							if c.workspace != nil && resultPath != "" {
								remoteURL, _ := taskMap["project_remote_url"].(string)
								projName, _ := taskMap["project_name"].(string)
								if proj, perr := c.workspace.ForProject(int64(projectID), remoteURL, projName); perr == nil {
									taskMap["_review_target_abs_path"] = filepath.Join(proj.WorkDir(), resultPath, "result.md")
									contentPath := filepath.Join(resultPath, "result.md")
									if content, rerr := proj.ReadFile(contentPath); rerr == nil {
										preview := string(content)
										if len(preview) > 200 {
											preview = preview[:200] + "…"
										}
										taskMap["_review_target_preview"] = preview
									}
								}
							}

							// Re-marshal with the enriched fields.
							data, _ = json.Marshal(taskMap)
						}
					}
				}
			}
		}
	}

	return mcp.NewToolResultText(formatTaskDetail(data, inputs, c.username)), nil
}
// handleListTemplates — pure client-side tool. Walks the
// project's enju_templates/ directory in the local clone and
// returns one entry per YAML file with its metadata.
func (c *apiClient) handleFailTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	reason, err := req.RequireString("reason")
	if err != nil {
		return mcp.NewToolResultError("reason is required"), nil
	}
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/fail", map[string]string{
		"reason": reason,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)
	if errMsg, ok := resp["error"].(string); ok {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✗ Task %s failed: %s", taskID, reason)), nil
}
func (c *apiClient) handleExecuteTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("enju_execute_task requires a local workspace"), nil
	}

	// Fetch task metadata up front — we need project + branch
	// to open the workspace and run the reconcile hook. The
	// state we read here is a PROBE, not the claim gate: an
	// async chain scenario (stage2 depends on stage1) can have
	// stage2 still PENDING at this moment even though stage1's
	// commit is on origin, because no reconcile has run yet.
	// Source-of-truth state comes after the reconcile below.
	meta, err := c.fetchTaskMeta(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("task %q not found: %v", taskID, err)), nil
	}
	if meta.Action != "compute" {
		return mcp.NewToolResultError(fmt.Sprintf("enju_execute_task is only for action:compute tasks (got %q) — use enju_submit_result for %s tasks", meta.Action, meta.Action)), nil
	}
	if meta.Script == "" {
		return mcp.NewToolResultError("task has no script field declared"), nil
	}

	// Open the project workspace — openProject wires the
	// project's default_branch into the Project, but this flow
	// targets the RUN's branch explicitly via meta.Branch so
	// the default is only used as a fallback for operations
	// that don't take a branch override.
	proj, remoteURL, projName, _, err := c.openProject(ctx, meta.ProjectID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Reconcile BEFORE the claim check. Without this, an
	// async→async chain orphans stage2: stage1 completes, the
	// coordinator still sees it as claimed (no one has run a
	// tool yet), so stage2 is PENDING. If we probed state
	// first and gated the claim on ready/collecting, we'd
	// skip the claim and launch the wrapper anyway — stage2
	// stays READY forever with an orphaned completion
	// commit.
	_ = c.pullBranchWithReconcile(ctx, proj, meta.ProjectID, meta.Branch)

	// Re-fetch meta so the claim decision below sees the
	// post-reconcile state. For the chain case, stage2
	// should now be ready.
	fresh, err := c.fetchTaskMeta(ctx, taskID)
	if err == nil && fresh != nil {
		meta = fresh
	}

	// Claim gate: the task must be in a state that permits
	// claiming (ready / collecting) OR already be in a state
	// where we can run the wrapper without a fresh claim
	// (claimed / running — e.g. someone retried after a
	// transient failure). Anything else — pending, accepted,
	// failed, skipped, invalidated — means launching a wrapper
	// would leak: the commit has no task to advance via the
	// reconcile path.
	switch meta.State {
	case "ready", "collecting":
		claimData, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
			"username": c.username,
		})
		if err != nil {
			return mcp.NewToolResultError("failed to claim: " + err.Error()), nil
		}
		var claimResp map[string]interface{}
		if json.Unmarshal(claimData, &claimResp) == nil {
			if errMsg, ok := claimResp["error"].(string); ok {
				return mcp.NewToolResultError("claim failed: " + errMsg), nil
			}
		}
	case "claimed", "running":
		// Already owned — proceed (typically the retry case
		// after a transient wrapper failure).
	default:
		return mcp.NewToolResultError(fmt.Sprintf(
			"task %q is not claimable (state: %s) — run enju_run_status or wait for upstream to complete before retrying",
			taskID, meta.State,
		)), nil
	}

	workDir := proj.WorkDir()
	resultDir := mcpgit.ResultDir(meta.RunSeq, meta.InstanceKey, meta.TaskDefID)

	// Script resolution: runs instantiated from a template
	// bundle resolve `script:` relative to the per-run
	// snapshot at `.enju/runs/{seq}/template/`, not the live
	// enju_templates/ path. Guarantees reproducibility —
	// editing the template after the run was created can't
	// retroactively change this run's behavior.
	//
	// Runs created from inline YAML (no source_path) keep the
	// legacy resolution: script path is project-relative as
	// declared.
	//
	// ENJU_TEMPLATE_DIR is exposed for scripts that want to
	// read bundled data files (e.g. `$ENJU_TEMPLATE_DIR/data/ref.csv`)
	// without hardcoding the snapshot path.
	var scriptPath, templateDir string
	if meta.RunSourcePath != "" {
		templateDir = filepath.Join(workDir, fmt.Sprintf(".enju/runs/%d/template", meta.RunSeq))
		scriptPath = filepath.Join(templateDir, meta.Script)
	} else {
		scriptPath = filepath.Join(workDir, meta.Script)
	}
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return mcp.NewToolResultError(fmt.Sprintf("script %q not found at %s", meta.Script, scriptPath)), nil
	}

	// Build environment variables.
	env := os.Environ()
	env = append(env,
		"ENJU_TASK_ID="+taskID,
		"ENJU_PROJECT_DIR="+workDir,
		"ENJU_RUN_DIR="+filepath.Join(workDir, resultDir),
	)
	if templateDir != "" {
		env = append(env, "ENJU_TEMPLATE_DIR="+templateDir)
	}
	// ENJU_PARAM_<name> — run-level params + per-iteration
	// for_each vars, flattened. Gives shell scripts direct
	// access to the values the user supplied at create_run
	// and whatever stem/gene/etc. bound this instance of a
	// for_each. Without these, scripts were trapped with
	// just the 4 "infrastructure" vars (TASK_ID, PROJECT_DIR,
	// RUN_DIR, TEMPLATE_DIR) and couldn't reach run context.
	//
	// Iteration vars take precedence on name collision —
	// though the parser already rejects run params and
	// for_each vars sharing a name, this is a belt-and-
	// suspenders guard.
	for k, v := range meta.RunParams {
		env = append(env, "ENJU_PARAM_"+k+"="+encodeParamEnv(v))
	}
	for k, v := range meta.InstanceParams {
		env = append(env, "ENJU_PARAM_"+k+"="+encodeParamEnv(v))
	}
	// Task-definition-level env: block. Injected verbatim —
	// values were already {{param}}-substituted at parse time,
	// and the validator rejected any ENJU_-prefixed keys up
	// front so these three namespaces (ENJU_* system,
	// ENJU_PARAM_<name>, task env) stay disjoint. No
	// precedence rule needed.
	for k, v := range meta.Env {
		env = append(env, k+"="+v)
	}

	// context.json — structured companion to the env vars.
	// Writes to $ENJU_RUN_DIR/context.json BEFORE the script
	// runs so scripts in any language can read
	//   jq -r '.params.source_repo' "$ENJU_RUN_DIR/context.json"
	// Covers cases env vars can't: list values with commas,
	// typed numbers/bools, structured artifact lists.
	// Committed as part of the result (below, via the files
	// slice) so each run's `.enju/runs/{seq}/{task}/` directory
	// is self-documenting — "what was this task told?" is a
	// git-log question with a concrete answer.
	// Build the payload. ReadsArtifacts isn't on taskMeta
	// today (only writes are — we pipe them through for the
	// executor's "pick up declared outputs after script exits"
	// logic). Fetch the task response inline for reads; the
	// one extra GET is acceptable for the convenience of a
	// structured context drop.
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
		// context.json exposes paths-only for back-compat with
		// compute scripts that expect a flat string list.
		// Track-flag routing lives in the wrapper, not script
		// userspace — the script always writes to disk the same
		// way regardless.
		"writes_artifacts": stringSliceNonNil(meta.WritesArtifacts.Paths()),
	}
	contextBytes, _ := json.MarshalIndent(contextPayload, "", "  ")
	contextFullPath := filepath.Join(workDir, resultDir, "context.json")
	if err := os.MkdirAll(filepath.Dir(contextFullPath), 0755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("creating run dir for context.json: %v", err)), nil
	}
	if err := os.WriteFile(contextFullPath, contextBytes, 0644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("writing context.json: %v", err)), nil
	}

	// Execute the script via the compute wrapper. In phase 1
	// sync mode this is an in-process call — the same logic the
	// `enju wrap-task` subcommand runs, just without the fork.
	// Phase 4 flips async tasks to a detached subprocess so
	// long-running jobs survive MCP session close; sync keeps
	// the in-process path for latency + simpler error surface.
	spec := compute.Spec{
		TaskID:        taskID,
		ProjectID:     meta.ProjectID,
		RemoteURL:     remoteURL,
		WorkspaceRoot: c.workspace.RootDir(),
		ProjectName:   projName,
		Branch:        meta.Branch,
		ResultDir:     resultDir,
		ScriptPath:    scriptPath,
		ScriptLabel:   meta.Script,
		// Tracked paths → committed in the task's result commit.
		// Untracked paths → kept on disk, reported to the
		// coordinator with tracked=false so downstream tasks can
		// see they were produced (Phase C of untracked artifacts).
		WritesArtifacts:    meta.WritesArtifacts.TrackedPaths(),
		UntrackedArtifacts: meta.WritesArtifacts.UntrackedPaths(),
		AuthorName:         c.citizenName,
		AuthorEmail:        c.citizenEmail,
		Username:           c.username,
		Model:              c.modelName,
		Container:          meta.Container,
		// Container mode uses this as the host-env allowlist
		// (alongside ENJU_*). Direct-exec mode ignores it —
		// the flat env slice above already carries these.
		Env: meta.Env,
	}

	// Async kickoff: long-running compute jobs (SLURM, multi-
	// hour pipelines) must outlive the MCP session. Spawn a
	// detached wrapper subprocess, return immediately; the
	// fetch-path scanner (phase 4c) reconciles the result when
	// the wrapper's commit lands on origin.
	if resolvedMode(meta) == "async" {
		kick, err := c.kickoffAsyncWrapTask(spec, env, resultDir, workDir)
		if err != nil {
			return mcp.NewToolResultError("async kickoff: " + err.Error()), nil
		}
		return mcp.NewToolResultText(formatAsyncKickoff(taskID, meta.Script, kick)), nil
	}

	res := compute.Run(ctx, spec, env, c.logger)
	if res.Error != "" {
		return mcp.NewToolResultError(res.Error), nil
	}
	elapsed := time.Duration(res.ElapsedMS) * time.Millisecond

	// Exit non-zero → auto-fail the task via the coordinator.
	// script.log stays on local disk as the debug transcript;
	// it's not committed on failure paths because no result
	// commit happens when a task fails. A human who wants it
	// archived can commit manually.
	if res.ExitCode != 0 {
		stderrStr := res.Stderr
		if len(stderrStr) > 1000 {
			stderrStr = stderrStr[:1000] + "...(truncated)"
		}
		reason := fmt.Sprintf("script %s exited with code %d", meta.Script, res.ExitCode)
		if stderrStr != "" {
			reason += ": " + stderrStr
		}
		c.post(ctx, "/api/v1/tasks/"+taskID+"/fail", map[string]string{
			"reason": reason,
		})
		var b strings.Builder
		b.WriteString(fmt.Sprintf("✗ Script failed (exit %d, %s)\n", res.ExitCode, elapsed))
		if stderrStr != "" {
			b.WriteString(fmt.Sprintf("  stderr: %s\n", stderrStr))
		}
		if res.ScriptLogPath != "" {
			b.WriteString(fmt.Sprintf("  Transcript: %s (local only, not committed on failure)\n", res.ScriptLogPath))
		}
		b.WriteString(fmt.Sprintf("  Task %s failed — downstream tasks blocked.\n", taskID))
		return mcp.NewToolResultText(b.String()), nil
	}

	// Exit 0 → wrapper already committed + pushed. Cursor was
	// advanced inside SubmitTaskResult (via spec.StateDir), so
	// subsequent pullBranchWithReconcile calls won't re-post
	// this commit as a "new" trailer event.

	// Report the landed commit to the coordinator so the state
	// machine, artifact index, and contribution counter advance.
	content := res.Content
	reportBody := map[string]interface{}{
		"commit_sha":  res.CommitSHA,
		"result_path": resultDir,
		"model":       c.modelName,
		"username":    c.username,
		"content":     content,
	}
	if len(res.ArtifactsWritten) > 0 {
		reportBody["artifacts_written"] = res.ArtifactsWritten
	}
	reportData, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", reportBody)
	if err != nil {
		return mcp.NewToolResultError("coordinator report failed: " + err.Error()), nil
	}

	// Format response.
	var b strings.Builder
	b.WriteString(fmt.Sprintf("✓ Script completed (exit 0, %s)\n", elapsed))
	b.WriteString(fmt.Sprintf("  Script:  %s\n", meta.Script))
	b.WriteString(fmt.Sprintf("  Output:  %d bytes written to result.md\n", len(content)))
	b.WriteString(fmt.Sprintf("  Commit:  %s\n", shortSHA(res.CommitSHA)))
	if len(res.ArtifactsWritten) > 0 {
		b.WriteString(fmt.Sprintf("  Artifacts: %s\n", strings.Join(res.ArtifactsWritten, ", ")))
	}
	if len(res.MissingArtifacts) > 0 {
		// Warn loud — a declared path that the script didn't
		// produce is usually a silent bug the author wants to
		// know about.
		b.WriteString(fmt.Sprintf("  ⚠ Missing (declared but not written by script): %s\n", strings.Join(res.MissingArtifacts, ", ")))
	}

	// Contribution counter from the report response.
	var report map[string]interface{}
	if json.Unmarshal(reportData, &report) == nil {
		if n := jsonFloat(report["contribution_number"]); n > 0 {
			b.WriteString(fmt.Sprintf("\nContribution #%d\n", int(n)))
		}
		if ready := jsonFloat(report["newly_ready"]); ready > 0 {
			b.WriteString(fmt.Sprintf("Impact: %d new task(s) unlocked.\n", int(ready)))
		}
	}

	return mcp.NewToolResultText(b.String()), nil
}

// stringSliceNonNil normalizes a possibly-nil []string to an
// empty slice. context.json consumers expect `reads_artifacts`
// / `writes_artifacts` to always be JSON arrays — `null` forces
// every script to special-case absent keys. `[]` is equally
// valid JSON and skips the null-check.
func stringSliceNonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// encodeParamEnv renders a run param or for_each iteration
// value as a shell-safe env var string. Scalars → fmt.Sprint;
// []interface{} → comma-joined (list<string> round-trips
// through JSON as []interface{} of strings). Nested structures
// fall back to JSON — unlikely for param types Enju supports
// today (string / int / bool / list<string>) but keeps the
// encoder defensible if the type surface grows later.
//
// Comma-joining loses fidelity when list elements contain
// commas; that's what the upcoming context.json Phase B
// exists to cover (structured, language-agnostic JSON drop).
// For the common case — identifiers, paths, gene symbols —
// comma-joining is exactly what shell authors want.
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
		// JSON numbers decode as float64. Render integers
		// without trailing ".000000" so scripts can use them
		// directly (count math, seed args).
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
