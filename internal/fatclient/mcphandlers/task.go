package mcphandlers

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
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/service"
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
	return mcp.NewToolResultText(format.InvalidateResult(data, taskID)), nil
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
	return mcp.NewToolResultText(format.TallyResult(data, taskID)), nil
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
	return mcp.NewToolResultText(format.IterationList(taskID, data)), nil
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
	return mcp.NewToolResultText(format.ReadyTasks(data)), nil
}
func (c *apiClient) handleReleaseTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/release", map[string]string{
		"username": c.username(),
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
							if c.fc.Enjugit() != nil && resultPath != "" {
								remoteURL, _ := taskMap["project_remote_url"].(string)
								if wf, perr := c.fc.Enjugit().ForProject(int64(projectID), remoteURL); perr == nil {
									taskMap["_review_target_abs_path"] = filepath.Join(wf.WorkDir(), resultPath, "result.md")
									contentPath := filepath.Join(resultPath, "result.md")
									if content, rerr := wf.ReadFile(contentPath); rerr == nil {
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

	return mcp.NewToolResultText(format.TaskDetail(data, inputs, c.username())), nil
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
	outcome, err := c.fc.ExecuteComputeTask(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatExecuteOutcome(outcome)), nil
}

// handleRetryTask is a two-step composition: ask the coordinator
// to re-open the failed_retryable task (state + provenance), then
// re-run it client-side (re-materializing the fixed script first
// when from=head). The execute half reports through the same
// formatter as enju_execute_task so a retry reads identically to
// a first run, just prefixed with what was retried.
func (c *apiClient) handleRetryTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	from := req.GetString("from", "head")

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/retry", map[string]string{
		"from": from,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp struct {
		Error string `json:"error"`
		From  string `json:"from"`
	}
	json.Unmarshal(data, &resp)
	if resp.Error != "" {
		return mcp.NewToolResultError(resp.Error), nil
	}
	if resp.From != "" {
		from = resp.From // coordinator-normalized intent
	}

	outcome, err := c.fc.RetryComputeTask(ctx, taskID, from)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	header := fmt.Sprintf("↻ Retrying %s (from=%s — %s)\n\n", taskID, from, retryFromBlurb(from))
	return mcp.NewToolResultText(header + formatExecuteOutcome(outcome)), nil
}

// retryFromBlurb is the one-line reminder of what the from mode
// actually did, so the retry response is self-explaining.
func retryFromBlurb(from string) string {
	if from == "snapshot" {
		return "re-ran the pinned snapshot script unchanged"
	}
	return "re-materialized the script from the run branch tip"
}

// formatExecuteOutcome renders the free-text response returned
// to MCP callers. Split out so enju_execute_task and the batch
// tool can share the formatter for per-task entries.
func formatExecuteOutcome(out *service.ExecuteOutcome) string {
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
		b.WriteString(fmt.Sprintf("  Commit:  %s\n", format.ShortSHA(out.CommitSHA)))
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

