package mcpserver

// Run-lifecycle handlers. enju_create_run instantiates a DAG
// (from inline YAML, a saved template path, or either + a
// params map); enju_list_runs / enju_run_status surface state;
// enju_export_run assembles every task result in DAG order
// into one markdown document.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListRuns(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var data []byte
	var err error
	if pid := req.GetInt("project_id", 0); pid != 0 {
		data, err = c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs", pid))
	} else {
		data, err = c.get(ctx, "/api/v1/runs")
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatRunList(data)), nil
}
func (c *apiClient) handleRunStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}

	base := fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID)
	run, err := c.get(ctx, base)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	tasks, err := c.get(ctx, base+"/tasks")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Inject project name into the run data for the header.
	_, projName, _ := c.fetchProjectMetaFull(ctx, int64(projectID))
	if projName != "" {
		var runMap map[string]interface{}
		if json.Unmarshal(run, &runMap) == nil {
			runMap["_project_name"] = projName
			run, _ = json.Marshal(runMap)
		}
	}

	// Format switch: default = textual summary + tree; mermaid =
	// flowchart TD syntax for pasting into mermaid.live / README
	// / preprint. Invalid values fall back to default silently so
	// older clients that don't know the param never error.
	switch req.GetString("format", "default") {
	case "mermaid":
		return mcp.NewToolResultText(formatRunStatusMermaid(run, tasks)), nil
	default:
		return mcp.NewToolResultText(formatRunStatus(run, tasks, c.username)), nil
	}
}
func (c *apiClient) handleCreateRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required — create a project first with enju_create_project"), nil
	}

	// Phase H.1: three input shapes —
	//   1. yaml (inline definition, no params)
	//   2. path (template file under enju_templates/, optional params)
	//   3. yaml + params (inline definition with a declared params: block)
	//
	// Exactly one of (yaml, path) must be set. Params are optional in
	// all cases; if set, the coordinator calls ParseWithParams and
	// substitutes before validating.
	yamlContent := req.GetString("yaml", "")
	templatePath := req.GetString("path", "")
	params := req.GetArguments()["params"]
	var paramMap map[string]interface{}
	if params != nil {
		if m, ok := params.(map[string]interface{}); ok {
			paramMap = m
		} else {
			return mcp.NewToolResultError("params must be an object mapping parameter names to values"), nil
		}
	}

	if yamlContent == "" && templatePath == "" {
		return mcp.NewToolResultError("either 'yaml' (inline definition) or 'path' (template under enju_templates/) is required"), nil
	}
	if yamlContent != "" && templatePath != "" {
		return mcp.NewToolResultError("'yaml' and 'path' are mutually exclusive — pass one or the other"), nil
	}

	var sourceCommitSHA string
	if templatePath != "" {
		// Template mode: pull the project's local clone so new
		// templates pushed by other citizens show up, then read
		// the file and capture the project HEAD for provenance.
		// Substitution + validation happen server-side in the
		// coordinator's parser (consistent with the existing
		// inline-YAML path).
		if c.workspace == nil {
			return mcp.NewToolResultError("enju_create_run with 'path' requires a local workspace (MCP client mode)"), nil
		}
		remoteURL, projName, err := c.fetchProjectMetaFull(ctx, int64(projectID))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		proj, err := c.workspace.ForProject(int64(projectID), remoteURL, projName)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// Best-effort pull. If the remote is unreachable or has
		// diverged, fall through and scan whatever's on disk —
		// the loader will surface a clear "template not found"
		// if the file truly isn't there yet.
		proj.Lock()
		_ = proj.Pull()
		proj.Unlock()
		loaded, err := proj.LoadTemplate(templatePath)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		yamlContent = string(loaded.Raw)
		if head, herr := proj.HeadHash(); herr == nil {
			sourceCommitSHA = head
		}
	}

	body := map[string]interface{}{
		"yaml":     yamlContent,
		"username": c.username,
	}
	if paramMap != nil {
		body["params"] = paramMap
	}
	if templatePath != "" {
		body["source_path"] = templatePath
		if sourceCommitSHA != "" {
			body["source_commit_sha"] = sourceCommitSHA
		}
	}

	apiPath := fmt.Sprintf("/api/v1/projects/%d/runs", projectID)
	data, err := c.post(ctx, apiPath, body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(formatCreateRun(data)), nil
}
func (c *apiClient) handleExportRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runSeq, err := req.RequireInt("run_seq")
	if err != nil {
		return mcp.NewToolResultError("run_seq is required"), nil
	}

	// Fetch run + tasks from coordinator.
	runData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runSeq))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var run map[string]interface{}
	json.Unmarshal(runData, &run)
	if errMsg, _ := run["error"].(string); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}

	tasksData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", projectID, runSeq))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var tasks []map[string]interface{}
	json.Unmarshal(tasksData, &tasks)

	// Read each accepted task's result from the local clone.
	var remoteURL, projName string
	if c.workspace != nil {
		if u, n, err := c.fetchProjectMetaFull(ctx, int64(projectID)); err == nil {
			remoteURL = u
			projName = n
		}
	}

	var b strings.Builder
	runName, _ := run["name"].(string)
	runState, _ := run["state"].(string)
	b.WriteString(fmt.Sprintf("# Run: %s\n\n", runName))
	b.WriteString(fmt.Sprintf("Project: #%d, Run: #%d, State: %s, Tasks: %d\n\n", projectID, runSeq, runState, len(tasks)))
	b.WriteString("---\n\n")

	for _, t := range tasks {
		tid, _ := t["id"].(string)
		tstate, _ := t["state"].(string)
		action, _ := t["action"].(string)
		prompt, _ := t["prompt"].(string)
		commitSHA, _ := t["commit_sha"].(string)
		resultPath, _ := t["result_path"].(string)
		claimedBy, _ := t["claimed_by"].(string)
		defID, _ := t["task_def_id"].(string)

		b.WriteString(fmt.Sprintf("## %s\n\n", tid))
		b.WriteString(fmt.Sprintf("Action: %s | State: %s", action, tstate))
		if claimedBy != "" {
			b.WriteString(fmt.Sprintf(" | By: @%s", claimedBy))
		}
		b.WriteString("\n\n")

		// Read result from git first — for the preprint,
		// the output is what matters. Show the prompt only
		// as context below the result.
		resultShown := false
		if tstate == "accepted" && commitSHA != "" && c.workspace != nil && remoteURL != "" {
			if proj, err := c.workspace.ForProject(int64(projectID), remoteURL, projName); err == nil {
				resultFile := resultPath + "/result.md"
				if defID != "" && resultPath != "" {
					content, found, err := proj.ReadFileAtCommit(commitSHA, resultFile)
					if err == nil && found && len(content) > 0 {
						b.WriteString(string(content) + "\n\n")
						resultShown = true
					}
				}
				_ = defID
			}
		}
		if tstate == "skipped" {
			b.WriteString("*(skipped — losing branch of a vote)*\n\n")
		}
		if !resultShown && prompt != "" {
			// No result available — show the prompt template
			// so the reader at least knows what was asked.
			b.WriteString("**Prompt:** " + prompt + "\n\n")
		}
		b.WriteString("---\n\n")
	}

	return mcp.NewToolResultText(b.String()), nil
}
