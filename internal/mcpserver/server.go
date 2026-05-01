// Package mcpserver implements the Enju MCP server for Claude Desktop/Code integration.
// It's a thin bridge: MCP tool calls → coordinator REST API calls.
package mcpserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/enju-ai/enju/internal/mcpgit"
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

	// Register tools
	s.AddTool(toolListRuns(), client.handleListRuns)
	s.AddTool(toolListReadyTasks(), client.handleListReadyTasks)
	s.AddTool(toolClaimTask(), client.handleClaimTask)
	s.AddTool(toolGetTaskInputs(), client.handleGetTaskInputs)
	s.AddTool(toolSubmitResult(), client.handleSubmitResult)
	s.AddTool(toolSubmitResultsBatch(), client.handleSubmitResultsBatch)
	s.AddTool(toolClaimReadyMatching(), client.handleClaimReadyMatching)
	s.AddTool(toolReleaseTask(), client.handleReleaseTask)
	s.AddTool(toolGetTask(), client.handleGetTask)
	s.AddTool(toolRunStatus(), client.handleRunStatus)
	s.AddTool(toolCreateRun(), client.handleCreateRun)
	s.AddTool(toolPauseRun(), client.handlePauseRun)
	s.AddTool(toolResumeRun(), client.handleResumeRun)
	s.AddTool(toolSpawnTask(), client.handleSpawnTask)
	s.AddTool(toolRequestClarification(), client.handleRequestClarification)
	s.AddTool(toolSetCycleBudget(), client.handleSetCycleBudget)
	s.AddTool(toolShowEvents(), client.handleShowEvents)
	s.AddTool(toolRecentEvents(), client.handleRecentEvents)
	s.AddTool(toolEventsStatus(), client.handleEventsStatus)
	s.AddTool(toolListIterations(), client.handleListIterations)
	s.AddTool(toolFileIssue(), client.handleFileIssue)
	s.AddTool(toolListIssues(), client.handleListIssues)
	s.AddTool(toolGetIssue(), client.handleGetIssue)
	s.AddTool(toolTriageIssue(), client.handleTriageIssue)
	s.AddTool(toolCloseIssue(), client.handleCloseIssue)
	s.AddTool(toolMyDashboard(), client.handleMyDashboard)
	s.AddTool(toolUpdateProfile(), client.handleUpdateProfile)
	s.AddTool(toolListProjects(), client.handleListProjects)
	s.AddTool(toolCreateProject(), client.handleCreateProject)
	s.AddTool(toolInit(), client.handleInit)
	s.AddTool(toolSetProjectRemote(), client.handleSetProjectRemote)
	s.AddTool(toolSetProjectDefaultBranch(), client.handleSetProjectDefaultBranch)
	s.AddTool(toolProjectRemoteStatus(), client.handleProjectRemoteStatus)
	s.AddTool(toolProjectSync(), client.handleProjectSync)
	s.AddTool(toolLeaveProject(), client.handleLeaveProject)
	s.AddTool(toolAddProjectMember(), client.handleAddProjectMember)
	s.AddTool(toolRemoveProjectMember(), client.handleRemoveProjectMember)
	s.AddTool(toolListProjectMembers(), client.handleListProjectMembers)
	s.AddTool(toolPromoteMember(), client.handlePromoteMember)
	s.AddTool(toolDemoteOwner(), client.handleDemoteOwner)
	s.AddTool(toolListArtifacts(), client.handleListArtifacts)
	s.AddTool(toolGetArtifact(), client.handleGetArtifact)
	s.AddTool(toolGetArtifactHistory(), client.handleGetArtifactHistory)
	s.AddTool(toolListUntrackedArtifacts(), client.handleListUntrackedArtifacts)
	s.AddTool(toolMyProfile(), client.handleMyProfile)
	s.AddTool(toolInvalidateTask(), client.handleInvalidateTask)
	s.AddTool(toolTallyTask(), client.handleTallyTask)
	s.AddTool(toolFailTask(), client.handleFailTask)
	s.AddTool(toolExecuteTask(), client.handleExecuteTask)
	s.AddTool(toolExecuteRun(), client.handleExecuteRun)
	s.AddTool(toolExportRun(), client.handleExportRun)
	s.AddTool(toolExportDiagram(), client.handleExportDiagram)
	s.AddTool(toolExportRunEvents(), client.handleExportRunEvents)
	s.AddTool(toolListTemplates(), client.handleListTemplates)
	s.AddTool(toolDescribeTemplate(), client.handleDescribeTemplate)

	// operator/model design — bot + model
	// registration tools. See docs/operator-model-design.md.
	s.AddTool(toolRegisterBot(), client.handleRegisterBot)
	s.AddTool(toolListMyBots(), client.handleListMyBots)
	s.AddTool(toolRevokeToken(), client.handleRevokeToken)
	s.AddTool(toolListModels(), client.handleListModels)
	s.AddTool(toolRegisterModel(), client.handleRegisterModel)

	return s
}



// --- Tool Handlers ---



