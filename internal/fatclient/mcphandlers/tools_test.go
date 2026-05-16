package mcphandlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/enjumcp"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestEveryRegisteredToolHasHandler pins the registry-handler
// contract: every entry in enjumcp.All() must have a handler
// returned by handlerByToolName. The runtime panic in New()
// would also catch this, but a test catches it pre-deploy with
// a clearer error.
func TestEveryRegisteredToolHasHandler(t *testing.T) {
	c := &apiClient{} // empty — we only call handlerByToolName, not the handlers
	for _, tool := range enjumcp.All() {
		if _, ok := c.handlerByToolName(tool.Name); !ok {
			t.Errorf("tool %q is in enjumcp.Registry but has no handler in handlerByToolName", tool.Name)
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
		if _, ok := enjumcp.ByName(name); !ok {
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
	{"enju_list_runs", enjumcp.ListRuns},
	{"enju_list_ready_tasks", enjumcp.ListReadyTasks},
	{"enju_claim_task", enjumcp.ClaimTask},
	{"enju_get_task_inputs", enjumcp.GetTaskInputs},
	{"enju_submit_result", enjumcp.SubmitResult},
	{"enju_submit_results_batch", enjumcp.SubmitResultsBatch},
	{"enju_claim_ready_matching", enjumcp.ClaimReadyMatching},
	{"enju_list_artifacts", enjumcp.ListArtifacts},
	{"enju_get_artifact", enjumcp.GetArtifact},
	{"enju_get_artifact_history", enjumcp.GetArtifactHistory},
	{"enju_list_untracked_artifacts", enjumcp.ListUntrackedArtifacts},
	{"enju_release_task", enjumcp.ReleaseTask},
	{"enju_get_task", enjumcp.GetTask},
	{"enju_run_status", enjumcp.RunStatus},
	{"enju_create_run", enjumcp.CreateRun},
	{"enju_fail_task", enjumcp.FailTask},
	{"enju_execute_task", enjumcp.ExecuteTask},
	{"enju_execute_run", enjumcp.ExecuteRun},
	{"enju_pause_run", enjumcp.PauseRun},
	{"enju_resume_run", enjumcp.ResumeRun},
	{"enju_terminate_run", enjumcp.TerminateRun},
	{"enju_spawn_task", enjumcp.SpawnTask},
	{"enju_request_clarification", enjumcp.RequestClarification},
	{"enju_set_cycle_budget", enjumcp.SetCycleBudget},
	{"enju_show_events", enjumcp.ShowEvents},
	{"enju_recent_events", enjumcp.RecentEvents},
	{"enju_inbox", enjumcp.Inbox},
	{"enju_review", enjumcp.Review},
	{"enju_events_status", enjumcp.EventsStatus},
	{"enju_list_iterations", enjumcp.ListIterations},
	{"enju_file_issue", enjumcp.FileIssue},
	{"enju_list_issues", enjumcp.ListIssues},
	{"enju_get_issue", enjumcp.GetIssue},
	{"enju_triage_issue", enjumcp.TriageIssue},
	{"enju_close_issue", enjumcp.CloseIssue},
	{"enju_export_run_events", enjumcp.ExportRunEvents},
	{"enju_export_diagram", enjumcp.ExportDiagram},
	{"enju_export_run", enjumcp.ExportRun},
	{"enju_list_templates", enjumcp.ListTemplates},
	{"enju_describe_template", enjumcp.DescribeTemplate},
	{"enju_list_projects", enjumcp.ListProjects},
	{"enju_create_project", enjumcp.CreateProject},
	{"enju_set_project_default_branch", enjumcp.SetProjectDefaultBranch},
	{"enju_set_project_remote", enjumcp.SetProjectRemote},
	{"enju_project_remote_status", enjumcp.ProjectRemoteStatus},
	{"enju_project_sync", enjumcp.ProjectSync},
	{"enju_leave_project", enjumcp.LeaveProject},
	{"enju_add_project_member", enjumcp.AddProjectMember},
	{"enju_remove_project_member", enjumcp.RemoveProjectMember},
	{"enju_list_project_members", enjumcp.ListProjectMembers},
	{"enju_promote_member", enjumcp.PromoteMember},
	{"enju_demote_owner", enjumcp.DemoteOwner},
	{"enju_update_profile", enjumcp.UpdateProfile},
	{"enju_my_dashboard", enjumcp.MyDashboard},
	{"enju_my_profile", enjumcp.MyProfile},
	{"enju_invalidate_task", enjumcp.InvalidateTask},
	{"enju_retry_task", enjumcp.RetryTask},
	{"enju_tally_task", enjumcp.TallyTask},
	// operator/model design — bot + model registration tools.
	{"enju_register_bot", enjumcp.RegisterBot},
	{"enju_list_my_bots", enjumcp.ListMyBots},
	{"enju_revoke_token", enjumcp.RevokeToken},
	{"enju_list_models", enjumcp.ListModels},
	{"enju_register_model", enjumcp.RegisterModel},
	{"enju_bot_start", enjumcp.BotStart},
	{"enju_bot_stop", enjumcp.BotStop},
	{"enju_bot_status", enjumcp.BotStatus},
	{"enju_bot_logs", enjumcp.BotLogs},
	{"enju_bot_start_all", enjumcp.BotStartAll},
	{"enju_bot_stop_all", enjumcp.BotStopAll},
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

// TestAllowTools_FiltersServerSurface verifies the --allow-tools
// pinning the bot runner relies on: when AllowTools is set, the
// MCP server registers exactly those tools and no others.
// Critical for the "runner pins the allowlist at process boundary"
// trust leg — if filtering broke, a reviewer-bot manifest of
// [Read, Grep, Glob] could end up with Write/Edit silently
// reachable.
func TestAllowTools_FiltersServerSurface(t *testing.T) {
	allow := []string{"enju_get_task", "enju_list_runs", "enju_run_status"}
	s := New(Config{
		CoordinatorURL: "http://unused",
		AllowTools:     allow,
	})
	registered := s.ListTools()
	if len(registered) != len(allow) {
		t.Errorf("AllowTools=%d tools, server registered %d", len(allow), len(registered))
	}
	for _, name := range allow {
		if _, ok := registered[name]; !ok {
			t.Errorf("expected %q to be registered, but missing", name)
		}
	}
	// Confirm something definitely-not-in-the-allowlist isn't
	// reachable.
	for _, blocked := range []string{"enju_submit_result", "enju_claim_task", "enju_terminate_run"} {
		if _, ok := registered[blocked]; ok {
			t.Errorf("tool %q must NOT be registered when AllowTools excludes it", blocked)
		}
	}
}

// TestAllowTools_EmptyMeansAll keeps the backwards-compatible
// default: callers (the human's normal `enju mcp`) that don't
// pass AllowTools see every tool, exactly as before the flag
// existed.
func TestAllowTools_EmptyMeansAll(t *testing.T) {
	s := New(Config{CoordinatorURL: "http://unused"})
	registered := s.ListTools()
	if len(registered) != len(allToolFactories) {
		t.Errorf("empty AllowTools should yield all %d tools, got %d", len(allToolFactories), len(registered))
	}
}

// TestAllowTools_UnknownNamesDropped keeps a manifest stable
// across tool renames: an entry that doesn't match a real tool
// is silently dropped (not panicked on, not fatal).
func TestAllowTools_UnknownNamesDropped(t *testing.T) {
	s := New(Config{
		CoordinatorURL: "http://unused",
		AllowTools:     []string{"enju_get_task", "Read"}, // "Read" is not an enju MCP tool name
	})
	registered := s.ListTools()
	if len(registered) != 1 {
		t.Errorf("expected 1 registered tool (unknown 'Read' dropped), got %d", len(registered))
	}
	if _, ok := registered["enju_get_task"]; !ok {
		t.Error("enju_get_task should be present")
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
		{"enju_claim_task", enjumcp.ClaimTask, []string{"task_id"}},
		{"enju_submit_result", enjumcp.SubmitResult, []string{"task_id"}},
		{"enju_submit_results_batch", enjumcp.SubmitResultsBatch, []string{"submissions"}},
		{"enju_claim_ready_matching", enjumcp.ClaimReadyMatching, []string{"project_id", "run_id"}},
		{"enju_get_task", enjumcp.GetTask, []string{"task_id"}},
		{"enju_get_task_inputs", enjumcp.GetTaskInputs, []string{"task_id"}},
		{"enju_release_task", enjumcp.ReleaseTask, []string{"task_id"}},
		{"enju_fail_task", enjumcp.FailTask, []string{"task_id"}},
		{"enju_invalidate_task", enjumcp.InvalidateTask, []string{"task_id"}},
		{"enju_execute_task", enjumcp.ExecuteTask, []string{"task_id"}},
		{"enju_execute_run", enjumcp.ExecuteRun, []string{"project_id", "run_id"}},
		{"enju_describe_template", enjumcp.DescribeTemplate, []string{"project_id", "path"}},
		{"enju_list_templates", enjumcp.ListTemplates, []string{"project_id"}},
		{"enju_create_run", enjumcp.CreateRun, []string{"project_id"}},
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
