package mcphandlers

// TestClient exposes the MCP tool handlers to external test packages
// so integration suites can drive the exact code path a real MCP
// client takes, without spinning up a stdio subprocess. It wraps the
// unexported apiClient so test code can construct one with a real
// coordinator URL, real workspace.Workspace, real credentials — the
// same wiring the production New() constructor uses — and call tool
// handlers directly.
//
// Intended strictly for test harnesses. The production binary never
// uses this type; real MCP hosts invoke handlers through the
// mark3labs/mcp-go server via stdio.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestClient is a thin wrapper around apiClient that exposes each
// MCP tool handler as a public method. External tests call it by
// tool name via Call(), or use one of the typed convenience methods
// below.
type TestClient struct {
	c *apiClient
}

// NewTestClient builds a TestClient from a Config, mirroring the
// wiring inside New(). The only omission vs. New() is that no
// mark3labs/mcp-go server is constructed — tests talk to handler
// methods in-process.
func NewTestClient(cfg Config) *TestClient {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	coordClient := coord.New(coord.Config{
		BaseURL:         cfg.CoordinatorURL,
		Username:        cfg.Username,
		CitizenName:     cfg.CitizenName,
		CitizenEmail:    cfg.CitizenEmail,
		AuthToken:       cfg.AuthToken,
		SaveCredentials: cfg.SaveCredentials,
		Logger:          logger,
	})
	fc := service.New(service.Config{
		Coord:           coordClient,
		WorkspaceRoot:   cfg.WorkspaceRoot,
		ModelName:       cfg.ModelName,
		Logger:          logger,
		ProjectRegistry: cfg.ProjectRegistry,
	})
	c := &apiClient{fc: fc}
	return &TestClient{c: c}
}

// Username returns the citizen username the TestClient was built
// with. Handy for tests that claim + submit as a specific citizen
// and want to spot-check who the submission credits.
func (t *TestClient) Username() string { return t.c.username() }

// Call invokes an MCP tool handler by its registered tool name with
// a map of arguments. Returns the tool-level *mcp.CallToolResult
// exactly as the handler produced it (including IsError=true for
// tool-level errors) and the Go error if the handler signaled one
// — handlers normally return Go error = nil and encode tool errors
// in the CallToolResult.
func (t *TestClient) Call(ctx context.Context, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: toolName, Arguments: args},
	}
	switch toolName {
	case "enju_claim_task":
		return t.c.handleClaimTask(ctx, req)
	case "enju_submit_result":
		return t.c.handleSubmitResult(ctx, req)
	case "enju_submit_results_batch":
		return t.c.handleSubmitResultsBatch(ctx, req)
	case "enju_claim_ready_matching":
		return t.c.handleClaimReadyMatching(ctx, req)
	case "enju_get_task_inputs":
		return t.c.handleGetTaskInputs(ctx, req)
	case "enju_list_ready_tasks":
		return t.c.handleListReadyTasks(ctx, req)
	case "enju_get_task":
		return t.c.handleGetTask(ctx, req)
	case "enju_run_status":
		return t.c.handleRunStatus(ctx, req)
	case "enju_create_run":
		return t.c.handleCreateRun(ctx, req)
	case "enju_pause_run":
		return t.c.handlePauseRun(ctx, req)
	case "enju_resume_run":
		return t.c.handleResumeRun(ctx, req)
	case "enju_terminate_run":
		return t.c.handleTerminateRun(ctx, req)
	case "enju_spawn_task":
		return t.c.handleSpawnTask(ctx, req)
	case "enju_request_clarification":
		return t.c.handleRequestClarification(ctx, req)
	case "enju_set_cycle_budget":
		return t.c.handleSetCycleBudget(ctx, req)
	case "enju_show_events":
		return t.c.handleShowEvents(ctx, req)
	case "enju_recent_events":
		return t.c.handleRecentEvents(ctx, req)
	case "enju_inbox":
		return t.c.handleInbox(ctx, req)
	case "enju_review":
		return t.c.handleReview(ctx, req)
	case "enju_list_iterations":
		return t.c.handleListIterations(ctx, req)
	case "enju_file_issue":
		return t.c.handleFileIssue(ctx, req)
	case "enju_list_issues":
		return t.c.handleListIssues(ctx, req)
	case "enju_get_issue":
		return t.c.handleGetIssue(ctx, req)
	case "enju_triage_issue":
		return t.c.handleTriageIssue(ctx, req)
	case "enju_close_issue":
		return t.c.handleCloseIssue(ctx, req)
	case "enju_list_runs":
		return t.c.handleListRuns(ctx, req)
	case "enju_create_project":
		return t.c.handleCreateProject(ctx, req)
	case "enju_release_task":
		return t.c.handleReleaseTask(ctx, req)
	case "enju_invalidate_task":
		return t.c.handleInvalidateTask(ctx, req)
	case "enju_tally_task":
		return t.c.handleTallyTask(ctx, req)
	case "enju_fail_task":
		return t.c.handleFailTask(ctx, req)
	case "enju_my_profile":
		return t.c.handleMyProfile(ctx, req)
	case "enju_my_dashboard":
		return t.c.handleMyDashboard(ctx, req)
	case "enju_list_projects":
		return t.c.handleListProjects(ctx, req)
	case "enju_update_profile":
		return t.c.handleUpdateProfile(ctx, req)
	case "enju_project_remote_status":
		return t.c.handleProjectRemoteStatus(ctx, req)
	case "enju_project_sync":
		return t.c.handleProjectSync(ctx, req)
	case "enju_set_project_remote":
		return t.c.handleSetProjectRemote(ctx, req)
	case "enju_set_project_default_branch":
		return t.c.handleSetProjectDefaultBranch(ctx, req)
	case "enju_leave_project":
		return t.c.handleLeaveProject(ctx, req)
	case "enju_add_project_member":
		return t.c.handleAddProjectMember(ctx, req)
	case "enju_remove_project_member":
		return t.c.handleRemoveProjectMember(ctx, req)
	case "enju_list_project_members":
		return t.c.handleListProjectMembers(ctx, req)
	case "enju_promote_member":
		return t.c.handlePromoteMember(ctx, req)
	case "enju_demote_owner":
		return t.c.handleDemoteOwner(ctx, req)
	case "enju_list_artifacts":
		return t.c.handleListArtifacts(ctx, req)
	case "enju_get_artifact":
		return t.c.handleGetArtifact(ctx, req)
	case "enju_get_artifact_history":
		return t.c.handleGetArtifactHistory(ctx, req)
	case "enju_list_untracked_artifacts":
		return t.c.handleListUntrackedArtifacts(ctx, req)
	case "enju_export_run":
		return t.c.handleExportRun(ctx, req)
	case "enju_export_diagram":
		return t.c.handleExportDiagram(ctx, req)
	case "enju_export_run_events":
		return t.c.handleExportRunEvents(ctx, req)
	case "enju_list_templates":
		return t.c.handleListTemplates(ctx, req)
	case "enju_describe_template":
		return t.c.handleDescribeTemplate(ctx, req)
	case "enju_execute_task":
		return t.c.handleExecuteTask(ctx, req)
	case "enju_execute_run":
		return t.c.handleExecuteRun(ctx, req)
	// operator/model design — bot + model registration tools.
	case "enju_register_bot":
		return t.c.handleRegisterBot(ctx, req)
	case "enju_list_my_bots":
		return t.c.handleListMyBots(ctx, req)
	case "enju_revoke_token":
		return t.c.handleRevokeToken(ctx, req)
	case "enju_list_models":
		return t.c.handleListModels(ctx, req)
	case "enju_register_model":
		return t.c.handleRegisterModel(ctx, req)
	case "enju_bot_start":
		return t.c.handleBotStart(ctx, req)
	case "enju_bot_stop":
		return t.c.handleBotStop(ctx, req)
	case "enju_bot_status":
		return t.c.handleBotStatus(ctx, req)
	case "enju_bot_logs":
		return t.c.handleBotLogs(ctx, req)
	case "enju_bot_start_all":
		return t.c.handleBotStartAll(ctx, req)
	case "enju_bot_stop_all":
		return t.c.handleBotStopAll(ctx, req)
	default:
		return nil, fmt.Errorf("mcpserver.TestClient: unknown tool %q", toolName)
	}
}
