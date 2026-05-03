// Package mcpserver implements the Enju MCP server for Claude Desktop/Code integration.
// It's a thin bridge: MCP tool calls → coordinator REST API calls.
package mcpserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/enju-ai/enju/internal/core/mcptools"
	"github.com/enju-ai/enju/internal/fatclient/mcpgit"
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
	SaveCredentials func(username, name, email, token string)
	// ModelName is the LLM model used by this citizen, for
	// contribution tracking (e.g. "claude-opus-4", "gpt-4o").
	ModelName string
	// AuthToken is the citizen's registration token. Sent
	// with every write request so the coordinator can verify
	// the citizen's identity. Prevents impersonation after
	// registration.
	AuthToken string
	// Logger is used for client-side diagnostic output. If nil,
	// a slog.Default() is used.
	Logger *slog.Logger

	// Notify, when set, enables the auto-subscribe-on-touch
	// notification supervisor. The MCP server activates a notify
	// poller for the project named in any successful
	// enju_create_project / enju_init call, persists the active
	// project to disk, and switches when the user moves to a
	// different project. Nil = no notification subsystem.
	//
	// cmdMCP builds this struct so the supervisor knows the
	// coordinator URL, citizen identity, config paths, and
	// shutdown context. mcpserver consumes it through Switch().
	Notify *NotifyOptions
}

// NotifyOptions carries the boot-time wiring for the auto-
// subscribe notification session. Exported (vs the internal
// notifySessionConfig) so cmdMCP can populate it without
// importing private types.
//
// All notify state is now project-scoped (lives under
// {project_clone}/enju/events/ and enju/notify.yaml), so the
// only boot-time inputs are the parent shutdown context and the
// project clone resolver (Workspace). The previous
// active-project file is gone — sessions stay dormant until a
// tool call (create_project / init) calls Switch.
type NotifyOptions struct {
	ParentCtx context.Context
}

// New creates and configures the MCP server with all Enju tools.
func New(cfg Config) *server.MCPServer {
	s := server.NewMCPServer(
		"enju",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithInstructions(`Enju is a human-AI collaborative task orchestration system. Work is structured as DAGs of tasks within runs within projects.

Core model:
- A claimed task is YOUR workspace. Iterate with the human freely — discuss, draft, refine. Only the final submission is committed to git. Internal back-and-forth doesn't need tracking.
- Reviews are quality gates — you're deciding whether an upstream result is ready to feed into downstream tasks, not doing a line-by-line code review. Decisions: approve (ship it), request_changes (revise, target → READY), reject (fail cascade, target → FAILED terminal), or comment (non-blocking).
- Every submission produces a git commit. The human is the author; you are credited via Co-Authored-By trailer. This is collaborative work, not autonomous — the human is accountable.
- Tasks flow through a DAG: upstream results are automatically injected into downstream prompts via {{task.content}} references.
- Templates are reproducible bundles. enju_create_run(path=enju/templates/foo) snapshots the full bundle (enju.yaml + scripts + data) into enju/runs/{seq}/template-snapshot/ at creation. The run is pinned to that frozen copy — later live-template edits don't affect in-flight runs. Compute scripts resolve from the snapshot.
- Compute scripts get both env vars (ENJU_TASK_ID, ENJU_PROJECT_DIR, ENJU_RUN_DIR, ENJU_TEMPLATE_DIR, ENJU_PARAM_<name> for each param + iteration var) and a structured $ENJU_RUN_DIR/context.json with typed params/iteration/artifact declarations. Use env vars for shell, context.json for anything richer.

Starting: If the user wants to start fresh, use enju_create_project — workspace lands at ~/.enju/workspaces/<slug>-<id>/, no risk of adopting your cwd. If they have an existing folder or repo, use enju_init and pass path= explicitly (your cwd may be a different project than the one being adopted — very common when running inside one repo while creating an Enju project for another). When unclear, ask.

Workflow: list ready tasks → claim one → read the prompt and upstream context → do the work with the human → submit when ready → check run status to see what unlocked → next task.

Conventions: After claiming, remind the human which task is active. After submitting, show the updated run status so progress is visible. When working on a task, keep the human oriented — a brief context line (e.g. "Working on 1:1:draft") at the start of task-related responses helps.

Status icons: ✅ completed · 🔵 in progress · 🟡 available (claim it) · ⚪ waiting · 🔴 failed · ⚫ skipped · ⊘ skipped (upstream failed) · ⏸ parked (awaiting reconciliation).`),
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
	client.setToken(cfg.AuthToken)

	// Notify session — exists when caller opts in via
	// Config.Notify. Nil session means tool-handler Switch
	// calls are no-ops, matching the legacy behavior where
	// nothing happens unless the user touches a project.
	//
	// Session stays dormant until create_project or init fires
	// Switch. There is no cross-MCP-restart resume — each
	// session activates on first project touch and dies on
	// process exit. Multi-session is naturally supported: each
	// MCP process has its own notifySession with its own
	// in-memory project pointer, no shared state.
	if cfg.Notify != nil {
		client.notifySess = newNotifySession(notifySessionConfig{
			CoordinatorURL: cfg.CoordinatorURL,
			TokenFn:        client.Token, // live read — picks up auto-reregister rotations
			Username:       cfg.Username,
			Workspace:      cfg.Workspace,
			ParentCtx:      cfg.Notify.ParentCtx,
			Logger:         logger,
		})
	}

	// Drive registration off the registry. The handler map is
	// the load-bearing contract: every entry in mcptools.All()
	// must have a handler here, or startup panics. Adding a tool
	// to the registry without a handler in this map fails fast
	// at process start, not silently at first call.
	//
	// Until coord-side native handlers exist (Phase 3.4), the
	// fat-client registers handlers for every tool — heavy ones
	// natively, thin ones as HTTP-forwarders living in the same
	// per-tool handler files. After 3.4/3.5 land, this map will
	// shrink to fat-client-side tools only and the thin ones
	// will be registered via a generic forwarder.
	for _, t := range mcptools.All() {
		h, ok := client.handlerByToolName(t.Tool.Name)
		if !ok {
			panic("mcpserver: no handler registered for tool: " + t.Tool.Name)
		}
		s.AddTool(t.Tool, h)
	}

	return s
}

// handlerByToolName returns the apiClient method that handles the
// given MCP tool. Centralized here so the registry-driven loop in
// New() can validate that every tool in mcptools.All() has a
// handler at startup. Adding a tool to the registry = add an entry
// here; the build won't fail but startup will panic, surfacing the
// gap loudly.
func (c *apiClient) handlerByToolName(name string) (server.ToolHandlerFunc, bool) {
	switch name {
	// Read-side / listings
	case "enju_list_runs":
		return c.handleListRuns, true
	case "enju_list_ready_tasks":
		return c.handleListReadyTasks, true
	case "enju_list_iterations":
		return c.handleListIterations, true
	case "enju_list_projects":
		return c.handleListProjects, true
	case "enju_list_project_members":
		return c.handleListProjectMembers, true
	case "enju_list_artifacts":
		return c.handleListArtifacts, true
	case "enju_list_my_bots":
		return c.handleListMyBots, true
	case "enju_list_models":
		return c.handleListModels, true
	case "enju_list_issues":
		return c.handleListIssues, true
	case "enju_recent_events":
		return c.handleRecentEvents, true
	case "enju_show_events":
		return c.handleShowEvents, true
	case "enju_run_status":
		return c.handleRunStatus, true
	case "enju_events_status":
		return c.handleEventsStatus, true
	case "enju_my_dashboard":
		return c.handleMyDashboard, true
	case "enju_my_profile":
		return c.handleMyProfile, true
	case "enju_get_issue":
		return c.handleGetIssue, true
	case "enju_get_task":
		return c.handleGetTask, true
	case "enju_get_artifact":
		return c.handleGetArtifact, true
	case "enju_get_artifact_history":
		return c.handleGetArtifactHistory, true
	case "enju_list_untracked_artifacts":
		return c.handleListUntrackedArtifacts, true
	case "enju_list_templates":
		return c.handleListTemplates, true
	case "enju_describe_template":
		return c.handleDescribeTemplate, true

	// Run admin
	case "enju_pause_run":
		return c.handlePauseRun, true
	case "enju_resume_run":
		return c.handleResumeRun, true
	case "enju_set_cycle_budget":
		return c.handleSetCycleBudget, true
	case "enju_spawn_task":
		return c.handleSpawnTask, true
	case "enju_request_clarification":
		return c.handleRequestClarification, true

	// Task lifecycle
	case "enju_claim_task":
		return c.handleClaimTask, true
	case "enju_claim_ready_matching":
		return c.handleClaimReadyMatching, true
	case "enju_get_task_inputs":
		return c.handleGetTaskInputs, true
	case "enju_release_task":
		return c.handleReleaseTask, true
	case "enju_submit_result":
		return c.handleSubmitResult, true
	case "enju_submit_results_batch":
		return c.handleSubmitResultsBatch, true
	case "enju_review":
		return c.handleReview, true
	case "enju_invalidate_task":
		return c.handleInvalidateTask, true
	case "enju_tally_task":
		return c.handleTallyTask, true
	case "enju_fail_task":
		return c.handleFailTask, true
	case "enju_execute_task":
		return c.handleExecuteTask, true
	case "enju_execute_run":
		return c.handleExecuteRun, true

	// Project lifecycle
	case "enju_create_project":
		return c.handleCreateProject, true
	case "enju_create_run":
		return c.handleCreateRun, true
	case "enju_init":
		return c.handleInit, true
	case "enju_set_project_remote":
		return c.handleSetProjectRemote, true
	case "enju_set_project_default_branch":
		return c.handleSetProjectDefaultBranch, true
	case "enju_project_remote_status":
		return c.handleProjectRemoteStatus, true
	case "enju_project_sync":
		return c.handleProjectSync, true
	case "enju_leave_project":
		return c.handleLeaveProject, true
	case "enju_add_project_member":
		return c.handleAddProjectMember, true
	case "enju_remove_project_member":
		return c.handleRemoveProjectMember, true
	case "enju_promote_member":
		return c.handlePromoteMember, true
	case "enju_demote_owner":
		return c.handleDemoteOwner, true
	case "enju_update_profile":
		return c.handleUpdateProfile, true

	// Notifications + inbox
	case "enju_notifications":
		return c.handleNotifications, true
	case "enju_inbox":
		return c.handleInbox, true

	// Issues
	case "enju_file_issue":
		return c.handleFileIssue, true
	case "enju_triage_issue":
		return c.handleTriageIssue, true
	case "enju_close_issue":
		return c.handleCloseIssue, true

	// Exports
	case "enju_export_run":
		return c.handleExportRun, true
	case "enju_export_diagram":
		return c.handleExportDiagram, true
	case "enju_export_run_events":
		return c.handleExportRunEvents, true

	// Bot + model registration
	case "enju_register_bot":
		return c.handleRegisterBot, true
	case "enju_revoke_token":
		return c.handleRevokeToken, true
	case "enju_register_model":
		return c.handleRegisterModel, true
	}
	return nil, false
}
