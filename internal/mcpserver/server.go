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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
	CitizenEmail   string // email used when re-registering after a DB wipe, optional
	// Workspace is the per-client git workspace used by the
	// iteration A.2 fat-client path. When non-nil and a project
	// has a remote_url, the MCP client writes task results to a
	// local clone here and reports commit SHAs back to the
	// coordinator, bypassing the legacy content-over-wire path.
	// When nil, only the legacy path is used.
	Workspace *mcpgit.Workspace
	// SaveCredentials is called after a successful auto re-register
	// so the new server-side identity is persisted to disk. The
	// username passed back may be the same (DB wipe case) or new
	// (unusual — shouldn't happen with stable-handle registration).
	// Email is passed through so future GitHub integration work
	// can rely on the persisted address staying present across
	// re-registrations. If nil, auto re-register still updates
	// in-memory state but won't persist.
	SaveCredentials func(username, name, email string)
	// ModelName is the LLM model used by this citizen, for
	// contribution tracking (e.g. "claude-opus-4", "gpt-4o").
	// Optional — when set, included in contribution event
	// metadata so cost analysis can segment by model.
	ModelName string
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
		baseURL:       cfg.CoordinatorURL,
		username:      cfg.Username,
		citizenName:   cfg.CitizenName,
		citizenEmail:  cfg.CitizenEmail,
		modelName:    cfg.ModelName,
		saveCreds:     cfg.SaveCredentials,
		workspace:     cfg.Workspace,
		logger:        logger,
		httpClient:    &http.Client{},
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
	s.AddTool(toolLeaveProject(), client.handleLeaveProject)
	s.AddTool(toolListArtifacts(), client.handleListArtifacts)
	s.AddTool(toolGetArtifact(), client.handleGetArtifact)
	s.AddTool(toolGetArtifactHistory(), client.handleGetArtifactHistory)
	s.AddTool(toolMyProfile(), client.handleMyProfile)
	s.AddTool(toolInvalidateTask(), client.handleInvalidateTask)
	s.AddTool(toolTallyTask(), client.handleTallyTask)
	s.AddTool(toolFailTask(), client.handleFailTask)
	s.AddTool(toolExecuteTask(), client.handleExecuteTask)
	s.AddTool(toolExportRun(), client.handleExportRun)
	s.AddTool(toolListTemplates(), client.handleListTemplates)
	s.AddTool(toolDescribeTemplate(), client.handleDescribeTemplate)

	return s
}

// --- API Client ---

type apiClient struct {
	baseURL      string
	username     string // caller's citizen username — stable across auto re-registers
	citizenName  string // display name, used when re-registering after a DB wipe
	citizenEmail string // optional, passed to the register endpoint
	modelName   string // LLM model for contribution tracking
	saveCreds    func(username, name, email string)
	workspace    *mcpgit.Workspace
	logger       *slog.Logger
	httpClient   *http.Client

	// reRegisterMu serializes re-registration attempts so concurrent
	// tool calls only trigger one refresh. Acquired by
	// ensureCitizenFresh before firing the register POST.
	reRegisterMu sync.Mutex

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
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		return c.httpClient.Do(req)
	})
}

func (c *apiClient) put(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+path, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return c.httpClient.Do(req)
	})
}

func (c *apiClient) post(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return c.httpClient.Do(req)
	})
}

// doWithAutoReregister runs an HTTP request closure and, if the
// response body signals that the caller's citizen record no longer
// exists on the coordinator (typically: the server DB was wiped),
// re-registers with the same username + display name and replays
// the request once. Registering with a stable username is
// idempotent — the coordinator recreates a citizen with the same
// handle, so URLs embedding c.username and request bodies built
// from c.username stay valid across the retry.
//
// Only one retry is attempted. If the retry also fails (for any
// reason), the retry's response is returned as-is.
func (c *apiClient) doWithAutoReregister(ctx context.Context, do func() (*http.Response, error)) ([]byte, error) {
	resp, err := do()
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !isStaleCitizenResponse(resp.StatusCode, data) {
		return data, nil
	}
	if c.citizenName == "" {
		// No display name to re-register with — the caller
		// invoked `enju mcp` with only a username, which means
		// we can't recreate the record automatically. Return
		// the original response so the handler surfaces the
		// coordinator's error.
		c.logger.Warn("stale citizen detected but CitizenName is empty; cannot auto re-register",
			"username", c.username)
		return data, nil
	}
	if err := c.ensureCitizenFresh(ctx); err != nil {
		c.logger.Warn("auto re-register failed", "username", c.username, "error", err)
		return data, nil
	}
	c.logger.Info("auto re-registered stale citizen, retrying request", "username", c.username)
	resp2, err := do()
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable (after re-register): %w", err)
	}
	data2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	return data2, nil
}

// isStaleCitizenResponse tells whether the response body looks like
// a coordinator "citizen not found" error. Matches the two error
// message forms writeError currently emits from
// internal/api/router.go: `citizen "foo" not found` and the plain
// `citizen not found`. Only considers 404 responses to avoid
// misidentifying a 200 that happens to contain the phrase.
func isStaleCitizenResponse(status int, body []byte) bool {
	if status != http.StatusNotFound {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "citizen") && strings.Contains(s, "not found")
}

// ensureCitizenFresh POSTs /citizens/register with the client's
// cached username + display name. Used by the auto-reregister flow
// to recreate a citizen record after a coordinator DB wipe.
// Serialized by reRegisterMu so concurrent tool calls only fire
// one register.
func (c *apiClient) ensureCitizenFresh(ctx context.Context) error {
	c.reRegisterMu.Lock()
	defer c.reRegisterMu.Unlock()
	body := map[string]string{"name": c.citizenName}
	if c.username != "" {
		body["username"] = c.username
	}
	if c.citizenEmail != "" {
		body["email"] = c.citizenEmail
	}
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v1/citizens/register", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("register returned %d: %s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decoding register response: %w", err)
	}
	if errMsg, ok := result["error"].(string); ok && errMsg != "" {
		return fmt.Errorf("%s", errMsg)
	}
	got, _ := result["username"].(string)
	if got == "" {
		return fmt.Errorf("register response missing username")
	}
	c.username = got
	if c.saveCreds != nil {
		c.saveCreds(got, c.citizenName, c.citizenEmail)
	}
	return nil
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
For action:review tasks: provide 'decision' as "approve" or "reject". Your prose content is the reviewer's comment. A reject verdict automatically invalidates the target task (the one named in its 'reviews:' field) so the author can re-submit a fixed version.
For action:vote tasks: provide 'option' as one of the declared option ids from the task's 'options:' list. Your prose content is free-form commentary. If the winning option has 'activates:' set, the DAG routes down that branch and tasks on losing branches flip to SKIPPED. Votes without 'activates:' are pure decisions — downstream tasks can still read the choice via {{task.winning_option}}.
The task detail shows the schema (outputs, writes_artifacts, reviews target, options) so you know what's expected.`),
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
		mcp.WithString("decision",
			mcp.Description(`Required for action:review tasks: "approve" or "reject". Ignored on non-review tasks. A reject cascades an invalidation on the reviewed target task, bouncing it back to READY so the author can re-submit.`),
		),
		mcp.WithString("option",
			mcp.Description(`Required for action:vote tasks: one of the declared option ids from the task's 'options:' YAML list (as shown in the claim response's Options block). Ignored on non-vote tasks.`),
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
		mcp.WithDescription(`Create a new Enju run. Three ways to provide the run definition, pick one:

1. WRITE IT DIRECTLY: pass "yaml" with the full run definition — use this for one-off runs the user is authoring from scratch.
2. FROM A SAVED TEMPLATE: pass "path" pointing at a templates/*.yaml recipe in the project clone, plus "params" with the values that template asks for. Use this whenever a user's request matches a known recipe — see enju_list_templates.
3. DIRECT + PARAMS: pass "yaml" AND "params" together — a one-off run whose prompts reference top-level {{param}} values. Less common; mostly useful when the LLM is composing a parameterized run programmatically without saving it as a template file first.

YAML format (same for inline and template files):
  name: "Run name"
  description: "Optional prose — shown in template menus"
  version: 1
  params:                       # optional; makes the YAML reusable
    - name: disease
      type: string               # string | int | bool | list<string>
      required: true
      description: "The disease to analyze"
  for_each:
    variable: [value1, value2]   # optional, for parallel expansion
  tasks:
    - id: task_name
      action: answer
      prompt: "Analyze {{disease}} using {{other_task.content}}."

To browse available templates in a project, call enju_list_templates first. To see a specific template's parameter docs before filling them in, call enju_describe_template.

Dependencies between tasks are inferred automatically from {{task_id.content}} references. Tasks without references to other tasks run in parallel.

If you don't have a project yet, create one first with enju_create_project.`),
		mcp.WithString("yaml",
			mcp.Description("The run definition in YAML format. Required unless 'path' is provided."),
		),
		mcp.WithString("path",
			mcp.Description("Repo-relative path to a template under templates/, e.g. 'templates/gwas.yaml'. The template is read from the local project clone. Mutually exclusive with 'yaml'."),
		),
		mcp.WithObject("params",
			mcp.Description("Parameter values for a run that declares a top-level 'params:' block. Keys are parameter names; values must match the declared types. Use enju_describe_template to see what a template expects."),
		),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID to create this run in (use enju_list_projects to see existing projects)"),
		),
	)
}

// toolListTemplates is the LLM's template-discovery entry
// point. Returns every YAML file under the project clone's
// templates/ directory with its name, description, and
// parameter summary so the LLM can pick a recipe that fits
// the user's request without reading each file.
func toolFailTask() mcp.Tool {
	return mcp.NewTool("enju_fail_task",
		mcp.WithDescription(`Mark a task as failed with a reason. Works for any action type (answer, contribute, compute, review, vote).

Use this when you can't complete a task — missing data, broken upstream, environment issue, or any other blocker. The task moves to a terminal "failed" state, downstream descendants are blocked, and the reason is visible to all citizens in run_status.

Recovery: the run author or any citizen can use enju_invalidate_task to bounce a failed task back to READY for re-assignment.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The task to fail"),
		),
		mcp.WithString("reason",
			mcp.Required(),
			mcp.Description("Why the task failed (shown to all citizens in run_status)"),
		),
	)
}

func toolExecuteTask() mcp.Tool {
	return mcp.NewTool("enju_execute_task",
		mcp.WithDescription(`Execute a compute task's script, capture its output, and submit the result — all in one call.

For action:compute tasks only. Claims the task if not already claimed, runs the declared script in the project's local clone, captures stdout as the result, and submits automatically.

Environment variables available to the script:
  ENJU_TASK_ID      — the full task ID
  ENJU_PROJECT_DIR  — the project's local clone root
  ENJU_RUN_DIR      — the result directory for this task

Exit code semantics:
  0     → submit as completed (stdout → result.md)
  non-0 → task fails (stderr shown as the failure reason)

The script runs in the project's workspace directory. It has full access to the local clone (upstream results, artifacts, etc.).`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The task to execute"),
		),
	)
}

func toolExportRun() mcp.Tool {
	return mcp.NewTool("enju_export_run",
		mcp.WithDescription(`Export a completed run as a single markdown document. Assembles all task results in DAG order — each task becomes a section with its prompt and result. Use this for the preprint appendix or to review the full output of a run in one place.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_seq",
			mcp.Required(),
			mcp.Description("The run sequence number within the project"),
		),
	)
}

func toolListTemplates() mcp.Tool {
	return mcp.NewTool("enju_list_templates",
		mcp.WithDescription(`List the reusable run recipes (templates) available in a project. Each entry shows the template's name, description, and its declared parameters. Use this first when a user asks to do something that matches a known recipe — the template saves them from hand-writing a run YAML.

Templates are just regular run YAML files that live under templates/ in the project git repo. Any run can be promoted to a template by copying its YAML file into templates/; no conversion step.

To see full parameter docs for one template (types, defaults, descriptions), call enju_describe_template <path>. To instantiate a template into a run, call enju_create_run with 'path' and 'params'.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose templates/ directory to scan"),
		),
	)
}

// toolDescribeTemplate returns the full parameter block for a
// single template so the LLM can turn each param into a
// user-facing question ("which disease?", "which tissue?")
// before filling in values and calling enju_create_run.
func toolDescribeTemplate() mcp.Tool {
	return mcp.NewTool("enju_describe_template",
		mcp.WithDescription(`Show the full metadata for one template: name, description, and every declared parameter with its type, default, and prose description. Use this when a user picks a template from enju_list_templates and you need to gather the parameter values before calling enju_create_run.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose template to describe"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Repo-relative template path, e.g. 'templates/gwas.yaml'"),
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

func toolLeaveProject() mcp.Tool {
	return mcp.NewTool("enju_leave_project",
		mcp.WithDescription("Forget a project's local clone and delete its workspace directory. Use this to reclaim disk space when you're done working on a project, or to recover from a corrupted local clone. The remote repo is untouched — this is a local cache wipe only. The next time you touch the project (claim a task, read an artifact, etc.) it will be re-cloned from the remote."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose local clone should be deleted"),
		),
	)
}

func toolUpdateProfile() mcp.Tool {
	return mcp.NewTool("enju_update_profile",
		mcp.WithDescription("Update your citizen profile. Merge semantics: any field you omit from the call is left untouched on both the server and in your local credentials file. Pass only what you want to change. At least one of name or email must be provided."),
		mcp.WithString("name",
			mcp.Description("Your display name. Omit this field to leave the current name unchanged; passing an empty string is rejected."),
		),
		mcp.WithString("email",
			mcp.Description("Your email (must be unique). Omit this field to leave the current email unchanged; pass an empty string to explicitly clear it."),
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

func toolTallyTask() mcp.Tool {
	return mcp.NewTool("enju_tally_task",
		mcp.WithDescription(`Force a tally evaluation on a multi-citizen vote or review task that is currently in the 'collecting' state. Runs the same tally logic as a submission would: counts the per-citizen submissions, applies the task's threshold + quorum + deadline rules, and resolves the task to 'accepted' if a winner emerges. Reports the current tally state either way.

Use this when:
- A vote/review task is stuck in collecting and you want to check whether it has enough votes to resolve under its threshold rule
- The deadline has passed and you want to force the past-deadline resolution (the server's lazy check fires on the next task read, but this tool makes the trigger explicit)
- You're the run author and want to check "is this ready to go?" without waiting for another submission

The tally response includes the current counts, whether the task resolved, and if it resolved the winning option (vote) or verdict (review) + any newly-unlocked downstream tasks.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The fully-qualified ID of the vote or review task to tally"),
		),
	)
}

// --- Tool Handlers ---

func (c *apiClient) handleUpdateProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Merge semantics: only include fields the caller actually
	// provided in the request. Omitted fields stay untouched on
	// both the server and in credentials.json. This prevents
	// update_profile(name="X") from silently clearing email.
	args := req.GetArguments()
	body := map[string]interface{}{}
	var providedName, providedEmail string
	var haveName, haveEmail bool
	if v, ok := args["name"]; ok {
		s, _ := v.(string)
		if s == "" {
			return mcp.NewToolResultError("name cannot be empty"), nil
		}
		body["name"] = s
		providedName = s
		haveName = true
	}
	if v, ok := args["email"]; ok {
		s, _ := v.(string)
		body["email"] = s
		providedEmail = s
		haveEmail = true
	}
	if len(body) == 0 {
		return mcp.NewToolResultError("at least one of name or email must be provided"), nil
	}

	data, err := c.put(ctx, "/api/v1/citizens/by-username/"+c.username+"/profile", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if errMsg, ok := result["error"].(string); ok {
			return mcp.NewToolResultError(errMsg), nil
		}
	}

	// Local credentials file: same merge semantics — only touch
	// fields the caller actually provided.
	updateLocalCredentials(haveName, providedName, haveEmail, providedEmail)

	// Show the authoritative current display name in the response.
	// When the caller provided a name, we echo theirs; when they
	// only changed email, we re-fetch the profile so the user
	// sees their existing display name instead of the username
	// handle.
	label := providedName
	if !haveName {
		if profileData, perr := c.get(ctx, "/api/v1/citizens/by-username/"+c.username); perr == nil {
			var prof map[string]interface{}
			if json.Unmarshal(profileData, &prof) == nil {
				if n, _ := prof["name"].(string); n != "" {
					label = n
				}
			}
		}
		if label == "" {
			label = c.username
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Profile updated: %s", label)), nil
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
		return mcp.NewToolResultError(err.Error()), nil
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
		return mcp.NewToolResultError(err.Error()), nil
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

// handleLeaveProject deletes the local clone of a project from the
// MCP client's workspace. Purely local — the coordinator and the
// remote repo are untouched. Re-clones on next access.
//
// Validates that the project actually exists on the coordinator
// before removing anything, so a typo'd project_id returns a
// crisp "not found" error instead of silently pretending the
// no-op succeeded.
func (c *apiClient) handleLeaveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("leave project is only available in MCP client mode"), nil
	}
	// Existence check. fetchProjectMeta returns an error if the
	// coordinator's GET /projects/{id} responds with 404 (or any
	// other error); surface it verbatim.
	if _, err := c.fetchProjectMeta(ctx, int64(projectID)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("✗ Project #%d not found", projectID)), nil
	}
	hadClone := c.workspace.HasLocalClone(int64(projectID))
	if err := c.workspace.LeaveProject(int64(projectID)); err != nil {
		return mcp.NewToolResultError("removing local clone: " + err.Error()), nil
	}
	var line string
	if hadClone {
		line = fmt.Sprintf("✓ Project #%d: local clone removed — next access will re-clone from the remote", projectID)
	} else {
		line = fmt.Sprintf("• Project #%d: no clone to remove (already absent)", projectID)
	}
	return mcp.NewToolResultText(line), nil
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
	// Inject model name from client config so the profile
	// display shows which model this session is using.
	if c.modelName != "" {
		var profileMap map[string]interface{}
		if json.Unmarshal(data, &profileMap) == nil {
			profileMap["model"] = c.modelName
			data, _ = json.Marshal(profileMap)
		}
	}
	// Fetch contribution summary for the enriched profile.
	contribData, contribErr := c.get(ctx, "/api/v1/citizens/by-username/"+c.username+"/contributions")
	if contribErr != nil {
		// Contributions are best-effort — show the basic
		// profile if contributions endpoint fails.
		contribData = nil
	}
	return mcp.NewToolResultText(formatProfile(data, contribData)), nil
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
	// State is the task's current lifecycle state. Populated
	// from the coordinator's task record so the fat-client
	// submit helper can pre-reject submissions against tasks
	// that are already in a terminal state, avoiding a
	// phantom-commit-style round-trip (commit+push → server
	// rejects with "task cannot accept result").
	State string
	// Action is the task's action type ("answer", "review", etc).
	// Used by the fat-client submit helper to pre-validate
	// action-specific fields (e.g. decision on review) BEFORE
	// touching the local clone, so a rejected submission never
	// leaves a phantom commit in git history.
	Action string
	// ReviewsTarget is the short task id this review task
	// evaluates. Empty for non-review tasks. Surfaced so the
	// client-side formatter can show the reviewer what they're
	// reviewing without a separate fetch.
	ReviewsTarget string
	// VoteOptionsJSON is the declared options list for
	// action:vote tasks, copied verbatim from the coordinator's
	// task record. Used by client-side pre-validation (to
	// reject unknown option ids before any git write) and by
	// the claim-response formatter (to show the voter what the
	// choices are). Empty for non-vote tasks.
	VoteOptionsJSON string
	// Citizens is the declared citizens count for multi-voter /
	// multi-reviewer tasks. Defaults to 1. When > 1, the
	// fat-client submit path writes to per-citizen result
	// subdirectories so parallel submissions don't race on the
	// same result.md file.
	Citizens int
	// OutputsSchemaJSON is the serialized outputs schema from the
	// task's YAML, or empty if the task has no named outputs.
	// Parsed via mcpgit.ParseNamedOutputSchema by the fat-client
	// submit helper.
	OutputsSchemaJSON string
	// Script is the script path for action:compute tasks.
	Script string
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
	if v, ok := raw["action"].(string); ok {
		meta.Action = v
	}
	if v, ok := raw["reviews_target"].(string); ok {
		meta.ReviewsTarget = v
	}
	if v, ok := raw["vote_options"].(string); ok {
		meta.VoteOptionsJSON = v
	}
	if v, ok := raw["state"].(string); ok {
		meta.State = v
	}
	if v, ok := raw["citizens"].(float64); ok {
		meta.Citizens = int(v)
	}
	if v, ok := raw["script"].(string); ok {
		meta.Script = v
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

	return mcp.NewToolResultText(formatClaimResult(data, inputs, c.username)), nil
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
		return nil, err
	}
	proj.Lock()
	defer proj.Unlock()
	if err := proj.Pull(); err != nil {
		return nil, fmt.Errorf("refreshing local clone before resolving task %s: %w", meta.ID, err)
	}

	input := mcpgit.ResolveInput{
		TaskID:             meta.ID,
		PromptTemplate:     desc.PromptTemplate,
		UserPromptTemplate: desc.UserPromptTemplate,
		ForEachParams:      desc.ForEachParams,
	}
	for _, d := range desc.Dependencies {
		ref := mcpgit.DependencyRef{
			TaskDefID:      d.TaskDefID,
			InstanceKey:    d.InstanceKey,
			InstanceParams: d.InstanceParams,
			CommitSHA:      d.CommitSHA,
			ResultPath:     d.ResultPath,
			VoteChoice:     d.VoteChoice,
		}
		for _, r := range d.Responses {
			ref.Responses = append(ref.Responses, mcpgit.CitizenResponseRef{
				Username: r.Username,
				Option:   r.Option,
				Content:  r.Content,
			})
		}
		input.Dependencies = append(input.Dependencies, ref)
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

	// Review-task surfacing: when the caller is claiming an
	// action:review task, show the reviewed target's content
	// inline in the claim response. The reviewer shouldn't need a
	// separate enju_get_task round-trip just to see what they're
	// evaluating. The target is always in Dependencies (the
	// parser auto-inserts the reviews: edge), and the fat-client
	// has already pulled the commit to the local clone above, so
	// this is a plain file read.
	if meta.Action == "review" && meta.ReviewsTarget != "" {
		for _, d := range desc.Dependencies {
			if d.TaskDefID != meta.ReviewsTarget {
				continue
			}
			contentPath := filepath.Join(d.ResultPath, "result.md")
			data, ok, rerr := proj.ReadFileAtCommit(d.CommitSHA, contentPath)
			if rerr != nil || !ok {
				// Non-fatal — we'd rather show a partial claim
				// response than fail the claim over a formatter
				// nicety. Log and move on.
				c.logger.Warn("reading reviewed target content",
					"review_task", meta.ID,
					"target", meta.ReviewsTarget,
					"path", contentPath,
					"commit", d.CommitSHA,
					"error", rerr,
				)
				break
			}
			reviewingBlock := map[string]interface{}{
				"target_def_id": meta.ReviewsTarget,
				"commit_sha":    d.CommitSHA,
				"content":       string(data),
			}
			// Fetch the target task to pick up the claimer's
			// username so the block can render "(by @alice)".
			// One extra GET per review claim — negligible, and
			// the output is much more useful with it.
			runPrefix := fmt.Sprintf("%d:%d:", meta.ProjectID, meta.RunSeq)
			targetFullID := runPrefix + meta.ReviewsTarget
			if targetData, terr := c.get(ctx, "/api/v1/tasks/"+targetFullID); terr == nil {
				var targetRaw map[string]interface{}
				if json.Unmarshal(targetData, &targetRaw) == nil {
					if u, _ := targetRaw["claimed_by"].(string); u != "" {
						reviewingBlock["claimed_by"] = u
					}
				}
			}
			out["reviewing"] = reviewingBlock
			break
		}
	}
	return json.Marshal(out)
}

type descDependencyRef struct {
	TaskDefID      string            `json:"task_def_id"`
	InstanceKey    string            `json:"instance_key"`
	InstanceParams map[string]string `json:"instance_params"`
	CommitSHA      string            `json:"commit_sha"`
	ResultPath     string            `json:"result_path"`
	// VoteChoice is the upstream vote task's winning option id
	// (Phase E.2). Empty for non-vote upstreams.
	VoteChoice string `json:"vote_choice,omitempty"`
	// Responses is the per-citizen submission list for
	// multi-citizen upstreams (Phase E.2 session 2b). Each
	// entry has a username + option; the client-side resolver
	// reads the per-citizen result.md from the local clone
	// and attaches the content before substituting into the
	// downstream prompt via {{task.responses}}.
	Responses []descResponseRef `json:"responses,omitempty"`
}

type descResponseRef struct {
	Username string `json:"username"`
	Option   string `json:"option"`
	Content  string `json:"content,omitempty"`
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
	decision := req.GetString("decision", "")
	option := req.GetString("option", "")

	// Primary-field presence check. A vote task can submit with
	// just `option`, a review task with just `decision` — those
	// actions treat the action-specific field as the primary
	// signal and prose content is optional commentary. Without
	// the decision/option here the check emits a misleading
	// "content is required" error on an option-only vote.
	if content == "" && outputsJSON == "" && artifactsJSON == "" && decision == "" && option == "" {
		return mcp.NewToolResultError("at least one of 'content', 'outputs_json', 'artifacts_json', 'decision' (review), or 'option' (vote) is required"), nil
	}
	// Any non-empty decision must be valid, regardless of action.
	// The "required for review" check happens in the fat-client
	// pre-validation and on the coordinator.
	if decision != "" && decision != "approve" && decision != "reject" {
		return mcp.NewToolResultError(invalidDecisionMessage(decision)), nil
	}

	// Parse outputs_json into two buckets: plain string
	// values (the existing named-outputs path, one file per
	// output) and list<string> values (Phase J.1 — routed to
	// the coordinator so dynamic for_each can materialize
	// downstream instances from the resolved list). Accepting
	// interface{} here keeps the tool's input JSON shape
	// natural — a list param is a JSON array, a string param
	// is a JSON string.
	var outputs map[string]string
	var outputLists map[string][]string
	if outputsJSON != "" {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(outputsJSON), &raw); err != nil {
			return mcp.NewToolResultError("outputs_json must be valid JSON object: " + err.Error()), nil
		}
		for name, v := range raw {
			switch val := v.(type) {
			case string:
				if outputs == nil {
					outputs = make(map[string]string)
				}
				outputs[name] = val
			case []interface{}:
				list := make([]string, 0, len(val))
				for i, item := range val {
					s, ok := item.(string)
					if !ok {
						return mcp.NewToolResultError(fmt.Sprintf("outputs_json[%q][%d]: list items must be strings", name, i)), nil
					}
					list = append(list, s)
				}
				if outputLists == nil {
					outputLists = make(map[string][]string)
				}
				outputLists[name] = list
			default:
				return mcp.NewToolResultError(fmt.Sprintf("outputs_json[%q]: value must be a string or a list of strings", name)), nil
			}
		}
	}
	var artifacts map[string]string
	if artifactsJSON != "" {
		if err := json.Unmarshal([]byte(artifactsJSON), &artifacts); err != nil {
			return mcp.NewToolResultError("artifacts_json must be valid JSON object: " + err.Error()), nil
		}
	}

	// Task-existence check up front. fetchTaskMeta returns an
	// error if the coordinator can't find the task (typo, wiped
	// DB, wrong ID). Surface that as a clean "task not found"
	// instead of letting the legacy fallback path POST into a
	// void and surface the server's internal "commit_sha is
	// required" contract error as if it were the real problem.
	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("task %q not found: %v", taskID, metaErr)), nil
	}
	if c.useFatClient(meta) {
		return c.submitResultFatClient(ctx, taskID, meta, content, outputs, outputLists, artifacts, decision, option)
	}

	// Legacy coordinator-writes path.
	body := map[string]interface{}{
		"model": c.modelName,
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
	if decision != "" {
		body["decision"] = decision
	}
	if option != "" {
		body["option"] = option
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
	outputLists map[string][]string,
	artifacts map[string]string,
	decision string,
	option string,
) (*mcp.CallToolResult, error) {
	// Pre-validate action-specific invariants BEFORE touching the
	// local clone. The fat-client submit does commit+push before
	// the coordinator sees the report, so any server-side reject
	// after that point would leave a phantom commit stranded in
	// git history (append-only, nothing to roll back). Anything
	// the client can check up front belongs here.
	//
	// Task-state gate: a submission against an already-terminal
	// task (accepted / skipped / invalidated / rejected) has no
	// legitimate landing state. Reject it client-side with a
	// task-specific message — mirrors the server's existing
	// "task X cannot accept result (state: Y)" but saves a git
	// round-trip.
	if meta != nil && meta.State != "" {
		switch meta.State {
		case "accepted", "skipped", "failed", "invalid", "invalidated", "rejected":
			return mcp.NewToolResultError(fmt.Sprintf(
				"task %s is already in terminal state %q — re-open it with enju_invalidate_task first if you need to resubmit",
				taskID, meta.State,
			)), nil
		case "pending":
			return mcp.NewToolResultError(fmt.Sprintf(
				"task %s is blocked (waiting on upstream dependencies) — it's not ready for submission yet",
				taskID,
			)), nil
		case "ready":
			// Multi-citizen tasks stay in READY while claims
			// are being collected. Only reject for single-
			// citizen tasks where READY means "not yet claimed."
			if meta.Citizens <= 1 {
				return mcp.NewToolResultError(fmt.Sprintf(
					"task %s is available but not claimed — use enju_claim_task first",
					taskID,
				)), nil
			}
			// Multi-citizen: READY is valid — the engine
			// validates the citizen's active claim server-side.
		}
	}
	if meta != nil && meta.Action == "review" {
		if msg := validateReviewDecision(decision); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
	}
	if meta != nil && meta.Action == "vote" {
		if msg := validateVoteOption(option, meta.VoteOptionsJSON); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
	}

	proj, err := c.workspace.ForProject(meta.ProjectID, meta.ProjectRemoteURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Multi-citizen tasks route each citizen's submission into
	// its own `citizen-<username>/` subdirectory so parallel
	// submitters don't race on the same result.md. Single-
	// citizen tasks keep the flat `runs/{seq}/{task}/` layout.
	// The task's declared citizens count is stored on the DB
	// row and surfaced via taskMeta.Citizens.
	baseResultDir := mcpgit.ResultDir(meta.ProjectID, meta.RunSeq, meta.InstanceKey, meta.TaskDefID)
	resultDir := baseResultDir
	if meta.Citizens > 1 {
		resultDir = filepath.Join(baseResultDir, "citizen-"+c.username)
	}

	// Build the metadata.json that accompanies every submit.
	// Result type defaults to text; it gets flipped to json
	// below when the caller supplies named outputs.
	resultType := "text"
	if outputs != nil {
		resultType = "json"
	}
	metadata := map[string]interface{}{
		"task_id":     taskID,
		"model":       c.modelName,
		"result_type": resultType,
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	// Review-action metadata: persist decision + target into
	// metadata.json so git-log archaeology can reconstruct the
	// verdict without the coordinator DB. The coordinator also
	// records the decision in tasks.review_decision, but that's
	// mutable (invalidation clears it) — the git commit is the
	// immutable audit record.
	if meta != nil && meta.Action == "review" {
		metadata["action"] = "review"
		metadata["decision"] = decision
		if meta.ReviewsTarget != "" {
			metadata["reviews_target"] = meta.ReviewsTarget
		}
	}
	// Vote-action metadata: mirror the review audit shape so
	// git-log archaeology on vote tasks reveals the winning
	// option plus the declared options list (so an auditor can
	// see what the choices were, not just which one won).
	if meta != nil && meta.Action == "vote" {
		metadata["action"] = "vote"
		metadata["option"] = option
		if meta.VoteOptionsJSON != "" {
			// Embed the parsed options as a structured field so
			// the commit's metadata.json is self-describing —
			// no need to reference the coordinator DB or the
			// original run YAML.
			var parsed interface{}
			if json.Unmarshal([]byte(meta.VoteOptionsJSON), &parsed) == nil {
				metadata["options"] = parsed
			}
		}
	}

	files := []mcpgit.FileWrite{}

	// Single-file result path: `content` is a string blob.
	if content != "" {
		files = append(files, mcpgit.FileWrite{
			RepoRelPath: filepath.Join(resultDir, "result.md"),
			Content:     []byte(content),
		})
	}

	// Phase J.1 — list<string> named outputs are stringified
	// to newline-joined text for on-disk storage so the
	// existing file-per-output path and downstream
	// `{{task.field}}` template resolution keep working
	// unchanged. The structured list value is separately
	// carried to the coordinator via reportBody.output_lists
	// so dynamic for_each materialization doesn't need to
	// re-parse the git file.
	if len(outputLists) > 0 {
		if outputs == nil {
			outputs = make(map[string]string, len(outputLists))
		}
		for name, list := range outputLists {
			outputs[name] = strings.Join(list, "\n")
		}
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
	// For action:review tasks the decision field rides along in the
	// same report; the coordinator validates and (on reject) fires
	// the cascade on the reviewed target.
	reportBody := map[string]interface{}{
		"commit_sha":        submitRes.CommitSHA,
		"result_path":       resultDir,
		"artifacts_written": artifactPaths,
		"tokens_used":       0,
		"model":             c.modelName,
		// Username identifies the submitting citizen for
		// multi-citizen task bookkeeping (so the coordinator
		// credits the right task_claims row). Single-citizen
		// tasks tolerate it but use tasks.claimed_by as the
		// implicit claimer.
		"username": c.username,
		// Content rides along so the coordinator can persist
		// the citizen's prose on task_claims.content for
		// multi-citizen vote/review tasks. The fat-client
		// already wrote this prose to the per-citizen
		// result.md, but the DB column is the authoritative
		// source for {{task.responses}} rendering.
		"content": content,
	}
	if len(outputLists) > 0 {
		// Phase J.1 — carry list<string> named output
		// values through to the coordinator so it can
		// materialize dynamic for_each downstreams from
		// the resolved lists.
		reportBody["output_lists"] = outputLists
	}
	if decision != "" {
		reportBody["decision"] = decision
	}
	if option != "" {
		reportBody["option"] = option
	}
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", reportBody)
	if err != nil {
		return mcp.NewToolResultError("reporting commit: " + err.Error()), nil
	}
	if errMsg := extractErrorString(data); errMsg != "" {
		return mcp.NewToolResultError(decorateCoordinatorRejection(errMsg)), nil
	}
	return mcp.NewToolResultText(formatSubmitResult(data, taskID)), nil
}

// validateReviewDecision returns an empty string when the decision
// is acceptable for a review-action task ("approve" or "reject"),
// or a single-sentence error message otherwise. Centralized so the
// missing/invalid variants share identical phrasing — the bug
// tripped on three different messages being emitted from three
// different places.
func validateReviewDecision(decision string) string {
	switch decision {
	case "approve", "reject":
		return ""
	case "":
		return "decision is required on action:review tasks (must be \"approve\" or \"reject\")"
	default:
		return invalidDecisionMessage(decision)
	}
}

// invalidDecisionMessage renders the shared phrasing for an
// unrecognized decision value — same copy everywhere so users
// don't see three slightly-different wordings from three
// different validation points.
func invalidDecisionMessage(decision string) string {
	return fmt.Sprintf("decision %q is invalid (must be \"approve\" or \"reject\")", decision)
}

// validateVoteOption is the client-side pre-validation guard for
// action:vote submissions. Returns an empty string when the
// option is acceptable, or a single-sentence error message
// otherwise. Runs BEFORE any git write in submitResultFatClient
// so a bad option id can't strand a phantom commit in the
// append-only history.
//
// optionsJSON is the serialized options list from the task's
// vote_options column. An empty JSON (e.g. a storage row that
// somehow lost its declared options) falls through as a
// coordinator-side consistency error rather than a vote-option
// UX error — we don't try to second-guess the DB.
func validateVoteOption(option, optionsJSON string) string {
	if optionsJSON == "" {
		// Don't block the submit client-side if we can't see
		// the declared options; let the coordinator respond
		// with its own consistency error. Better to surface
		// one error from one place than two slightly different
		// ones.
		return ""
	}
	var declared []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(optionsJSON), &declared); err != nil || len(declared) == 0 {
		return ""
	}
	known := make([]string, 0, len(declared))
	for _, o := range declared {
		known = append(known, o.ID)
	}
	if option == "" {
		return fmt.Sprintf(`option is required on action:vote tasks (must be one of: %s)`, strings.Join(known, ", "))
	}
	for _, id := range known {
		if id == option {
			return ""
		}
	}
	return fmt.Sprintf(`option %q is invalid (must be one of: %s)`,
		option, strings.Join(known, ", "))
}

// decorateCoordinatorRejection wraps a raw coordinator error string
// with an actionable hint when the rejection looks like a
// stale-state issue (commit SHA mismatch, unknown commit, state
// transition conflict, etc.). For unrelated rejections it returns
// the original message unchanged.
func decorateCoordinatorRejection(errMsg string) string {
	lower := strings.ToLower(errMsg)
	staleSignals := []string{
		"stale",
		"unknown commit",
		"commit not found",
		"invalid state transition",
		"not in state",
		"already accepted",
		"superseded",
	}
	for _, sig := range staleSignals {
		if strings.Contains(lower, sig) {
			return "coordinator rejected report: " + errMsg +
				" (hint: your local clone may be out of sync — try enju_project_sync and re-claim the task)"
		}
	}
	return "coordinator rejected report: " + errMsg
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
		return mcp.NewToolResultError(err.Error()), nil
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
		return mcp.NewToolResultError(err.Error()), nil
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

	return mcp.NewToolResultText(formatTaskDetail(data, inputs, c.username)), nil
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
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required — create a project first with enju_create_project"), nil
	}

	// Phase H.1: three input shapes —
	//   1. yaml (inline definition, no params)
	//   2. path (template file under templates/, optional params)
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
		return mcp.NewToolResultError("either 'yaml' (inline definition) or 'path' (template under templates/) is required"), nil
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
		remoteURL, err := c.fetchProjectMeta(ctx, int64(projectID))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		proj, err := c.workspace.ForProject(int64(projectID), remoteURL)
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

// handleListTemplates — pure client-side tool. Walks the
// project's templates/ directory in the local clone and
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

	// Open the project workspace.
	proj, err := c.workspace.ForProject(meta.ProjectID, meta.ProjectRemoteURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj.Lock()
	_ = proj.Pull()
	proj.Unlock()

	workDir := proj.WorkDir()
	resultDir := mcpgit.ResultDir(meta.ProjectID, meta.RunSeq, meta.InstanceKey, meta.TaskDefID)

	// Build environment variables.
	env := os.Environ()
	env = append(env,
		"ENJU_TASK_ID="+taskID,
		"ENJU_PROJECT_DIR="+workDir,
		"ENJU_RUN_DIR="+filepath.Join(workDir, resultDir),
	)

	// Resolve the script path.
	scriptPath := filepath.Join(workDir, meta.Script)
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return mcp.NewToolResultError(fmt.Sprintf("script %q not found in workspace", meta.Script)), nil
	}

	// Execute the script.
	startTime := time.Now()
	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Dir = workDir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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

	// Exit non-zero → auto-fail the task via the coordinator.
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

	proj.Lock()
	submitRes, err := proj.SubmitTaskResult(mcpgit.SubmitRequest{
		TaskID:      taskID,
		Username:    c.username,
		AuthorName:  c.citizenName,
		AuthorEmail: c.citizenEmail,
		Files:       files,
	})
	proj.Unlock()
	if err != nil {
		return mcp.NewToolResultError("git submit failed: " + err.Error()), nil
	}

	// Report to coordinator.
	reportBody := map[string]interface{}{
		"commit_sha":  submitRes.CommitSHA,
		"result_path": resultDir,
		"model":       c.modelName,
		"username":    c.username,
		"content":     content,
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
	var remoteURL string
	if c.workspace != nil {
		if meta, err := c.fetchProjectMeta(ctx, int64(projectID)); err == nil {
			remoteURL = meta
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
			if proj, err := c.workspace.ForProject(int64(projectID), remoteURL); err == nil {
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

func (c *apiClient) handleListTemplates(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("enju_list_templates requires a local workspace (MCP client mode)"), nil
	}
	remoteURL, err := c.fetchProjectMeta(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj, err := c.workspace.ForProject(int64(projectID), remoteURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Best-effort pull so templates pushed by other citizens
	// since the local clone was last updated show up in the
	// menu. A failed pull (offline, diverged, auth, etc.) is
	// logged and we scan whatever's currently on disk — the
	// user still gets a menu, and the error surfaces on the
	// next tool call if it's load-bearing.
	proj.Lock()
	if perr := proj.Pull(); perr != nil {
		c.logger.Debug("list_templates pull failed, scanning local state", "err", perr)
	}
	proj.Unlock()
	templates, err := proj.ListTemplates()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(map[string]interface{}{
		"project_id": projectID,
		"templates":  templates,
	})
	return mcp.NewToolResultText(formatListTemplates(data)), nil
}

// handleDescribeTemplate — pure client-side tool. Loads one
// template file from the local clone and returns its full
// metadata + param documentation.
func (c *apiClient) handleDescribeTemplate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	templatePath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required (e.g. 'templates/gwas.yaml')"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("enju_describe_template requires a local workspace (MCP client mode)"), nil
	}
	remoteURL, err := c.fetchProjectMeta(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj, err := c.workspace.ForProject(int64(projectID), remoteURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Best-effort pull: surface templates pushed after the
	// clone was last updated. Same fallback as list_templates
	// — on failure, read whatever's on disk.
	proj.Lock()
	if perr := proj.Pull(); perr != nil {
		c.logger.Debug("describe_template pull failed, reading local state", "err", perr)
	}
	proj.Unlock()
	loaded, err := proj.LoadTemplate(templatePath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(loaded.Summary)
	return mcp.NewToolResultText(formatDescribeTemplate(data)), nil
}

// --- Helpers ---

// updateLocalCredentials merges the caller-provided identity
// fields into ~/.enju/credentials.json via a read-modify-write
// pass that preserves unknown fields. Fields not provided by the
// caller (haveName/haveEmail false) are left untouched, so
// update_profile(name=X) doesn't silently clear a previously-set
// email on disk.
func updateLocalCredentials(haveName bool, name string, haveEmail bool, email string) {
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
	if haveName {
		creds["name"] = name
	}
	if haveEmail {
		creds["email"] = email
	}
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
