package mcphandlers

// Run-lifecycle handlers. enju_create_run instantiates a DAG
// (from inline YAML, a saved template path, or either + a
// params map); enju_list_runs / enju_run_status surface state;
// enju_export_run assembles every task result in DAG order
// into one markdown document.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/mcpgit"
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
	return mcp.NewToolResultText(format.RunList(data)), nil
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

	// Opportunistic reconcile: any async compute tasks that
	// committed + pushed since the last scan on this run's
	// branch get picked up here, so the status we're about to
	// render reflects the freshest coordinator state. Best-
	// effort — a fetch or reconcile failure just means stale
	// status this cycle, not a tool error.
	c.reconcileRunBranch(ctx, int64(projectID), run)

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
		return mcp.NewToolResultText(format.RunStatusMermaid(run, tasks)), nil
	default:
		return mcp.NewToolResultText(format.RunStatus(run, tasks, c.username)), nil
	}
}
// handlePauseRun moves a run into the `paused` state. Living-
// workflow phase 1: the state value is observable now; spawn-time
// gating (refusing claims/submits while paused) arrives with
// phase 4. Use to inspect a run mid-flight without auto-state
// transitions racing against you.
func (c *apiClient) handlePauseRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/pause", projectID, runID), map[string]string{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.PauseRunResult(data)), nil
}

// handleResumeRun lifts a paused run back to active or idle.
// Lands on idle when no ready work exists, active when ready
// tasks are present; the response carries the resolved state.
func (c *apiClient) handleResumeRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/resume", projectID, runID), map[string]string{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.ResumeRunResult(data)), nil
}

// handleShowEvents queries the project event log and returns
// JSONL — one event per line, newest-first. Filters: run_id,
// citizen, event_types (comma-separated), since (RFC3339),
// limit (default 100, max 1000). Living-workflow phase 2: this
// is the read-only projection over events. For
// git-tracked snapshots use enju_export_run_events instead.
func (c *apiClient) handleShowEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	q := url.Values{}
	if rs := req.GetInt("run_id", 0); rs != 0 {
		q.Set("run_seq", fmt.Sprintf("%d", rs))
	}
	if u := req.GetString("citizen", ""); u != "" {
		q.Set("citizen", u)
	}
	if et := req.GetString("event_types", ""); et != "" {
		q.Set("event_types", et)
	}
	if since := req.GetString("since", ""); since != "" {
		q.Set("since", since)
	}
	if limit := req.GetInt("limit", 0); limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}

	endpoint := fmt.Sprintf("/api/v1/projects/%d/events", projectID)
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	data, err := c.get(ctx, endpoint)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// The events endpoint normally returns a JSON array. When
	// the server hits an error path (e.g. run_seq names a run
	// that doesn't exist) it returns a single error object
	// like {"error": "run not found"} with a 4xx status. Try
	// to decode that shape first so the caller gets a usable
	// message instead of the raw "cannot unmarshal object
	// into []map" decoder error.
	if errMsg := errorFromResponse(data); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	var events []map[string]interface{}
	if err := json.Unmarshal(data, &events); err != nil {
		return mcp.NewToolResultError("decoding events: " + err.Error()), nil
	}
	return mcp.NewToolResultText(format.EventListJSONL(events)), nil
}

// handleRecentEvents is the assistant-side counterpart to
// handleShowEvents — same underlying endpoint, smaller default
// limit, human-readable output (one line per event). Designed
// for the LLM to call at natural pause points and surface
// "what's new" without flooding the conversation with raw JSONL.
//
// The "smaller default limit" is the only behavioral difference
// in v1; future work could add since_seq cursoring once
// RunEventRecord exposes Seq (Phase 4 territory). For now,
// timestamp-based filtering via `since` is the resume cursor.
func (c *apiClient) handleRecentEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	limit := req.GetInt("limit", 20)
	if limit > 100 {
		limit = 100
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if since := req.GetString("since", ""); since != "" {
		q.Set("since", since)
	}
	endpoint := fmt.Sprintf("/api/v1/projects/%d/events?%s", projectID, q.Encode())
	data, err := c.get(ctx, endpoint)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if errMsg := errorFromResponse(data); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	var events []map[string]interface{}
	if err := json.Unmarshal(data, &events); err != nil {
		return mcp.NewToolResultError("decoding events: " + err.Error()), nil
	}
	return mcp.NewToolResultText(format.EventListRecent(events)), nil
}

// handleRequestClarification is the bot-asks-human idiom — a
// thin wrapper over /spawn with sensible defaults locked in:
// action=answer, citizens=1, trigger=bot, single human assignee.
// Bots get a one-line "ask the human a question" tool instead
// of constructing a full spawn-task call themselves.
//
// See toolRequestClarification for the full intent + the
// "doesn't auto-pause caller's task in v1" caveat.
func (c *apiClient) handleRequestClarification(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	taskDefID, err := req.RequireString("task_def_id")
	if err != nil {
		return mcp.NewToolResultError("task_def_id is required"), nil
	}
	prompt, err := req.RequireString("prompt")
	if err != nil {
		return mcp.NewToolResultError("prompt is required"), nil
	}
	assignTo, err := req.RequireString("assign_to")
	if err != nil {
		return mcp.NewToolResultError("assign_to is required"), nil
	}

	// Validate assign_to is a member of the project. The
	// underlying spawn endpoint accepts any string and creates
	// the task; an unmembered or typo'd assignee produces an
	// unclaimable task that the bot waits on indefinitely. The
	// bot-asks-human idiom is exactly the case where this
	// "silent unclaimable" failure mode hurts most — bots often
	// don't know the project's membership, and the cure
	// (rename, retry) is cheap if surfaced.
	membersData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/members", projectID))
	if err != nil {
		return mcp.NewToolResultError("validating assign_to: " + err.Error()), nil
	}
	if errMsg := errorFromResponse(membersData); errMsg != "" {
		return mcp.NewToolResultError("validating assign_to: " + errMsg), nil
	}
	var members []map[string]interface{}
	if err := json.Unmarshal(membersData, &members); err == nil {
		matched := false
		usernames := make([]string, 0, len(members))
		for _, m := range members {
			u, _ := m["username"].(string)
			if u == "" {
				continue
			}
			usernames = append(usernames, u)
			if u == assignTo {
				matched = true
			}
		}
		if !matched && len(usernames) > 0 {
			return mcp.NewToolResultError(fmt.Sprintf(
				"@%s is not a member of project %d. Members: %s. Add a citizen with enju_add_project_member, or pick one from the list.",
				assignTo, projectID, strings.Join(usernames, ", "),
			)), nil
		}
		// Empty members → legacy "open project" mode (zero-member
		// gating bucket). Skip validation; spawn will accept
		// anyone, matching pre-Phase-3 behavior.
	}

	// Trigger reflects who asked — derived from the calling
	// citizen's kind (cached on apiClient via citizenKind, no
	// per-call round-trip). Bots calling this idiom emit
	// trigger=bot (the common case, audit-clear "bot needed
	// clarification"). Humans calling are valid too (a reviewer
	// pinging the author for context); we don't want their spawn
	// event mislabeled. Fall back to "human" on lookup failure:
	// that's spawn.go's own default for an empty trigger, and the
	// citizen field on the event still carries the actual identity.
	trigger := "human"
	if c.citizenKind(ctx) == "bot" {
		trigger = "bot"
	}

	body := map[string]interface{}{
		"task_def_id":    taskDefID,
		"action":         "answer",
		"prompt":         prompt,
		"assign_to":      []string{assignTo},
		"citizens":       1,
		"trigger":        trigger,
		"parent_task_id": req.GetString("parent_task_id", ""),
	}

	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/spawn", projectID, runID), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	taskID, _ := resp["task_id"].(string)
	out := fmt.Sprintf(
		"✓ Clarification requested from @%s — task %s\n"+
			"  Question: %s\n"+
			"  Watch for task_completed on this task to know when answered.",
		assignTo, taskID, prompt,
	)
	return mcp.NewToolResultText(out), nil
}

// handleSpawnTask creates a new task in an in-flight run.
// Living-workflow phase 4a — manual spawn primitive. The
// spawning citizen is the authenticated caller; trigger
// defaults to "human". Subject to the per-run cycle budget;
// budget exhaustion auto-pauses the run.
func (c *apiClient) handleSpawnTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	taskDefID, err := req.RequireString("task_def_id")
	if err != nil {
		return mcp.NewToolResultError("task_def_id is required"), nil
	}
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError("action is required"), nil
	}

	body := map[string]interface{}{
		"task_def_id":    taskDefID,
		"action":         action,
		"prompt":         req.GetString("prompt", ""),
		"user_prompt":    req.GetString("user_prompt", ""),
		"parent_task_id": req.GetString("parent_task_id", ""),
		"trigger":        req.GetString("trigger", "human"),
		"require_role":   req.GetString("require_role", ""),
		"result_type":    req.GetString("result_type", ""),
		"citizens":       req.GetInt("citizens", 1),
	}
	if dep := req.GetString("depends_on", ""); dep != "" {
		body["depends_on"] = strings.Split(dep, ",")
	}
	if as := req.GetString("assign_to", ""); as != "" {
		body["assign_to"] = strings.Split(as, ",")
	}

	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/spawn", projectID, runID), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	taskID, _ := resp["task_id"].(string)
	out := fmt.Sprintf("✓ Spawned %s", taskID)
	if budget, ok := resp["cycle_budget"].(map[string]interface{}); ok {
		used, _ := budget["used"].(float64)
		max, _ := budget["max"].(float64)
		out += fmt.Sprintf(" (cycle_budget: %d/%d)", int(used), int(max))
	}
	return mcp.NewToolResultText(out), nil
}

// handleSetCycleBudget bumps the per-run spawn cap. Use to
// extend room after a runaway has been triaged.
func (c *apiClient) handleSetCycleBudget(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	max, err := req.RequireInt("max")
	if err != nil {
		return mcp.NewToolResultError("max is required"), nil
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/cycle_budget", projectID, runID), map[string]int{"max": max})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Cycle budget set to %d for run %d:%d", max, projectID, runID)), nil
}

// runBranchFromData pulls the `branch` field out of a run JSON
// payload as returned by GET /runs/{seq} or POST /runs. Empty
// when the payload is malformed or missing — callers pass the
// empty string through to CommitFiles, which falls back to the
// project default. Central so every export-style tool threads
// the value identically.
func runBranchFromData(runData []byte) string {
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return ""
	}
	if b, ok := run["branch"].(string); ok {
		return b
	}
	return ""
}

// runSlugFromData extracts the run's filesystem slug (the
// tail of enju/runs/{seq}-{slug}/) from a coordinator
// run-detail payload. Empty means "fall back to the engine
// default" — callers pass the empty string to
// corelayout.RunDir, which treats it as "run".
func runSlugFromData(runData []byte) string {
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return ""
	}
	if s, ok := run["slug"].(string); ok {
		return s
	}
	return ""
}

func (c *apiClient) handleCreateRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required — create a project first with enju_create_project"), nil
	}

	// Phase H.1: three input shapes —
	//   1. yaml (inline definition, no params)
	//   2. path (template file under enju/templates/, optional params)
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
		return mcp.NewToolResultError("either 'yaml' (inline definition) or 'path' (template under enju/templates/) is required"), nil
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
		openedProj, _, _, _, err := c.openProject(ctx, int64(projectID))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		proj = openedProj
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
		// Template-as-recipe invariant: templates live on the
		// project's default branch. If the bundle files aren't
		// tracked there yet (e.g. user authored them in the
		// worktree and hasn't committed), auto-commit to
		// default before the run branches off. Without this,
		// the snapshot+branch-create flow below would sweep
		// untracked template files onto the run's branch only,
		// leaving the template unreachable on the default
		// branch — so subsequent runs on other branches would
		// see "template not found." See docs/runs-and-branches.md
		// § Templates.
		authorName, authorEmail := c.commitAuthor(ctx)
		proj.Lock()
		committedSHA, bundleErr := proj.EnsureBundleOnDefault(loadedTemplate.BundleDir, authorName, authorEmail, c.modelName)
		proj.Unlock()
		if bundleErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("pinning template to default branch: %v", bundleErr)), nil
		}
		if committedSHA != "" {
			sourceCommitSHA = committedSHA
		} else if head, herr := proj.HeadHash(); herr == nil {
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
	if branch := req.GetString("branch", ""); branch != "" {
		body["branch"] = branch
	}

	apiPath := fmt.Sprintf("/api/v1/projects/%d/runs", projectID)
	data, err := c.post(ctx, apiPath, body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Template mode: after the coordinator assigns a run seq,
	// commit a frozen copy of the bundle into
	// `enju/runs/{seq}/template-snapshot/` so the run owns its scripts,
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
				// The run's branch — pass to CommitFiles so the
				// template snapshot lands on THIS run's branch
				// (not whatever branch the worktree is currently
				// on). Missing this caused template-mode create_run
				// to commit snapshots to main regardless of the
				// run's branch= value, leaving the branch ref
				// uncreated and the run's first submit pushing to
				// main.
				runBranch, _ := created["branch"].(string)
				// Use the server-computed slug so the snapshot
				// target matches the run's result-dir prefix.
				// Falls back to client-side slug computation if
				// the coordinator response predates the slug
				// field (defense-in-depth for mid-rollout).
				runSlug, _ := created["slug"].(string)
				if runSlug == "" {
					runSlug = corelayout.ComputeRunSlug(templatePath, "")
				}
				snapshotTarget := corelayout.RunTemplateSnapshotDir(int(seq), runSlug)
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
						Branch:      runBranch,
					})
					proj.Unlock()
					if cerr != nil {
						snapshotWarning = fmt.Sprintf("snapshot commit failed: %v", cerr)
					}
				}
			}
		}
	}

	text := format.CreateRun(data)
	if snapshotWarning != "" {
		text += fmt.Sprintf("\n⚠ Template %s\n", snapshotWarning)
	}
	return mcp.NewToolResultText(text), nil
}
// handleExportDiagram snapshots the run's current DAG as raw
// Mermaid and commits it to enju/runs/{seq}/graph/{phase}.mmd.
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

	// Render the raw body for the file. The body is "" when
	// the run lookup failed (coordinator returned an error
	// object) — surface that to the caller rather than
	// committing an empty .mmd. The include_external flag that
	// used to render cross-run artifact edges was removed with
	// the branch-per-run model — branches isolate runs, so
	// there's no "external" edge to visualize.
	body := format.RenderMermaidBody(runData, tasksData)
	if body == "" {
		return mcp.NewToolResultError(fmt.Sprintf("could not render diagram for run %d:%d (run not found or no tasks yet)", projectID, runID)), nil
	}

	// Pull the run's branch out of the coordinator response so
	// CommitFiles lands the export on the right branch — not
	// the worktree's current HEAD (which could be any prior
	// run's branch).
	runBranch := runBranchFromData(runData)

	// Acquire a workspace for the project so we can commit.
	proj, _, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	repoPath := filepath.Join(corelayout.RunDir(int(runID), runSlugFromData(runData)), "graph", fmt.Sprintf("%s.mmd", phase))
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
		Branch:      runBranch,
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
		b.WriteString(fmt.Sprintf("✓ Diagram written to %s (commit %s)\n", repoPath, format.ShortSHA(res.CommitSHA)))
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

// handleExportRunEvents pulls the coordinator's synthesized
// event timeline for a run and commits it as JSONL under
// enju/runs/{seq}/events/{phase}.jsonl. Same snapshot-
// on-demand pattern as handleExportDiagram: authoritative
// data stays in the DB, git gets a frozen copy when the
// caller explicitly asks.
//
// Design notes:
//   - Lines are JSONL (one event per line, pretty-printed
//     to match `jq -c` style) so shell tooling and Python's
//     jsonl libraries consume it without extra parsing.
//   - Same-phase re-export overwrites the existing file;
//     CommitFiles treats byte-identical content as a no-op
//     so calling repeatedly doesn't churn history.
//   - Response inlines the first ~10 events for a quick
//     glance — the full file is on disk + committed.
func (c *apiClient) handleExportRunEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultError("enju_export_run_events requires a local workspace (MCP client mode)"), nil
	}

	// Fetch the run record first so the events commit lands on
	// the run's branch, not the worktree's current HEAD.
	runData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	runBranch := runBranchFromData(runData)

	// Pull events from the coordinator.
	eventsData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/events", projectID, runID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var events []map[string]interface{}
	if err := json.Unmarshal(eventsData, &events); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parsing events response: %v", err)), nil
	}

	// JSONL = one compact JSON object per line. json.Marshal
	// (not MarshalIndent) keeps each event on a single line,
	// which is the contract downstream consumers expect.
	var body bytes.Buffer
	for _, e := range events {
		line, merr := json.Marshal(e)
		if merr != nil {
			continue
		}
		body.Write(line)
		body.WriteByte('\n')
	}

	// Commit the snapshot into git. Matches the
	// handleExportDiagram pattern exactly — workspace lock,
	// CommitFiles, embed path in response.
	proj, _, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	repoPath := filepath.Join(corelayout.RunDir(int(runID), runSlugFromData(runData)), "events", fmt.Sprintf("%s.jsonl", phase))
	authorName, authorEmail := c.commitAuthor(ctx)
	commitMsg := fmt.Sprintf("Export run events: run %d:%d phase %s (%d events)", projectID, runID, phase, len(events))

	proj.Lock()
	res, err := proj.CommitFiles(mcpgit.CommitFilesRequest{
		Files: []mcpgit.FileWrite{{
			RepoRelPath: repoPath,
			Content:     body.Bytes(),
		}},
		CommitMsg:   commitMsg,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		ModelName:   c.modelName,
		Branch:      runBranch,
	})
	proj.Unlock()
	if err != nil {
		return mcp.NewToolResultError("writing events to clone: " + err.Error()), nil
	}

	var b strings.Builder
	if res.NoOp {
		b.WriteString(fmt.Sprintf("✓ Events unchanged (%d total) — skipped commit. File: %s\n", len(events), repoPath))
	} else {
		b.WriteString(fmt.Sprintf("✓ %d events written to %s (commit %s)\n", len(events), repoPath, format.ShortSHA(res.CommitSHA)))
	}
	// Inline preview — up to 10 lines so the LLM can show
	// the tail of the timeline without opening the file.
	preview := events
	if len(preview) > 10 {
		b.WriteString(fmt.Sprintf("  (showing first 10 of %d)\n", len(events)))
		preview = preview[:10]
	}
	b.WriteString("\n```jsonl\n")
	for _, e := range preview {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString("```\n")
	return mcp.NewToolResultText(b.String()), nil
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
