package mcpserver

import (
	"github.com/enju-ai/enju/internal/core/mcptools"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestEveryRegisteredToolHasHandler pins the registry-handler
// contract: every entry in mcptools.All() must have a handler
// returned by handlerByToolName. The runtime panic in New()
// would also catch this, but a test catches it pre-deploy with
// a clearer error.
func TestEveryRegisteredToolHasHandler(t *testing.T) {
	c := &apiClient{} // empty — we only call handlerByToolName, not the handlers
	for _, tool := range mcptools.All() {
		if _, ok := c.handlerByToolName(tool.Tool.Name); !ok {
			t.Errorf("tool %q is in mcptools.Registry but has no handler in handlerByToolName", tool.Tool.Name)
		}
	}
}

// TestNoOrphanHandlers walks the symmetric direction: every tool
// name handlerByToolName recognizes must exist in the registry.
// Catches the case where someone deletes a tool from the registry
// but forgets to clean up the handler switch case.
//
// Implementation note: there's no introspection helper for "what
// names does the switch accept?" — instead we exercise a handful
// of representative names and trust that the forward test
// (TestEveryRegisteredToolHasHandler) plus the visible enumeration
// in the switch covers the rest.
func TestNoOrphanHandlers_Spot(t *testing.T) {
	c := &apiClient{}
	for _, name := range []string{
		"enju_list_runs", "enju_get_task", "enju_claim_task",
		"enju_submit_result", "enju_register_bot",
	} {
		if _, ok := c.handlerByToolName(name); !ok {
			t.Errorf("expected handlerByToolName(%q) to find a handler", name)
		}
		if _, ok := mcptools.ByName(name); !ok {
			t.Errorf("registry missing tool %q referenced by handler switch", name)
		}
	}
}

// allToolFactories is the canonical list of every MCP tool Enju
// exposes. Keep this in sync with the AddTool calls in server.go's
// New(). The test below treats it as a contract snapshot — if a
// tool gets renamed, removed, or loses its description, this table
// tells you about it.
var allToolFactories = []struct {
	name    string
	factory func() mcp.Tool
}{
	{"enju_list_runs", mcptools.ListRuns},
	{"enju_list_ready_tasks", mcptools.ListReadyTasks},
	{"enju_claim_task", mcptools.ClaimTask},
	{"enju_get_task_inputs", mcptools.GetTaskInputs},
	{"enju_submit_result", mcptools.SubmitResult},
	{"enju_submit_results_batch", mcptools.SubmitResultsBatch},
	{"enju_claim_ready_matching", mcptools.ClaimReadyMatching},
	{"enju_list_artifacts", mcptools.ListArtifacts},
	{"enju_get_artifact", mcptools.GetArtifact},
	{"enju_get_artifact_history", mcptools.GetArtifactHistory},
	{"enju_list_untracked_artifacts", mcptools.ListUntrackedArtifacts},
	{"enju_release_task", mcptools.ReleaseTask},
	{"enju_get_task", mcptools.GetTask},
	{"enju_run_status", mcptools.RunStatus},
	{"enju_create_run", mcptools.CreateRun},
	{"enju_fail_task", mcptools.FailTask},
	{"enju_execute_task", mcptools.ExecuteTask},
	{"enju_execute_run", mcptools.ExecuteRun},
	{"enju_pause_run", mcptools.PauseRun},
	{"enju_resume_run", mcptools.ResumeRun},
	{"enju_spawn_task", mcptools.SpawnTask},
	{"enju_request_clarification", mcptools.RequestClarification},
	{"enju_set_cycle_budget", mcptools.SetCycleBudget},
	{"enju_show_events", mcptools.ShowEvents},
	{"enju_recent_events", mcptools.RecentEvents},
	{"enju_notifications", mcptools.Notifications},
	{"enju_inbox", mcptools.Inbox},
	{"enju_review", mcptools.Review},
	{"enju_events_status", mcptools.EventsStatus},
	{"enju_list_iterations", mcptools.ListIterations},
	{"enju_file_issue", mcptools.FileIssue},
	{"enju_list_issues", mcptools.ListIssues},
	{"enju_get_issue", mcptools.GetIssue},
	{"enju_triage_issue", mcptools.TriageIssue},
	{"enju_close_issue", mcptools.CloseIssue},
	{"enju_export_run_events", mcptools.ExportRunEvents},
	{"enju_export_diagram", mcptools.ExportDiagram},
	{"enju_export_run", mcptools.ExportRun},
	{"enju_list_templates", mcptools.ListTemplates},
	{"enju_describe_template", mcptools.DescribeTemplate},
	{"enju_list_projects", mcptools.ListProjects},
	{"enju_create_project", mcptools.CreateProject},
	{"enju_init", mcptools.Init},
	{"enju_set_project_default_branch", mcptools.SetProjectDefaultBranch},
	{"enju_set_project_remote", mcptools.SetProjectRemote},
	{"enju_project_remote_status", mcptools.ProjectRemoteStatus},
	{"enju_project_sync", mcptools.ProjectSync},
	{"enju_leave_project", mcptools.LeaveProject},
	{"enju_add_project_member", mcptools.AddProjectMember},
	{"enju_remove_project_member", mcptools.RemoveProjectMember},
	{"enju_list_project_members", mcptools.ListProjectMembers},
	{"enju_promote_member", mcptools.PromoteMember},
	{"enju_demote_owner", mcptools.DemoteOwner},
	{"enju_update_profile", mcptools.UpdateProfile},
	{"enju_my_dashboard", mcptools.MyDashboard},
	{"enju_my_profile", mcptools.MyProfile},
	{"enju_invalidate_task", mcptools.InvalidateTask},
	{"enju_tally_task", mcptools.TallyTask},
	// operator/model design — bot + model registration tools.
	{"enju_register_bot", mcptools.RegisterBot},
	{"enju_list_my_bots", mcptools.ListMyBots},
	{"enju_revoke_token", mcptools.RevokeToken},
	{"enju_list_models", mcptools.ListModels},
	{"enju_register_model", mcptools.RegisterModel},
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
		{"enju_claim_task", mcptools.ClaimTask, []string{"task_id"}},
		{"enju_submit_result", mcptools.SubmitResult, []string{"task_id"}},
		{"enju_submit_results_batch", mcptools.SubmitResultsBatch, []string{"submissions"}},
		{"enju_claim_ready_matching", mcptools.ClaimReadyMatching, []string{"project_id", "run_id"}},
		{"enju_get_task", mcptools.GetTask, []string{"task_id"}},
		{"enju_get_task_inputs", mcptools.GetTaskInputs, []string{"task_id"}},
		{"enju_release_task", mcptools.ReleaseTask, []string{"task_id"}},
		{"enju_fail_task", mcptools.FailTask, []string{"task_id"}},
		{"enju_invalidate_task", mcptools.InvalidateTask, []string{"task_id"}},
		{"enju_execute_task", mcptools.ExecuteTask, []string{"task_id"}},
		{"enju_execute_run", mcptools.ExecuteRun, []string{"project_id", "run_id"}},
		{"enju_describe_template", mcptools.DescribeTemplate, []string{"project_id", "path"}},
		{"enju_list_templates", mcptools.ListTemplates, []string{"project_id"}},
		{"enju_create_run", mcptools.CreateRun, []string{"project_id"}},
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
