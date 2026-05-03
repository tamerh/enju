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
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
// handleListIterations is the living-workflow phase 5 surface
// for the iteration history of a task. Returns one row per
// task_claims row, with the per-task seq computed and the
// claimant + commit_sha + review decision joined in. Renders
// as a plain text table; raw JSON form is one HTTP hop away
// for callers that need to parse.
func (c *apiClient) handleListIterations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	data, err := c.get(ctx, "/api/v1/tasks/"+taskID+"/iterations")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if errMsg := errorFromResponse(data); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	var iters []map[string]interface{}
	if err := json.Unmarshal(data, &iters); err != nil {
		return mcp.NewToolResultError("decoding iterations: " + err.Error()), nil
	}
	if len(iters) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("(no iterations for %s — task hasn't been claimed yet)", taskID)), nil
	}
	out := fmt.Sprintf("Iteration history for %s:\n\n", taskID)
	for _, it := range iters {
		seq, _ := it["seq"].(float64)
		citizen, _ := it["citizen"].(string)
		outcome, _ := it["outcome"].(string)
		out += fmt.Sprintf("  iter-%d  @%s  [%s]\n", int(seq), citizen, outcome)
		if claimed, ok := it["claimed_at"].(string); ok {
			out += "    claimed_at:  " + claimed + "\n"
		}
		if submitted, ok := it["submitted_at"].(string); ok {
			out += "    submitted_at: " + submitted + "\n"
		}
		if commit, ok := it["commit_sha"].(string); ok && commit != "" {
			short := commit
			if len(short) > 8 {
				short = short[:8]
			}
			out += "    commit:       " + short + "\n"
		}
		if branch, ok := it["branch"].(string); ok && branch != "" {
			out += "    branch:       " + branch + "\n"
		}
		if dec, ok := it["review_decision"].(string); ok && dec != "" {
			out += "    review:       " + dec + "\n"
		}
		if opt, ok := it["option"].(string); ok && opt != "" {
			out += "    option:       " + opt + "\n"
		}
		if model, ok := it["model"].(string); ok && model != "" {
			out += "    model:        " + model + "\n"
		}
		if content, ok := it["content"].(string); ok && content != "" {
			snippet := content
			if len(snippet) > 80 {
				snippet = snippet[:77] + "..."
			}
			out += "    content:      " + snippet + "\n"
		}
	}
	return mcp.NewToolResultText(out), nil
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
// project's enju/templates/ directory in the local clone and
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
	outcome, err := c.executeComputeTask(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatExecuteOutcome(outcome)), nil
}

// formatExecuteOutcome renders the free-text response returned
// to MCP callers. Split out so enju_execute_task and the batch
// tool can share the formatter for per-task entries.
func formatExecuteOutcome(out *executeOutcome) string {
	var b strings.Builder
	elapsed := time.Duration(out.ElapsedMS) * time.Millisecond
	switch out.Status {
	case "async_started":
		return formatAsyncKickoff(out.TaskID, out.Script, out.Async)
	case "failed":
		b.WriteString(fmt.Sprintf("✗ Script failed (exit %d, %s)\n", out.ExitCode, elapsed))
		if out.Stderr != "" {
			b.WriteString(fmt.Sprintf("  stderr: %s\n", out.Stderr))
		}
		if out.ScriptLogPath != "" {
			b.WriteString(fmt.Sprintf("  Transcript: %s (local only, not committed on failure)\n", out.ScriptLogPath))
		}
		b.WriteString(fmt.Sprintf("  Task %s failed — downstream tasks blocked.\n", out.TaskID))
	case "git_failed":
		// Distinct from "failed" so the user knows the script
		// itself ran fine — the failure is at the git layer
		// (commit/push). Work product is still on disk; recovery
		// is fix-the-git-state, not re-run-the-script.
		b.WriteString(fmt.Sprintf("✗ Git operation failed after script ran (%s)\n", elapsed))
		b.WriteString(fmt.Sprintf("  Script:  %s — completed successfully\n", out.Script))
		b.WriteString(fmt.Sprintf("  Output:  %d bytes on disk in run dir (NOT committed)\n", out.ContentLen))
		if out.ErrorMessage != "" {
			b.WriteString(fmt.Sprintf("  Git error: %s\n", out.ErrorMessage))
		}
		if out.ScriptLogPath != "" {
			b.WriteString(fmt.Sprintf("  Transcript: %s\n", out.ScriptLogPath))
		}
		// Recovery note: don't suggest enju_invalidate_task —
		// the task is still in `claimed` state, and invalidate
		// only operates on accepted tasks. The execute path's
		// claim gate handles "claimed by us" as a retry, so a
		// plain re-call after fixing the remote is enough.
		b.WriteString(fmt.Sprintf("  Task %s — fix the remote/branch state (enju_project_remote_status), then call enju_execute_task again to retry.\n", out.TaskID))
	case "completed":
		b.WriteString(fmt.Sprintf("✓ Script completed (exit 0, %s)\n", elapsed))
		b.WriteString(fmt.Sprintf("  Script:  %s\n", out.Script))
		b.WriteString(fmt.Sprintf("  Output:  %d bytes written to result.md\n", out.ContentLen))
		b.WriteString(fmt.Sprintf("  Commit:  %s\n", shortSHA(out.CommitSHA)))
		if len(out.ArtifactsWritten) > 0 {
			b.WriteString(fmt.Sprintf("  Artifacts: %s\n", strings.Join(out.ArtifactsWritten, ", ")))
		}
		if len(out.MissingArtifacts) > 0 {
			b.WriteString(fmt.Sprintf("  ⚠ Missing (declared but not written by script): %s\n", strings.Join(out.MissingArtifacts, ", ")))
		}
		if out.ContribNum > 0 {
			b.WriteString(fmt.Sprintf("\nContribution #%d\n", out.ContribNum))
		}
		if out.NewlyReady > 0 {
			b.WriteString(fmt.Sprintf("Impact: %d new task(s) unlocked.\n", out.NewlyReady))
		}
	}
	return b.String()
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
