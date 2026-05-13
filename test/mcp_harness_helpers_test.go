package test

// Shared mcpHarness helpers for the MCP-layer integration tests.
//
// Kept separate from mcp_integration_test.go so the test file stays
// focused on tests. Every helper here exists to make a migrated
// scenario test as close as possible to "swap REST helper for MCP
// helper, keep the assertions identical."
//
// Conventions:
//   - All helpers take *testing.T first and call t.Helper() so
//     failure line numbers point at the caller.
//   - Helpers that can operate "as" a specific citizen take a
//     *mcphandlers.TestClient first (after t). The default-client
//     convenience wrappers delegate to h.client.
//   - Helpers that compose tool args return the CallToolResult
//     raw — the caller decides whether success or error is
//     expected via callOK / callExpectError / mcpText.
//
// If a helper is used once and not worth a name, inline it at the
// call site rather than bulking up this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/enju-ai/enju/internal/fatclient/mcphandlers"
	"github.com/mark3labs/mcp-go/mcp"
)

// ===========================================================================
// Generic dispatcher — any TestClient, any tool, any args
// ===========================================================================

// mcpCallVia routes a tool call through an explicit client. The
// default-client helpers elsewhere in this file delegate to this.
// Used by multi-citizen tests where each citizen has its own
// TestClient.
func (h *mcpHarness) mcpCallVia(t *testing.T, client *mcphandlers.TestClient, toolName string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := client.Call(context.Background(), toolName, args)
	if err != nil {
		t.Fatalf("call %s (as %s): transport error: %v", toolName, client.Username(), err)
	}
	if res == nil {
		t.Fatalf("call %s (as %s): nil result", toolName, client.Username())
	}
	return res
}

// mcpCallOKVia asserts a non-error tool result from the given client.
func (h *mcpHarness) mcpCallOKVia(t *testing.T, client *mcphandlers.TestClient, toolName string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res := h.mcpCallVia(t, client, toolName, args)
	if res.IsError {
		t.Fatalf("call %s (as %s) returned tool error: %s", toolName, client.Username(), mcpText(res))
	}
	return res
}

// mcpCallExpectErrorVia asserts a tool-level error result from the
// given client and returns the error text for substring checks.
func (h *mcpHarness) mcpCallExpectErrorVia(t *testing.T, client *mcphandlers.TestClient, toolName string, args map[string]any) string {
	t.Helper()
	res := h.mcpCallVia(t, client, toolName, args)
	if !res.IsError {
		t.Fatalf("call %s (as %s) expected tool error, got success: %s", toolName, client.Username(), mcpText(res))
	}
	return mcpText(res)
}

// ===========================================================================
// Claim / submit / lifecycle — default client
// ===========================================================================

// mcpClaimOK claims a task by short id, expecting success.
func (h *mcpHarness) mcpClaimOK(t *testing.T, shortID string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_claim_task", map[string]any{"task_id": h.taskID(shortID)})
}

// mcpSubmitText submits a plain-text result, expecting success.
func (h *mcpHarness) mcpSubmitText(t *testing.T, shortID, content string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": h.taskID(shortID),
		"content": content,
	})
}

// mcpSubmitReview submits a review-action result. decision must be
// one of approve / reject / request_changes / comment.
func (h *mcpHarness) mcpSubmitReview(t *testing.T, shortID, content, decision string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_submit_result", map[string]any{
		"task_id":  h.taskID(shortID),
		"content":  content,
		"decision": decision,
	})
}

// mcpSubmitVote submits a vote-action result. option must be one of
// the declared option ids on the task.
func (h *mcpHarness) mcpSubmitVote(t *testing.T, shortID, content, option string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": h.taskID(shortID),
		"content": content,
		"option":  option,
	})
}

// mcpSubmitOutputs submits a named-outputs result. outputs is the
// plain string-to-string map; use mcpSubmitOutputLists for
// list-valued outputs (dynamic for_each).
func (h *mcpHarness) mcpSubmitOutputs(t *testing.T, shortID string, outputs map[string]string) *mcp.CallToolResult {
	t.Helper()
	outJSON, err := json.Marshal(outputs)
	if err != nil {
		t.Fatalf("marshal outputs: %v", err)
	}
	return h.callOK(t, "enju_submit_result", map[string]any{
		"task_id":      h.taskID(shortID),
		"outputs_json": string(outJSON),
	})
}

// mcpSubmitOutputLists submits a mixed outputs payload where
// values may be strings OR []string. The list shape is what
// dynamic for_each consumes on the coordinator side. Use a
// map[string]any so callers can pass either form.
func (h *mcpHarness) mcpSubmitOutputLists(t *testing.T, shortID string, outputs map[string]any) *mcp.CallToolResult {
	t.Helper()
	outJSON, err := json.Marshal(outputs)
	if err != nil {
		t.Fatalf("marshal outputs: %v", err)
	}
	return h.callOK(t, "enju_submit_result", map[string]any{
		"task_id":      h.taskID(shortID),
		"outputs_json": string(outJSON),
	})
}

// mcpSubmitArtifacts submits a result with one or more artifact
// writes in the same commit.
func (h *mcpHarness) mcpSubmitArtifacts(t *testing.T, shortID, content string, artifacts map[string]string) *mcp.CallToolResult {
	t.Helper()
	artJSON, err := json.Marshal(artifacts)
	if err != nil {
		t.Fatalf("marshal artifacts: %v", err)
	}
	args := map[string]any{
		"task_id":        h.taskID(shortID),
		"artifacts_json": string(artJSON),
	}
	if content != "" {
		args["content"] = content
	}
	return h.callOK(t, "enju_submit_result", args)
}

// mcpRelease releases a claimed task back to the pool.
func (h *mcpHarness) mcpRelease(t *testing.T, shortID string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_release_task", map[string]any{"task_id": h.taskID(shortID)})
}

// mcpInvalidate invalidates an accepted task with a reason. The
// reason is required by the tool so even tests that don't care
// about it should pass a meaningful string.
func (h *mcpHarness) mcpInvalidate(t *testing.T, shortID, reason string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_invalidate_task", map[string]any{
		"task_id": h.taskID(shortID),
		"reason":  reason,
	})
}

// mcpFail marks a task as FAILED with a reason. Used by tests that
// simulate an autonomous agent giving up on a compute task or a
// reviewer hitting a non-recoverable error.
func (h *mcpHarness) mcpFail(t *testing.T, shortID, reason string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_fail_task", map[string]any{
		"task_id": h.taskID(shortID),
		"reason":  reason,
	})
}

// mcpTally manually fires the tally evaluator on a multi-citizen
// vote/review task. Complements lazy-eval-on-read.
func (h *mcpHarness) mcpTally(t *testing.T, shortID string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_tally_task", map[string]any{"task_id": h.taskID(shortID)})
}

// ===========================================================================
// Claim / submit — via specific citizen (multi-citizen tests)
// ===========================================================================

// mcpClaimAs claims a task via a non-default client. Used when a
// specific citizen's identity matters (multi-reviewer, access
// control, attribution).
func (h *mcpHarness) mcpClaimAs(t *testing.T, client *mcphandlers.TestClient, shortID string) *mcp.CallToolResult {
	t.Helper()
	return h.mcpCallOKVia(t, client, "enju_claim_task", map[string]any{"task_id": h.taskID(shortID)})
}

// mcpSubmitTextAs is the multi-citizen variant of mcpSubmitText.
func (h *mcpHarness) mcpSubmitTextAs(t *testing.T, client *mcphandlers.TestClient, shortID, content string) *mcp.CallToolResult {
	t.Helper()
	return h.mcpCallOKVia(t, client, "enju_submit_result", map[string]any{
		"task_id": h.taskID(shortID),
		"content": content,
	})
}

// mcpSubmitReviewAs is the multi-citizen variant of mcpSubmitReview.
func (h *mcpHarness) mcpSubmitReviewAs(t *testing.T, client *mcphandlers.TestClient, shortID, content, decision string) *mcp.CallToolResult {
	t.Helper()
	return h.mcpCallOKVia(t, client, "enju_submit_result", map[string]any{
		"task_id":  h.taskID(shortID),
		"content":  content,
		"decision": decision,
	})
}

// mcpSubmitVoteAs is the multi-citizen variant of mcpSubmitVote.
func (h *mcpHarness) mcpSubmitVoteAs(t *testing.T, client *mcphandlers.TestClient, shortID, content, option string) *mcp.CallToolResult {
	t.Helper()
	return h.mcpCallOKVia(t, client, "enju_submit_result", map[string]any{
		"task_id": h.taskID(shortID),
		"content": content,
		"option":  option,
	})
}

// ===========================================================================
// Run / project construction
// ===========================================================================

// mcpCreateRunFromFixture reads testdata/{fixture}, creates a run
// via enju_create_run, populates lastProjectID / lastRunSeq so
// subsequent shortID calls resolve, and returns the first ready
// task id (useful for kicking off the typical claim-next pattern).
func (h *mcpHarness) mcpCreateRunFromFixture(t *testing.T, projectID int64, fixture string) string {
	t.Helper()
	yamlBody, err := readFixture(fixture)
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})
	ready := h.readyTasks("")
	if len(ready) == 0 {
		// Some runs (reviews with an answer-first chain) have only
		// one initial ready task; others may start empty if the
		// first tasks need inputs. Either way, surface a useful
		// error.
		t.Fatalf("fixture %s produced no ready tasks after create_run", fixture)
	}
	first, _ := ready[0].(map[string]interface{})
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatalf("fixture %s: ready task missing id: %+v", fixture, first)
	}
	h.rememberRunFromTaskID(t, id)
	return id
}

// mcpCreateRunInline submits an inline YAML body via
// enju_create_run. Same populate-lastRunSeq contract as
// mcpCreateRunFromFixture.
func (h *mcpHarness) mcpCreateRunInline(t *testing.T, projectID int64, yamlBody string) string {
	t.Helper()
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})
	ready := h.readyTasks("")
	if len(ready) == 0 {
		t.Fatalf("inline yaml produced no ready tasks after create_run")
	}
	first, _ := ready[0].(map[string]interface{})
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatalf("inline yaml: ready task missing id: %+v", first)
	}
	h.rememberRunFromTaskID(t, id)
	return id
}

// mcpCreateRunFromTemplate invokes enju_create_run in template
// mode: `path` points at a enju/templates/*.yaml recipe in the
// project clone, `params` populates the template's declared
// parameters. Requires the template file to already be present
// in the project's bare remote (seed it via createTestProject
// plus a direct git commit, or via another submit).
func (h *mcpHarness) mcpCreateRunFromTemplate(t *testing.T, projectID int64, templatePath string, params map[string]any) *mcp.CallToolResult {
	t.Helper()
	return h.call(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       templatePath,
		"params":     params,
	})
}

// ===========================================================================
// Info / query handlers
// ===========================================================================

// mcpRunStatusText returns the formatted run_status text for the
// currently-tracked run (lastProjectID / lastRunSeq).
func (h *mcpHarness) mcpRunStatusText(t *testing.T) string {
	t.Helper()
	res := h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(h.lastProjectID),
		"run_id":     float64(h.lastRunSeq),
	})
	return mcpText(res)
}

// mcpTaskInputs returns the resolved inputs JSON object for a
// task via handleGetTaskInputs. Mostly used to assert on
// artifact resolution / upstream injection.
func (h *mcpHarness) mcpTaskInputs(t *testing.T, shortID string) map[string]interface{} {
	t.Helper()
	res := h.callOK(t, "enju_get_task_inputs", map[string]any{"task_id": h.taskID(shortID)})
	return mcpParseJSON(t, res)
}

// mcpDashboardText returns the formatted dashboard for the
// default client.
func (h *mcpHarness) mcpDashboardText(t *testing.T) string {
	t.Helper()
	res := h.callOK(t, "enju_my_dashboard", map[string]any{})
	return mcpText(res)
}

// mcpProfileText returns the formatted profile for the default
// client.
func (h *mcpHarness) mcpProfileText(t *testing.T) string {
	t.Helper()
	res := h.callOK(t, "enju_my_profile", map[string]any{})
	return mcpText(res)
}

// mcpProfileTextAs returns the profile for any TestClient.
func (h *mcpHarness) mcpProfileTextAs(t *testing.T, client *mcphandlers.TestClient) string {
	t.Helper()
	res := h.mcpCallOKVia(t, client, "enju_my_profile", map[string]any{})
	return mcpText(res)
}

// ===========================================================================
// Artifact handlers
// ===========================================================================

// mcpListArtifacts lists artifacts for a project, optionally
// filtered by prefix.
func (h *mcpHarness) mcpListArtifacts(t *testing.T, projectID int64, prefix string) *mcp.CallToolResult {
	t.Helper()
	args := map[string]any{"project_id": float64(projectID)}
	if prefix != "" {
		args["prefix"] = prefix
	}
	return h.callOK(t, "enju_list_artifacts", args)
}

// mcpGetArtifact reads an artifact's current content and
// provenance.
func (h *mcpHarness) mcpGetArtifact(t *testing.T, projectID int64, path string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_get_artifact", map[string]any{
		"project_id": float64(projectID),
		"path":       path,
	})
}

// mcpGetArtifactHistory returns the chronological write history
// for an artifact.
func (h *mcpHarness) mcpGetArtifactHistory(t *testing.T, projectID int64, path string) *mcp.CallToolResult {
	t.Helper()
	return h.callOK(t, "enju_get_artifact_history", map[string]any{
		"project_id": float64(projectID),
		"path":       path,
	})
}

// ===========================================================================
// Bare-remote helpers (post-assertion reads)
// ===========================================================================

// runDir returns the repo-relative directory the given run
// owns — `enju/runs/{seq}-{slug}/`. Fetches the run over the
// REST API to pull the server-computed slug rather than
// recomputing it client-side, so tests stay in lock-step
// with whatever engine.ComputeRunSlug produces now and later.
//
// Exists because 20+ integration tests hard-coded
// `enju/runs/1/<taskdef>/...` back when the layout didn't
// include the slug; those tests now go through this helper so
// they don't have to know each YAML's name-derived slug.
func (h *mcpHarness) runDir(runSeq int) string {
	h.t.Helper()
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d", h.lastProjectID, runSeq)
	data := h.get(path)
	slug, _ := data["slug"].(string)
	if slug == "" {
		slug = "run"
	}
	return fmt.Sprintf(".enju/runs/%d-%s", runSeq, slug)
}

// mcpBareResultMD returns the result.md content written by a task
// to the project's bare remote. shortID uses the current run's
// task IDs. For multi-citizen tasks the result lives under a
// per-citizen subdirectory — pass the citizen username as the
// optional third argument.
func (h *mcpHarness) mcpBareResultMD(t *testing.T, shortID string, citizen ...string) string {
	t.Helper()
	task := h.taskGet(shortID)
	projectID := int64(task["project_id"].(float64))

	// Use the server-computed result_dir from the task
	// response — the layout schema lives coordinator-side
	// (engine.ComputeResultDir), so test callers consume it
	// directly rather than duplicating the build rule.
	dir, _ := task["result_dir"].(string)
	if len(citizen) > 0 && citizen[0] != "" {
		dir = filepath.Join(dir, "citizen-"+citizen[0])
	}
	path := filepath.Join(dir, "result.md")
	// Foundational v1: tasks whose topic hasn't merged to
	// main (rejected reviews, in-flight iterations) only
	// have their result.md on the topic branch. Prefer the
	// topic ref when the task response surfaces it, falling
	// back to the bare's default branch.
	var body []byte
	var ok bool
	if topic, _ := task["latest_completed_branch"].(string); topic != "" {
		remoteURL := h.remoteFor(projectID)
		body, ok = readRepoFileOnBranch(t, remoteURL, topic, path)
	}
	if !ok {
		body, ok = h.readRepoFile(projectID, path)
	}
	if !ok {
		t.Fatalf("mcpBareResultMD: %s not found in bare remote", path)
	}
	return string(body)
}

// mcpBareMetadataJSON parses a task's metadata.json from the
// bare remote into a generic map. Used by audit-trail tests that
// verify embedded decision / option / username fields.
//
// Foundational v1: rejected / request_changes reviews don't
// merge their topic branch onto main, so their metadata.json
// only lives on the topic ref. When the task response surfaces
// a `latest_completed_branch`, prefer that branch for the
// read; otherwise fall back to the bare's default branch
// (the legacy run-branch path for tasks that did merge to
// main, plus pre-foundational tests).
func (h *mcpHarness) mcpBareMetadataJSON(t *testing.T, shortID string, citizen ...string) map[string]interface{} {
	t.Helper()
	task := h.taskGet(shortID)
	projectID := int64(task["project_id"].(float64))

	dir, _ := task["result_dir"].(string)
	if len(citizen) > 0 && citizen[0] != "" {
		dir = filepath.Join(dir, "citizen-"+citizen[0])
	}
	path := filepath.Join(dir, "metadata.json")
	var body []byte
	var ok bool
	if topic, _ := task["latest_completed_branch"].(string); topic != "" {
		remoteURL := h.remoteFor(projectID)
		body, ok = readRepoFileOnBranch(t, remoteURL, topic, path)
	}
	if !ok {
		body, ok = h.readRepoFile(projectID, path)
	}
	if !ok {
		t.Fatalf("mcpBareMetadataJSON: %s not found in bare remote", path)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse metadata.json at %s: %v\nraw: %s", path, err, body)
	}
	return out
}

// ===========================================================================
// Scheduling / task discovery helpers
// ===========================================================================

// mcpReadyTaskIDs returns the full task IDs of currently-ready
// tasks in the tracked run, optionally filtered by task_def_id
// prefix. Used by migrated tests that need to advance the DAG
// state ("submit next ready task").
func (h *mcpHarness) mcpReadyTaskIDs(t *testing.T, defIDPrefix string) []string {
	t.Helper()
	ready := h.readyTasks("")
	out := make([]string, 0, len(ready))
	for _, raw := range ready {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		if defIDPrefix != "" {
			parts := strings.SplitN(id, ":", 3)
			if len(parts) != 3 || !strings.HasPrefix(parts[2], defIDPrefix) {
				continue
			}
		}
		out = append(out, id)
	}
	return out
}

// mcpInstancesOf returns the short task ids (task_def_id portion
// plus instance key if present) for every materialized instance
// of a for_each-expanded task def. Lets migrated tests say
// "claim every instance of 'analyze'" without knowing the
// instance keys a priori.
func (h *mcpHarness) mcpInstancesOf(t *testing.T, defID string) []string {
	t.Helper()
	runTasks := h.get(fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", h.lastProjectID, h.lastRunSeq))
	// runTasks is a map; the tasks array is under a JSON key
	// that's already been flattened for top-level GETs. Fall
	// back to getList for the correct list shape.
	listPath := fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", h.lastProjectID, h.lastRunSeq)
	tasks := h.getList(listPath)
	_ = runTasks
	out := []string{}
	for _, raw := range tasks {
		m, _ := raw.(map[string]interface{})
		td, _ := m["task_def_id"].(string)
		if td != defID {
			continue
		}
		ik, _ := m["instance_key"].(string)
		if ik != "" {
			out = append(out, ik+":"+td)
		} else {
			out = append(out, td)
		}
	}
	return out
}

// ===========================================================================
// Administrative / direct-store setup (no MCP handler for these)
// ===========================================================================

// mcpExpectProseRejection asserts the call was rejected — either
// as a tool-level error (`IsError=true`) OR as a "soft" non-error
// CallToolResult whose formatted prose contains a failure marker
// ("✗ Failed"). MCP handlers surface coordinator-level rejections
// in either shape depending on the path: legacy handlers wrap the
// server error in a success result with formatted prose; newer
// handlers (post coord-side hardening for access-control + multi-
// citizen vote claims) flip `IsError=true` directly. Both are
// "the call was rejected" semantically — the helper accepts
// either and substring-checks the resulting text.
func (h *mcpHarness) mcpExpectProseRejection(t *testing.T, client *mcphandlers.TestClient, toolName string, args map[string]any, substrs ...string) string {
	t.Helper()
	res := h.mcpCallVia(t, client, toolName, args)
	text := mcpText(res)
	if !res.IsError {
		// Soft-error shape: success result + "✗"/"failed" prose.
		if !strings.Contains(text, "✗") && !strings.Contains(strings.ToLower(text), "failed") {
			t.Fatalf("call %s expected a rejection (IsError=true OR ✗/Failed prose), got success: %s", toolName, text)
		}
	}
	for _, s := range substrs {
		if !strings.Contains(text, s) {
			t.Errorf("call %s rejection should contain %q, got: %s", toolName, s, text)
		}
	}
	return text
}

// mcpSetRole directly updates a citizen's role in the store.
// There's no user-facing "make me an admin" MCP tool — role
// assignment is an out-of-band operation — so tests that need it
// call the store directly. Kept as a harness method so migrated
// access-control tests don't have to re-derive the pattern.
func (h *mcpHarness) mcpSetRole(t *testing.T, username, role string) {
	t.Helper()
	citizen, err := h.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		t.Fatalf("mcpSetRole: citizen %q not found: %v", username, err)
	}
	if _, err := h.store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetCitizenRole{CitizenID: citizen.ID, Role: role},
		},
	}); err != nil {
		t.Fatalf("mcpSetRole: set role %q on %s: %v", role, username, err)
	}
}

// TestMCPHarnessHelpersSmoke is a lightweight regression guard on
// the helpers themselves. It exercises the most-used helpers in
// one run so failures in the helper code don't masquerade as
// failures across N migrated tests.
func TestMCPHarnessHelpersSmoke(t *testing.T) {
	h := newMCPHarness(t, "Helpers Smoke")
	projectID := h.createTestProject()

	// Use a tiny inline run so we exercise mcpCreateRunInline +
	// mcpClaimOK + mcpSubmitText + mcpRunStatusText + taskGet in
	// one pass.
	yamlBody := `name: "helper smoke"
version: 1
tasks:
  - id: hello
    action: answer
    prompt: "Say hi."
`
	firstID := h.mcpCreateRunInline(t, projectID, yamlBody)
	if !strings.HasSuffix(firstID, ":hello") {
		t.Fatalf("expected first ready task to be :hello, got %s", firstID)
	}
	h.mcpClaimOK(t, "hello")
	h.mcpSubmitText(t, "hello", "hi")

	// mcpBareResultMD reads what was just written.
	if got := h.mcpBareResultMD(t, "hello"); got != "hi" {
		t.Errorf("bare result.md: want %q, got %q", "hi", got)
	}
	// mcpBareMetadataJSON parses the metadata sibling.
	meta := h.mcpBareMetadataJSON(t, "hello")
	if tid, _ := meta["task_id"].(string); !strings.HasSuffix(tid, ":hello") {
		t.Errorf("metadata task_id: want suffix :hello, got %v", tid)
	}
	if u, _ := meta["username"].(string); u != h.username {
		t.Errorf("metadata username: want %q, got %v", h.username, u)
	}

	// mcpRunStatusText + mcpDashboardText + mcpProfileText all run
	// against the same state.
	if status := h.mcpRunStatusText(t); !strings.Contains(status, "helper smoke") {
		t.Errorf("run_status missing run name: %s", status)
	}
	if dash := h.mcpDashboardText(t); !strings.Contains(dash, h.username) {
		t.Errorf("dashboard missing username: %s", dash)
	}
	if prof := h.mcpProfileText(t); !strings.Contains(prof, h.username) {
		t.Errorf("profile missing username: %s", prof)
	}
}
