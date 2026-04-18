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

	"github.com/enju-ai/enju/internal/mcpgit"
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
		var artifactsData []byte
		if req.GetBool("include_external", false) {
			artifactsData, _ = c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID))
		}
		return mcp.NewToolResultText(formatRunStatusMermaidWith(run, tasks, artifactsData)), nil
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

	// Template-mode state kept through the flow so the
	// post-create snapshot commit has everything it needs.
	var (
		sourceCommitSHA string
		proj            *mcpgit.Project
		loadedTemplate  *mcpgit.LoadedTemplate
	)
	if templatePath != "" {
		// Template mode: pull the project's local clone so new
		// templates pushed by other citizens show up, then load
		// the bundle and capture the project HEAD for provenance.
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
		proj, err = c.workspace.ForProject(int64(projectID), remoteURL, projName)
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
		loadedTemplate, err = proj.LoadTemplate(templatePath)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		yamlContent = string(loadedTemplate.Raw)
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

	// Template mode: after the coordinator assigns a run seq,
	// commit a frozen copy of the bundle into
	// `.enju/runs/{seq}/template/` so the run owns its scripts,
	// data, and docs. A live template edit after this point
	// cannot retroactively change this run's behavior — the
	// executor resolves `script:` paths from the snapshot (see
	// handleExecuteTask).
	//
	// Errors here are non-fatal for the API response (the run
	// exists on the coordinator side) but surface as a warning
	// so the author knows the snapshot didn't land.
	var snapshotWarning string
	if loadedTemplate != nil && proj != nil {
		var created map[string]interface{}
		if err := json.Unmarshal(data, &created); err == nil {
			if seqF, ok := created["seq"].(float64); ok {
				seq := int(seqF)
				snapshotTarget := fmt.Sprintf(".enju/runs/%d/template", seq)
				files, ferr := proj.ReadBundleFiles(loadedTemplate.BundleDir, snapshotTarget)
				if ferr != nil {
					snapshotWarning = fmt.Sprintf("snapshot skipped: %v", ferr)
				} else if len(files) > 0 {
					authorName, authorEmail := c.commitAuthor(ctx)
					proj.Lock()
					_, cerr := proj.CommitFiles(mcpgit.CommitFilesRequest{
						Files:       files,
						CommitMsg:   fmt.Sprintf("Snapshot template %s into run %d", loadedTemplate.BundleDir, seq),
						AuthorName:  authorName,
						AuthorEmail: authorEmail,
						ModelName:   c.modelName,
					})
					proj.Unlock()
					if cerr != nil {
						snapshotWarning = fmt.Sprintf("snapshot commit failed: %v", cerr)
					}
				}
			}
		}
	}

	text := formatCreateRun(data)
	if snapshotWarning != "" {
		text += fmt.Sprintf("\n⚠ Template %s\n", snapshotWarning)
	}
	return mcp.NewToolResultText(text), nil
}
// handleExportDiagram snapshots the run's current DAG as raw
// Mermaid and commits it to .enju/runs/{seq}/graph/{phase}.mmd.
// See toolExportDiagram for the tool-facing contract; design
// notes:
//
//   - File is pure .mmd source (no markdown fences). Consumers
//     (GitHub, mermaid.live, `mmdc`, preprint minted blocks)
//     wrap it themselves.
//   - Same phase overwrites — "final.mmd" is always the current
//     final state. If the user invalidates and re-runs, the new
//     export replaces the previous final. History is in git.
//   - No-op optimization: if the would-be content is byte-
//     identical to what's on disk, we skip the write + commit
//     so repeated calls don't produce empty "export again"
//     commits.
//   - Response includes both the file path and the fenced
//     rendered Mermaid so the LLM can paste the image into
//     its reply while also citing the archival location.
func (c *apiClient) handleExportDiagram(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	phase := strings.TrimSpace(req.GetString("phase", ""))
	if err := validateDiagramPhase(phase); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("enju_export_diagram requires a local workspace (MCP client mode)"), nil
	}

	// Fetch run + tasks from coordinator — same inputs as
	// handleRunStatus so the diagram reflects the state the
	// coordinator has committed, not anything the client
	// might be holding locally.
	base := fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID)
	runData, err := c.get(ctx, base)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	tasksData, err := c.get(ctx, base+"/tasks")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Optional: cross-run artifact edges. When include_external
	// is true, fetch the project's artifact index and pass it
	// through — the renderer adds 📎 external nodes for reads
	// whose current writer lives in another run. Default off to
	// keep the diagram focused on intra-run dataflow; opt in
	// when the preprint or audit context wants the full data
	// dependency graph.
	var artifactsData []byte
	if req.GetBool("include_external", false) {
		artifactsData, _ = c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID))
	}

	// Render the raw body for the file. The body is "" when
	// the run lookup failed (coordinator returned an error
	// object) — surface that to the caller rather than
	// committing an empty .mmd.
	body := renderMermaidBody(runData, tasksData, artifactsData)
	if body == "" {
		return mcp.NewToolResultError(fmt.Sprintf("could not render diagram for run %d:%d (run not found or no tasks yet)", projectID, runID)), nil
	}

	// Acquire a workspace for the project so we can commit.
	remoteURL, projName, err := c.fetchProjectMetaFull(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj, err := c.workspace.ForProject(int64(projectID), remoteURL, projName)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoPath := fmt.Sprintf(".enju/runs/%d/graph/%s.mmd", runID, phase)
	authorName, authorEmail := c.commitAuthor(ctx)
	commitMsg := fmt.Sprintf("Export diagram: run %d:%d phase %s", projectID, runID, phase)

	proj.Lock()
	res, err := proj.CommitFiles(mcpgit.CommitFilesRequest{
		Files: []mcpgit.FileWrite{{
			RepoRelPath: repoPath,
			Content:     []byte(body),
		}},
		CommitMsg:   commitMsg,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   c.modelName,
	})
	proj.Unlock()
	if err != nil {
		return mcp.NewToolResultError("writing diagram to clone: " + err.Error()), nil
	}

	// Build the reply: path + embed hint + fenced inline render
	// so the LLM has everything it needs to both show the user
	// the diagram right now and cite where it lives.
	var b strings.Builder
	if res.NoOp {
		b.WriteString(fmt.Sprintf("✓ Diagram unchanged — skipped commit. File: %s\n", repoPath))
	} else {
		b.WriteString(fmt.Sprintf("✓ Diagram written to %s (commit %s)\n", repoPath, shortSHA(res.CommitSHA)))
	}
	b.WriteString(fmt.Sprintf("  Embed in markdown: ![](%s)\n\n", repoPath))
	b.WriteString("```mermaid\n")
	b.WriteString(body)
	b.WriteString("```\n")
	return mcp.NewToolResultText(b.String()), nil
}

// validateDiagramPhase enforces the safe-filename contract on
// the `phase` argument. We deliberately allow any other char so
// the LLM can pick descriptive labels ("post_vote_stack_choice",
// "after_reject_v2"), but we block path-traversal characters and
// the null byte, cap length, and reject empty.
func validateDiagramPhase(phase string) error {
	if phase == "" {
		return fmt.Errorf("phase is required (common values: 'initial', 'final', or a custom label)")
	}
	if len(phase) > 64 {
		return fmt.Errorf("phase is too long (%d chars) — max 64", len(phase))
	}
	if strings.Contains(phase, "/") || strings.Contains(phase, "\\") || strings.Contains(phase, "..") || strings.ContainsRune(phase, 0) {
		return fmt.Errorf("phase contains forbidden characters ('/', '\\\\', '..', or null byte)")
	}
	return nil
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
