package mcpserver

// Task-lifecycle handlers (excluding claim and submit, which
// live in their own files because of their size + density).
// Covers the discovery + state-management tools: list ready,
// get task detail, release, invalidate, fail, tally (force a
// multi-citizen resolution), execute (action:compute).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

	// Fetch task metadata.
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

	// Claim if not already claimed.
	if meta.State == "ready" || meta.State == "collecting" {
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
	}

	// Open the project workspace — openProject wires the
	// project's default_branch into the Project, but this flow
	// targets the RUN's branch explicitly via meta.Branch so
	// the default is only used as a fallback for operations
	// that don't take a branch override.
	proj, _, _, _, err := c.openProject(ctx, meta.ProjectID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj.Lock()
	_ = proj.PullBranch(meta.Branch)
	proj.Unlock()

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
		"writes_artifacts": stringSliceNonNil(meta.WritesArtifacts),
	}
	contextBytes, _ := json.MarshalIndent(contextPayload, "", "  ")
	contextFullPath := filepath.Join(workDir, resultDir, "context.json")
	if err := os.MkdirAll(filepath.Dir(contextFullPath), 0755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("creating run dir for context.json: %v", err)), nil
	}
	if err := os.WriteFile(contextFullPath, contextBytes, 0644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("writing context.json: %v", err)), nil
	}

	// Execute the script.
	startTime := time.Now()
	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Dir = workDir
	cmd.Env = env
	// Three buffers:
	//   stdout     — stdout-only, becomes result.md (the
	//                contract-defined task answer).
	//   stderr    — stderr-only, becomes failure reason on
	//                non-zero exit.
	//   scriptLog — combined stdout+stderr, written to
	//                $ENJU_RUN_DIR/script.log as the full
	//                debug transcript. Preserves chronological
	//                order via io.MultiWriter fan-out to both
	//                the stream-specific buffer and the shared
	//                log.
	var stdout, stderr, scriptLog bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, &scriptLog)
	cmd.Stderr = io.MultiWriter(&stderr, &scriptLog)

	execErr := cmd.Run()
	elapsed := time.Since(startTime).Round(time.Millisecond)
	exitCode := 0
	if execErr != nil {
		if exitErr, ok := execErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return mcp.NewToolResultError(fmt.Sprintf("failed to run script: %v", execErr)), nil
		}
	}

	// Write the combined script.log to disk before we branch
	// on exit code, so the file is always present — audit
	// trail works whether the script succeeded or failed, and
	// a human debugging a failure can `cat .enju/runs/.../script.log`
	// even before the coordinator has processed the fail.
	scriptLogPath := filepath.Join(workDir, resultDir, "script.log")
	if err := os.MkdirAll(filepath.Dir(scriptLogPath), 0755); err == nil {
		_ = os.WriteFile(scriptLogPath, scriptLog.Bytes(), 0644)
	}

	// Exit non-zero → auto-fail the task via the coordinator.
	// script.log stays on local disk as the debug transcript;
	// it's not committed on failure paths because no result
	// commit happens when a task fails. A human who wants it
	// archived can commit manually.
	if exitCode != 0 {
		stderrStr := stderr.String()
		if len(stderrStr) > 1000 {
			stderrStr = stderrStr[:1000] + "...(truncated)"
		}
		reason := fmt.Sprintf("script %s exited with code %d", meta.Script, exitCode)
		if stderrStr != "" {
			reason += ": " + stderrStr
		}
		c.post(ctx, "/api/v1/tasks/"+taskID+"/fail", map[string]string{
			"reason": reason,
		})
		var b strings.Builder
		b.WriteString(fmt.Sprintf("✗ Script failed (exit %d, %s)\n", exitCode, elapsed))
		if stderrStr != "" {
			b.WriteString(fmt.Sprintf("  stderr: %s\n", stderrStr))
		}
		b.WriteString(fmt.Sprintf("  Transcript: %s (local only, not committed on failure)\n", scriptLogPath))
		b.WriteString(fmt.Sprintf("  Task %s failed — downstream tasks blocked.\n", taskID))
		return mcp.NewToolResultText(b.String()), nil
	}

	// Exit 0 → submit the result.
	content := stdout.String()
	if content == "" {
		content = "(script produced no output)"
	}

	// Write result + metadata, commit, push.
	files := []mcpgit.FileWrite{
		{
			RepoRelPath: filepath.Join(resultDir, "result.md"),
			Content:     []byte(content),
		},
		// context.json was written to disk before exec;
		// include it in the commit for auditability.
		{
			RepoRelPath: filepath.Join(resultDir, "context.json"),
			Content:     contextBytes,
		},
		// script.log — full stdout+stderr transcript. Gets
		// committed alongside result.md on the success path
		// so a reader can reconstruct exactly what the script
		// printed (including verbose tool output, warnings
		// that didn't trip exit code, progress bars). Distinct
		// from result.md which carries only the
		// contract-defined stdout answer.
		{
			RepoRelPath: filepath.Join(resultDir, "script.log"),
			Content:     scriptLog.Bytes(),
		},
	}
	metadata := map[string]interface{}{
		"task_id":     taskID,
		"model":       c.modelName,
		"result_type": "text",
		"action":      "compute",
		"script":      meta.Script,
		"exit_code":   0,
		"elapsed_ms":  elapsed.Milliseconds(),
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	metaBytes, _ := json.MarshalIndent(metadata, "", "  ")
	files = append(files, mcpgit.FileWrite{
		RepoRelPath: filepath.Join(resultDir, "metadata.json"),
		Content:     metaBytes,
	})

	// Declared-artifact pickup. The script wrote its outputs
	// to the declared writes_artifacts paths (or it didn't,
	// and that's either a silent script bug or a truly
	// optional output). Read each declared path from the
	// workspace, include it in the commit, and report it to
	// the coordinator so the artifact index gets the
	// per-instance entry.
	//
	// Missing files are skipped with a soft warning accumulated
	// into the response. They're not a hard failure — the
	// script may legitimately skip writing conditionally —
	// but the absence is surfaced so the author knows nothing
	// registered.
	var artifactsWritten []string
	var missingArtifacts []string
	for _, rel := range meta.WritesArtifacts {
		if rel == "" {
			continue
		}
		full := filepath.Join(workDir, mcpgit.ArtifactPath(rel))
		body, rerr := os.ReadFile(full)
		if rerr != nil {
			missingArtifacts = append(missingArtifacts, rel)
			continue
		}
		files = append(files, mcpgit.FileWrite{
			RepoRelPath: mcpgit.ArtifactPath(rel),
			Content:     body,
		})
		artifactsWritten = append(artifactsWritten, rel)
	}

	proj.Lock()
	submitRes, err := proj.SubmitTaskResult(mcpgit.SubmitRequest{
		TaskID:        taskID,
		Username:      c.username,
		AuthorName:    c.citizenName,
		AuthorEmail:   c.citizenEmail,
		ModelName:     c.modelName,
		Files:         files,
		ArtifactPaths: artifactsWritten,
		Branch:        meta.Branch,
	})
	proj.Unlock()
	if err != nil {
		return mcp.NewToolResultError("git submit failed: " + err.Error()), nil
	}

	// Report to coordinator. When the task declared
	// writes_artifacts, include the list we actually picked
	// up so the coordinator can upsert the artifact index
	// (and validate against the declaration — unknown paths
	// in artifacts_written are rejected at the engine layer).
	reportBody := map[string]interface{}{
		"commit_sha":  submitRes.CommitSHA,
		"result_path": resultDir,
		"model":       c.modelName,
		"username":    c.username,
		"content":     content,
	}
	if len(artifactsWritten) > 0 {
		reportBody["artifacts_written"] = artifactsWritten
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
	b.WriteString(fmt.Sprintf("  Commit:  %s\n", shortSHA(submitRes.CommitSHA)))
	if len(artifactsWritten) > 0 {
		b.WriteString(fmt.Sprintf("  Artifacts: %s\n", strings.Join(artifactsWritten, ", ")))
	}
	if len(missingArtifacts) > 0 {
		// Warn loud — a declared path that the script didn't
		// produce is usually a silent bug the author wants to
		// know about.
		b.WriteString(fmt.Sprintf("  ⚠ Missing (declared but not written by script): %s\n", strings.Join(missingArtifacts, ", ")))
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
