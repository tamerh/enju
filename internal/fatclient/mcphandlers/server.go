// Package mcphandlers provides the fat-client MCP handlers and
// composes them into a runnable MCP server using the dispatcher
// at internal/enjumcp/.
//
// All MCP tools are handled here. The thin ones HTTP-forward to
// the coordinator's REST API; the heavy ones (workspace git
// operations, claim/submit, batch tools) own their own logic
// against the local clone. The coordinator never embeds an MCP
// server today — if hosted-mode adds one, it'll be a thin
// transport over service.* in internal/coordinator/service/.
package mcphandlers

import (
	"context"
	"log/slog"

	"github.com/enju-ai/enju/internal/enjumcp"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/server"
)

// Config holds the MCP server configuration.
type Config struct {
	CoordinatorURL string
	Username       string // citizen's username (stable handle)
	CitizenName    string // display name, for greetings
	CitizenEmail   string // email used when re-registering after a DB wipe, optional
	// WorkspaceRoot is the fat-client's host-side housekeeping
	// directory (logs, scratch, reconcile-cursor .state/). Post-
	// NDW.5 it is NOT where project clones live — those live at
	// registry-resolved paths the operator chose via
	// enju_create_project path=<abs/dir>. Empty disables on-disk
	// workspace flows entirely (test fixtures with coord-only
	// setup).
	WorkspaceRoot string
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
	// enju_create_project call, persists the active
	// project to disk, and switches when the user moves to a
	// different project. Nil = no notification subsystem.
	//
	// cmdMCP builds this struct so the supervisor knows the
	// coordinator URL, citizen identity, config paths, and
	// shutdown context. mcpserver consumes it through Switch().
	Notify *NotifyOptions

	// AllowTools, when non-empty, restricts the MCP server's
	// tool surface to exactly the named tools. The dispatcher
	// only registers tools whose names appear in the list, so
	// the LLM never sees the others — they don't exist as far
	// as the MCP host is concerned. Empty (nil or zero-length)
	// means "all tools," matching the previous default.
	//
	// Used by the bot runner to pin a per-bot tool allowlist
	// at process boundary: a reviewer bot started with
	// AllowTools=[Read, Grep, Glob] cannot call Edit/Write
	// because those tools are physically absent from the
	// toolbox the LLM sees. This is the "runner pins" leg of
	// the manifest+runner+audit-log trust model — see
	// docs/bots.md.
	//
	// Names that don't match any tool in enjumcp.All() are
	// silently dropped. We could panic for a clearer signal,
	// but that would couple bot-manifest authoring tightly to
	// the current tool catalog (a tool rename would break
	// every bot manifest). Drop-and-warn is the more humane
	// trade.
	AllowTools []string

	// ProjectRegistry, when non-nil, overrides the default
	// `projectreg.Open(projectreg.DefaultPath())` (i.e.
	// `~/.enju/projects.json`). Used by tests to point the MCP
	// server at a per-test registry file that the test harness
	// also reads, so direct test workspace ops + MCP-driven ops
	// share the same project→path mapping. Nil keeps the
	// production default.
	ProjectRegistry *projectreg.Registry
}

// NotifyOptions carries the boot-time wiring for the auto-
// subscribe notification session. Exported (vs the internal
// notifySessionConfig) so cmdMCP can populate it without
// importing private types.
//
// All notify state is now project-scoped (lives under
// {project_clone}/.enju/events/ and enju/notify.yaml), so the
// only boot-time inputs are the parent shutdown context and the
// project clone resolver (Workspace). The previous
// active-project file is gone — sessions stay dormant until a
// tool call (create_project / init) calls Switch.
type NotifyOptions struct {
	ParentCtx context.Context

	// SlogRepoint, when non-nil, is invoked with the active
	// project's local clone dir each time notifySession.Switch
	// resolves one. cmdMCP supplies a closure that re-points the
	// MCP process slog to <projectDir>/.enju/logs/ so it sits
	// beside the project's oplog ledger (see cmd/enju/slogsink.go).
	// nil when the slog sink is pinned (ENJU_MCP_LOG) or in
	// callers that don't manage a switchable slog.
	SlogRepoint func(projectDir string)
}

// agentInstructions is the long agent-facing prompt passed to
// the dispatcher at construction. Lives here (not in the
// dispatcher) because it's product / UX content tied to the
// fat-client's tool surface, not generic dispatcher config.
const agentInstructions = `Enju is a human-AI collaborative task orchestration system. Work is structured as DAGs of tasks within runs within projects.

Core model:
- A claimed task is YOUR project. Iterate with the human freely — discuss, draft, refine. Only the final submission is committed to git. Internal back-and-forth doesn't need tracking.
- Reviews are quality gates — you're deciding whether an upstream result is ready to feed into downstream tasks, not doing a line-by-line code review. Decisions: approve (ship it), request_changes (revise, target → READY), reject (fail cascade, target → FAILED terminal), or comment (non-blocking).
- Every submission produces a git commit. The human is the author; you are credited via Co-Authored-By trailer. This is collaborative work, not autonomous — the human is accountable.
- Tasks flow through a DAG: upstream results are automatically injected into downstream prompts via {{task.content}} references.
- Workflows are recipes committed in the project repo. enju_create_run(path=workflows/foo/enju.yaml) — or any YAML path; root-level (path=enju.yaml) works too — forks a run branch from the base SHA and materializes the snapshot at .enju/runs/{seq}/snapshot/. The run is pinned to that base SHA for reproducibility; later edits to the live workflow YAML don't affect in-flight runs. Compute scripts resolve relative to the workflow YAML's directory inside the snapshot.
- Compute scripts get both env vars (ENJU_TASK_ID, ENJU_PROJECT_DIR, ENJU_RUN_DIR, ENJU_TEMPLATE_DIR, ENJU_PARAM_<name> for each param + iteration var) and a structured $ENJU_RUN_DIR/context.json with typed params/iteration/artifact declarations. Use env vars for shell, context.json for anything richer.

Starting: enju_create_project takes path=<absolute folder> and smart-detects what to do: empty/nonexistent → init + seed README; populated, no git → init + commit existing files; existing repo → adopt as-is. No managed bare gets created (Phase 8 dropped it; the operator's own .git/ is the single store). Pass path explicitly (your cwd may be a different project than the one being adopted — very common when running inside one repo while creating an Enju project for another). When unclear, ask.

Workflow: list ready tasks → claim one → read the prompt and upstream context → do the work with the human → submit when ready → check run status to see what unlocked → next task.

Catching up: "what's on my plate?" → enju_inbox (action queue — tasks waiting on you). "what's been happening?" or "any updates for me?" → enju_recent_events with for_me=true (descriptive history of events about you). For incremental "what's new since last check?" remember the highest event seq from your previous response and pass it as since_seq next time — there is no implicit read/unread cursor.

Conventions: After claiming, remind the human which task is active. After submitting, show the updated run status so progress is visible. When working on a task, keep the human oriented — a brief context line (e.g. "Working on 1:1:draft") at the start of task-related responses helps.

Status icons: ✅ completed · 🔵 in progress · 🟡 available (claim it) · ⚪ waiting · 🔴 failed · ⚫ skipped · ⊘ skipped (upstream failed) · ⏸ parked (awaiting reconciliation).`

// New constructs the fat-client MCP server: builds the apiClient,
// wires the notify session, and hands the per-tool handler map
// to the dispatcher (internal/mcp). Returns the underlying
// *server.MCPServer for the caller to drive (stdio today, SSE
// future).
//
// New is the convenience entry point for callers (today: cmd/enju)
// that just want a ready-to-serve fat-client MCP. Callers that
// need to compose handlers with another source (e.g. cmd/enju
// merging fat-client + coord handlers in a single process) call
// Register directly and hand the merged map to enjumcp.New.
func New(cfg Config) *server.MCPServer {
	handlers := map[string]enjumcp.Handler{}
	Register(handlers, cfg)
	srv := enjumcp.New(enjumcp.Config{
		Name:         "enju",
		Version:      "0.1.0",
		Instructions: agentInstructions,
		Handlers:     handlers,
	})
	return srv.MCPServer()
}

// Register populates the dispatcher handler map with every
// fat-client-side tool's handler. The fat-client owns ALL tools
// (heavy natively, thin as HTTP-forwarders to the coordinator's
// REST API), so this adds an entry for every tool in
// enjumcp.All().
//
// Panics if a tool in enjumcp.All() doesn't have a handler in
// handlerByToolName — the same loud-startup behavior the
// dispatcher provides for unknown names, but on the
// missing-handler axis.
//
// Side effects: constructs the apiClient (the long-lived
// handle the handlers close over) and, if cfg.Notify is non-nil,
// the notify session. Both live for the lifetime of the
// returned handlers — they are NOT process-globals.
func Register(handlers map[string]enjumcp.Handler, cfg Config) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	coordClient := coord.New(coord.Config{
		BaseURL:        cfg.CoordinatorURL,
		Username:        cfg.Username,
		CitizenName:      cfg.CitizenName,
		CitizenEmail:     cfg.CitizenEmail,
		AuthToken:       cfg.AuthToken,
		SaveCredentials: cfg.SaveCredentials,
		Logger:         logger,
	})
	reg := cfg.ProjectRegistry
	if reg == nil {
		reg = projectreg.Open(projectreg.DefaultPath())
	}
	fc := service.New(service.Config{
		Coord:           coordClient,
		WorkspaceRoot:   cfg.WorkspaceRoot,
		ModelName:       cfg.ModelName,
		Logger:          logger,
		LogName:         "operator",
		ProjectRegistry: reg,
	})
	client := &apiClient{fc: fc}

	// Notify session — exists when caller opts in via
	// Config.Notify. Nil session means tool-handler Switch
	// calls are no-ops, matching the legacy behavior where
	// nothing happens unless the user touches a project.
	//
	// The notify session stays dormant until create_project or
	// init fires Switch. There is no cross-MCP-restart resume —
	// each session activates on first project touch and dies on
	// process exit. Multi-session is naturally supported: each
	// MCP process has its own notifySession with its own
	// in-memory project pointer, no shared state.
	if cfg.Notify != nil {
		client.notifySess = newNotifySession(notifySessionConfig{
			CoordinatorURL: cfg.CoordinatorURL,
			TokenFn:        client.Token, // live read — picks up auto-reregister rotations
			Username:       cfg.Username,
			Workspace:      fc.Enjugit(),
			ParentCtx:      cfg.Notify.ParentCtx,
			SlogRepoint:    cfg.Notify.SlogRepoint,
			Logger:         logger,
		})
		// Background opportunistic reconcile (the "ticker"). Tick
		// every 20s for the project covering CWD. Scoped to
		// Notify.ParentCtx so it dies on process exit.
		go client.runReconcileTicker(cfg.Notify.ParentCtx)
	}

	// Build the allowlist set once outside the loop. Empty
	// AllowTools means "no filter" — register every tool.
	var allow map[string]struct{}
	if len(cfg.AllowTools) > 0 {
		allow = make(map[string]struct{}, len(cfg.AllowTools))
		for _, name := range cfg.AllowTools {
			allow[name] = struct{}{}
		}
	}

	for _, t := range enjumcp.All() {
		if allow != nil {
			if _, ok := allow[t.Name]; !ok {
				// Tool not in the allowlist — skip
				// registration. The dispatcher won't see
				// it; the LLM won't see it.
				continue
			}
		}
		h, ok := client.handlerByToolName(t.Name)
		if !ok {
			panic("mcphandlers: no handler registered for tool: " + t.Name)
		}
		handlers[t.Name] = h
	}
}

// handlerByToolName returns the apiClient method that handles the
// given MCP tool. Centralized here so the registry-driven loop in
// New() can validate that every tool in enjumcp.All() has a
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
	case "enju_list_my_agents":
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
	case "enju_terminate_run":
		return c.handleTerminateRun, true
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
	case "enju_retry_task":
		return c.handleRetryTask, true
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

	// Inbox
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
	case "enju_register_agent":
		return c.handleRegisterBot, true
	case "enju_revoke_token":
		return c.handleRevokeToken, true
	case "enju_register_model":
		return c.handleRegisterModel, true
	case "enju_agent_start":
		return c.handleBotStart, true
	case "enju_agent_stop":
		return c.handleBotStop, true
	case "enju_agent_status":
		return c.handleBotStatus, true
	case "enju_agent_logs":
		return c.handleBotLogs, true
	case "enju_agent_start_all":
		return c.handleBotStartAll, true
	case "enju_agent_stop_all":
		return c.handleBotStopAll, true
	}
	return nil, false
}
