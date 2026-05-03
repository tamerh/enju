package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// allToolFactories is the canonical list of every MCP tool Enju
// exposes. Keep this in sync with the AddTool calls in server.go's
// New(). The test below treats it as a contract snapshot — if a
// tool gets renamed, removed, or loses its description, this table
// tells you about it.
var allToolFactories = []struct {
	name    string
	factory func() mcp.Tool
}{
	{"enju_list_runs", toolListRuns},
	{"enju_list_ready_tasks", toolListReadyTasks},
	{"enju_claim_task", toolClaimTask},
	{"enju_get_task_inputs", toolGetTaskInputs},
	{"enju_submit_result", toolSubmitResult},
	{"enju_submit_results_batch", toolSubmitResultsBatch},
	{"enju_claim_ready_matching", toolClaimReadyMatching},
	{"enju_list_artifacts", toolListArtifacts},
	{"enju_get_artifact", toolGetArtifact},
	{"enju_get_artifact_history", toolGetArtifactHistory},
	{"enju_list_untracked_artifacts", toolListUntrackedArtifacts},
	{"enju_release_task", toolReleaseTask},
	{"enju_get_task", toolGetTask},
	{"enju_run_status", toolRunStatus},
	{"enju_create_run", toolCreateRun},
	{"enju_fail_task", toolFailTask},
	{"enju_execute_task", toolExecuteTask},
	{"enju_execute_run", toolExecuteRun},
	{"enju_pause_run", toolPauseRun},
	{"enju_resume_run", toolResumeRun},
	{"enju_spawn_task", toolSpawnTask},
	{"enju_request_clarification", toolRequestClarification},
	{"enju_set_cycle_budget", toolSetCycleBudget},
	{"enju_show_events", toolShowEvents},
	{"enju_recent_events", toolRecentEvents},
	{"enju_notifications", toolNotifications},
	{"enju_inbox", toolInbox},
	{"enju_review", toolReview},
	{"enju_events_status", toolEventsStatus},
	{"enju_list_iterations", toolListIterations},
	{"enju_file_issue", toolFileIssue},
	{"enju_list_issues", toolListIssues},
	{"enju_get_issue", toolGetIssue},
	{"enju_triage_issue", toolTriageIssue},
	{"enju_close_issue", toolCloseIssue},
	{"enju_export_run_events", toolExportRunEvents},
	{"enju_export_diagram", toolExportDiagram},
	{"enju_export_run", toolExportRun},
	{"enju_list_templates", toolListTemplates},
	{"enju_describe_template", toolDescribeTemplate},
	{"enju_list_projects", toolListProjects},
	{"enju_create_project", toolCreateProject},
	{"enju_init", toolInit},
	{"enju_set_project_default_branch", toolSetProjectDefaultBranch},
	{"enju_set_project_remote", toolSetProjectRemote},
	{"enju_project_remote_status", toolProjectRemoteStatus},
	{"enju_project_sync", toolProjectSync},
	{"enju_leave_project", toolLeaveProject},
	{"enju_add_project_member", toolAddProjectMember},
	{"enju_remove_project_member", toolRemoveProjectMember},
	{"enju_list_project_members", toolListProjectMembers},
	{"enju_promote_member", toolPromoteMember},
	{"enju_demote_owner", toolDemoteOwner},
	{"enju_update_profile", toolUpdateProfile},
	{"enju_my_dashboard", toolMyDashboard},
	{"enju_my_profile", toolMyProfile},
	{"enju_invalidate_task", toolInvalidateTask},
	{"enju_tally_task", toolTallyTask},
	// operator/model design — bot + model registration tools.
	{"enju_register_bot", toolRegisterBot},
	{"enju_list_my_bots", toolListMyBots},
	{"enju_revoke_token", toolRevokeToken},
	{"enju_list_models", toolListModels},
	{"enju_register_model", toolRegisterModel},
}

// TestAllToolsValidShape invokes every tool-schema factory and
// enforces the shared contract:
//   - name matches the expected enju_* identifier
//   - description is non-empty (shown to the LLM — must exist)
//   - every declared required arg also appears in Properties
//   - name is unique across the whole tool surface
func TestAllToolsValidShape(t *testing.T) {
	seen := make(map[string]bool, len(allToolFactories))
	for _, tf := range allToolFactories {
		t.Run(tf.name, func(t *testing.T) {
			tool := tf.factory()
			if tool.Name != tf.name {
				t.Errorf("factory returned name %q, expected %q", tool.Name, tf.name)
			}
			if !strings.HasPrefix(tool.Name, "enju_") {
				t.Errorf("tool name %q must start with 'enju_'", tool.Name)
			}
			if strings.TrimSpace(tool.Description) == "" {
				t.Errorf("tool %q has empty description", tool.Name)
			}
			// Every name in Required must appear in Properties.
			// mcp-go's builder can technically decouple them, so
			// assert the invariant directly.
			for _, req := range tool.InputSchema.Required {
				if _, ok := tool.InputSchema.Properties[req]; !ok {
					t.Errorf("tool %q declares required arg %q but no matching property", tool.Name, req)
				}
			}
			if seen[tool.Name] {
				t.Errorf("duplicate tool name %q", tool.Name)
			}
			seen[tool.Name] = true
		})
	}
}

// TestToolsCatalogueMatchesRegistry ensures the test table above
// covers every factory in tools.go — counted against the number of
// tools NewServer() actually registers. A drift here means a new
// factory was added without updating either the registration in
// server.go or this table.
func TestToolsCatalogueMatchesRegistry(t *testing.T) {
	s := New(Config{CoordinatorURL: "http://unused"})
	registered := s.ListTools()
	if len(registered) != len(allToolFactories) {
		t.Fatalf("registered tool count (%d) != test catalogue (%d); update allToolFactories when tools are added/removed",
			len(registered), len(allToolFactories))
	}
	for _, tf := range allToolFactories {
		if _, ok := registered[tf.name]; !ok {
			t.Errorf("tool %q listed in test catalogue but not registered in NewServer()", tf.name)
		}
	}
}

// TestKeySchemasHaveRequiredArgs spot-checks the most load-bearing
// tools — the contract the LLM depends on. Broken required lists
// here would cause tool calls to succeed at the protocol layer but
// fail inside the handler with a confusing error.
func TestKeySchemasHaveRequiredArgs(t *testing.T) {
	cases := []struct {
		toolName string
		factory  func() mcp.Tool
		required []string
	}{
		{"enju_claim_task", toolClaimTask, []string{"task_id"}},
		{"enju_submit_result", toolSubmitResult, []string{"task_id"}},
		{"enju_submit_results_batch", toolSubmitResultsBatch, []string{"submissions"}},
		{"enju_claim_ready_matching", toolClaimReadyMatching, []string{"project_id", "run_id"}},
		{"enju_get_task", toolGetTask, []string{"task_id"}},
		{"enju_get_task_inputs", toolGetTaskInputs, []string{"task_id"}},
		{"enju_release_task", toolReleaseTask, []string{"task_id"}},
		{"enju_fail_task", toolFailTask, []string{"task_id"}},
		{"enju_invalidate_task", toolInvalidateTask, []string{"task_id"}},
		{"enju_execute_task", toolExecuteTask, []string{"task_id"}},
		{"enju_execute_run", toolExecuteRun, []string{"project_id", "run_id"}},
		{"enju_describe_template", toolDescribeTemplate, []string{"project_id", "path"}},
		{"enju_list_templates", toolListTemplates, []string{"project_id"}},
		{"enju_create_run", toolCreateRun, []string{"project_id"}},
	}
	for _, tc := range cases {
		t.Run(tc.toolName, func(t *testing.T) {
			tool := tc.factory()
			got := make(map[string]bool, len(tool.InputSchema.Required))
			for _, r := range tool.InputSchema.Required {
				got[r] = true
			}
			for _, want := range tc.required {
				if !got[want] {
					t.Errorf("tool %q missing required arg %q (got required=%v)",
						tc.toolName, want, tool.InputSchema.Required)
				}
			}
		})
	}
}

// TestToolsMarshalJSON verifies every factory produces a schema
// that actually serializes. mcp-go builds schemas through option
// funcs that can misconfigure silently; catching it at JSON time
// is cheaper than catching it over the wire.
func TestToolsMarshalJSON(t *testing.T) {
	for _, tf := range allToolFactories {
		t.Run(tf.name, func(t *testing.T) {
			tool := tf.factory()
			raw, err := json.Marshal(tool)
			if err != nil {
				t.Fatalf("marshal failed for %q: %v", tool.Name, err)
			}
			if !strings.Contains(string(raw), tf.name) {
				t.Errorf("serialized payload for %q doesn't contain the tool name", tf.name)
			}
		})
	}
}
