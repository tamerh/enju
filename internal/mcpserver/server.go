// Package mcpserver implements the Enju MCP server for Claude Desktop/Code integration.
// It's a thin bridge: MCP tool calls → coordinator REST API calls.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/enju-ai/enju/internal/mcpgit"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Config holds the MCP server configuration.
type Config struct {
	CoordinatorURL string
	Username       string // citizen's username (stable handle)
	CitizenName    string // display name, for greetings
	// Workspace is the per-client git workspace used by the
	// iteration A.2 fat-client path. When non-nil and a project
	// has a remote_url, the MCP client writes task results to a
	// local clone here and reports commit SHAs back to the
	// coordinator, bypassing the legacy content-over-wire path.
	// When nil, only the legacy path is used.
	Workspace *mcpgit.Workspace
	// Logger is used for client-side diagnostic output. If nil,
	// a slog.Default() is used.
	Logger *slog.Logger
}

// New creates and configures the MCP server with all Enju tools.
func New(cfg Config) *server.MCPServer {
	s := server.NewMCPServer(
		"enju",
		"0.1.0",
		server.WithToolCapabilities(true),
	)

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	client := &apiClient{
		baseURL:    cfg.CoordinatorURL,
		username:   cfg.Username,
		workspace:  cfg.Workspace,
		logger:     logger,
		httpClient: &http.Client{},
	}

	// Register tools
	s.AddTool(toolListRuns(), client.handleListRuns)
	s.AddTool(toolListReadyTasks(), client.handleListReadyTasks)
	s.AddTool(toolClaimTask(), client.handleClaimTask)
	s.AddTool(toolGetTaskInputs(), client.handleGetTaskInputs)
	s.AddTool(toolSubmitResult(), client.handleSubmitResult)
	s.AddTool(toolReleaseTask(), client.handleReleaseTask)
	s.AddTool(toolGetTask(), client.handleGetTask)
	s.AddTool(toolRunStatus(), client.handleRunStatus)
	s.AddTool(toolCreateRun(), client.handleCreateRun)
	s.AddTool(toolMyDashboard(), client.handleMyDashboard)
	s.AddTool(toolUpdateProfile(), client.handleUpdateProfile)
	s.AddTool(toolListProjects(), client.handleListProjects)
	s.AddTool(toolCreateProject(), client.handleCreateProject)
	s.AddTool(toolSetProjectRemote(), client.handleSetProjectRemote)
	s.AddTool(toolProjectRemoteStatus(), client.handleProjectRemoteStatus)
	s.AddTool(toolProjectSync(), client.handleProjectSync)
	s.AddTool(toolListArtifacts(), client.handleListArtifacts)
	s.AddTool(toolGetArtifact(), client.handleGetArtifact)
	s.AddTool(toolGetArtifactHistory(), client.handleGetArtifactHistory)
	s.AddTool(toolMyProfile(), client.handleMyProfile)
	s.AddTool(toolInvalidateTask(), client.handleInvalidateTask)

	return s
}

// --- API Client ---

type apiClient struct {
	baseURL    string
	username   string // caller's citizen username
	workspace  *mcpgit.Workspace
	logger     *slog.Logger
	httpClient *http.Client

	// Cached citizen profile (name + email) used to populate git
	// commit author fields on the fat-client submit path. Fetched
	// lazily on first use and held for the life of the MCP client
	// process. Reasoning: citizen profile changes via
	// enju_update_profile are rare within a single session, and
	// paying one GET per submit just to avoid staleness is
	// wasteful. If a citizen does update their profile mid-session
	// the next process restart will pick up the new values.
	profileOnce  sync.Once
	profileName  string
	profileEmail string
}

func (c *apiClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *apiClient) put(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *apiClient) post(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// --- Tool Definitions ---

func toolListRuns() mcp.Tool {
	return mcp.NewTool("enju_list_runs",
		mcp.WithDescription("List runs. Optionally filter by project."),
		mcp.WithNumber("project_id",
			mcp.Description("Filter by project ID (integer, optional)"),
		),
	)
}

func toolListReadyTasks() mcp.Tool {
	return mcp.NewTool("enju_list_ready_tasks",
		mcp.WithDescription("List tasks that are ready to be claimed. Optionally filter by project and run."),
		mcp.WithNumber("project_id",
			mcp.Description("Filter by project ID (optional)"),
		),
		mcp.WithNumber("run_id",
			mcp.Description("Filter by run ID within project (optional, requires project_id)"),
		),
	)
}

func toolClaimTask() mcp.Tool {
	return mcp.NewTool("enju_claim_task",
		mcp.WithDescription("Claim a task to work on. Returns the task prompt and any upstream results needed."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to claim"),
		),
	)
}

func toolGetTaskInputs() mcp.Tool {
	return mcp.NewTool("enju_get_task_inputs",
		mcp.WithDescription("Get the upstream dependency results for a task. Use this to see what previous tasks produced."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func toolSubmitResult() mcp.Tool {
	return mcp.NewTool("enju_submit_result",
		mcp.WithDescription(`Submit a result for a claimed task. The task must be claimed by you first.

For simple tasks: provide 'content' as a string.
For tasks with named outputs: provide 'outputs_json' as a JSON object mapping output names to their values.
For tasks with writes_artifacts: provide 'artifacts_json' mapping each declared artifact path to its new content. You may write any subset of declared paths (permissive — declared is an upper bound).
The task detail shows the schema (outputs and writes_artifacts) so you know what's expected.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
		mcp.WithString("content",
			mcp.Description("The result content as plain text (for simple tasks)"),
		),
		mcp.WithString("outputs_json",
			mcp.Description(`For tasks with named outputs: a JSON string of the outputs object. Example: '{"gene_list": "BRCA1, TP53", "pathways": "KEGG:hsa04110"}'`),
		),
		mcp.WithString("artifacts_json",
			mcp.Description(`For tasks with writes_artifacts: a JSON string mapping each artifact path to its new content. Example: '{"src/analyze.py": "def analyze():\n    pass\n"}'. Paths must be in the task's writes_artifacts list.`),
		),
	)
}

func toolListArtifacts() mcp.Tool {
	return mcp.NewTool("enju_list_artifacts",
		mcp.WithDescription("List artifacts in a project's repository. Artifacts are mutable project-scoped files (source code, datasets, templates, docs) shared across all runs in the project."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to list artifacts from"),
		),
		mcp.WithString("prefix",
			mcp.Description("Optional path prefix filter (e.g., 'src/' or 'data/')"),
		),
	)
}

func toolGetArtifact() mcp.Tool {
	return mcp.NewTool("enju_get_artifact",
		mcp.WithDescription("Read the current content of an artifact in a project's repository, plus its provenance (who last wrote it, in which task and run)."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project the artifact belongs to"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("The artifact path relative to the artifacts/ directory (e.g., 'src/analyze.py')"),
		),
	)
}

func toolGetArtifactHistory() mcp.Tool {
	return mcp.NewTool("enju_get_artifact_history",
		mcp.WithDescription("List the chronological write history of an artifact in a project's repository. Returns each commit that touched the artifact, newest first, with the task that produced it when applicable."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project the artifact belongs to"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("The artifact path relative to the artifacts/ directory"),
		),
	)
}

func toolReleaseTask() mcp.Tool {
	return mcp.NewTool("enju_release_task",
		mcp.WithDescription("Release a claimed task back to the pool if you can't complete it. No penalty for voluntary release."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to release"),
		),
	)
}

func toolGetTask() mcp.Tool {
	return mcp.NewTool("enju_get_task",
		mcp.WithDescription("Get details of a specific task including its state, prompt, and dependencies."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func toolRunStatus() mcp.Tool {
	return mcp.NewTool("enju_run_status",
		mcp.WithDescription("Get the status of a run including all its tasks. Run is addressed by project_id + run_id (per-project sequence)."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project (#1, #2, #3)"),
		),
	)
}

func toolCreateRun() mcp.Tool {
	return mcp.NewTool("enju_create_run",
		mcp.WithDescription(`Create a new Enju run by submitting a YAML definition. The run must belong to an existing project.

The YAML format:
  name: "Run name"
  version: 1
  ref: "https://github.com/..." (optional)
  for_each:
    variable: [value1, value2] (optional, for parallel expansion)
  tasks:
    - id: task_name
      action: answer
      prompt: "The prompt. Use {{other_task.content}} to reference upstream results."

Dependencies are inferred automatically from {{task_id.content}} references.
Tasks without references to other tasks run in parallel.

If you don't have a project yet, create one first with enju_create_project.`),
		mcp.WithString("yaml",
			mcp.Required(),
			mcp.Description("The run definition in YAML format"),
		),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID to create this run in (use enju_list_projects to see existing projects)"),
		),
	)
}

func toolListProjects() mcp.Tool {
	return mcp.NewTool("enju_list_projects",
		mcp.WithDescription("List all long-lived projects. A project is a workspace that holds many runs over time."),
	)
}

func toolCreateProject() mcp.Tool {
	return mcp.NewTool("enju_create_project",
		mcp.WithDescription("Create a new long-lived project (workspace). Projects hold runs and artifacts over time."),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Unique project name"),
		),
		mcp.WithString("description",
			mcp.Description("Optional project description"),
		),
		mcp.WithString("remote_url",
			mcp.Description("Optional external git remote URL (e.g., git@github.com:org/repo.git). When set, the coordinator pushes every task result commit to this remote. Auth follows the host's SSH/credential configuration."),
		),
	)
}

func toolSetProjectRemote() mcp.Tool {
	return mcp.NewTool("enju_set_project_remote",
		mcp.WithDescription("Set or clear the external git remote URL for a project. Subsequent task result commits will be pushed to this remote. Pass an empty string to clear the remote."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose remote to update"),
		),
		mcp.WithString("remote_url",
			mcp.Required(),
			mcp.Description("Git remote URL, or empty string to clear"),
		),
	)
}

func toolProjectRemoteStatus() mcp.Tool {
	return mcp.NewTool("enju_project_remote_status",
		mcp.WithDescription("Show live git remote status for a project: local HEAD vs remote HEAD (via ls-remote), last push timestamp, and last push error if any. Use this when enju_list_projects shows a remote warning."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to inspect"),
		),
	)
}

func toolProjectSync() mcp.Tool {
	return mcp.NewTool("enju_project_sync",
		mcp.WithDescription("Push a project's local HEAD to its configured remote without requiring a new commit. Safe by default: a fast-forward push succeeds, a diverged remote is REFUSED unless force=true. Use this to sweep stuck commits (e.g. after a push failure or an earlier invalidation that didn't push). Set force=true ONLY when you intentionally want to overwrite the remote — force-push is destructive and can discard remote-side contributions."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to push"),
		),
		mcp.WithBoolean("force",
			mcp.Description("If true, do a force-push that overwrites the remote branch even when histories have diverged. Default false — diverged remotes are refused with guidance to reconcile manually."),
		),
	)
}

func toolUpdateProfile() mcp.Tool {
	return mcp.NewTool("enju_update_profile",
		mcp.WithDescription("Update your citizen profile — name and email."),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Your display name"),
		),
		mcp.WithString("email",
			mcp.Description("Your email (optional, must be unique)"),
		),
	)
}

func toolMyDashboard() mcp.Tool {
	return mcp.NewTool("enju_my_dashboard",
		mcp.WithDescription("Show your citizen dashboard: stats, active tasks, and recent completions."),
	)
}

func toolMyProfile() mcp.Tool {
	return mcp.NewTool("enju_my_profile",
		mcp.WithDescription("Show your own citizen profile — username (the stable handle used in assign_to and everywhere user-facing), display name, email, and role. Use this to confirm your handle before asking someone to put you in assign_to."),
	)
}

func toolInvalidateTask() mcp.Tool {
	return mcp.NewTool("enju_invalidate_task",
		mcp.WithDescription(`Mark an accepted task as invalid because its result turned out to be wrong. Cascades to all downstream dependents — they transition back to PENDING and wait for the target to re-complete. The target itself goes back to READY so any citizen can re-claim and re-run it.

Git history preserves the previous result; the new one overwrites it when submitted.

Only tasks in the 'accepted' state can be invalidated. Use this when you notice a task produced a bad result after the fact (hallucination, wrong data, missing piece).`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The fully-qualified ID of the task to invalidate"),
		),
		mcp.WithString("reason",
			mcp.Description("Short explanation for the invalidation — shown in logs and the response"),
		),
	)
}

// --- Tool Handlers ---

func (c *apiClient) handleUpdateProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	email := req.GetString("email", "")

	data, err := c.put(ctx, "/api/v1/citizens/by-username/"+c.username+"/profile", map[string]string{
		"name":  name,
		"email": email,
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

	// Update local credentials file
	updateLocalCredentials(name)

	return mcp.NewToolResultText(fmt.Sprintf("✓ Profile updated: %s", name)), nil
}

func (c *apiClient) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/projects")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Decorate the coordinator's project list with local push-status
	// info from the MCP workspace — but only for projects whose clone
	// already exists on disk. This keeps list_projects cheap (no fresh
	// clones triggered as a side effect of a listing call) while
	// restoring the at-a-glance `✓ last push: ...` indicator that was
	// server-side in Phase 1 / iteration 4.
	decorated := c.decorateProjectListWithPushStatus(data)
	return mcp.NewToolResultText(formatProjectList(decorated)), nil
}

// decorateProjectListWithPushStatus reads the coordinator's JSON
// project list and injects per-project `last_push_at` fields
// pulled from the MCP workspace's local clones. Clones that don't
// exist on disk get no decoration (the `remote: ...` line simply
// omits the check-mark suffix). If decoration fails for any
// reason, the original bytes are returned unchanged so the
// formatter still renders the list.
func (c *apiClient) decorateProjectListWithPushStatus(data []byte) []byte {
	if c.workspace == nil {
		return data
	}
	var projects []map[string]interface{}
	if err := json.Unmarshal(data, &projects); err != nil {
		return data
	}
	changed := false
	for _, p := range projects {
		remoteURL, _ := p["remote_url"].(string)
		if remoteURL == "" {
			continue
		}
		var projectID int64
		switch v := p["id"].(type) {
		case float64:
			projectID = int64(v)
		}
		if projectID == 0 {
			continue
		}
		if !c.workspace.HasLocalClone(projectID) {
			continue
		}
		proj, err := c.workspace.ForProject(projectID, remoteURL)
		if err != nil {
			continue
		}
		if t := proj.LastPushAt(); !t.IsZero() {
			p["last_push_at"] = t.Format(time.RFC3339)
			changed = true
		}
		if e := proj.LastPushError(); e != "" {
			p["last_push_error"] = e
			changed = true
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(projects)
	if err != nil {
		return data
	}
	return out
}

func (c *apiClient) handleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	description := req.GetString("description", "")
	remoteURL := req.GetString("remote_url", "")

	data, err := c.post(ctx, "/api/v1/projects", map[string]string{
		"name":        name,
		"description": description,
		"remote_url":  remoteURL,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatCreateProjectResult(data)), nil
}

// commitAuthor returns the `name email` pair to use as git commit
// author for submits made on this citizen's behalf. Fetches the
// citizen profile from the coordinator once and caches it for the
// life of the MCP client process. Falls back to the configured
// display name (from `enju mcp -name`) when no profile is
// available, and to a synthetic `{username}@enju.local` address
// when the citizen hasn't set a real email.
//
// Real email addresses attribute commits to the right GitHub user
// when they match the citizen's GitHub email; synthetic ones at
// least make different citizens' commits distinguishable in
// contributor graphs instead of collapsing to one bot identity.
func (c *apiClient) commitAuthor(ctx context.Context) (name, email string) {
	c.profileOnce.Do(func() {
		// Default values — used if the fetch fails.
		c.profileName = c.username
		c.profileEmail = c.username + "@enju.local"

		data, err := c.get(ctx, "/api/v1/citizens/by-username/"+c.username)
		if err != nil {
			c.logger.Warn("commitAuthor: failed to fetch profile, using defaults",
				"username", c.username, "error", err)
			return
		}
		var p map[string]interface{}
		if err := json.Unmarshal(data, &p); err != nil {
			return
		}
		if n, ok := p["name"].(string); ok && n != "" {
			c.profileName = n
		}
		if e, ok := p["email"].(string); ok && e != "" {
			c.profileEmail = e
		}
	})
	return c.profileName, c.profileEmail
}

// fetchProjectMeta reads a project's metadata from the coordinator.
// Used by the client-side project_remote_status / project_sync /
// get_artifact / get_artifact_history / set_project_remote handlers
// that need the project's remote_url to open the local clone.
func (c *apiClient) fetchProjectMeta(ctx context.Context, projectID int64) (remoteURL string, err error) {
	data, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d", projectID))
	if err != nil {
		return "", err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parsing project: %w", err)
	}
	if errMsg, ok := raw["error"].(string); ok {
		return "", fmt.Errorf("%s", errMsg)
	}
	if v, ok := raw["remote_url"].(string); ok {
		remoteURL = v
	}
	return remoteURL, nil
}

// handleProjectRemoteStatus runs the remote-status diagnostic
// entirely on the client side. Phase 1 ran this in the coordinator
// against a server-owned clone; iteration A moves the clone to the
// client, so this tool now opens the MCP workspace's local clone
// and calls mcpgit.Project.CompareToRemote. The output shape is
// unchanged from the Phase 1 tool so formatters keep working.
func (c *apiClient) handleProjectRemoteStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("remote status is only available in MCP client mode"), nil
	}
	remoteURL, err := c.fetchProjectMeta(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
	}
	if remoteURL == "" {
		resp["status"] = string(mcpgit.RemoteNoRemote)
		data, _ := json.Marshal(resp)
		return mcp.NewToolResultText(formatProjectRemoteStatus(data)), nil
	}

	proj, err := c.workspace.ForProject(int64(projectID), remoteURL)
	if err != nil {
		return mcp.NewToolResultError("opening local clone: " + err.Error()), nil
	}
	cmp, err := proj.CompareToRemote()
	if err != nil {
		return mcp.NewToolResultError("comparing to remote: " + err.Error()), nil
	}

	resp["status"] = string(cmp.Status)
	resp["local_head"] = cmp.LocalHead
	resp["remote_head"] = cmp.RemoteHead
	resp["ahead_by"] = cmp.AheadBy
	resp["behind_by"] = cmp.BehindBy
	if cmp.Unreachable != "" {
		resp["remote_error"] = cmp.Unreachable
	}
	// A.5 polish: surface the in-memory push-status bookkeeping
	// so the formatter can render "last push: <time>" / "last
	// push failed: <error>" the same way iteration 4 did.
	if t := proj.LastPushAt(); !t.IsZero() {
		resp["last_push_at"] = t.Format(time.RFC3339)
	}
	if e := proj.LastPushError(); e != "" {
		resp["last_push_error"] = e
	}
	data, _ := json.Marshal(resp)
	return mcp.NewToolResultText(formatProjectRemoteStatus(data)), nil
}

// handleProjectSync force-syncs the client's local clone to its
// remote. Runs entirely client-side: open the clone, preflight via
// CompareToRemote, refuse diverged state unless force=true, push.
// The coordinator is not involved beyond the initial project
// lookup.
func (c *apiClient) handleProjectSync(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("project sync is only available in MCP client mode"), nil
	}
	force := req.GetBool("force", false)

	remoteURL, err := c.fetchProjectMeta(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if remoteURL == "" {
		return mcp.NewToolResultError("project has no remote configured"), nil
	}

	proj, err := c.workspace.ForProject(int64(projectID), remoteURL)
	if err != nil {
		return mcp.NewToolResultError("opening local clone: " + err.Error()), nil
	}

	proj.Lock()
	defer proj.Unlock()

	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
		"force":      force,
	}

	// Preflight comparison so we can refuse destructive force-less
	// pushes to a diverged remote.
	cmp, cmpErr := proj.CompareToRemote()
	if cmpErr == nil && cmp != nil {
		resp["status"] = string(cmp.Status)
		resp["local_head"] = cmp.LocalHead
		resp["remote_head"] = cmp.RemoteHead
		resp["ahead_by"] = cmp.AheadBy
		resp["behind_by"] = cmp.BehindBy

		switch cmp.Status {
		case mcpgit.RemoteInSync:
			resp["result"] = "noop"
			resp["message"] = "already in sync"
			data, _ := json.Marshal(resp)
			return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
		case mcpgit.RemoteBehind:
			resp["result"] = "noop"
			resp["message"] = fmt.Sprintf("local is behind remote by %d commit(s); nothing to push — fetch+merge to catch up", cmp.BehindBy)
			data, _ := json.Marshal(resp)
			return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
		case mcpgit.RemoteDiverged, mcpgit.RemoteUnrelated:
			if !force {
				resp["result"] = "refused"
				resp["message"] = fmt.Sprintf(
					"remote has diverged (local ahead by %d, behind by %d) — refuse to push without force=true; re-run with force=true to overwrite remote, or reconcile manually",
					cmp.AheadBy, cmp.BehindBy,
				)
				data, _ := json.Marshal(resp)
				return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
			}
		}
	}

	var pushErr error
	if force {
		pushErr = proj.PushForce()
	} else {
		pushErr = proj.Push()
	}
	if pushErr != nil {
		resp["result"] = "failed"
		resp["error"] = pushErr.Error()
		data, _ := json.Marshal(resp)
		return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
	}
	if force {
		resp["result"] = "force_pushed"
	} else {
		resp["result"] = "pushed"
	}
	data, _ := json.Marshal(resp)
	return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
}

// handleSetProjectRemote updates a project's remote URL in the
// coordinator DB and, if a local clone exists, reconfigures its
// origin remote to match. Kept as a single tool (not split between
// coordinator and client) because the DB update and the local
// clone reconfiguration must stay consistent.
func (c *apiClient) handleSetProjectRemote(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	remoteURL, err := req.RequireString("remote_url")
	if err != nil {
		return mcp.NewToolResultError("remote_url is required"), nil
	}
	data, err := c.put(ctx, fmt.Sprintf("/api/v1/projects/%d/remote", projectID), map[string]string{
		"remote_url": remoteURL,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) == nil {
		if errMsg, ok := resp["error"].(string); ok {
			return mcp.NewToolResultError(errMsg), nil
		}
	}

	// Mirror the remote change into any existing local clone so
	// future pushes go to the new URL.
	if c.workspace != nil {
		if proj, err := c.workspace.ForProject(int64(projectID), remoteURL); err == nil {
			proj.Lock()
			_ = proj.SetRemote(remoteURL)
			proj.Unlock()
		}
	}

	if remoteURL == "" {
		return mcp.NewToolResultText(fmt.Sprintf("✓ Cleared remote for project %d", projectID)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Set remote for project %d to %s", projectID, remoteURL)), nil
}

func (c *apiClient) handleMyProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/citizens/by-username/"+c.username)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatProfile(data)), nil
}

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

func (c *apiClient) handleMyDashboard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/citizens/by-username/"+c.username+"/dashboard")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatDashboard(data)), nil
}

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

// taskMeta captures the fields the MCP client needs to drive the
// fat-client submit + claim paths: project identity, run layout,
// and the named-outputs schema (if any) so multi-file submits can
// compute per-output filenames without a second round-trip.
type taskMeta struct {
	ID               string
	ProjectID        int64
	ProjectRemoteURL string
	RunSeq           int
	TaskDefID        string
	InstanceKey      string
	// OutputsSchemaJSON is the serialized outputs schema from the
	// task's YAML, or empty if the task has no named outputs.
	// Parsed via mcpgit.ParseNamedOutputSchema by the fat-client
	// submit helper.
	OutputsSchemaJSON string
}

// fetchTaskMeta reads a task's metadata from the coordinator. Used
// by handleClaimTask, handleGetTaskInputs, and handleSubmitResult to
// decide whether to use the fat-client or legacy path.
func (c *apiClient) fetchTaskMeta(ctx context.Context, taskID string) (*taskMeta, error) {
	data, err := c.get(ctx, "/api/v1/tasks/"+taskID)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing task: %w", err)
	}
	if errMsg, ok := raw["error"].(string); ok {
		return nil, fmt.Errorf("%s", errMsg)
	}
	meta := &taskMeta{ID: taskID}
	if v, ok := raw["project_id"].(float64); ok {
		meta.ProjectID = int64(v)
	}
	if v, ok := raw["project_remote_url"].(string); ok {
		meta.ProjectRemoteURL = v
	}
	if v, ok := raw["run_seq"].(float64); ok {
		meta.RunSeq = int(v)
	}
	if v, ok := raw["task_def_id"].(string); ok {
		meta.TaskDefID = v
	}
	if v, ok := raw["instance_key"].(string); ok {
		meta.InstanceKey = v
	}
	if v, ok := raw["outputs"].(string); ok {
		meta.OutputsSchemaJSON = v
	}
	return meta, nil
}

// useFatClient reports whether the MCP client should take the
// iteration A.2 path for a given task: the client has a workspace
// configured AND the project has an external remote URL.
func (c *apiClient) useFatClient(meta *taskMeta) bool {
	return c.workspace != nil && meta != nil && meta.ProjectRemoteURL != ""
}

func (c *apiClient) handleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
		"username": c.username,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Decide which inputs path to take based on whether the
	// project has a remote_url configured. Fat clients pull their
	// own clone and resolve templates locally; legacy clients get
	// a fully-resolved prompt from the coordinator.
	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		c.logger.Warn("fetchTaskMeta after claim failed", "task_id", taskID, "error", metaErr)
	}
	var inputs []byte
	if c.useFatClient(meta) {
		inputs, err = c.fetchAndResolveLocally(ctx, meta)
		if err != nil {
			c.logger.Warn("fat-client resolve failed, falling back to legacy", "task_id", taskID, "error", err)
			inputs, _ = c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
		}
	} else {
		inputs, _ = c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
	}

	return mcp.NewToolResultText(formatClaimResult(data, inputs)), nil
}

func (c *apiClient) handleGetTaskInputs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		return mcp.NewToolResultError(metaErr.Error()), nil
	}

	if c.useFatClient(meta) {
		data, err := c.fetchAndResolveLocally(ctx, meta)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(formatJSON(data)), nil
	}

	data, err := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}

// fetchAndResolveLocally is the fat-client claim-time resolver: ask
// the coordinator for a dependency descriptor, open/pull the local
// clone, read upstream results and artifacts locally, render the
// resolved prompt via mcpgit. Returns a JSON blob that looks like the
// legacy /inputs response so formatters don't need to know which
// path produced it.
func (c *apiClient) fetchAndResolveLocally(ctx context.Context, meta *taskMeta) ([]byte, error) {
	descData, err := c.get(ctx, fmt.Sprintf("/api/v1/tasks/%s/inputs?client_mode=true", meta.ID))
	if err != nil {
		return nil, err
	}
	var desc struct {
		TaskID             string              `json:"task_id"`
		PromptTemplate     string              `json:"prompt_template"`
		UserPromptTemplate string              `json:"user_prompt_template"`
		ForEachParams      map[string]string   `json:"for_each_params"`
		Dependencies       []descDependencyRef `json:"dependencies"`
		ArtifactReads      []descArtifactRef   `json:"artifact_reads"`
		ProjectRemoteURL   string              `json:"project_remote_url"`
	}
	if err := json.Unmarshal(descData, &desc); err != nil {
		return nil, fmt.Errorf("parsing descriptor: %w", err)
	}
	if errMsg := extractErrorString(descData); errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}

	proj, err := c.workspace.ForProject(meta.ProjectID, meta.ProjectRemoteURL)
	if err != nil {
		return nil, fmt.Errorf("opening project clone: %w", err)
	}
	proj.Lock()
	defer proj.Unlock()
	if err := proj.Pull(); err != nil {
		return nil, fmt.Errorf("pulling: %w", err)
	}

	input := mcpgit.ResolveInput{
		PromptTemplate:     desc.PromptTemplate,
		UserPromptTemplate: desc.UserPromptTemplate,
		ForEachParams:      desc.ForEachParams,
	}
	for _, d := range desc.Dependencies {
		input.Dependencies = append(input.Dependencies, mcpgit.DependencyRef{
			TaskDefID:      d.TaskDefID,
			InstanceKey:    d.InstanceKey,
			InstanceParams: d.InstanceParams,
			CommitSHA:      d.CommitSHA,
			ResultPath:     d.ResultPath,
		})
	}
	for _, a := range desc.ArtifactReads {
		input.ArtifactReads = append(input.ArtifactReads, mcpgit.ArtifactRef{
			Path:      a.Path,
			CommitSHA: a.CommitSHA,
		})
	}

	resolved, err := proj.Resolve(input)
	if err != nil {
		return nil, err
	}

	// Shape the output to match the legacy /inputs response so
	// existing formatters (formatClaimResult, etc.) keep working.
	out := map[string]interface{}{
		"task_id":         meta.ID,
		"resolved_prompt": resolved.Prompt,
	}
	if resolved.UserPrompt != "" {
		out["resolved_user_prompt"] = resolved.UserPrompt
	}
	if len(resolved.ResolvedArtifacts) > 0 {
		out["artifacts"] = resolved.ResolvedArtifacts
	}
	if len(resolved.MissingArtifacts) > 0 {
		out["missing_artifacts"] = resolved.MissingArtifacts
	}
	return json.Marshal(out)
}

type descDependencyRef struct {
	TaskDefID      string            `json:"task_def_id"`
	InstanceKey    string            `json:"instance_key"`
	InstanceParams map[string]string `json:"instance_params"`
	CommitSHA      string            `json:"commit_sha"`
	ResultPath     string            `json:"result_path"`
}

type descArtifactRef struct {
	Path      string `json:"path"`
	CommitSHA string `json:"commit_sha"`
}

// extractErrorString pulls an `error` field out of a JSON response
// if present — used to surface coordinator error bodies through
// handlers that don't do full response parsing.
func extractErrorString(data []byte) string {
	var raw map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	if s, ok := raw["error"].(string); ok {
		return s
	}
	return ""
}

func (c *apiClient) handleSubmitResult(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	content := req.GetString("content", "")
	outputsJSON := req.GetString("outputs_json", "")
	artifactsJSON := req.GetString("artifacts_json", "")

	if content == "" && outputsJSON == "" && artifactsJSON == "" {
		return mcp.NewToolResultError("at least one of 'content', 'outputs_json', or 'artifacts_json' is required"), nil
	}

	var outputs map[string]string
	if outputsJSON != "" {
		if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
			return mcp.NewToolResultError("outputs_json must be valid JSON object: " + err.Error()), nil
		}
	}
	var artifacts map[string]string
	if artifactsJSON != "" {
		if err := json.Unmarshal([]byte(artifactsJSON), &artifacts); err != nil {
			return mcp.NewToolResultError("artifacts_json must be valid JSON object: " + err.Error()), nil
		}
	}

	// Decide between fat-client and legacy paths based on whether
	// the project has a remote_url configured.
	meta, _ := c.fetchTaskMeta(ctx, taskID)
	if c.useFatClient(meta) {
		return c.submitResultFatClient(ctx, taskID, meta, content, outputs, artifacts)
	}

	// Legacy coordinator-writes path.
	body := map[string]interface{}{
		"model": "claude",
	}
	if outputs != nil {
		body["outputs"] = outputs
	}
	if content != "" {
		body["content"] = content
	}
	if artifacts != nil {
		body["artifacts"] = artifacts
	}
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatSubmitResult(data, taskID)), nil
}

// submitResultFatClient is the iteration A.2 submit path: write the
// result and any artifacts into the project's local clone, commit,
// push (with retry on non-fast-forward), and report the resulting
// commit SHA back to the coordinator.
func (c *apiClient) submitResultFatClient(
	ctx context.Context,
	taskID string,
	meta *taskMeta,
	content string,
	outputs map[string]string,
	artifacts map[string]string,
) (*mcp.CallToolResult, error) {
	proj, err := c.workspace.ForProject(meta.ProjectID, meta.ProjectRemoteURL)
	if err != nil {
		return mcp.NewToolResultError("opening project clone: " + err.Error()), nil
	}

	resultDir := mcpgit.ResultDir(meta.ProjectID, meta.RunSeq, meta.InstanceKey, meta.TaskDefID)

	// Build the metadata.json that accompanies every submit.
	// Result type defaults to text; it gets flipped to json
	// below when the caller supplies named outputs.
	resultType := "text"
	if outputs != nil {
		resultType = "json"
	}
	metadata := map[string]interface{}{
		"task_id":     taskID,
		"model":       "claude",
		"result_type": resultType,
		"timestamp":   time.Now().Format(time.RFC3339),
	}

	files := []mcpgit.FileWrite{}

	// Single-file result path: `content` is a string blob.
	if content != "" {
		files = append(files, mcpgit.FileWrite{
			RepoRelPath: filepath.Join(resultDir, "result.md"),
			Content:     []byte(content),
		})
	}

	// Named outputs path: if the task declares an outputs schema
	// with per-output `file:` specs, each output lands in its own
	// file per the schema and metadata.json carries an
	// output_files index. Otherwise the outputs map is serialized
	// as a single result.json blob (legacy-compatible default).
	if outputs != nil {
		metadata["named_outputs"] = true
		schema := mcpgit.ParseNamedOutputSchema(meta.OutputsSchemaJSON)
		hasFileSpec := false
		for _, s := range schema {
			if s.File != "" {
				hasFileSpec = true
				break
			}
		}
		if hasFileSpec {
			outFiles, fileIndex := mcpgit.BuildNamedOutputFiles(resultDir, schema, outputs)
			files = append(files, outFiles...)
			metadata["output_files"] = fileIndex
		} else {
			outputsBytes, err := json.MarshalIndent(outputs, "", "  ")
			if err != nil {
				return mcp.NewToolResultError("encoding outputs: " + err.Error()), nil
			}
			files = append(files, mcpgit.FileWrite{
				RepoRelPath: filepath.Join(resultDir, "result.json"),
				Content:     outputsBytes,
			})
		}
	}

	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("encoding metadata: " + err.Error()), nil
	}
	files = append(files, mcpgit.FileWrite{
		RepoRelPath: filepath.Join(resultDir, "metadata.json"),
		Content:     metaBytes,
	})

	// Artifact writes. Kept in sorted-key order for deterministic
	// commit-message body ordering.
	var artifactPaths []string
	if len(artifacts) > 0 {
		artifactPaths = make([]string, 0, len(artifacts))
		for p := range artifacts {
			artifactPaths = append(artifactPaths, p)
		}
		sortStringsStable(artifactPaths)
		for _, p := range artifactPaths {
			files = append(files, mcpgit.FileWrite{
				RepoRelPath: mcpgit.ArtifactPath(meta.ProjectID, p),
				Content:     []byte(artifacts[p]),
			})
		}
	}

	authorName, authorEmail := c.commitAuthor(ctx)
	proj.Lock()
	submitRes, err := proj.SubmitTaskResult(mcpgit.SubmitRequest{
		TaskID:        taskID,
		Username:      c.username,
		AuthorName:    authorName,
		AuthorEmail:   authorEmail,
		Files:         files,
		ArtifactPaths: artifactPaths,
	})
	proj.Unlock()
	if err != nil {
		return mcp.NewToolResultError("writing commit to local clone: " + err.Error()), nil
	}

	// Report the commit to the coordinator so it can update the
	// state machine, result_path, commit_sha, and artifact index.
	reportBody := map[string]interface{}{
		"commit_sha":        submitRes.CommitSHA,
		"result_path":       resultDir,
		"artifacts_written": artifactPaths,
		"tokens_used":       0,
		"model":             "claude",
	}
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", reportBody)
	if err != nil {
		return mcp.NewToolResultError("reporting commit: " + err.Error()), nil
	}
	if errMsg := extractErrorString(data); errMsg != "" {
		return mcp.NewToolResultError("coordinator rejected report: " + errMsg), nil
	}
	return mcp.NewToolResultText(formatSubmitResult(data, taskID)), nil
}

// sortStringsStable is a tiny wrapper so server.go doesn't need its
// own sort import for one call.
func sortStringsStable(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// indexOfNewline returns the byte index of the first newline in s,
// or -1 if none. Used by the artifact-history formatter to trim
// commit message bodies down to their subject lines.
func indexOfNewline(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

// commitTaskSubjectRe matches the first line of commit messages the
// enju client writes, so get_artifact_history can enrich each entry
// with the submitting task_id and owner. Kept in sync with
// mcpgit.buildCommitMessage's format. A non-match means the commit
// wasn't produced by a task submission (project init, rollback,
// manual commit), in which case the entry's task_id / owner fields
// stay empty.
var commitTaskSubjectRe = regexp.MustCompile(`^Task (\S+) by @(\S+):`)

// parseTaskCommitMessage extracts the task ID and username from a
// commit subject. Returns empty strings if the commit didn't come
// from an enju task submission.
func parseTaskCommitMessage(msg string) (taskID, username string) {
	if idx := indexOfNewline(msg); idx >= 0 {
		msg = msg[:idx]
	}
	m := commitTaskSubjectRe.FindStringSubmatch(msg)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

func (c *apiClient) handleListArtifacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path := fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID)
	if prefix := req.GetString("prefix", ""); prefix != "" {
		path += "?prefix=" + prefix
	}
	data, err := c.get(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatArtifactList(data, int64(projectID))), nil
}

// handleGetArtifact reads an artifact's current content from the
// client's local clone. The coordinator provides the provenance
// metadata (via its artifact index), the client reads the actual
// bytes. This replaces the Phase 1 path where the coordinator
// served file contents from a server-side clone.
func (c *apiClient) handleGetArtifact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("get_artifact requires a local workspace (MCP client mode)"), nil
	}

	// Provenance metadata comes from the coordinator's artifact
	// index (last_writer, last_task_id, last_run_id, commit_sha,
	// updated_at). File bytes come from the local clone.
	metaRaw, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/artifacts/%s", projectID, path))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var meta map[string]interface{}
	_ = json.Unmarshal(metaRaw, &meta)
	if meta == nil {
		meta = map[string]interface{}{}
	}
	if errMsg, ok := meta["error"].(string); ok {
		return mcp.NewToolResultError(errMsg), nil
	}

	remoteURL, _ := c.fetchProjectMeta(ctx, int64(projectID))
	proj, err := c.workspace.ForProject(int64(projectID), remoteURL)
	if err != nil {
		return mcp.NewToolResultError("opening local clone: " + err.Error()), nil
	}
	proj.Lock()
	_ = proj.Pull()
	proj.Unlock()

	// Read at the indexed commit SHA if available so the content
	// matches what the coordinator's index points at. Fall back
	// to the working tree when no commit SHA is recorded. A.7
	// backward compat: try the new namespaced layout
	// (`projects/{id}/artifacts/...`) first, then the pre-A.5
	// flat layout (`artifacts/...`) so projects created before
	// the namespacing still resolve.
	commitSHA, _ := meta["commit_sha"].(string)
	primaryPath := mcpgit.ArtifactPath(int64(projectID), path)
	legacyPath := mcpgit.LegacyArtifactPath(path)
	var content []byte
	tryPaths := []string{primaryPath, legacyPath}
	if commitSHA != "" {
		var found bool
		for _, p := range tryPaths {
			data, ok, rerr := proj.ReadFileAtCommit(commitSHA, p)
			if rerr == nil && ok {
				content = data
				found = true
				break
			}
		}
		if !found {
			return mcp.NewToolResultError(fmt.Sprintf("artifact %q not found at commit %s (tried new and legacy layouts)", path, commitSHA)), nil
		}
	} else {
		var found bool
		for _, p := range tryPaths {
			data, rerr := proj.ReadFile(p)
			if rerr == nil {
				content = data
				found = true
				break
			}
		}
		if !found {
			return mcp.NewToolResultError("reading artifact from working tree: not found at new or legacy path"), nil
		}
	}
	meta["path"] = path
	meta["content"] = string(content)
	out, _ := json.Marshal(meta)
	return mcp.NewToolResultText(formatArtifactDetail(out)), nil
}

// handleGetArtifactHistory walks the local clone's git log for a
// specific file, then enriches each commit with current-pointer
// and invalidation status by cross-referencing the coordinator's
// artifact index and the task state machine.
//
// A.5 polish: in the orchestrator model, a commit in history can
// correspond to an invalidated task (its content is in git forever
// but the DB pointer no longer references it). Marking each commit
// as `[current pointer]` or `[invalidated]` makes the "which
// version is actually in effect" question obvious from the tool
// output.
func (c *apiClient) handleGetArtifactHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("get_artifact_history requires a local workspace (MCP client mode)"), nil
	}

	remoteURL, err := c.fetchProjectMeta(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj, err := c.workspace.ForProject(int64(projectID), remoteURL)
	if err != nil {
		return mcp.NewToolResultError("opening local clone: " + err.Error()), nil
	}
	proj.Lock()
	_ = proj.Pull()
	proj.Unlock()

	// A.7 backward compat: try git log at the new namespaced
	// path, fall back to the pre-A.5 flat layout if the primary
	// lookup returns no history (which is what happens for
	// projects created before the namespacing).
	history, err := proj.LogFile(mcpgit.ArtifactPath(int64(projectID), path))
	if err != nil {
		return mcp.NewToolResultError("reading git history: " + err.Error()), nil
	}
	if len(history) == 0 {
		legacyHistory, legacyErr := proj.LogFile(mcpgit.LegacyArtifactPath(path))
		if legacyErr == nil && len(legacyHistory) > 0 {
			history = legacyHistory
		}
	}

	// Fetch the coordinator's current artifact index pointer for
	// this path. The commit SHA it names is the "current pointer"
	// — the one the DB treats as the active version.
	currentCommitSHA := ""
	if artData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/artifacts/%s", projectID, path)); err == nil {
		var art map[string]interface{}
		if json.Unmarshal(artData, &art) == nil {
			if s, ok := art["commit_sha"].(string); ok {
				currentCommitSHA = s
			}
		}
	}

	// Build the set of unique task IDs in the history and fetch
	// each one's current state + current commit SHA. The latter
	// is needed to spot `superseded` commits: a commit whose
	// author task is currently ACCEPTED but whose hash differs
	// from the task's current commit (because the task was
	// invalidated and later re-submitted with a new version).
	// One GET per unique task — the history of one file is
	// rarely more than a handful of commits, so this is fine.
	type historyTaskMeta struct {
		state     string
		commitSHA string
	}
	taskMetas := map[string]historyTaskMeta{}
	for _, commit := range history {
		taskID, _ := parseTaskCommitMessage(commit.Message)
		if taskID == "" {
			continue
		}
		if _, have := taskMetas[taskID]; have {
			continue
		}
		if tdata, err := c.get(ctx, "/api/v1/tasks/"+taskID); err == nil {
			var t map[string]interface{}
			if json.Unmarshal(tdata, &t) == nil {
				m := historyTaskMeta{}
				if st, ok := t["state"].(string); ok {
					m.state = st
				}
				if cs, ok := t["commit_sha"].(string); ok {
					m.commitSHA = cs
				}
				taskMetas[taskID] = m
			}
		}
	}

	entries := make([]map[string]interface{}, 0, len(history))
	for _, commit := range history {
		subject := commit.Message
		if i := indexOfNewline(subject); i >= 0 {
			subject = subject[:i]
		}
		taskID, owner := parseTaskCommitMessage(commit.Message)

		// Annotation classification, in order of precedence:
		//
		//   1. current pointer — commit's SHA matches the
		//      artifact index's current value. This is the
		//      version the coordinator treats as live.
		//
		//   2. invalidated — commit's task is currently in a
		//      non-ACCEPTED state (READY / PENDING / CLAIMED).
		//      The task's result is being re-done.
		//
		//   3. superseded — commit's task is ACCEPTED but its
		//      hash doesn't match the task's current commit SHA.
		//      This happens when a task was invalidated, the
		//      artifact reverted to an earlier writer, and then
		//      the task was re-submitted with a new version —
		//      the old pre-invalidation commit is still in git
		//      history but is no longer what the task points at.
		//
		//   4. (none) — commit is accepted and its hash matches
		//      its task's current commit SHA but isn't the
		//      artifact's current pointer (e.g., this task
		//      wrote the file but a different task is the live
		//      writer now).
		annotation := ""
		tm, haveTaskMeta := taskMetas[taskID]
		switch {
		case commit.Hash == currentCommitSHA && taskID != "":
			annotation = "current pointer"
		case haveTaskMeta && taskID != "" && tm.state != "accepted":
			annotation = "invalidated — task " + taskID + " now " + tm.state
		case haveTaskMeta && taskID != "" && tm.state == "accepted" && tm.commitSHA != "" && tm.commitSHA != commit.Hash:
			short := tm.commitSHA
			if len(short) > 8 {
				short = short[:8]
			}
			annotation = "superseded — task re-submitted as " + short
		}

		entry := map[string]interface{}{
			"hash":    commit.Hash,
			"subject": subject,
			"time":    commit.Time.Format(time.RFC3339),
			"task_id": taskID,
			"owner":   owner,
		}
		if annotation != "" {
			entry["annotation"] = annotation
		}
		entries = append(entries, entry)
	}
	out, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"history": entries,
	})
	return mcp.NewToolResultText(formatArtifactHistory(out)), nil
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

	return mcp.NewToolResultText(formatTaskDetail(data, inputs)), nil
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

	return mcp.NewToolResultText(formatRunStatus(run, tasks)), nil
}

func (c *apiClient) handleCreateRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	yamlContent, err := req.RequireString("yaml")
	if err != nil {
		return mcp.NewToolResultError("yaml is required"), nil
	}
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required — create a project first with enju_create_project"), nil
	}

	path := fmt.Sprintf("/api/v1/projects/%d/runs", projectID)
	data, err := c.post(ctx, path, map[string]interface{}{
		"yaml": yamlContent,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(formatCreateRun(data)), nil
}

// --- Helpers ---

// updateLocalCredentials updates the name in ~/.enju/credentials.json
func updateLocalCredentials(name string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := home + "/.enju/credentials.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var creds map[string]interface{}
	if json.Unmarshal(data, &creds) != nil {
		return
	}
	creds["name"] = name
	updated, _ := json.MarshalIndent(creds, "", "  ")
	os.WriteFile(path, updated, 0600)
}

func formatJSON(data []byte) string {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return string(data)
	}
	return pretty.String()
}
