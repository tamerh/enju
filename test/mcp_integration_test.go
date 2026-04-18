package test

// MCP-layer integration tests — the primary regression suite for
// user-facing scenarios.
//
// Tests in this file drive the MCP tool handlers (handleClaimTask,
// handleSubmitResult, ...) end-to-end against a real httptest
// coordinator and a real mcpgit.Workspace. They cover the layer
// where real users hit bugs: client-side pre-validation, review
// feedback replay, inline review content, multi-citizen result
// routing, format output on real data, workspace git
// interactions.
//
// Pure coordinator invariants (state-machine rules, YAML parser
// errors, auth middleware, reaper timeouts, numbering) live in
// coordinator_integration_test.go — those don't depend on any
// client and are cheaper to test at the REST layer. The
// principle: if a behavior could be exercised by a hypothetical
// web UI hitting REST directly, it belongs at REST; if it lives
// in handler code, it belongs here.
//
// Harness: each test builds a fresh mcpHarness (embeds testServer
// + a real *mcpserver.TestClient), registers one or more citizens,
// creates a project with a bare remote, and drives tool handlers
// via h.call / h.callOK / h.callExpectError. TestClient.Call
// dispatches by tool name matching what mark3labs/mcp-go routes
// through AddTool, so the test arg maps mirror the JSON a real
// MCP host sends over stdio. Shared harness helpers live in
// mcp_harness_helpers_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/mcpgit"
	"github.com/enju-ai/enju/internal/mcpserver"
	gogit "github.com/go-git/go-git/v5"
	"github.com/mark3labs/mcp-go/mcp"
)

// execCommand is a tiny wrapper around exec.Command so this file
// can build without re-importing exec symbols in multiple spots.
var execCommand = exec.Command

// mcpHarness extends testServer with a *mcpserver.TestClient wired
// to the same coordinator URL and workspace. Tests drive handle*
// methods via h.call(name, args) instead of hitting the REST API
// directly.
type mcpHarness struct {
	*testServer
	client *mcpserver.TestClient
	// Cached citizen username the client was registered under.
	// Equal to client.Username().
	username string
}

// newMCPHarness creates a fresh testServer, registers a citizen
// with the given display name, and wires a TestClient against the
// coordinator and workspace. The shared workspace lets both the
// MCP client path and the legacy testServer helpers write to the
// same local clones.
func newMCPHarness(t *testing.T, citizenName string) *mcpHarness {
	t.Helper()
	ts := newTestServer(t)

	// Register via the shared helper so the coordinator knows about
	// the citizen. The returned username is the stable handle the
	// MCP client uses to identify itself on every request.
	username := ts.register(citizenName)

	// Grab the auth token from the freshly-created citizen record.
	// The register helper throws it away; we look it up on the
	// store directly to avoid changing that helper's signature.
	citizen, err := ts.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		t.Fatalf("lookup citizen %q after register: %v", username, err)
	}

	cfg := mcpserver.Config{
		CoordinatorURL: ts.url,
		Username:       username,
		CitizenName:    citizenName,
		CitizenEmail:   citizen.Email,
		AuthToken:      citizen.Token,
		ModelName:      "test-model",
		Workspace:      ts.workspace,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &mcpHarness{
		testServer: ts,
		client:     mcpserver.NewTestClient(cfg),
		username:   username,
	}
}

// newMCPClientAs builds a second TestClient against the same
// coordinator and workspace under a different citizen identity.
// Used by multi-citizen tests where each citizen has its own
// handler instance.
func (h *mcpHarness) newMCPClientAs(t *testing.T, citizenName string) *mcpserver.TestClient {
	t.Helper()
	username := h.register(citizenName)
	citizen, err := h.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		t.Fatalf("lookup citizen %q: %v", username, err)
	}
	cfg := mcpserver.Config{
		CoordinatorURL: h.url,
		Username:       username,
		CitizenName:    citizenName,
		CitizenEmail:   citizen.Email,
		AuthToken:      citizen.Token,
		ModelName:      "test-model",
		Workspace:      h.workspace,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return mcpserver.NewTestClient(cfg)
}

// call invokes the named MCP tool with args and fails the test on
// transport-level errors. The returned CallToolResult may still
// have IsError=true — tests that want to assert on error phrasing
// should use callExpectError instead.
func (h *mcpHarness) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := h.client.Call(context.Background(), name, args)
	if err != nil {
		t.Fatalf("call %s: transport error: %v", name, err)
	}
	if res == nil {
		t.Fatalf("call %s: nil result", name)
	}
	return res
}

// callOK invokes the named MCP tool and fails the test if the
// handler returned a tool-level error. Use for the happy-path
// checks where a tool error is a test failure.
func (h *mcpHarness) callOK(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res := h.call(t, name, args)
	if res.IsError {
		t.Fatalf("call %s returned tool error: %s", name, mcpText(res))
	}
	return res
}

// callExpectError invokes the named MCP tool and fails the test if
// the handler DID NOT return a tool-level error. The returned text
// is the concatenated error message content for substring checks.
func (h *mcpHarness) callExpectError(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	res := h.call(t, name, args)
	if !res.IsError {
		t.Fatalf("call %s expected tool error, got success: %s", name, mcpText(res))
	}
	return mcpText(res)
}

// mcpText extracts the concatenated text content from a
// CallToolResult for substring assertions. Uses JSON round-tripping
// so it stays tolerant of SDK content-type internals.
func mcpText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	data, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	var shape struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(data, &shape) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range shape.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// mcpParseJSON extracts the first JSON object embedded in a tool
// result's text. Some formatters emit prose + an embedded JSON
// block; this returns the parsed object for assertions.
func mcpParseJSON(t *testing.T, res *mcp.CallToolResult) map[string]interface{} {
	t.Helper()
	text := mcpText(res)
	// Find the first "{" and parse from there. Tool responses from
	// claim / get_task_inputs put the JSON payload at the start.
	start := strings.Index(text, "{")
	if start < 0 {
		t.Fatalf("no JSON object found in tool text: %s", text)
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(text[start:]), &out); err != nil {
		// Some tools render prose before the JSON; fall back to
		// line-by-line scan for a JSON-parseable suffix.
		for i := start + 1; i < len(text); i++ {
			if text[i] != '{' {
				continue
			}
			if json.Unmarshal([]byte(text[i:]), &out) == nil {
				return out
			}
		}
		t.Fatalf("parse embedded JSON: %v\nraw: %s", err, text)
	}
	return out
}

// TestMCPHarnessSmokesEnjuMyProfile is the scaffolding smoke test.
// Verifies the harness wires up correctly: TestClient reaches the
// coordinator, registration happens, and handleMyProfile returns a
// non-error result that mentions the citizen.
func TestMCPHarnessSmokesEnjuMyProfile(t *testing.T) {
	h := newMCPHarness(t, "Harness Smoke")
	res := h.callOK(t, "enju_my_profile", map[string]any{})
	text := mcpText(res)
	if !strings.Contains(text, h.username) {
		t.Errorf("expected profile to mention username %q, got: %s", h.username, text)
	}
	// Silence unused-import guard when mcpgit is only used by other
	// tests in this file during early iterations.
	_ = mcpgit.ResultDir
}

// mcpSubmitSimple runs a simple answer-task submission through
// handleSubmitResult with just a content string. Returns the
// formatted tool response text for assertions.
func (h *mcpHarness) mcpSubmitSimple(t *testing.T, taskID, content string) *mcp.CallToolResult {
	t.Helper()
	return h.call(t, "enju_submit_result", map[string]any{
		"task_id": taskID,
		"content": content,
	})
}

// mcpClaim calls handleClaimTask for the given task id. Returns
// the full tool result (with the prose + inputs JSON).
func (h *mcpHarness) mcpClaim(t *testing.T, taskID string) *mcp.CallToolResult {
	t.Helper()
	return h.call(t, "enju_claim_task", map[string]any{"task_id": taskID})
}

// TestMCPHappyPathFullCycle drives handleCreateRun → handleClaimTask
// → handleSubmitResult → handleRunStatus end-to-end, verifying the
// MCP handler layer takes a run from YAML submission all the way to
// completion against a real coordinator + real workspace + real
// bare remote. This is the baseline that the targeted tests below
// build on; if it breaks, every other MCP-integration test is
// likely wrong.
func TestMCPHappyPathFullCycle(t *testing.T) {
	h := newMCPHarness(t, "Happy Path")
	projectID := h.createTestProject()

	// 1. handleCreateRun with an inline YAML payload.
	yamlBody, err := readFixture("simple-no-deps.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	runRes := h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})
	runText := mcpText(runRes)
	if !strings.Contains(runText, "Run created") && !strings.Contains(runText, "Created run") &&
		!strings.Contains(runText, "tasks") {
		t.Errorf("expected create_run confirmation text, got: %s", runText)
	}

	// Set lastProjectID / lastRunSeq so h.taskID() prefixes work.
	ready := h.readyTasks("")
	if len(ready) == 0 {
		t.Fatalf("expected ready tasks after create_run, got none")
	}
	first, _ := ready[0].(map[string]interface{})
	taskAID, _ := first["id"].(string)
	if taskAID == "" {
		t.Fatalf("ready task missing id: %+v", first)
	}
	h.rememberRunFromTaskID(t, taskAID)
	parts := strings.SplitN(taskAID, ":", 3)

	// 2. handleClaimTask on task_a.
	claimRes := h.callOK(t, "enju_claim_task", map[string]any{"task_id": taskAID})
	claimText := mcpText(claimRes)
	if !strings.Contains(claimText, "List 3 colors") {
		t.Errorf("expected claim to surface the prompt (List 3 colors), got: %s", claimText)
	}
	if !strings.Contains(claimText, "Claimed") {
		t.Errorf("expected claim response to confirm a successful claim, got: %s", claimText)
	}

	// 3. handleSubmitResult with content.
	submitRes := h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": taskAID,
		"content": "red, green, blue",
	})
	submitText := mcpText(submitRes)
	if !strings.Contains(submitText, "Result accepted") &&
		!strings.Contains(submitText, "accepted") {
		t.Errorf("expected submit to confirm acceptance, got: %s", submitText)
	}

	// Verify the commit landed in the bare remote.
	resultDir := mcpgit.ResultDir(parseInt(t, parts[1]), "", "task_a")
	body, ok := h.readRepoFile(projectID, resultDir+"/result.md")
	if !ok {
		t.Fatalf("result.md not found in bare remote at %s", resultDir)
	}
	if string(body) != "red, green, blue" {
		t.Errorf("bare remote result content mismatch: got %q", string(body))
	}

	// 4. Also submit task_b so the run completes.
	taskBID := ""
	for _, raw := range h.readyTasks("") {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, ":task_b") {
			taskBID = id
			break
		}
	}
	if taskBID == "" {
		t.Fatalf("task_b not ready after task_a acceptance")
	}
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": taskBID})
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": taskBID,
		"content": "cat, dog, fox",
	})

	// 5. handleRunStatus reports completion.
	statusRes := h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(parseInt(t, parts[1])),
	})
	statusText := mcpText(statusRes)
	// The DAG tree view uses ✅ for completed tasks; both should
	// show the completed marker.
	if strings.Count(statusText, "✅") < 2 {
		t.Errorf("expected two completed-task markers in run_status, got: %s", statusText)
	}
}

// readFixture reads a YAML file from test/testdata/.
func readFixture(name string) (string, error) {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseInt is a tiny t.Helper wrapper around strconv.Atoi used by
// tests that need to convert run-seq / project-id substrings out of
// fully-qualified task IDs like "1:1:task_a".
func parseInt(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parse int %q: %v", s, err)
	}
	return n
}

// rememberRunFromTaskID populates lastProjectID / lastRunSeq so
// subsequent shorthand taskID("check") calls resolve correctly.
// Tests that create a run via enju_create_run need this because
// the shared taskID helper expects those fields populated by the
// legacy submitYAML path.
func (h *mcpHarness) rememberRunFromTaskID(t *testing.T, fullTaskID string) {
	t.Helper()
	parts := strings.SplitN(fullTaskID, ":", 3)
	if len(parts) < 3 {
		t.Fatalf("rememberRunFromTaskID: expected projectID:runSeq:defID, got %q", fullTaskID)
	}
	h.lastProjectID = parseInt64(t, parts[0])
	h.lastRunSeq = parseInt(t, parts[1])
	h.lastRunID = parts[0] + ":" + parts[1]
}

// parseInt64 is parseInt's int64 sibling for project IDs.
func parseInt64(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse int64 %q: %v", s, err)
	}
	return n
}

// bareCommitCount counts commits in a project's bare remote. Used
// by the pre-validation tests to prove that rejected submissions
// don't leave phantom commits.
func (h *mcpHarness) bareCommitCount(t *testing.T, projectID int64) int {
	t.Helper()
	remoteURL := h.remoteFor(projectID)
	if remoteURL == "" {
		t.Fatalf("bareCommitCount: project %d has no remote_url", projectID)
	}
	cmd := execCommand("git", "--git-dir", remoteURL, "log", "--oneline")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// TestMCPSubmitResultReviewPreValidationBlocksGit is the core
// regression guard for the bug class the TODO calls out
// ("request_changes rejection found 2026-04-17"). A review task
// submission with a missing or invalid decision must be rejected
// by handleSubmitResult BEFORE any git write lands — otherwise a
// phantom commit stays in the append-only history.
//
// This test exercises the full handler path (including
// submitResultFatClient's pre-validation gate, which the existing
// unit test only reaches with a nil workspace) against a real
// coordinator and real bare remote. After each rejected submission
// it counts commits in the bare remote and asserts the count has
// not grown.
func TestMCPSubmitResultReviewPreValidationBlocksGit(t *testing.T) {
	h := newMCPHarness(t, "Review PreValidator")
	projectID := h.createTestProject()

	yamlBody, err := readFixture("review.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})

	// Find the draft task, submit it. This gives the review task
	// something to point at and drives the run to a state where
	// claiming "check" succeeds.
	ready := h.readyTasks("")
	var draftID string
	for _, raw := range ready {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, ":draft") {
			draftID = id
			break
		}
	}
	if draftID == "" {
		t.Fatalf("draft not ready; got: %+v", ready)
	}
	h.rememberRunFromTaskID(t, draftID)

	h.callOK(t, "enju_claim_task", map[string]any{"task_id": draftID})
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": draftID,
		"content": "Enju coordinates distributed human-AI problem solving.",
	})

	// Claim the review task so submit is state-legal.
	reviewID := h.taskID("check")
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": reviewID})

	// Baseline commit count.
	before := h.bareCommitCount(t, projectID)

	// Invalid / missing decisions must each return a tool error
	// AND leave the commit count unchanged.
	invalidCases := []struct {
		name     string
		args     map[string]any
		wantText string
	}{
		{
			"missing decision with prose",
			map[string]any{"task_id": reviewID, "content": "looks fine"},
			"decision is required",
		},
		{
			"invalid decision",
			map[string]any{"task_id": reviewID, "content": "fine", "decision": "maybe"},
			`"maybe" is invalid`,
		},
		{
			"uppercase decision",
			map[string]any{"task_id": reviewID, "content": "fine", "decision": "APPROVE"},
			`"APPROVE" is invalid`,
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			text := h.callExpectError(t, "enju_submit_result", tc.args)
			if !strings.Contains(text, tc.wantText) {
				t.Errorf("expected error text containing %q, got: %s", tc.wantText, text)
			}
			if got := h.bareCommitCount(t, projectID); got != before {
				t.Errorf("pre-validation allowed a phantom commit: count went %d → %d", before, got)
			}
		})
	}
}

// TestMCPRunStatusRendersTreeWithMixedStates asserts that
// handleRunStatus produces the expected DAG tree view with
// per-state icons when tasks are in a mix of states. This is the
// "format output correctness with real data" gap — the existing
// format_test.go unit tests hand synthetic JSON to the formatter;
// this test feeds it through the handler with state that was
// actually produced by the coordinator after real state
// transitions.
//
// After a single draft submission on review.yaml the tree should
// show: draft=✅ (accepted), check=🟡 (ready), publish=⚪
// (waiting). The exact icons and tree connectors are part of the
// contract — a format regression that drops one of them would be
// surfaced loudly here.
func TestMCPRunStatusRendersTreeWithMixedStates(t *testing.T) {
	h := newMCPHarness(t, "Run Status Formatter")
	projectID := h.createTestProject()
	yamlBody, err := readFixture("review.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})

	// Submit just the draft so the run has three distinct states.
	ready := h.readyTasks("")
	var draftID string
	for _, raw := range ready {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, ":draft") {
			draftID = id
			break
		}
	}
	if draftID == "" {
		t.Fatalf("draft not ready")
	}
	h.rememberRunFromTaskID(t, draftID)
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": draftID})
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": draftID,
		"content": "A concise draft.",
	})

	// Run status contract for small runs (≤10 tasks, compact
	// summary mode per TODO.md line 752): the per-task-def
	// summary uses ✅ for accepted and "available" / "waiting"
	// words for the other states, and the viewer-specific queue
	// renders 🟡 next to claim-ready tasks.
	res := h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(h.lastRunSeq),
	})
	text := mcpText(res)

	wantMarkers := map[string]string{
		"accepted checkmark (✅)":   "✅",
		"available summary word":   "available",
		"waiting summary word":     "waiting",
		"your-queue claim icon 🟡": "🟡",
	}
	for label, marker := range wantMarkers {
		if !strings.Contains(text, marker) {
			t.Errorf("expected %s in run_status output, got:\n%s", label, text)
		}
	}

	// Every task def id should appear inline so the human can see
	// which task the summary lines refer to.
	for _, defID := range []string{"draft", "check", "publish"} {
		if !strings.Contains(text, defID) {
			t.Errorf("expected %q in run_status output, got:\n%s", defID, text)
		}
	}

	// Progress line must report the done/total fraction.
	if !strings.Contains(text, "Progress: 1/3") {
		t.Errorf("expected 'Progress: 1/3' after one of three tasks accepted, got:\n%s", text)
	}
}

// TestMCPClaimRendersPromptBlock asserts handleClaimTask's tool
// response carries the standard "── Prompt ──" separator block so
// LLMs parsing the text have a stable anchor. Small but
// load-bearing: the format expectation is called out in the
// server-instructions string ("paste output verbatim"), so a
// regression here breaks every downstream LLM's rendering.
func TestMCPClaimRendersPromptBlock(t *testing.T) {
	h := newMCPHarness(t, "Claim Formatter")
	projectID := h.createTestProject()
	yamlBody, err := readFixture("simple-no-deps.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})
	ready := h.readyTasks("")
	first, _ := ready[0].(map[string]interface{})
	taskID, _ := first["id"].(string)
	h.rememberRunFromTaskID(t, taskID)

	res := h.callOK(t, "enju_claim_task", map[string]any{"task_id": taskID})
	text := mcpText(res)

	if !strings.Contains(text, "── Prompt") {
		t.Errorf("expected '── Prompt' separator in claim text, got:\n%s", text)
	}
	if !strings.Contains(text, "✓ Claimed") {
		t.Errorf("expected '✓ Claimed' headline in claim text, got:\n%s", text)
	}
	// The fixture task_a prompts "List 3 colors."; must appear in
	// the rendered prompt block.
	if !strings.Contains(text, "List 3 colors") {
		t.Errorf("expected fixture prompt text in claim output, got:\n%s", text)
	}
}

// TestMCPMultiCitizenSubmitRoutesPerCitizen exercises the
// multi-citizen routing invariant: when a review/vote task
// declares citizens > 1, each citizen's submission must land
// under runs/{seq}/{task}/citizen-{username}/ in the bare remote
// — not collide on a shared result.md. This routing lives inside
// submitResultFatClient (server.go:2178) and is invisible to the
// existing integration tests because they re-implement the layout
// in their own helper.
//
// Flow: 3-reviewer review.yaml variant. Each reviewer has its own
// TestClient (its own username + AuthToken). After all three
// submit "approve", we scan the bare remote for each expected
// citizen-{username}/result.md and verify the prose arrives at
// the right address.
func TestMCPMultiCitizenSubmitRoutesPerCitizen(t *testing.T) {
	h := newMCPHarness(t, "Drafter")
	reviewerA := h.newMCPClientAs(t, "Alice Reviewer")
	reviewerB := h.newMCPClientAs(t, "Bob Reviewer")
	reviewerC := h.newMCPClientAs(t, "Cara Reviewer")

	projectID := h.createTestProject()
	yamlBody, err := readFixture("review-multi.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})

	// Drafter submits the draft so the review task opens up.
	ready := h.readyTasks("")
	var draftID string
	for _, raw := range ready {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, ":draft") {
			draftID = id
			break
		}
	}
	if draftID == "" {
		t.Fatalf("draft not ready")
	}
	h.rememberRunFromTaskID(t, draftID)
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": draftID})
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": draftID,
		"content": "Enju summary.",
	})

	reviewID := h.taskID("check")
	ctx := context.Background()

	// Each reviewer claims + submits approve through their own
	// TestClient. We pair (client, prose) so we can assert on the
	// prose landing under the right citizen directory later.
	reviewers := []struct {
		client *mcpserver.TestClient
		prose  string
	}{
		{reviewerA, "Alice says approve."},
		{reviewerB, "Bob says approve."},
		{reviewerC, "Cara says approve."},
	}
	for _, r := range reviewers {
		if res, err := r.client.Call(ctx, "enju_claim_task", map[string]any{"task_id": reviewID}); err != nil {
			t.Fatalf("%s claim: %v", r.client.Username(), err)
		} else if res.IsError {
			t.Fatalf("%s claim rejected: %s", r.client.Username(), mcpText(res))
		}
		res, err := r.client.Call(ctx, "enju_submit_result", map[string]any{
			"task_id":  reviewID,
			"content":  r.prose,
			"decision": "approve",
		})
		if err != nil {
			t.Fatalf("%s submit: %v", r.client.Username(), err)
		}
		if res.IsError {
			t.Fatalf("%s submit rejected: %s", r.client.Username(), mcpText(res))
		}
	}

	// Each reviewer's prose must be at
	// .enju/runs/{seq}/check/citizen-{username}/result.md.
	// ResultDir already prefixes with .enju/.
	baseDir := mcpgit.ResultDir(h.lastRunSeq, "", "check")
	for _, r := range reviewers {
		path := baseDir + "/citizen-" + r.client.Username() + "/result.md"
		body, ok := h.readRepoFile(projectID, path)
		if !ok {
			t.Errorf("no result.md for reviewer %s at %s", r.client.Username(), path)
			continue
		}
		if string(body) != r.prose {
			t.Errorf("reviewer %s content mismatch at %s:\n  want: %q\n  got:  %q",
				r.client.Username(), path, r.prose, string(body))
		}
	}

	// After unanimous approve, the review task should tally to
	// approved and the draft should remain accepted. The publish
	// task (downstream of check) should be ready.
	check := h.taskGet("check")
	if state, _ := check["state"].(string); state != "accepted" {
		t.Errorf("review task state after 3 approvals = %q, want accepted", state)
	}
	publish := h.taskGet("publish")
	if state, _ := publish["state"].(string); state != "ready" && state != "accepted" {
		t.Errorf("publish should be ready after review approves, got %q", state)
	}
}

// TestMCPClaimReviewTaskInlinesTargetContent is the full-stack
// counterpart to TestFetchAndResolveLocallyInlinesReviewingBlock
// in server_test.go. The unit test mocks the coordinator +
// bare-remote wiring; this test drives the same code path via
// handleClaimTask against a real coordinator, real workspace, and
// real bare remote, asserting the "── Reviewing ──" block in the
// formatted output carries the draft's actual content.
func TestMCPClaimReviewTaskInlinesTargetContent(t *testing.T) {
	h := newMCPHarness(t, "Review Claimer")
	projectID := h.createTestProject()

	yamlBody, err := readFixture("review.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})

	// Submit the draft. Because we want to assert on its exact
	// content appearing in the review's claim response, the
	// content string is distinctive.
	ready := h.readyTasks("")
	var draftID string
	for _, raw := range ready {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, ":draft") {
			draftID = id
			break
		}
	}
	if draftID == "" {
		t.Fatalf("draft not ready")
	}
	h.rememberRunFromTaskID(t, draftID)
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": draftID})
	const draftContent = "MARKER-ENJU-REVIEW-E2E: distributed human-AI coordination."
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": draftID,
		"content": draftContent,
	})

	// Claim the review task. The formatter must render a
	// "── Reviewing ──" block and inline the draft content.
	reviewID := h.taskID("check")
	res := h.callOK(t, "enju_claim_task", map[string]any{"task_id": reviewID})
	text := mcpText(res)

	if !strings.Contains(text, "── Reviewing ──") {
		t.Errorf("expected Reviewing block in claim response, got:\n%s", text)
	}
	if !strings.Contains(text, draftContent) {
		t.Errorf("expected draft content inline in reviewing block, got:\n%s", text)
	}
	if !strings.Contains(text, "draft") {
		t.Errorf("expected target def id 'draft' in reviewing block, got:\n%s", text)
	}
}

// TestMCPClaimSurfacesRequestChangesFeedback is the end-to-end
// guarantee that a task bounced back via decision=request_changes
// carries the reviewer's prose AND the author's previous submission
// inline the next time the author claims. The current integration
// suite can't exercise this path because the replay logic lives
// entirely inside handleClaimTask (fetchReviewFeedback +
// workspace ReadFile) and reads from the local mcpgit clone —
// neither of which the direct-REST path touches.
//
// Flow:
//   1. draft submits "First try."
//   2. reviewer submits decision=request_changes with feedback
//      "please expand".
//   3. the draft re-opens to READY (request_changes cascade).
//   4. re-claim the draft; the MCP tool response must contain both
//      the "Previous submission" block (with "First try.") AND the
//      "Reviewer feedback" block (with "please expand" + the
//      reviewer's username + the decision label).
func TestMCPClaimSurfacesRequestChangesFeedback(t *testing.T) {
	// Two distinct citizens: one drafts, one reviews. Using
	// separate clients surfaces the username-in-feedback contract
	// — the author claiming the bounced task sees who bounced it.
	h := newMCPHarness(t, "Drafter")
	reviewerClient := h.newMCPClientAs(t, "Reviewer Bob")

	projectID := h.createTestProject()
	yamlBody, err := readFixture("review.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})

	// Drafter claims and submits the initial draft.
	ready := h.readyTasks("")
	var draftID string
	for _, raw := range ready {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, ":draft") {
			draftID = id
			break
		}
	}
	if draftID == "" {
		t.Fatalf("draft not ready")
	}
	h.rememberRunFromTaskID(t, draftID)
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": draftID})
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": draftID,
		"content": "First try.",
	})

	// Reviewer claims + submits request_changes with substantive
	// feedback. The reviewer must use a separate TestClient so
	// the commit metadata.json records the reviewer's username,
	// not the drafter's.
	reviewID := h.taskID("check")
	ctx := context.Background()
	if res, err := reviewerClient.Call(ctx, "enju_claim_task", map[string]any{"task_id": reviewID}); err != nil {
		t.Fatalf("reviewer claim: %v", err)
	} else if res.IsError {
		t.Fatalf("reviewer claim rejected: %s", mcpText(res))
	}
	res, err := reviewerClient.Call(ctx, "enju_submit_result", map[string]any{
		"task_id":  reviewID,
		"content":  "please expand",
		"decision": "request_changes",
	})
	if err != nil {
		t.Fatalf("reviewer submit: %v", err)
	}
	if res.IsError {
		t.Fatalf("reviewer submit rejected: %s", mcpText(res))
	}

	// The draft should have bounced back to READY.
	draft := h.taskGet("draft")
	if state, _ := draft["state"].(string); state != "ready" {
		t.Fatalf("draft should be ready after request_changes, got %q", state)
	}

	// Re-claim the draft as the original drafter; the response
	// must carry both inline blocks.
	reclaim := h.callOK(t, "enju_claim_task", map[string]any{"task_id": draftID})
	text := mcpText(reclaim)

	if !strings.Contains(text, "Previous submission") {
		t.Errorf("expected 'Previous submission' block in reclaim response, got:\n%s", text)
	}
	if !strings.Contains(text, "First try.") {
		t.Errorf("expected previous-submission block to include original content, got:\n%s", text)
	}
	if !strings.Contains(text, "Reviewer feedback") {
		t.Errorf("expected 'Reviewer feedback' block in reclaim response, got:\n%s", text)
	}
	if !strings.Contains(text, "please expand") {
		t.Errorf("expected feedback block to include reviewer prose, got:\n%s", text)
	}
	if !strings.Contains(text, reviewerClient.Username()) {
		t.Errorf("expected feedback block to credit reviewer @%s, got:\n%s", reviewerClient.Username(), text)
	}
	if !strings.Contains(text, "request_changes") {
		t.Errorf("expected feedback block to label the decision, got:\n%s", text)
	}
}

// TestMCPSubmitResultVotePreValidationBlocksGit is the
// vote-action sibling of the review pre-validation test. A vote
// task submitted through handleSubmitResult must:
//   1. reject a missing option with a "must be one of..." hint,
//   2. reject an unknown option id with the same phrasing,
//   3. leave the bare remote's commit count unchanged after each
//      rejected submission (no phantom commits in the append-only
//      history),
//   4. accept a valid option with a new commit AND route the run
//      down the activated branch (tasks on losing branches go to
//      SKIPPED).
func TestMCPSubmitResultVotePreValidationBlocksGit(t *testing.T) {
	h := newMCPHarness(t, "Vote PreValidator")
	projectID := h.createTestProject()

	yamlBody, err := readFixture("vote-gate.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})

	// Complete the analysis upstream so the vote task unblocks.
	ready := h.readyTasks("")
	var analysisID string
	for _, raw := range ready {
		m, _ := raw.(map[string]interface{})
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, ":analysis") {
			analysisID = id
			break
		}
	}
	if analysisID == "" {
		t.Fatalf("analysis task not ready")
	}
	h.rememberRunFromTaskID(t, analysisID)
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": analysisID})
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id": analysisID,
		"content": "Python is simpler; Rust is faster.",
	})

	// Claim the vote task.
	pickID := h.taskID("pick")
	h.callOK(t, "enju_claim_task", map[string]any{"task_id": pickID})

	before := h.bareCommitCount(t, projectID)

	// Missing option.
	missingText := h.callExpectError(t, "enju_submit_result", map[string]any{
		"task_id": pickID,
		"content": "I prefer Python.",
	})
	if !strings.Contains(missingText, "option is required") {
		t.Errorf("missing-option error should mention 'option is required', got: %s", missingText)
	}
	if got := h.bareCommitCount(t, projectID); got != before {
		t.Errorf("missing-option submission left a phantom commit: %d → %d", before, got)
	}

	// Unknown option id.
	unknownText := h.callExpectError(t, "enju_submit_result", map[string]any{
		"task_id": pickID,
		"content": "I prefer haskell.",
		"option":  "haskell",
	})
	if !strings.Contains(unknownText, `"haskell" is invalid`) {
		t.Errorf("unknown-option error should quote the bad option, got: %s", unknownText)
	}
	if got := h.bareCommitCount(t, projectID); got != before {
		t.Errorf("unknown-option submission left a phantom commit: %d → %d", before, got)
	}

	// Valid option — accepts + activates the python branch.
	validRes := h.call(t, "enju_submit_result", map[string]any{
		"task_id": pickID,
		"content": "Python wins.",
		"option":  "python",
	})
	if validRes.IsError {
		t.Fatalf("valid option rejected: %s", mcpText(validRes))
	}
	if after := h.bareCommitCount(t, projectID); after <= before {
		t.Errorf("valid submission did not land a commit (%d → %d)", before, after)
	}

	// The winning branch must become READY; losing branch must
	// become SKIPPED.
	buildPython := h.taskGet("build_python")
	if state, _ := buildPython["state"].(string); state != "ready" && state != "accepted" {
		t.Errorf("build_python should be ready after python vote, got state %q", state)
	}
	buildRust := h.taskGet("build_rust")
	if state, _ := buildRust["state"].(string); state != "skipped" {
		t.Errorf("build_rust should be skipped on python win, got state %q", state)
	}
}

// TestMCPSubmitResultAllFourReviewDecisionsLand walks through every
// valid decision ("approve", "reject", "request_changes", "comment")
// end-to-end via handleSubmitResult. Each decision must:
//   1. return a non-error CallToolResult,
//   2. produce a new commit in the bare remote,
//   3. trigger the expected downstream state (approve lets publish
//      progress, reject fails the draft, request_changes bounces
//      draft back to READY, comment is non-blocking).
//
// This is a regression guard for the class of bug where an inline
// check at the top of handleSubmitResult silently allowed only
// approve/reject.
func TestMCPSubmitResultAllFourReviewDecisionsLand(t *testing.T) {
	type caseSpec struct {
		decision            string
		wantDraftStateAfter string
		// wantResponseSubstr is the decision-specific headline
		// the formatter emits. Approve and reject get a dedicated
		// summary line; request_changes and comment fall through
		// to the generic "Result accepted" headline, so we only
		// assert on the state transition for those two.
		wantResponseSubstr string
	}
	cases := []caseSpec{
		{"approve", "accepted", "approved"},
		{"reject", "failed", "rejected"},
		{"request_changes", "ready", ""},
		{"comment", "accepted", ""},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			h := newMCPHarness(t, "Review Decision "+tc.decision)
			projectID := h.createTestProject()

			yamlBody, err := readFixture("review.yaml")
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			h.callOK(t, "enju_create_run", map[string]any{
				"project_id": float64(projectID),
				"yaml":       yamlBody,
			})
			ready := h.readyTasks("")
			var draftID string
			for _, raw := range ready {
				m, _ := raw.(map[string]interface{})
				id, _ := m["id"].(string)
				if strings.HasSuffix(id, ":draft") {
					draftID = id
					break
				}
			}
			if draftID == "" {
				t.Fatalf("draft not ready")
			}
			h.rememberRunFromTaskID(t, draftID)

			h.callOK(t, "enju_claim_task", map[string]any{"task_id": draftID})
			h.callOK(t, "enju_submit_result", map[string]any{
				"task_id": draftID,
				"content": "Draft content.",
			})

			reviewID := h.taskID("check")
			h.callOK(t, "enju_claim_task", map[string]any{"task_id": reviewID})

			before := h.bareCommitCount(t, projectID)
			res := h.call(t, "enju_submit_result", map[string]any{
				"task_id":  reviewID,
				"content":  "Reviewer prose.",
				"decision": tc.decision,
			})
			if res.IsError {
				t.Fatalf("decision %q should succeed, got error: %s", tc.decision, mcpText(res))
			}
			text := mcpText(res)
			if tc.wantResponseSubstr != "" && !strings.Contains(strings.ToLower(text), tc.wantResponseSubstr) {
				t.Errorf("decision %q: expected response mentioning %q, got: %s", tc.decision, tc.wantResponseSubstr, text)
			}

			after := h.bareCommitCount(t, projectID)
			if after <= before {
				t.Errorf("decision %q: expected commit count to grow (%d → %d)", tc.decision, before, after)
			}

			// Verify downstream state on the draft.
			draft := h.taskGet("draft")
			if state, _ := draft["state"].(string); state != tc.wantDraftStateAfter {
				t.Errorf("decision %q: draft state after review = %q, want %q", tc.decision, state, tc.wantDraftStateAfter)
			}
		})
	}
}

// TestMCPBranchingWithInferredDeps is the MCP-layer port of
// TestBranchingWithInferredDeps. Exercises inferred {{upstream.content}}
// dependencies, upstream-in-resolved-prompt rendering, and
// run-completed bookkeeping.
func TestMCPBranchingWithInferredDeps(t *testing.T) {
	h := newMCPHarness(t, "Branching Alice")
	bob := h.newMCPClientAs(t, "Branching Bob")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "branching.yaml")

	// python_pros and go_pros ready, comparison blocked.
	ready := h.readyTasks(h.lastRunID)
	if len(ready) != 2 {
		t.Fatalf("expected 2 ready, got %d", len(ready))
	}

	// comparison has inferred deps (checked via REST task record).
	compTask := h.taskGet("comparison")
	if depsStr, _ := compTask["depends_on"].(string); depsStr == "" {
		t.Fatal("comparison should have inferred depends_on")
	}

	// Alice does python_pros.
	h.mcpClaimOK(t, "python_pros")
	a1 := answer(t,
		"List 3 advantages of Python for CLI tools. One sentence each.",
		"1. Rich ecosystem. 2. Fast prototyping. 3. Easy distribution.")
	h.mcpSubmitText(t, "python_pros", a1)
	if got := h.taskGet("comparison")["state"]; got != "pending" {
		t.Fatalf("comparison should still be pending after 1/2 upstreams, got %v", got)
	}

	// Bob does go_pros.
	h.mcpClaimAs(t, bob, "go_pros")
	a2 := answer(t,
		"List 3 advantages of Go for CLI tools. One sentence each.",
		"1. Single binary. 2. Native concurrency. 3. Fast startup.")
	h.mcpSubmitTextAs(t, bob, "go_pros", a2)
	if got := h.taskGet("comparison")["state"]; got != "ready" {
		t.Fatalf("comparison should be ready after 2/2 upstreams, got %v", got)
	}

	// Template resolution via handleGetTaskInputs.
	inputs := h.mcpTaskInputs(t, "comparison")
	resolved, _ := inputs["resolved_prompt"].(string)
	if resolved == "" {
		t.Fatal("expected resolved prompt")
	}
	if strings.Contains(resolved, "{{python_pros") {
		t.Fatalf("unresolved placeholder in: %s", resolved)
	}
	if !strings.Contains(resolved, a1) && !llmMode() {
		t.Fatalf("expected upstream content in resolved prompt: %s", resolved)
	}

	// Complete comparison.
	h.mcpClaimOK(t, "comparison")
	a3 := answer(t, resolved, "Go wins for CLI distribution.")
	h.mcpSubmitText(t, "comparison", a3)

	// Run must report completed (structured REST state — MCP's
	// formatted run_status is asserted separately by other tests).
	status := h.runStatus(h.lastRunID)
	if status["state"] != "completed" {
		t.Fatalf("expected completed, got %v", status["state"])
	}

	// Output files + commit count on the bare remote.
	h.assertResultFile(h.lastRunID, "", "python_pros", "ecosystem")
	h.assertResultFile(h.lastRunID, "", "go_pros", "binary")
	h.assertResultFile(h.lastRunID, "", "comparison", "")
	// 4 commits: 1 initial README + 3 task results.
	h.assertGitCommits(projectID, 4)
}

// TestMCPForEachExpansion is the MCP-layer port of
// TestForEachExpansion. Exercises static for_each expansion
// (3 langs × 2 tasks = 6 instances) and instance-specific claim /
// submit through the handler layer.
func TestMCPForEachExpansion(t *testing.T) {
	h := newMCPHarness(t, "ForEach")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "for-each.yaml")

	// 3 langs × 2 tasks = 6 tasks.
	tasks := h.runTasks(h.lastRunID)
	if len(tasks) != 6 {
		t.Fatalf("expected 6 tasks, got %d", len(tasks))
	}
	// 3 root tasks ready (one :pros per language).
	ready := h.readyTasks(h.lastRunID)
	if len(ready) != 3 {
		t.Fatalf("expected 3 ready, got %d", len(ready))
	}

	langs := []string{"Python", "Go", "Rust"}
	cannedPros := map[string]string{
		"Python": "1. Easy to learn. 2. Large ecosystem.",
		"Go":     "1. Fast compilation. 2. Great concurrency.",
		"Rust":   "1. Memory safety. 2. Zero-cost abstractions.",
	}
	cannedSummary := map[string]string{
		"Python": "Python is beginner-friendly with many libraries.",
		"Go":     "Go excels at concurrent systems.",
		"Rust":   "Rust guarantees safety without GC.",
	}

	for _, lang := range langs {
		prosID := lang + ":pros"
		h.mcpClaimOK(t, prosID)
		h.mcpSubmitText(t, prosID, answer(t, "List 2 advantages of "+lang+".", cannedPros[lang]))

		summaryID := lang + ":summary"
		h.mcpClaimOK(t, summaryID)
		inputs := h.mcpTaskInputs(t, summaryID)
		resolvedPrompt, _ := inputs["resolved_prompt"].(string)

		// {{lang}} must have been resolved at creation time.
		prompt, _ := h.taskGet(summaryID)["prompt"].(string)
		if strings.Contains(prompt, "{{lang}}") {
			t.Fatalf("{{lang}} should be resolved at creation: %s", prompt)
		}
		h.mcpSubmitText(t, summaryID, answer(t, resolvedPrompt, cannedSummary[lang]))
	}

	// All 6 tasks accepted → run completed.
	if got := h.runStatus(h.lastRunID)["state"]; got != "completed" {
		t.Fatalf("expected run completed after all 6 tasks, got %v", got)
	}
}

// TestMCPLLMFullPipeline is the MCP-layer port of
// TestLLMFullPipeline. Behind ENJU_LLM_TEST=1. Generates run YAML
// from natural language via claude -p, submits via
// enju_create_run, and runs the DAG to completion through MCP
// handlers.
func TestMCPLLMFullPipeline(t *testing.T) {
	if !llmMode() {
		t.Skip("skipping LLM pipeline test (set ENJU_LLM_TEST=1)")
	}

	h := newMCPHarness(t, "LLM Researcher")
	bob := h.newMCPClientAs(t, "LLM Reviewer")

	runDesc, err := os.ReadFile("problems/microservices-vs-monolith.txt")
	if err != nil {
		t.Fatal(err)
	}

	yamlPrompt := `You are generating an Enju run definition in YAML format.

The user describes a run in natural language. Convert it to Enju YAML.

Enju YAML format:
- name: run name
- version: 1
- tasks: list of tasks, each with id, action ("answer"), and prompt
- Tasks can reference upstream results using {{task_id.content}} in their prompt
- Dependencies are inferred automatically from these references

Return ONLY valid YAML. No explanation, no markdown fences, no extra text.

User's run description:
` + string(runDesc)

	yamlContent := cleanYAML(answer(t, yamlPrompt, ""))
	t.Logf("Generated YAML:\n%s", yamlContent)

	projectID := h.createTestProject()
	h.mcpCreateRunInline(t, projectID, yamlContent)

	// Work all ready tasks until the DAG drains.
	for {
		ready := h.readyTasks(h.lastRunID)
		if len(ready) == 0 {
			break
		}
		for _, raw := range ready {
			tm, _ := raw.(map[string]interface{})
			taskID, _ := tm["id"].(string)
			prompt, _ := tm["prompt"].(string)

			// Alternate citizens so we exercise both handlers.
			client := h.client
			if len(taskID)%2 == 0 {
				client = bob
			}
			h.mcpClaimAs(t, client, taskID)

			if deps, _ := tm["depends_on"].(string); deps != "" {
				inputs := h.mcpTaskInputs(t, taskID)
				if rp, ok := inputs["resolved_prompt"].(string); ok && rp != "" {
					prompt = rp
				}
			}
			h.mcpSubmitTextAs(t, client, taskID, answer(t, prompt, ""))
			t.Logf("Completed: %s", taskID)
		}
	}

	if got := h.runStatus(h.lastRunID)["state"]; got != "completed" {
		t.Fatalf("expected completed, got %v", got)
	}
}

// TestMCPMultipleCitizensCollaborate is the MCP-layer port of
// TestMultipleCitizensCollaborate. Three citizens work the same
// DAG; each task must credit a distinct claimer.
func TestMCPMultipleCitizensCollaborate(t *testing.T) {
	h := newMCPHarness(t, "Trio Alice")
	bob := h.newMCPClientAs(t, "Trio Bob")
	charlie := h.newMCPClientAs(t, "Trio Charlie")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "branching.yaml")

	h.mcpClaimOK(t, "python_pros")
	h.mcpClaimAs(t, bob, "go_pros")
	h.mcpSubmitText(t, "python_pros",
		answer(t, "List 3 advantages of Python for CLI tools.", "Python advantages here"))
	h.mcpSubmitTextAs(t, bob, "go_pros",
		answer(t, "List 3 advantages of Go for CLI tools.", "Go advantages here"))

	h.mcpClaimAs(t, charlie, "comparison")
	inputs := h.mcpTaskInputs(t, "comparison")
	resolved, _ := inputs["resolved_prompt"].(string)
	h.mcpSubmitTextAs(t, charlie, "comparison", answer(t, resolved, "Final comparison"))

	if got := h.runStatus(h.lastRunID)["state"]; got != "completed" {
		t.Fatalf("expected completed, got %v", got)
	}

	// All three distinct citizens should have claimed a task.
	tasks := h.runTasks(h.lastRunID)
	citizens := make(map[string]bool)
	for _, raw := range tasks {
		tm, _ := raw.(map[string]interface{})
		if cb, ok := tm["claimed_by"].(string); ok && cb != "" {
			citizens[cb] = true
		}
	}
	if len(citizens) != 3 {
		t.Fatalf("expected 3 distinct claimants, got %d: %v", len(citizens), citizens)
	}
}

// TestMCPCitizenDashboard is the MCP-layer port of
// TestCitizenDashboard. Exercises handleMyDashboard across three
// states: pre-work, after-completion, after-claim.
func TestMCPCitizenDashboard(t *testing.T) {
	h := newMCPHarness(t, "Dashboard")
	projectID := h.createTestProject()

	// Dashboard pre-work.
	pre := h.mcpDashboardText(t)
	if !strings.Contains(pre, h.username) {
		t.Errorf("initial dashboard missing username: %s", pre)
	}

	// Do work.
	h.mcpCreateRunFromFixture(t, projectID, "simple-no-deps.yaml")
	h.mcpClaimOK(t, "task_a")
	h.mcpSubmitText(t, "task_a", "Red, Blue, Green")

	// Dashboard after completing one task.
	post := h.mcpDashboardText(t)
	if !strings.Contains(post, "task_a") {
		t.Errorf("dashboard after submit should mention task_a, got: %s", post)
	}

	// Claim another task → must show in active list.
	h.mcpClaimOK(t, "task_b")
	active := h.mcpDashboardText(t)
	if !strings.Contains(active, "task_b") {
		t.Errorf("dashboard after claim should mention task_b, got: %s", active)
	}
}

// TestMCPActionField is the MCP-layer port of TestActionField.
// The action field surfaces through handleGetTask; verifies the
// legacy type/mode fields are absent.
func TestMCPActionField(t *testing.T) {
	h := newMCPHarness(t, "Action Field")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "branching.yaml")

	task := h.taskGet("python_pros")
	if action, _ := task["action"].(string); action != "answer" {
		t.Fatalf("expected action 'answer', got %q", action)
	}
	if _, hasType := task["type"]; hasType {
		t.Fatal("legacy 'type' field should not be present")
	}
	if _, hasMode := task["mode"]; hasMode {
		t.Fatal("legacy 'mode' field should not be present")
	}
}

// TestMCPNamedOutputs is the MCP-layer port of TestNamedOutputs.
// Exercises outputs_json submission, per-output downstream
// template resolution, and output-schema surfacing in task detail.
func TestMCPNamedOutputs(t *testing.T) {
	h := newMCPHarness(t, "Named Outputs")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "named-outputs.yaml")

	// Schema surfaces on the task record.
	task := h.taskGet("gene_analysis")
	outputs, _ := task["outputs"].(string)
	if outputs == "" {
		t.Fatal("expected outputs schema on gene_analysis")
	}
	if !strings.Contains(outputs, "gene_list") || !strings.Contains(outputs, "pathways") {
		t.Fatalf("outputs schema missing expected fields: %s", outputs)
	}

	h.mcpClaimOK(t, "gene_analysis")
	h.mcpSubmitOutputs(t, "gene_analysis", map[string]string{
		"gene_list": "BRCA1, TP53, EGFR",
		"pathways":  "KEGG:hsa04110, KEGG:hsa04115",
		"stats":     "50 genes, p<0.01",
	})

	// drug_targets downstream sees only gene_list.
	drugInputs := h.mcpTaskInputs(t, "drug_targets")
	drugResolved, _ := drugInputs["resolved_prompt"].(string)
	if !strings.Contains(drugResolved, "BRCA1") {
		t.Fatalf("drug_targets should see gene_list content: %s", drugResolved)
	}
	if strings.Contains(drugResolved, "KEGG") {
		t.Fatalf("drug_targets should NOT see pathways: %s", drugResolved)
	}

	// pathway_viz downstream sees only pathways.
	vizInputs := h.mcpTaskInputs(t, "pathway_viz")
	vizResolved, _ := vizInputs["resolved_prompt"].(string)
	if !strings.Contains(vizResolved, "KEGG") {
		t.Fatalf("pathway_viz should see pathways content: %s", vizResolved)
	}
	if strings.Contains(vizResolved, "BRCA1") {
		t.Fatalf("pathway_viz should NOT see gene_list: %s", vizResolved)
	}
}

// TestMCPCreateProject is the MCP-layer port of TestCreateProject.
// Covers the happy-path create plus the duplicate-name rejection
// through handleCreateProject.
func TestMCPCreateProject(t *testing.T) {
	h := newMCPHarness(t, "Create Project")

	// Happy path.
	res := h.callOK(t, "enju_create_project", map[string]any{
		"name":        "Drug Target Discovery",
		"description": "Long-lived project for drug target analyses",
	})
	if text := mcpText(res); !strings.Contains(text, "Drug Target Discovery") {
		t.Errorf("expected project name in create response, got: %s", text)
	}

	// Duplicate name — the MCP handler surfaces the server's
	// rejection as a "✗ Failed to create project: ..." prose
	// line in a non-error CallToolResult (the post helper returns
	// 409 status but reads the body; the formatter shows the
	// error text). We assert on the prose rather than IsError to
	// match the handler's actual behavior.
	dupRes := h.callOK(t, "enju_create_project", map[string]any{
		"name": "Drug Target Discovery",
	})
	dupText := mcpText(dupRes)
	if !strings.Contains(dupText, "Failed to create") {
		t.Errorf("expected duplicate-name rejection text, got: %s", dupText)
	}
	if !strings.Contains(dupText, "already exists") {
		t.Errorf("expected 'already exists' in rejection text, got: %s", dupText)
	}
}

// TestMCPRunInProject is the MCP-layer port of TestRunInProject.
// Creates a project via handleCreateProject, then submits a run
// into it via handleCreateRun. Verifies run seq numbering + run
// listing via REST state inspection.
func TestMCPRunInProject(t *testing.T) {
	h := newMCPHarness(t, "Run-In-Project")

	// Create a project (MCP layer auto-wires a local bare via
	// handleCreateProject's auto-local fallback). Need to grab
	// its id from the returned payload — the handler's prose
	// output embeds it; use REST list to be safe.
	h.callOK(t, "enju_create_project", map[string]any{
		"name": "Bioinformatics Research",
	})
	projects := h.getList("/api/v1/projects")
	var projectID int64
	for _, raw := range projects {
		m, _ := raw.(map[string]interface{})
		if name, _ := m["name"].(string); name == "Bioinformatics Research" {
			projectID = int64(m["id"].(float64))
			break
		}
	}
	if projectID == 0 {
		t.Fatal("new project not found")
	}

	// Submit a run inside it.
	yamlBody, err := readFixture("simple-no-deps.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlBody,
	})

	// REST state inspection: first run in a new project gets seq=1.
	runs := h.getList(fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if len(runs) != 1 {
		t.Fatalf("expected 1 run in project, got %d", len(runs))
	}
	first, _ := runs[0].(map[string]interface{})
	if seq, _ := first["seq"].(float64); int(seq) != 1 {
		t.Fatalf("expected run seq=1, got %v", seq)
	}
	if pid, _ := first["project_id"].(float64); int64(pid) != projectID {
		t.Fatalf("expected project_id=%d, got %v", projectID, pid)
	}
}

// TestMCPEnvironmentRequirements is the MCP-layer port of
// TestEnvironmentRequirements. The requirements block is part of
// the claim response surface; here we verify it's correctly
// inherited / overridden / opted-out by reading the task records
// post-create-run.
func TestMCPEnvironmentRequirements(t *testing.T) {
	h := newMCPHarness(t, "Requirements")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "requirements.yaml")

	// Inherits project requirements.
	simple := h.taskGet("simple_task")
	simpleReqs, _ := simple["requirements"].(string)
	if !strings.Contains(simpleReqs, "python") {
		t.Fatalf("simple_task requirements should contain python: %s", simpleReqs)
	}
	if !strings.Contains(simpleReqs, "pandas") {
		t.Fatalf("simple_task requirements should contain pandas: %s", simpleReqs)
	}

	// Task-level replaces project-level entirely.
	custom := h.taskGet("custom_task")
	customReqs, _ := custom["requirements"].(string)
	if !strings.Contains(customReqs, "node") {
		t.Fatalf("custom_task should have node: %s", customReqs)
	}
	if !strings.Contains(customReqs, "chembl") {
		t.Fatalf("custom_task should have chembl: %s", customReqs)
	}
	if strings.Contains(customReqs, "pandas") {
		t.Fatalf("custom_task should NOT inherit pandas (task-level replaces): %s", customReqs)
	}

	// Explicit empty opts out.
	noReqs := h.taskGet("no_reqs_task")
	if v, _ := noReqs["requirements"].(string); v != "" {
		t.Fatalf("no_reqs_task should have empty requirements, got: %s", v)
	}
}

// TestMCPMultiFileOutputs is the MCP-layer port of
// TestMultiFileOutputs. Named outputs with per-output file: specs
// write one file per output in the bare remote.
func TestMCPMultiFileOutputs(t *testing.T) {
	h := newMCPHarness(t, "Multi File Outputs")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "multi-file-outputs.yaml")

	h.mcpClaimOK(t, "analyze")
	h.mcpSubmitOutputs(t, "analyze", map[string]string{
		"gene_list": "gene,score\nBRCA1,0.95\nTP53,0.87",
		"pathways":  `{"nodes":["BRCA1","TP53"],"edges":[]}`,
		"summary":   "# Analysis\n\nFound 2 genes.",
	})

	// Verify per-output files landed on the bare remote.
	cloneDir, err := os.MkdirTemp("", "mcp-mfo-verify-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(cloneDir)
	remoteURL := h.remoteFor(projectID)
	if remoteURL == "" {
		t.Fatal("no remote_url for multi-file-outputs project")
	}
	if _, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{URL: remoteURL}); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	resultsDir := filepath.Join(cloneDir, ".enju", "runs", fmt.Sprintf("%d", h.lastRunSeq), "analyze")

	fileChecks := []struct {
		name     string
		mustHave string
	}{
		{"genes.csv", "BRCA1"},
		{"pathways.json", "nodes"},
		{"summary.md", "Analysis"},
	}
	for _, fc := range fileChecks {
		data, err := os.ReadFile(filepath.Join(resultsDir, fc.name))
		if err != nil {
			t.Fatalf("%s not found: %v", fc.name, err)
		}
		if !strings.Contains(string(data), fc.mustHave) {
			t.Fatalf("%s missing expected content %q: %s", fc.name, fc.mustHave, data)
		}
	}

	// metadata.json must carry the file index.
	metaBytes, err := os.ReadFile(filepath.Join(resultsDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	metaStr := string(metaBytes)
	if !strings.Contains(metaStr, "output_files") {
		t.Fatalf("metadata.json missing output_files: %s", metaStr)
	}
	if !strings.Contains(metaStr, "genes.csv") {
		t.Fatalf("metadata.json missing genes.csv reference: %s", metaStr)
	}

	// Downstream: use_genes sees only gene_list; use_pathways sees
	// only pathways. Verifies per-output template resolution
	// continues to work with file-per-output submission.
	useGenesInputs := h.mcpTaskInputs(t, "use_genes")
	useGenesResolved, _ := useGenesInputs["resolved_prompt"].(string)
	if !strings.Contains(useGenesResolved, "BRCA1") {
		t.Fatalf("use_genes should see gene_list content, got: %s", useGenesResolved)
	}
	if strings.Contains(useGenesResolved, "nodes") {
		t.Fatalf("use_genes should NOT see pathways content: %s", useGenesResolved)
	}

	usePathwaysInputs := h.mcpTaskInputs(t, "use_pathways")
	usePathwaysResolved, _ := usePathwaysInputs["resolved_prompt"].(string)
	if !strings.Contains(usePathwaysResolved, "nodes") {
		t.Fatalf("use_pathways should see pathways content, got: %s", usePathwaysResolved)
	}
	// Note: we don't assert that use_pathways lacks "BRCA1" because
	// the pathways payload legitimately embeds BRCA1 as a node id.
	// The original test only checked the positive side here.
}

// TestMCPUpdateProfile is the MCP-layer port of TestUpdateProfile.
// handleUpdateProfile must round-trip a display-name change.
func TestMCPUpdateProfile(t *testing.T) {
	h := newMCPHarness(t, "Profile Owner")

	res := h.callOK(t, "enju_update_profile", map[string]any{
		"name":  "Alice Smith",
		"email": "alice-smith@example.com",
	})
	text := mcpText(res)
	if !strings.Contains(text, "Alice Smith") && !strings.Contains(text, "updated") {
		t.Errorf("expected update confirmation in profile response, got: %s", text)
	}

	// Verify via the profile handler — the change should persist.
	prof := h.mcpProfileText(t)
	if !strings.Contains(prof, "Alice Smith") {
		t.Errorf("profile after update should show new name, got: %s", prof)
	}
}

// TestMCPTaskInputsSurfacesMissingArtifacts is the MCP-layer port
// of TestTaskInputsSurfacesMissingArtifacts. handleGetTaskInputs
// must surface missing_artifacts AND leave the {{artifact:...}}
// placeholder literal in the resolved prompt.
func TestMCPTaskInputsSurfacesMissingArtifacts(t *testing.T) {
	h := newMCPHarness(t, "Missing Artifact")
	projectID := h.createTestProject()

	yaml := `name: "Missing read"
version: 1
tasks:
  - id: reader
    action: answer
    reads_artifacts: [doesnt/exist.md]
    prompt: "Use {{artifact:doesnt/exist.md}} to summarize."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	inputs := h.mcpTaskInputs(t, "reader")
	if arts, _ := inputs["artifacts"].(map[string]interface{}); len(arts) != 0 {
		t.Fatalf("expected empty artifacts map, got %v", arts)
	}
	missing, _ := inputs["missing_artifacts"].([]interface{})
	if len(missing) != 1 {
		t.Fatalf("expected 1 missing artifact, got %v", inputs["missing_artifacts"])
	}
	if missing[0] != "doesnt/exist.md" {
		t.Fatalf("expected doesnt/exist.md in missing, got %v", missing[0])
	}
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "{{artifact:doesnt/exist.md}}") {
		t.Fatalf("expected unresolved placeholder in prompt: %s", resolved)
	}
}

// TestMCPContributionEventsRecorded is the MCP-layer port of
// TestContributionEventsRecorded. After a claim+submit via MCP,
// the citizen's contribution attribution must reflect the work.
func TestMCPContributionEventsRecorded(t *testing.T) {
	h := newMCPHarness(t, "Contributor")
	projectID := h.createTestProject()

	h.mcpCreateRunFromFixture(t, projectID, "simple-no-deps.yaml")
	h.mcpClaimOK(t, "task_a")
	h.mcpSubmitText(t, "task_a", "Hello world result")

	// The contribution attribution lives on the profile. Fetch it
	// over REST for structured assertions (the text formatter
	// would also work but is noisier to match against).
	contribs := h.get(fmt.Sprintf("/api/v1/citizens/by-username/%s/contributions", h.username))
	if completed, _ := contribs["tasks_completed"].(float64); completed < 1 {
		t.Errorf("expected at least 1 tasks_completed, got %.0f", completed)
	}
	if tokens, _ := contribs["tokens_total"].(float64); tokens <= 0 {
		t.Errorf("expected positive tokens_total, got %.0f", tokens)
	}
}


// accessControlRun reads testdata/access-control.yaml, substitutes
// the __CITIZEN_ALICE__ / __CITIZEN_BOB__ placeholders with real
// usernames, and creates the run via handleCreateRun on the given
// project. Returns immediately after remembering the run id.
func (h *mcpHarness) accessControlRun(t *testing.T, projectID int64, alice, bob string) {
	t.Helper()
	raw, err := readFixture("access-control.yaml")
	if err != nil {
		t.Fatalf("read access-control.yaml: %v", err)
	}
	yaml := strings.ReplaceAll(raw, "__CITIZEN_ALICE__", alice)
	yaml = strings.ReplaceAll(yaml, "__CITIZEN_BOB__", bob)
	h.mcpCreateRunInline(t, projectID, yaml)
}

// TestMCPArtifactWriteAndRead is the MCP-layer port of
// TestArtifactWriteAndRead. Exercises the artifacts_json submit
// path, {{artifact:path}} template resolution against the current
// commit, last-write-wins on a second write, and artifact content
// landing on the bare remote.
func TestMCPArtifactWriteAndRead(t *testing.T) {
	h := newMCPHarness(t, "Artifact Writer")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "artifacts.yaml")

	// Bootstrap writes src/hello.py from scratch.
	h.mcpClaimOK(t, "bootstrap")
	h.mcpSubmitArtifacts(t, "bootstrap", "Wrote initial hello.py.", map[string]string{
		"src/hello.py": "def main():\n    print(\"hi\")\n",
	})

	// File landed on the bare remote.
	content, ok := h.readArtifactFile(projectID, "src/hello.py")
	if !ok {
		t.Fatal("artifact file missing on bare remote after write")
	}
	if !strings.Contains(content, "print(\"hi\")") {
		t.Fatalf("unexpected initial content: %q", content)
	}

	// refactor's {{artifact:src/hello.py}} template reference
	// resolves to the current artifact content. Artifacts map is
	// populated too.
	inputs := h.mcpTaskInputs(t, "refactor")
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "print(\"hi\")") {
		t.Fatalf("refactor prompt did not include current artifact content, got: %s", resolved)
	}
	artifactsMap, _ := inputs["artifacts"].(map[string]interface{})
	if _, present := artifactsMap["src/hello.py"]; !present {
		t.Fatalf("expected artifacts map to include src/hello.py, got %v", artifactsMap)
	}

	// Refactor: overwrite with new content (last-write-wins).
	h.mcpClaimOK(t, "refactor")
	h.mcpSubmitArtifacts(t, "refactor", "Refactored to use a constant.", map[string]string{
		"src/hello.py": "GREETING = \"hi\"\n\ndef main():\n    print(GREETING)\n",
	})

	// Run should be completed and the artifact should hold the new content.
	if got := h.runStatus(h.lastRunID)["state"]; got != "completed" {
		t.Fatalf("expected completed after refactor, got %v", got)
	}
	content2, _ := h.readArtifactFile(projectID, "src/hello.py")
	if !strings.Contains(content2, "GREETING") {
		t.Fatalf("artifact was not overwritten: %s", content2)
	}
}

// TestMCPArtifactWriteRejectedForUndeclaredPath is the MCP-layer
// port of TestArtifactWriteRejectedForUndeclaredPath. Submitting a
// path the task didn't declare via writes_artifacts must fail, and
// the task must remain in the claimed state.
func TestMCPArtifactWriteRejectedForUndeclaredPath(t *testing.T) {
	h := newMCPHarness(t, "Sneaky Writer")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "artifacts.yaml")

	h.mcpClaimOK(t, "bootstrap")

	// Sneak an undeclared path — must be rejected (tool error).
	artJSON := `{"src/sneaky.py":"print(\"oops\")"}`
	errText := h.callExpectError(t, "enju_submit_result", map[string]any{
		"task_id":        h.taskID("bootstrap"),
		"artifacts_json": artJSON,
	})
	if !strings.Contains(strings.ToLower(errText), "artifact") &&
		!strings.Contains(strings.ToLower(errText), "declar") &&
		!strings.Contains(strings.ToLower(errText), "sneaky") {
		t.Errorf("expected rejection to mention the undeclared artifact, got: %s", errText)
	}
	// Task state unchanged.
	if state, _ := h.taskGet("bootstrap")["state"].(string); state != "claimed" {
		t.Fatalf("bootstrap should remain claimed after rejection, got %q", state)
	}
}

// TestMCPArtifactListAndGet is the MCP-layer port of
// TestArtifactListAndGetEndpoints. Exercises enju_list_artifacts +
// enju_get_artifact as they'd be called by a real client.
func TestMCPArtifactListAndGet(t *testing.T) {
	h := newMCPHarness(t, "Artifact Lister")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "artifacts.yaml")

	h.mcpClaimOK(t, "bootstrap")
	h.mcpSubmitArtifacts(t, "bootstrap", "init", map[string]string{
		"src/hello.py": "print(\"hello\")\n",
	})

	// List surfaces the artifact path and the producing task id.
	// (The list formatter shows task ids, not writer usernames —
	// the writer is visible via enju_get_artifact below.)
	listText := mcpText(h.mcpListArtifacts(t, projectID, ""))
	if !strings.Contains(listText, "src/hello.py") {
		t.Errorf("list output missing src/hello.py: %s", listText)
	}
	if !strings.Contains(listText, "bootstrap") {
		t.Errorf("list output missing producing task 'bootstrap': %s", listText)
	}

	// Get returns provenance + content.
	getText := mcpText(h.mcpGetArtifact(t, projectID, "src/hello.py"))
	if !strings.Contains(getText, "hello") {
		t.Errorf("get output missing content: %s", getText)
	}
	if !strings.Contains(getText, h.username) {
		t.Errorf("get output missing last_writer: %s", getText)
	}

	// Missing artifact → tool error.
	missingErr := h.callExpectError(t, "enju_get_artifact", map[string]any{
		"project_id": float64(projectID),
		"path":       "does/not/exist.txt",
	})
	if missingErr == "" {
		t.Fatal("expected error text for missing artifact")
	}
}

// TestMCPArtifactFieldsInTaskResponse is the MCP-layer port of
// TestArtifactFieldsInTaskResponse. Inferred reads (from
// {{artifact:...}}) and declared writes come back in the task
// record so the formatter can surface them.
func TestMCPArtifactFieldsInTaskResponse(t *testing.T) {
	h := newMCPHarness(t, "Artifact Fields")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "artifacts.yaml")

	task := h.taskGet("refactor")
	reads, _ := task["reads_artifacts"].([]interface{})
	writes, _ := task["writes_artifacts"].([]interface{})

	if len(writes) != 1 || writes[0] != "src/hello.py" {
		t.Fatalf("expected writes_artifacts = [src/hello.py], got %v", task["writes_artifacts"])
	}
	if len(reads) != 1 || reads[0] != "src/hello.py" {
		t.Fatalf("expected reads_artifacts = [src/hello.py] (inferred), got %v", task["reads_artifacts"])
	}
}

// TestMCPArtifactPermissiveWrites is the MCP-layer port of
// TestArtifactPermissiveWrites. writes_artifacts is an upper
// bound — submitting a subset must succeed.
func TestMCPArtifactPermissiveWrites(t *testing.T) {
	h := newMCPHarness(t, "Permissive")
	projectID := h.createTestProject()

	yaml := `name: "Permissive writes"
version: 1
tasks:
  - id: maybe
    action: answer
    writes_artifacts:
      - src/a.py
      - src/b.py
    prompt: "Optionally update either or both files."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "maybe")
	h.mcpSubmitArtifacts(t, "maybe", "only updated one", map[string]string{
		"src/a.py": "# only this one\n",
	})

	// a.py written, b.py untouched.
	if _, ok := h.readArtifactFile(projectID, "src/a.py"); !ok {
		t.Fatal("expected src/a.py to be written")
	}
	if _, ok := h.readArtifactFile(projectID, "src/b.py"); ok {
		t.Fatal("expected src/b.py NOT to be written")
	}
}

// TestMCPAccessControlDefaultOpen is the MCP-layer port of
// TestAccessControlDefaultOpen. An unrestricted task is claimable
// by any registered citizen.
func TestMCPAccessControlDefaultOpen(t *testing.T) {
	h := newMCPHarness(t, "Access Alice")
	bob := h.newMCPClientAs(t, "Access Bob")
	charlie := h.newMCPClientAs(t, "Access Charlie")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	// Charlie (not listed anywhere) can claim the open task.
	h.mcpClaimAs(t, charlie, "open_task")
}

// TestMCPAccessControlAssignToAllowsListedCitizen is the MCP-layer
// port. A listed citizen can claim an assigned_to task.
func TestMCPAccessControlAssignToAllowsListedCitizen(t *testing.T) {
	h := newMCPHarness(t, "AssignTo Alice")
	bob := h.newMCPClientAs(t, "AssignTo Bob")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	// Alice (in assign_to) claims her task.
	h.mcpClaimOK(t, "assigned_task")
	if state, _ := h.taskGet("assigned_task")["state"].(string); state != "claimed" {
		t.Fatalf("expected claimed, got %q", state)
	}
}

// TestMCPAccessControlAssignToRejectsOtherCitizen is the MCP-layer
// port. A non-listed citizen's claim returns a tool error and
// doesn't change the task state.
func TestMCPAccessControlAssignToRejectsOtherCitizen(t *testing.T) {
	h := newMCPHarness(t, "AssignTo Alice")
	bob := h.newMCPClientAs(t, "AssignTo Bob")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	// Bob tries to claim Alice's assigned_task — rejected as
	// prose inside a non-error tool result (see
	// mcpExpectProseRejection comment).
	h.mcpExpectProseRejection(t, bob, "enju_claim_task",
		map[string]any{"task_id": h.taskID("assigned_task")},
		"assigned")

	// Task unchanged.
	if state, _ := h.taskGet("assigned_task")["state"].(string); state != "ready" {
		t.Fatalf("expected ready after rejected claim, got %q", state)
	}
}

// TestMCPAccessControlAssignToListRejectsWithBothNames is the
// MCP-layer port. The rejection message for a list-assign must
// mention every listed citizen.
func TestMCPAccessControlAssignToListRejectsWithBothNames(t *testing.T) {
	h := newMCPHarness(t, "ListAssign Alice")
	bob := h.newMCPClientAs(t, "ListAssign Bob")
	charlie := h.newMCPClientAs(t, "ListAssign Charlie")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	h.mcpExpectProseRejection(t, charlie, "enju_claim_task",
		map[string]any{"task_id": h.taskID("both_task")},
		h.username, bob.Username())
}

// TestMCPAccessControlRequireRoleAllows is the MCP-layer port of
// TestAccessControlRequireRoleAllowsMatchingCitizen. Promoting
// alice to "reviewer" lets her claim a role-restricted task.
func TestMCPAccessControlRequireRoleAllows(t *testing.T) {
	h := newMCPHarness(t, "RoleAllowed Alice")
	bob := h.newMCPClientAs(t, "RoleAllowed Bob")

	h.mcpSetRole(t, h.username, "reviewer")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	h.mcpClaimOK(t, "role_task")
}

// TestMCPAccessControlRequireRoleRejects is the MCP-layer port of
// TestAccessControlRequireRoleRejectsPlainCitizen. A plain citizen
// can't claim a role-restricted task.
func TestMCPAccessControlRequireRoleRejects(t *testing.T) {
	h := newMCPHarness(t, "RoleRejected Alice")
	bob := h.newMCPClientAs(t, "RoleRejected Bob")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	h.mcpExpectProseRejection(t, bob, "enju_claim_task",
		map[string]any{"task_id": h.taskID("role_task")},
		"role")
}

// TestMCPAccessControlBothRestrictionsMustMatch is the MCP-layer
// port. A task with both assign_to and require_role rejects
// anyone who fails EITHER check.
func TestMCPAccessControlBothRestrictionsMustMatch(t *testing.T) {
	h := newMCPHarness(t, "BothCheck Alice")
	bob := h.newMCPClientAs(t, "BothCheck Bob")
	charlie := h.newMCPClientAs(t, "BothCheck Charlie")

	// Alice is a reviewer; Bob is a plain citizen.
	h.mcpSetRole(t, h.username, "reviewer")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	// Charlie fails assign_to (prose rejection).
	h.mcpExpectProseRejection(t, charlie, "enju_claim_task",
		map[string]any{"task_id": h.taskID("both_task")})

	// Bob passes assign_to but lacks the role.
	h.mcpExpectProseRejection(t, bob, "enju_claim_task",
		map[string]any{"task_id": h.taskID("both_task")},
		"role")

	// Alice passes both.
	h.mcpClaimOK(t, "both_task")
}

// TestMCPAccessControlScalarAssignTo is the MCP-layer port. The
// parser normalizes `assign_to: X` (scalar) into a single-element
// list; the task response surfaces the list form.
func TestMCPAccessControlScalarAssignTo(t *testing.T) {
	h := newMCPHarness(t, "Scalar Alice")
	bob := h.newMCPClientAs(t, "Scalar Bob")

	projectID := h.createTestProject()
	h.accessControlRun(t, projectID, h.username, bob.Username())

	task := h.taskGet("assigned_task")
	assignees, _ := task["assign_to"].([]interface{})
	if len(assignees) != 1 || assignees[0] != h.username {
		t.Fatalf("expected assign_to = [%s], got %v", h.username, task["assign_to"])
	}
}


// TestMCPInvalidateCascadesDownstream is the MCP-layer port of
// TestInvalidateCascadesDownstream. Invalidating an accepted task
// cascades descendants to PENDING but leaves independent branches
// alone; the run flips back from completed to active.
func TestMCPInvalidateCascadesDownstream(t *testing.T) {
	h := newMCPHarness(t, "Cascade Alice")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "branching.yaml")

	// Complete the whole run.
	h.mcpClaimOK(t, "python_pros")
	h.mcpSubmitText(t, "python_pros", "1. ecosystem. 2. simple. 3. fast proto.")
	h.mcpClaimOK(t, "go_pros")
	h.mcpSubmitText(t, "go_pros", "1. single binary. 2. speed. 3. concurrency.")
	h.mcpClaimOK(t, "comparison")
	h.mcpSubmitText(t, "comparison", "Use Go for CLI tools.")

	if got := h.runStatus(h.lastRunID)["state"]; got != "completed" {
		t.Fatalf("expected completed pre-invalidate, got %v", got)
	}

	// Invalidate python_pros via the MCP handler.
	invRes := h.callOK(t, "enju_invalidate_task", map[string]any{
		"task_id": h.taskID("python_pros"),
		"reason":  "LLM hallucinated an advantage",
	})
	invText := mcpText(invRes)
	if !strings.Contains(invText, "comparison") {
		t.Errorf("expected cascade to mention comparison, got: %s", invText)
	}

	// State transitions (structured REST inspection).
	if got := h.taskGet("python_pros")["state"]; got != "ready" {
		t.Fatalf("expected python_pros READY after invalidation, got %v", got)
	}
	if cb := h.taskGet("python_pros")["claimed_by"]; cb != nil && cb != "" {
		t.Fatalf("expected claimed_by cleared, got %v", cb)
	}
	if got := h.taskGet("comparison")["state"]; got != "pending" {
		t.Fatalf("expected comparison PENDING, got %v", got)
	}
	if got := h.taskGet("go_pros")["state"]; got != "accepted" {
		t.Fatalf("go_pros should stay ACCEPTED (independent branch), got %v", got)
	}
	if got := h.runStatus(h.lastRunID)["state"]; got != "active" {
		t.Fatalf("expected run ACTIVE after invalidation, got %v", got)
	}
}

// TestMCPInvalidateAllowsReclaimAndProgression is the MCP-layer
// port of TestInvalidateAllowsReclaimAndProgression. After
// invalidation the target can be re-claimed and re-submitted, and
// its downstream automatically re-opens.
func TestMCPInvalidateAllowsReclaimAndProgression(t *testing.T) {
	h := newMCPHarness(t, "Reclaim Alice")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "branching.yaml")

	// Complete everything.
	h.mcpClaimOK(t, "python_pros")
	h.mcpSubmitText(t, "python_pros", "original python answer")
	h.mcpClaimOK(t, "go_pros")
	h.mcpSubmitText(t, "go_pros", "original go answer")
	h.mcpClaimOK(t, "comparison")
	h.mcpSubmitText(t, "comparison", "original comparison")

	// Invalidate python_pros.
	h.mcpInvalidate(t, "python_pros", "needs redo")

	// Re-claim + re-submit.
	h.mcpClaimOK(t, "python_pros")
	h.mcpSubmitText(t, "python_pros", "corrected python answer")

	// Comparison auto-promoted back to READY.
	if got := h.taskGet("comparison")["state"]; got != "ready" {
		t.Fatalf("expected comparison READY after upstream re-complete, got %v", got)
	}
	h.mcpClaimOK(t, "comparison")
	h.mcpSubmitText(t, "comparison", "updated comparison")

	if got := h.runStatus(h.lastRunID)["state"]; got != "completed" {
		t.Fatalf("expected completed after re-run, got %v", got)
	}
}

// TestMCPInvalidateRollsBackArtifactToPriorWriter is the MCP-layer
// port. Invalidating the second writer rolls the artifact index
// back to the first writer's commit; re-claim sees the rolled-back
// content in its resolved prompt.
func TestMCPInvalidateRollsBackArtifactToPriorWriter(t *testing.T) {
	h := newMCPHarness(t, "Rollback Alice")
	projectID := h.createTestProject()

	// Run 1: create the artifact.
	v1YAML := `name: "Writer v1"
version: 1
tasks:
  - id: write_v1
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write the first version."
`
	firstID := h.mcpCreateRunInline(t, projectID, v1YAML)
	_ = firstID
	h.mcpClaimOK(t, "write_v1")
	h.mcpSubmitArtifacts(t, "write_v1", "v1 result", map[string]string{
		"notes/intro.md": "version ONE",
	})

	// Run 2: overwrite.
	v2YAML := `name: "Writer v2"
version: 1
tasks:
  - id: write_v2
    action: answer
    reads_artifacts: [notes/intro.md]
    writes_artifacts: [notes/intro.md]
    prompt: "Read {{artifact:notes/intro.md}} and replace with v2."
`
	h.mcpCreateRunInline(t, projectID, v2YAML)
	h.mcpClaimOK(t, "write_v2")
	h.mcpSubmitArtifacts(t, "write_v2", "v2 result", map[string]string{
		"notes/intro.md": "version TWO",
	})
	content2, _ := h.readArtifactFile(projectID, "notes/intro.md")
	if content2 != "version TWO" {
		t.Fatalf("expected v2 on disk, got %q", content2)
	}

	// Invalidate write_v2 — rollback restores v1.
	invRes := h.callOK(t, "enju_invalidate_task", map[string]any{
		"task_id": h.taskID("write_v2"),
		"reason":  "wrong direction",
	})
	invText := mcpText(invRes)
	if !strings.Contains(invText, "notes/intro.md") {
		t.Errorf("expected invalidate output to mention the rolled-back artifact, got: %s", invText)
	}
	if !strings.Contains(invText, "write_v1") {
		t.Errorf("expected invalidate output to mention the restored writer, got: %s", invText)
	}

	// Artifact index now points at write_v1.
	list := h.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID))
	if len(list) != 1 {
		t.Fatalf("expected 1 artifact in index, got %d", len(list))
	}
	if lt, _ := list[0].(map[string]interface{})["last_task_id"].(string); !strings.HasSuffix(lt, ":write_v1") {
		t.Fatalf("expected last_task_id write_v1 after rollback, got %v", lt)
	}

	// Re-claim write_v2 — client-side resolution must read v1
	// content from the rolled-back commit SHA.
	h.mcpClaimOK(t, "write_v2")
	inputs := h.mcpTaskInputs(t, "write_v2")
	artifacts, _ := inputs["artifacts"].(map[string]interface{})
	if artifacts["notes/intro.md"] != "version ONE" {
		t.Fatalf("re-claim should see v1 in artifacts map, got %v", artifacts["notes/intro.md"])
	}
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "version ONE") {
		t.Fatalf("re-claim resolved prompt should contain v1, got: %s", resolved)
	}
	if strings.Contains(resolved, "version TWO") {
		t.Fatalf("re-claim resolved prompt should NOT contain v2, got: %s", resolved)
	}
}

// TestMCPInvalidateFirstWriterDeletesArtifact is the MCP-layer
// port. If the invalidated task was the artifact's only writer,
// the DB index row is dropped (git history stays intact).
func TestMCPInvalidateFirstWriterDeletesArtifact(t *testing.T) {
	h := newMCPHarness(t, "FirstWriter")
	projectID := h.createTestProject()

	yaml := `name: "Creator"
version: 1
tasks:
  - id: create
    action: answer
    writes_artifacts: [config/settings.yaml]
    prompt: "Create the config."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "create")
	h.mcpSubmitArtifacts(t, "create", "made config", map[string]string{
		"config/settings.yaml": "key: value",
	})

	if _, ok := h.readArtifactFile(projectID, "config/settings.yaml"); !ok {
		t.Fatal("expected config file to exist pre-invalidate")
	}

	// Invalidate — expect deletion.
	invRes := h.callOK(t, "enju_invalidate_task", map[string]any{
		"task_id": h.taskID("create"),
		"reason":  "bad config",
	})
	invText := mcpText(invRes)
	if !strings.Contains(strings.ToLower(invText), "delet") &&
		!strings.Contains(strings.ToLower(invText), "removed") {
		t.Errorf("expected invalidate output to mention deletion, got: %s", invText)
	}

	// Artifact index empty.
	list := h.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID))
	if len(list) != 0 {
		t.Fatalf("expected empty artifact index after deletion, got %d entries", len(list))
	}
}

// TestMCPInvalidateWalkerSkipsPreviouslyInvalidatedWriter is the
// MCP-layer port. After a previous invalidation has returned a
// writer's task to READY, its commit must be treated as a "ghost
// revision" during the next walker pass.
func TestMCPInvalidateWalkerSkipsPreviouslyInvalidatedWriter(t *testing.T) {
	h := newMCPHarness(t, "Walker Alice")
	projectID := h.createTestProject()

	yaml1 := `name: "v1"
version: 1
tasks:
  - id: write_v1
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write v1."
`
	h.mcpCreateRunInline(t, projectID, yaml1)
	h.mcpClaimOK(t, "write_v1")
	h.mcpSubmitArtifacts(t, "write_v1", "first", map[string]string{"notes/intro.md": "version ONE"})

	yaml2 := `name: "v2"
version: 1
tasks:
  - id: write_v2
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write v2."
`
	h.mcpCreateRunInline(t, projectID, yaml2)
	h.mcpClaimOK(t, "write_v2")
	h.mcpSubmitArtifacts(t, "write_v2", "second", map[string]string{"notes/intro.md": "version TWO"})

	// First invalidation: write_v2 → rollback to write_v1.
	// After this, write_v2 is READY — its commit is a ghost revision.
	h.mcpInvalidate(t, "write_v2", "wrong")

	// Sanity.
	h.lastRunSeq = 1
	if got := h.taskGet("write_v1")["state"]; got != "accepted" {
		t.Fatalf("write_v1 should stay accepted, got %v", got)
	}
	h.lastRunSeq = 2
	if got := h.taskGet("write_v2")["state"]; got != "ready" {
		t.Fatalf("write_v2 should be READY after invalidation, got %v", got)
	}

	// Second invalidation: write_v1. The walker must NOT pick
	// write_v2 (ghost) as the prior writer — artifact must delete.
	h.lastRunSeq = 1
	h.mcpInvalidate(t, "write_v1", "original was wrong too")

	list := h.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID))
	if len(list) != 0 {
		t.Fatalf("expected empty index after double-invalidate, got %d: %v", len(list), list)
	}
}

// TestMCPInvalidateCascadesAcrossRunsViaArtifactReads is the
// MCP-layer port. Invalidating an artifact writer must cascade
// readers in OTHER runs too, flipping them from ACCEPTED to
// PENDING via the artifact-aware scheduler.
func TestMCPInvalidateCascadesAcrossRunsViaArtifactReads(t *testing.T) {
	h := newMCPHarness(t, "CrossRun Alice")
	projectID := h.createTestProject()

	// Run 1: writer.
	yaml1 := `name: "writer"
version: 1
tasks:
  - id: write_v1
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write it."
`
	h.mcpCreateRunInline(t, projectID, yaml1)
	h.mcpClaimOK(t, "write_v1")
	h.mcpSubmitArtifacts(t, "write_v1", "made v1", map[string]string{"notes/intro.md": "version ONE"})

	// Run 2: reader (separate run).
	yaml2 := `name: "reader"
version: 1
tasks:
  - id: summarize
    action: answer
    reads_artifacts: [notes/intro.md]
    prompt: "Summarize {{artifact:notes/intro.md}}"
`
	h.mcpCreateRunInline(t, projectID, yaml2)
	h.mcpClaimOK(t, "summarize")
	h.mcpSubmitText(t, "summarize", "summary of version ONE")
	if got := h.taskGet("summarize")["state"]; got != "accepted" {
		t.Fatalf("summarize should be accepted pre-invalidate, got %v", got)
	}

	// Invalidate write_v1 — summarize must cascade across runs.
	h.lastRunSeq = 1
	invRes := h.callOK(t, "enju_invalidate_task", map[string]any{
		"task_id": h.taskID("write_v1"),
		"reason":  "testing cross-run cascade",
	})
	invText := mcpText(invRes)
	if !strings.Contains(invText, "summarize") {
		t.Errorf("expected invalidate output to mention cross-run reader 'summarize', got: %s", invText)
	}

	// summarize dropped to PENDING (artifact-aware scheduler
	// blocks promotion until the artifact is re-written).
	h.lastRunSeq = 2
	if got := h.taskGet("summarize")["state"]; got != "pending" {
		t.Fatalf("expected summarize PENDING after cross-run cascade, got %v", got)
	}

	// Run 2 should be back to active.
	run2 := h.get(fmt.Sprintf("/api/v1/projects/%d/runs/2", projectID))
	if run2["state"] != "active" {
		t.Fatalf("expected run 2 active after cross-run cascade, got %v", run2["state"])
	}
}

// TestMCPInvalidateRejectsNonAcceptedTarget is the MCP-layer
// port. Invalidating a task that isn't in the ACCEPTED state must
// fail cleanly without changing the task.
func TestMCPInvalidateRejectsNonAcceptedTarget(t *testing.T) {
	h := newMCPHarness(t, "InvRejecter")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "branching.yaml")

	// python_pros is READY, not ACCEPTED — invalidate should fail.
	// The handler surfaces the rejection as a non-error prose
	// result (same shape as the claim rejections).
	h.mcpExpectProseRejection(t, h.client, "enju_invalidate_task",
		map[string]any{"task_id": h.taskID("python_pros"), "reason": "testing rejection"})

	if got := h.taskGet("python_pros")["state"]; got != "ready" {
		t.Fatalf("expected python_pros unchanged (READY), got %v", got)
	}

	// After completion, invalidate works.
	h.mcpClaimOK(t, "python_pros")
	h.mcpSubmitText(t, "python_pros", "now accepted")
	h.mcpInvalidate(t, "python_pros", "try again")
}

// TestMCPReviewMetadataAuditTrail is the MCP-layer port of
// TestReviewMetadataAuditTrail. The review commit's metadata.json
// must carry action + decision + reviews_target for git-log
// archaeology. Non-review tasks must NOT leak those fields.
func TestMCPReviewMetadataAuditTrail(t *testing.T) {
	h := newMCPHarness(t, "Audit Drafter")
	reviewer := h.newMCPClientAs(t, "Audit Reviewer")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "review.yaml")

	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "Enju is a DAG-based task coordinator.")
	h.mcpClaimAs(t, reviewer, "check")
	h.mcpSubmitReviewAs(t, reviewer, "check", "Looks accurate.", "approve")

	// Review metadata.json carries the audit fields.
	reviewMeta := h.mcpBareMetadataJSON(t, "check")
	if action, _ := reviewMeta["action"].(string); action != "review" {
		t.Errorf("expected review metadata.action=review, got %v", reviewMeta["action"])
	}
	if decision, _ := reviewMeta["decision"].(string); decision != "approve" {
		t.Errorf("expected review metadata.decision=approve, got %v", reviewMeta["decision"])
	}
	if target, _ := reviewMeta["reviews_target"].(string); target != "draft" {
		t.Errorf("expected review metadata.reviews_target=draft, got %v", reviewMeta["reviews_target"])
	}

	// Draft metadata.json (non-review) must NOT carry those fields.
	draftMeta := h.mcpBareMetadataJSON(t, "draft")
	if _, leaks := draftMeta["decision"]; leaks {
		t.Error("draft metadata should not include 'decision'")
	}
	if _, leaks := draftMeta["reviews_target"]; leaks {
		t.Error("draft metadata should not include 'reviews_target'")
	}
	if _, leaks := draftMeta["action"]; leaks {
		t.Error("draft metadata should not include 'action' (review-only field)")
	}
}

// TestMCPReviewRejectMetadataCarriesVerdict is the MCP-layer port
// of TestReviewRejectMetadataCarriesVerdict. After a
// request_changes cascade, the DB's review_decision is cleared
// but metadata.json preserves the verdict — that divergence IS
// the audit trail.
func TestMCPReviewRejectMetadataCarriesVerdict(t *testing.T) {
	h := newMCPHarness(t, "Reject-Audit Drafter")
	reviewer := h.newMCPClientAs(t, "Reject-Audit Reviewer")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "review.yaml")

	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "A draft that will be rejected.")
	h.mcpClaimAs(t, reviewer, "check")
	h.mcpSubmitReviewAs(t, reviewer, "check", "Needs more detail.", "request_changes")

	// metadata.json still records the verdict.
	meta := h.mcpBareMetadataJSON(t, "check")
	if decision, _ := meta["decision"].(string); decision != "request_changes" {
		t.Errorf("expected metadata.decision=request_changes, got %v", meta["decision"])
	}
	if target, _ := meta["reviews_target"].(string); target != "draft" {
		t.Errorf("expected metadata.reviews_target=draft, got %v", meta["reviews_target"])
	}

	// DB field cleared by the cascade — audit IS the git commit.
	if got, _ := h.taskGet("check")["review_decision"].(string); got != "" {
		t.Errorf("expected check.review_decision cleared after cascade, got %q", got)
	}
}


// mcpSubmitDiscoverWithList is the dynamic-for_each variant of the
// submit helpers: it calls enju_submit_result with outputs_json
// carrying a list<string> value. The MCP handler splits
// string-valued vs list-valued outputs internally and routes the
// list through to the coordinator's materialization pass.
func (h *mcpHarness) mcpSubmitDiscoverWithList(t *testing.T, genes []string) {
	t.Helper()
	items := make([]any, len(genes))
	for i, g := range genes {
		items[i] = g
	}
	outputs := map[string]any{"gene_symbols": items}
	outJSON, err := json.Marshal(outputs)
	if err != nil {
		t.Fatalf("marshal list outputs: %v", err)
	}
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id":      h.taskID("discover"),
		"outputs_json": string(outJSON),
	})
}

// mcpCountTasksByDef counts tasks in a list that share a
// task_def_id. Used by dynamic-materialization tests.
func mcpCountTasksByDef(tasks []interface{}, defID string) int {
	n := 0
	for _, raw := range tasks {
		tk, _ := raw.(map[string]interface{})
		if id, _ := tk["task_def_id"].(string); id == defID {
			n++
		}
	}
	return n
}

// TestMCPVotePureDecisionNoSkipCascade is the MCP-layer port. A
// vote with no activates: on any option is a pure decision — no
// branches are skipped; downstream tasks just need `pick` to
// accept.
func TestMCPVotePureDecisionNoSkipCascade(t *testing.T) {
	h := newMCPHarness(t, "PureVote")
	projectID := h.createTestProject()

	yaml := `name: "Pure decision"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "Pick one."
    options:
      - {id: a, label: "Option A"}
      - {id: b, label: "Option B"}
  - id: followup
    action: answer
    depends_on: [pick]
    prompt: "Do the thing."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "pick")
	h.mcpSubmitVote(t, "pick", "A feels right.", "a")

	if got := h.taskGet("pick")["state"]; got != "accepted" {
		t.Fatalf("expected pick accepted, got %v", got)
	}
	if got := h.taskGet("pick")["vote_choice"]; got != "a" {
		t.Errorf("expected vote_choice=a, got %v", got)
	}
	// Pure decision: no tasks should have gone to SKIPPED.
	tasks := h.runTasks(h.lastRunID)
	for _, raw := range tasks {
		m, _ := raw.(map[string]interface{})
		if st, _ := m["state"].(string); st == "skipped" {
			t.Errorf("no task should be skipped on pure-decision vote, got skipped: %v", m["id"])
		}
	}
	// followup should be ready.
	if got := h.taskGet("followup")["state"]; got != "ready" {
		t.Errorf("expected followup ready, got %v", got)
	}
}

// TestMCPVoteInvalidationResetsSkipped is the MCP-layer port.
// Invalidating a resolved vote resets every SKIPPED branch to
// PENDING; re-voting for the other option swaps which branch is
// live.
func TestMCPVoteInvalidationResetsSkipped(t *testing.T) {
	h := newMCPHarness(t, "VoteReset")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "vote-gate.yaml")

	// First vote: python wins.
	h.mcpClaimOK(t, "analysis")
	h.mcpSubmitText(t, "analysis", "Analysis.")
	h.mcpClaimOK(t, "pick")
	h.mcpSubmitVote(t, "pick", "Python.", "python")

	if got := h.taskGet("build_rust")["state"]; got != "skipped" {
		t.Fatalf("expected build_rust skipped before invalidate, got %v", got)
	}

	// Invalidate the vote — every downstream flips back to PENDING.
	h.mcpInvalidate(t, "pick", "want rust instead")
	if got := h.taskGet("pick")["state"]; got != "ready" {
		t.Fatalf("expected pick ready after invalidate, got %v", got)
	}
	for _, id := range []string{"build_python", "ship_python", "build_rust", "ship_rust"} {
		if got := h.taskGet(id)["state"]; got != "pending" {
			t.Errorf("expected %s pending after vote invalidation, got %v", id, got)
		}
	}

	// Re-vote for rust.
	h.mcpClaimOK(t, "pick")
	h.mcpSubmitVote(t, "pick", "Changed my mind.", "rust")

	if got := h.taskGet("build_python")["state"]; got != "skipped" {
		t.Errorf("expected build_python skipped after re-vote, got %v", got)
	}
	if got := h.taskGet("build_rust")["state"]; got != "ready" {
		t.Errorf("expected build_rust ready after re-vote, got %v", got)
	}

	// Finish rust branch to completion.
	h.mcpClaimOK(t, "build_rust")
	h.mcpSubmitText(t, "build_rust", "Rust built.")
	h.mcpClaimOK(t, "ship_rust")
	h.mcpSubmitText(t, "ship_rust", "Rust shipped.")

	if got := h.runStatus(h.lastRunID)["state"]; got != "completed" {
		t.Fatalf("expected completed after rust branch, got %v", got)
	}
}

// TestMCPVoteMultiCitizenCollectsThenResolves is the MCP-layer
// port. min_quorum:3 forces the tally to wait for all citizens;
// majority resolves on the third submission.
func TestMCPVoteMultiCitizenCollectsThenResolves(t *testing.T) {
	h := newMCPHarness(t, "QuorumA")
	bob := h.newMCPClientAs(t, "QuorumB")
	charlie := h.newMCPClientAs(t, "QuorumC")

	projectID := h.createTestProject()
	yaml := `name: "Quorum Vote"
version: 1
tasks:
  - id: pick
    action: vote
    citizens: 3
    min_quorum: 3
    threshold: majority
    prompt: "Pick a database."
    options:
      - id: duckdb
        label: "DuckDB"
        activates: [build_duckdb]
      - id: sqlite
        label: "SQLite"
        activates: [build_sqlite]
  - id: build_duckdb
    action: answer
    prompt: "Build with DuckDB."
  - id: build_sqlite
    action: answer
    prompt: "Build with SQLite."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// Alice claims + votes — still collecting.
	h.mcpClaimOK(t, "pick")
	h.mcpSubmitVote(t, "pick", "duckdb is fast enough", "duckdb")
	if got := h.taskGet("pick")["state"]; got != "collecting" {
		t.Fatalf("expected collecting after 1 vote, got %v", got)
	}

	// Bob — still collecting.
	h.mcpClaimAs(t, bob, "pick")
	h.mcpSubmitVoteAs(t, bob, "pick", "duckdb for me too", "duckdb")
	if got := h.taskGet("pick")["state"]; got != "collecting" {
		t.Fatalf("expected collecting after 2 votes, got %v", got)
	}

	// Charlie votes minority — majority resolves on his submit.
	h.mcpClaimAs(t, charlie, "pick")
	h.mcpSubmitVoteAs(t, charlie, "pick", "sqlite ftw", "sqlite")
	if got := h.taskGet("pick")["state"]; got != "accepted" {
		t.Fatalf("expected accepted after 3rd vote, got %v", got)
	}
	if got := h.taskGet("pick")["vote_choice"]; got != "duckdb" {
		t.Errorf("expected winning=duckdb, got %v", got)
	}
	if got := h.taskGet("build_sqlite")["state"]; got != "skipped" {
		t.Errorf("expected build_sqlite skipped, got %v", got)
	}
	if got := h.taskGet("build_duckdb")["state"]; got != "ready" {
		t.Errorf("expected build_duckdb ready, got %v", got)
	}
}

// TestMCPVoteMultiCitizenRejectDoubleClaim is the MCP-layer port.
// A single citizen can't hold two slots on a multi-citizen task.
func TestMCPVoteMultiCitizenRejectDoubleClaim(t *testing.T) {
	h := newMCPHarness(t, "Double")
	projectID := h.createTestProject()

	yaml := `name: "Double claim test"
version: 1
tasks:
  - id: pick
    action: vote
    citizens: 3
    options:
      - {id: a}
      - {id: b}
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// First claim succeeds.
	h.mcpClaimOK(t, "pick")

	// Second claim as same citizen — prose rejection with
	// "already have an active claim".
	h.mcpExpectProseRejection(t, h.client, "enju_claim_task",
		map[string]any{"task_id": h.taskID("pick")},
		"already have an active claim")
}

// TestMCPVoteMultiCitizenCapAtCitizensLimit is the MCP-layer
// port. A 4th claimer on a citizens:3 task gets a cap rejection.
func TestMCPVoteMultiCitizenCapAtCitizensLimit(t *testing.T) {
	h := newMCPHarness(t, "CapA")
	bob := h.newMCPClientAs(t, "CapB")
	charlie := h.newMCPClientAs(t, "CapC")
	dave := h.newMCPClientAs(t, "CapD")

	projectID := h.createTestProject()
	yaml := `name: "Cap test"
version: 1
tasks:
  - id: pick
    action: vote
    citizens: 3
    options:
      - {id: a}
      - {id: b}
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "pick")
	h.mcpClaimAs(t, bob, "pick")
	h.mcpClaimAs(t, charlie, "pick")

	// Dave is over the cap.
	h.mcpExpectProseRejection(t, dave, "enju_claim_task",
		map[string]any{"task_id": h.taskID("pick")},
		"citizens cap")
}

// TestMCPMultiReviewerAnyRejectKills is the MCP-layer port.
// any-reject-kills is the default: a single request_changes
// immediately resolves the task; later reviewers never get to
// submit.
func TestMCPMultiReviewerAnyRejectKills(t *testing.T) {
	h := newMCPHarness(t, "AnyRejectA")
	bob := h.newMCPClientAs(t, "AnyRejectB")
	charlie := h.newMCPClientAs(t, "AnyRejectC")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "review-multi.yaml")

	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "A summary that bob will reject.")

	h.mcpClaimOK(t, "check")
	h.mcpClaimAs(t, bob, "check")
	h.mcpClaimAs(t, charlie, "check")

	// Alice approves — still collecting.
	h.mcpSubmitReview(t, "check", "LGTM.", "approve")
	if got := h.taskGet("check")["state"]; got != "collecting" {
		t.Fatalf("expected collecting after alice approve, got %v", got)
	}

	// Bob request_changes — any-reject fires immediately.
	h.mcpSubmitReviewAs(t, bob, "check", "This needs work.", "request_changes")

	// Draft bounces back to READY; check itself goes PENDING
	// (auto-invalidated through the dep edge).
	if got := h.taskGet("draft")["state"]; got != "ready" {
		t.Errorf("expected draft ready after request_changes, got %v", got)
	}
	if got := h.taskGet("check")["state"]; got != "pending" {
		t.Errorf("expected check pending after cascade, got %v", got)
	}
	_ = charlie // never submitted — task was already resolved.
}

// TestMCPMultiReviewerHardRejectOverridesSoft is the MCP-layer
// port. A single hard reject in a multi-reviewer tally overrides
// any soft verdicts — the target goes FAILED, not READY.
func TestMCPMultiReviewerHardRejectOverridesSoft(t *testing.T) {
	h := newMCPHarness(t, "HardA")
	bob := h.newMCPClientAs(t, "HardB")
	charlie := h.newMCPClientAs(t, "HardC")

	projectID := h.createTestProject()
	yaml := `
name: hard vs soft reject
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write something."
  - id: check
    action: review
    reviews: draft
    citizens: 3
    prompt: "Review the draft."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "A draft.")

	h.mcpClaimOK(t, "check")
	h.mcpClaimAs(t, bob, "check")
	h.mcpClaimAs(t, charlie, "check")

	// Bob hard-rejects first — hasHardReject wins.
	h.mcpSubmitReviewAs(t, bob, "check", "Completely wrong.", "reject")

	// Target goes FAILED, not READY.
	if got := h.taskGet("draft")["state"]; got != "failed" {
		t.Errorf("expected draft FAILED after hard reject, got %v", got)
	}
}

// TestMCPRejectCascadesFailArtifactAndDownstream verifies the
// full reject-cascade contract introduced to close a semantic
// gap: before this, a writer rejected on review went FAILED,
// but the artifact it wrote stayed in the project's artifact
// index (pointing at a rejected commit) and downstream tasks
// with depends_on on the writer stalled in PENDING forever.
//
// Expected behavior now (see docs/rollback.md § Rejection vs
// invalidation):
//   - writer → FAILED (terminal, not back to READY — that's
//     request_changes)
//   - writer's written artifact is removed from the index
//     (no prior writer to roll back to)
//   - intra-run depends_on descendants → SKIPPED with
//     skip_reason = "upstream failed: <writerID>"
//   - run_status renders ⊘ + "(upstream failed: X)" on those
//     rows, distinct from the ⚫ used for vote-cascade skips
func TestMCPRejectCascadesFailArtifactAndDownstream(t *testing.T) {
	h := newMCPHarness(t, "RCReviewer")
	projectID := h.createTestProject()

	yaml := `name: "reject cascade"
version: 1
tasks:
  - id: write_data
    action: answer
    writes_artifacts: [data/payload.md]
    prompt: "Write the payload."
  - id: check
    action: review
    reviews: write_data
    prompt: "Review the payload."
  - id: consume
    action: answer
    depends_on: [write_data]
    prompt: "Use the payload: {{write_data.content}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// 1. Writer submits with artifact. This upserts the
	//    artifact index ahead of review resolution.
	h.mcpClaimOK(t, "write_data")
	h.mcpSubmitArtifacts(t, "write_data", "the payload", map[string]string{
		"data/payload.md": "THE DATA",
	})

	// Sanity: artifact is in the index pointing at write_data.
	artifactsBefore := h.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID))
	if len(artifactsBefore) != 1 {
		t.Fatalf("expected 1 artifact before reject, got %d", len(artifactsBefore))
	}
	if last, _ := artifactsBefore[0].(map[string]interface{})["last_task_id"].(string); !strings.HasSuffix(last, ":write_data") {
		t.Fatalf("expected index to point at write_data before reject, got %q", last)
	}

	// Downstream consume is PENDING — its upstream is not
	// yet accepted (review gate open).
	if got := h.taskGet("consume")["state"]; got != "pending" {
		t.Fatalf("expected consume PENDING before review, got %v", got)
	}

	// 2. Reviewer hard-rejects. This fires performFailCascade.
	h.mcpClaimOK(t, "check")
	submitRes := h.mcpSubmitReview(t, "check", "Not good enough.", "reject")

	// Submit message must describe terminal failure, not the
	// stale request_changes "bounced back to READY" phrasing.
	submitText := mcpText(submitRes)
	if strings.Contains(submitText, "bounced back to READY") {
		t.Errorf("reject submit message should not say 'bounced back to READY' (that's request_changes); got:\n%s", submitText)
	}
	if !strings.Contains(submitText, "rejected (terminal)") {
		t.Errorf("expected reject submit message to say 'rejected (terminal)'; got:\n%s", submitText)
	}
	if !strings.Contains(submitText, "rolled back") {
		t.Errorf("expected reject submit message to mention artifact rollback; got:\n%s", submitText)
	}

	// 3. Writer is terminally FAILED.
	w := h.taskGet("write_data")
	if got := w["state"]; got != "failed" {
		t.Fatalf("expected write_data FAILED after reject, got %v", got)
	}
	if fr, _ := w["fail_reason"].(string); fr == "" {
		t.Errorf("expected write_data fail_reason set, got empty")
	}

	// 4. Artifact was removed from the index (no prior writer).
	artifactsAfter := h.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID))
	if len(artifactsAfter) != 0 {
		t.Errorf("expected artifact removed after reject cascade, got %d entries: %v", len(artifactsAfter), artifactsAfter)
	}

	// 5. Downstream depends_on descendant is SKIPPED with a
	//    reason that identifies the failing upstream.
	c := h.taskGet("consume")
	if got := c["state"]; got != "skipped" {
		t.Fatalf("expected consume SKIPPED after reject, got %v", got)
	}
	reason, _ := c["skip_reason"].(string)
	if !strings.Contains(reason, "upstream failed:") {
		t.Errorf("expected skip_reason to mention upstream failure, got %q", reason)
	}
	if !strings.Contains(reason, "write_data") {
		t.Errorf("expected skip_reason to name write_data, got %q", reason)
	}

	// 6. run_status tree renders the distinct ⊘ glyph + the
	//    upstream-failed annotation on the skipped row.
	status := h.mcpRunStatusText(t)
	if !strings.Contains(status, "⊘") {
		t.Errorf("expected run_status to contain ⊘ for upstream-failed skip, got:\n%s", status)
	}
	if !strings.Contains(status, "upstream failed:") {
		t.Errorf("expected run_status to annotate with 'upstream failed:', got:\n%s", status)
	}
}

// TestMCPRunStatusMermaidFormat exercises the opt-in
// format="mermaid" path on enju_run_status. For large or
// complex DAGs the text tree gets unreadable; emitting
// Mermaid `flowchart TD` lets the user paste the graph into
// mermaid.live, a README, or the preprint directly.
func TestMCPRunStatusMermaidFormat(t *testing.T) {
	h := newMCPHarness(t, "MermaidA")
	projectID := h.createTestProject()

	yaml := `name: "mermaid diagram run"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: check
    action: answer
    depends_on: [draft]
    prompt: "Check: {{draft.content}}"
  - id: publish
    action: answer
    depends_on: [check]
    prompt: "Publish."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// Fetch run_id from the default status first so we call
	// the mermaid variant with the right seq.
	res := h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
		"format":     "mermaid",
	})
	out := mcpText(res)
	if !strings.Contains(out, "```mermaid") {
		t.Errorf("expected fenced mermaid block; got:\n%s", out)
	}
	if !strings.Contains(out, "flowchart TD") {
		t.Errorf("expected 'flowchart TD' header; got:\n%s", out)
	}
	// Every task node should be present with its short name.
	for _, name := range []string{"draft", "check", "publish"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected node label %q in mermaid output; got:\n%s", name, out)
		}
	}
	// Depends_on edges must appear as Mermaid arrows.
	if !strings.Contains(out, "-->") {
		t.Errorf("expected at least one --> edge; got:\n%s", out)
	}
	// Exactly two edges in a linear chain (draft→check, check→publish).
	if got := strings.Count(out, "-->"); got != 2 {
		t.Errorf("expected 2 edges in linear chain, got %d; output:\n%s", got, out)
	}
	// Class definitions are what make downstream renderers
	// color nodes by state.
	if !strings.Contains(out, "classDef accepted") {
		t.Errorf("expected classDef declarations; got:\n%s", out)
	}

	// Default format should still render the textual summary
	// (the opt-in nature of the knob).
	res2 := h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
	})
	defaultOut := mcpText(res2)
	if strings.Contains(defaultOut, "flowchart TD") {
		t.Errorf("default format should not include mermaid syntax; got:\n%s", defaultOut)
	}
}

// TestMCPMermaidCrossRunArtifactEdges exercises the opt-in
// include_external flag. A task in run #2 reads an artifact
// written by run #1. Default Mermaid output omits the
// cross-run dependency (intra-run graph only); with
// include_external=true the artifact shows up as a dashed
// "📎 <path> (from run #N)" external node with an edge into
// the reader, making the full data-flow explicit. Needed for
// preprint figures tracing upstream provenance, or audit
// contexts where "where did this input come from?" matters.
func TestMCPMermaidCrossRunArtifactEdges(t *testing.T) {
	h := newMCPHarness(t, "CrossRunArtifactA")
	projectID := h.createTestProject()

	// Run #1: produce the artifact.
	v1YAML := `name: "writer run"
version: 1
tasks:
  - id: write_intro
    action: answer
    writes_artifacts: [data/intro.md]
    prompt: "Write the intro."
`
	h.mcpCreateRunInline(t, projectID, v1YAML)
	h.mcpClaimOK(t, "write_intro")
	h.mcpSubmitArtifacts(t, "write_intro", "intro v1", map[string]string{
		"data/intro.md": "THE INTRO",
	})

	// Run #2: read the artifact from run #1.
	v2YAML := `name: "reader run"
version: 1
tasks:
  - id: consume
    action: answer
    reads_artifacts: [data/intro.md]
    prompt: "Using {{artifact:data/intro.md}}, do a thing."
`
	h.mcpCreateRunInline(t, projectID, v2YAML)

	// Default mermaid output on run #2: no external node. The
	// cross-run artifact is invisible by design — keeps the
	// "show me this run" use case focused.
	resDefault := h.callOK(t, "enju_run_status", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(2),
		"format":     "mermaid",
	})
	defaultOut := mcpText(resDefault)
	if strings.Contains(defaultOut, "ext_art_") {
		t.Errorf("default mermaid should not include external artifact nodes; got:\n%s", defaultOut)
	}
	if strings.Contains(defaultOut, "from run #1") {
		t.Errorf("default mermaid should not mention cross-run writer; got:\n%s", defaultOut)
	}

	// With include_external=true, run #1's writer shows up as
	// an external 📎 node with the `external` class, and an
	// edge flows from that node into the reader task.
	resExt := h.callOK(t, "enju_run_status", map[string]any{
		"project_id":       float64(projectID),
		"run_id":           float64(2),
		"format":           "mermaid",
		"include_external": true,
	})
	extOut := mcpText(resExt)
	if !strings.Contains(extOut, "📎 data/intro.md") {
		t.Errorf("expected 📎 external node labeling the artifact; got:\n%s", extOut)
	}
	if !strings.Contains(extOut, "from run #1") {
		t.Errorf("expected writer run id surfaced in external node label; got:\n%s", extOut)
	}
	if !strings.Contains(extOut, ":::external") {
		t.Errorf("expected external class on the artifact node; got:\n%s", extOut)
	}
	// Edge: external node → reader. Node id is prefixed
	// ext_art_ and the reader's full task id gets the standard
	// t_ sanitization.
	if !strings.Contains(extOut, "ext_art_data_intro_md --> t_1_2_consume") {
		t.Errorf("expected edge from external artifact node to consume task; got:\n%s", extOut)
	}

	// enju_export_diagram honors include_external too — same
	// artifact node + edge appear in the committed .mmd.
	expRes := h.callOK(t, "enju_export_diagram", map[string]any{
		"project_id":       float64(projectID),
		"run_id":           float64(2),
		"phase":            "with_external",
		"include_external": true,
	})
	expOut := mcpText(expRes)
	if !strings.Contains(expOut, "📎 data/intro.md") {
		t.Errorf("export_diagram with include_external=true missing external artifact node; got:\n%s", expOut)
	}
	// File content should also contain the external node (raw
	// .mmd, no fences).
	// Path is .enju/runs/{run_seq}/graph/..., where run_seq is
	// the 2nd run in this project — so "2", not some global id.
	fileBody, ok := h.readRepoFile(projectID, ".enju/runs/2/graph/with_external.mmd")
	if !ok {
		t.Fatalf("expected with_external.mmd in bare remote")
	}
	if !strings.Contains(string(fileBody), "ext_art_data_intro_md") {
		t.Errorf("committed .mmd file missing external artifact node; got:\n%s", fileBody)
	}
}

// TestMCPMermaidFanOutFromDynamicForEach is a regression test
// for the "discover floats as a disconnected node" bug: when a
// task uses `for_each: {x: "{{source.items}}"}`, the
// materialized instances must record `source` in their
// depends_on so downstream consumers (Mermaid renderer, DAG
// walkers, any visualizer) see the fan-out edge. Before the
// fix, materialization seeded the DAG edge for cascade
// purposes but omitted it from depends_on, so
// enju_export_diagram produced mermaid with dangling expand
// nodes and no source edge.
func TestMCPMermaidFanOutFromDynamicForEach(t *testing.T) {
	h := newMCPHarness(t, "FanOutA")
	projectID := h.createTestProject()

	yaml := `name: "dynamic for_each fan-out"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List topics."
    outputs:
      topics:
        format: list<string>
  - id: expand
    for_each:
      topic: "{{discover.topics}}"
    action: answer
    prompt: "Explore {{topic}}."
  - id: aggregate
    action: answer
    depends_on: [expand]
    prompt: "Summarize: {{expand.content}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// Accept discover with a 3-item list → materializes
	// alpha:expand, beta:expand, gamma:expand.
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitOutputLists(t, "discover", map[string]any{
		"topics": []string{"alpha", "beta", "gamma"},
	})

	// The bug: each materialized expand:* instance must have
	// discover in its depends_on. Without this, the Mermaid
	// renderer has no edge to draw from discover to the
	// instances.
	for _, key := range []string{"alpha", "beta", "gamma"} {
		shortID := key + ":expand"
		task := h.taskGet(shortID)
		deps, _ := task["depends_on"].(string)
		if !strings.Contains(deps, ":discover") {
			t.Errorf("expected %s depends_on to include discover (fan-out edge); got %q", shortID, deps)
		}
	}

	// End-to-end verification: export the diagram and confirm
	// the mermaid source actually contains the fan-out edges.
	res := h.callOK(t, "enju_export_diagram", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
		"phase":      "after_materialize",
	})
	out := mcpText(res)
	for _, key := range []string{"alpha", "beta", "gamma"} {
		// One edge per instance: t_1_1_discover --> t_1_1_<key>_expand
		want := fmt.Sprintf("t_1_1_discover --> t_1_1_%s_expand", key)
		if !strings.Contains(out, want) {
			t.Errorf("missing fan-out edge %q in mermaid output; got:\n%s", want, out)
		}
	}
}

// TestMCPExportDiagram exercises enju_export_diagram end-to-end:
// write an initial snapshot, overwrite with a re-exported
// "initial" (idempotent, no accumulating final-1 / final-2),
// verify no-op when content hasn't changed, reject bad phases,
// and confirm the response carries both the archive path and a
// fenced inline render so the LLM can display the diagram in
// the same turn it commits it.
func TestMCPExportDiagram(t *testing.T) {
	h := newMCPHarness(t, "ExportDiagramA")
	projectID := h.createTestProject()

	yaml := `name: "export diagram run"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: review
    action: answer
    depends_on: [draft]
    prompt: "Review: {{draft.content}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// --- initial snapshot ---
	res := h.callOK(t, "enju_export_diagram", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
		"phase":      "initial",
	})
	out := mcpText(res)
	if !strings.Contains(out, ".enju/runs/1/graph/initial.mmd") {
		t.Errorf("expected initial file path in response; got:\n%s", out)
	}
	if !strings.Contains(out, "flowchart TD") {
		t.Errorf("expected fenced inline render; got:\n%s", out)
	}
	if !strings.Contains(out, "![](") {
		t.Errorf("expected markdown embed hint; got:\n%s", out)
	}
	// File must land in the bare remote with raw .mmd content
	// (no code fence, no %% comment header — those are for the
	// response only).
	body, ok := h.readRepoFile(projectID, ".enju/runs/1/graph/initial.mmd")
	if !ok {
		t.Fatalf("expected .enju/runs/1/graph/initial.mmd in the bare remote")
	}
	if strings.Contains(string(body), "```mermaid") {
		t.Errorf("file should not contain markdown fences — it's raw .mmd source")
	}
	if !strings.Contains(string(body), "flowchart TD") {
		t.Errorf("file missing flowchart TD header; got:\n%s", body)
	}

	// --- no-op path: re-export the same phase with unchanged
	// state. Must not create a new commit.
	res2 := h.callOK(t, "enju_export_diagram", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
		"phase":      "initial",
	})
	out2 := mcpText(res2)
	if !strings.Contains(out2, "unchanged") && !strings.Contains(out2, "skipped commit") {
		t.Errorf("expected no-op message on identical re-export; got:\n%s", out2)
	}

	// --- state-change path: claim + submit draft so the tasks
	// move. Re-export as "final" — content should differ and
	// a fresh commit should land.
	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "done")
	res3 := h.callOK(t, "enju_export_diagram", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
		"phase":      "final",
	})
	out3 := mcpText(res3)
	if !strings.Contains(out3, ".enju/runs/1/graph/final.mmd") {
		t.Errorf("expected final file path in response; got:\n%s", out3)
	}
	if strings.Contains(out3, "unchanged") {
		t.Errorf("expected a real write after state change, got no-op message:\n%s", out3)
	}
	// The previous initial snapshot must still exist — new phases
	// land in new files, they don't clobber siblings.
	if _, ok := h.readRepoFile(projectID, ".enju/runs/1/graph/initial.mmd"); !ok {
		t.Errorf("initial.mmd disappeared after final export — phases should be independent files")
	}
	finalBody, ok := h.readRepoFile(projectID, ".enju/runs/1/graph/final.mmd")
	if !ok {
		t.Fatalf("expected .enju/runs/1/graph/final.mmd after export")
	}
	// Final diagram must reflect the state change — draft is
	// now accepted. The initial.mmd still sees it as ready.
	// This is the "planned vs actually ran" contract the tool
	// exists to capture.
	if !strings.Contains(string(finalBody), "✅") {
		t.Errorf("expected final.mmd to contain the accepted glyph for draft; got:\n%s", finalBody)
	}

	// --- custom phase label: arbitrary descriptive name.
	res4 := h.callOK(t, "enju_export_diagram", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(1),
		"phase":      "after_draft_accepted",
	})
	if !strings.Contains(mcpText(res4), "after_draft_accepted.mmd") {
		t.Errorf("expected custom phase label in path; got:\n%s", mcpText(res4))
	}

	// --- phase validation: path-traversal attempts and empty
	// must be rejected with clear errors, not silently coerced.
	for _, bad := range []string{"", "../etc/passwd", "foo/bar", "with\\slash", strings.Repeat("x", 65)} {
		text := h.callExpectError(t, "enju_export_diagram", map[string]any{
			"project_id": float64(projectID),
			"run_id":     float64(1),
			"phase":      bad,
		})
		if !strings.Contains(strings.ToLower(text), "phase") {
			t.Errorf("expected validation error mentioning 'phase' for %q, got: %s", bad, text)
		}
	}
}

// TestMCPVoteResponsesTemplate is the MCP-layer port. The
// downstream task's resolved prompt embeds per-voter commentary
// via {{upstream.responses}}.
func TestMCPVoteResponsesTemplate(t *testing.T) {
	h := newMCPHarness(t, "VResponsesA")
	bob := h.newMCPClientAs(t, "VResponsesB")
	charlie := h.newMCPClientAs(t, "VResponsesC")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "vote-responses.yaml")

	h.mcpClaimOK(t, "gather")
	h.mcpClaimAs(t, bob, "gather")
	h.mcpClaimAs(t, charlie, "gather")

	h.mcpSubmitVote(t, "gather", "DuckDB is plenty for our scale.", "duckdb")
	h.mcpSubmitVoteAs(t, bob, "gather", "Agreed, DuckDB.", "duckdb")
	h.mcpSubmitVoteAs(t, charlie, "gather", "I'd prefer SQLite for portability.", "sqlite")

	if got := h.taskGet("gather")["state"]; got != "accepted" {
		t.Fatalf("expected gather accepted, got %v", got)
	}

	// Synthesize resolves via handleGetTaskInputs / claim.
	h.mcpClaimOK(t, "synthesize")
	inputs := h.mcpTaskInputs(t, "synthesize")
	resolved, _ := inputs["resolved_prompt"].(string)
	if resolved == "" {
		t.Fatal("expected resolved prompt on synthesize")
	}
	if !strings.Contains(resolved, "duckdb") {
		t.Errorf("expected winning_option duckdb in prompt, got: %s", resolved)
	}
	for _, want := range []string{
		"@" + h.username, "@" + bob.Username(), "@" + charlie.Username(),
		"DuckDB is plenty for our scale.",
		"I'd prefer SQLite for portability.",
	} {
		if !strings.Contains(resolved, want) {
			t.Errorf("expected %q in resolved prompt, got: %s", want, resolved)
		}
	}
}

// TestMCPVoteLateSubmitAfterResolve is the MCP-layer port. After
// a vote resolves on the first submission, a late second submit
// must fail with "already resolved" — not a 500 — and land as a
// tool error from the handler.
func TestMCPVoteLateSubmitAfterResolve(t *testing.T) {
	h := newMCPHarness(t, "LateVoteA")
	bob := h.newMCPClientAs(t, "LateVoteB")

	projectID := h.createTestProject()
	yaml := `name: "Fast resolve"
version: 1
tasks:
  - id: pick
    action: vote
    citizens: 3
    min_quorum: 1
    threshold: plurality
    prompt: "Pick one."
    options:
      - {id: a, label: "A"}
      - {id: b, label: "B"}
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "pick")
	h.mcpClaimAs(t, bob, "pick")

	// First vote resolves immediately (quorum=1).
	h.mcpSubmitVote(t, "pick", "I pick A.", "a")

	// Bob's late submit — the submit handler surfaces server-side
	// "already resolved" as a tool error (handleSubmitResult
	// returns NewToolResultError on c.post error) — OR as prose
	// inside a success. Accept either; assert on the phrase.
	res, err := bob.Call(context.Background(), "enju_submit_result", map[string]any{
		"task_id": h.taskID("pick"),
		"content": "I pick B.",
		"option":  "b",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	text := mcpText(res)
	// The MCP layer may surface this in two ways: (a) the handler's
	// client-side pre-validation sees the task is in terminal state
	// and rejects before hitting the server, or (b) the server's
	// "already resolved" reaches the client. Both are acceptable —
	// the invariant is that a late submission does NOT succeed.
	if !strings.Contains(text, "already resolved") &&
		!strings.Contains(text, "terminal state") {
		t.Errorf("expected late-submit rejection phrasing, got: %s", text)
	}
}

// TestMCPVoteDefaultQuorumMatchesCitizens is the MCP-layer port.
// Without explicit min_quorum, a citizens:3 task waits for all
// three submissions.
func TestMCPVoteDefaultQuorumMatchesCitizens(t *testing.T) {
	h := newMCPHarness(t, "DefQuorumA")
	bob := h.newMCPClientAs(t, "DefQuorumB")
	charlie := h.newMCPClientAs(t, "DefQuorumC")

	projectID := h.createTestProject()
	yaml := `name: "Default quorum"
version: 1
tasks:
  - id: pick
    action: vote
    citizens: 3
    threshold: plurality
    prompt: "Pick one."
    options:
      - {id: a, label: "A"}
      - {id: b, label: "B"}
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "pick")
	h.mcpClaimAs(t, bob, "pick")
	h.mcpClaimAs(t, charlie, "pick")

	h.mcpSubmitVote(t, "pick", "first", "a")
	if got := h.taskGet("pick")["state"]; got != "collecting" {
		t.Fatalf("expected collecting after 1 vote, got %v", got)
	}
	h.mcpSubmitVoteAs(t, bob, "pick", "second", "a")
	if got := h.taskGet("pick")["state"]; got != "collecting" {
		t.Fatalf("expected collecting after 2 votes, got %v", got)
	}
	h.mcpSubmitVoteAs(t, charlie, "pick", "third", "b")
	if got := h.taskGet("pick")["state"]; got != "accepted" {
		t.Fatalf("expected accepted after 3rd vote, got %v", got)
	}
	if got := h.taskGet("pick")["vote_choice"]; got != "a" {
		t.Errorf("expected plurality winner=a, got %v", got)
	}
}

// TestMCPReviewLateSubmitAfterResolve is the MCP-layer port.
// Multi-reviewer, any-reject-kills resolves on the first
// request_changes. A third reviewer's late submit must fail
// cleanly.
func TestMCPReviewLateSubmitAfterResolve(t *testing.T) {
	h := newMCPHarness(t, "LateReviewA")
	bob := h.newMCPClientAs(t, "LateReviewB")
	charlie := h.newMCPClientAs(t, "LateReviewC")

	projectID := h.createTestProject()
	yaml := `name: "Late review submit"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write something."
  - id: check
    action: review
    reviews: draft
    citizens: 3
    prompt: "Review."
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "A draft.")

	h.mcpClaimOK(t, "check")
	h.mcpClaimAs(t, bob, "check")
	h.mcpClaimAs(t, charlie, "check")

	h.mcpSubmitReview(t, "check", "LGTM.", "approve")
	h.mcpSubmitReviewAs(t, bob, "check", "Nope.", "request_changes")

	// Charlie's late submit — expect rejection.
	res, err := charlie.Call(context.Background(), "enju_submit_result", map[string]any{
		"task_id":  h.taskID("check"),
		"content":  "Too late.",
		"decision": "approve",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	text := mcpText(res)
	// After the any-reject-kills cascade, the check task is
	// invalidated to PENDING. Charlie's late submit could surface
	// as "already resolved" (server), "no open claim" (server),
	// or "task is blocked" (client-side pre-validation on the
	// PENDING state). Any of these means "rejected" — the
	// invariant is that the late submission doesn't land.
	if !strings.Contains(text, "already resolved") &&
		!strings.Contains(text, "no open claim") &&
		!strings.Contains(text, "blocked") &&
		!strings.Contains(text, "terminal state") {
		t.Errorf("expected late-submit rejection phrasing, got: %s", text)
	}
}

// TestMCPReviewResponsesTemplate is the MCP-layer port. Multi-
// reviewer majority-approve resolves; downstream synthesize sees
// each reviewer's verdict + prose via {{peer_review.responses}}.
func TestMCPReviewResponsesTemplate(t *testing.T) {
	h := newMCPHarness(t, "RRespA")
	bob := h.newMCPClientAs(t, "RRespB")
	charlie := h.newMCPClientAs(t, "RRespC")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "review-multi-responses.yaml")

	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "A proposal to adopt DuckDB.")

	h.mcpClaimOK(t, "peer_review")
	h.mcpClaimAs(t, bob, "peer_review")
	h.mcpClaimAs(t, charlie, "peer_review")

	// Charlie dissents first so the test survives the majority
	// short-circuit logic in evaluateReviewTally.
	h.mcpSubmitReviewAs(t, charlie, "peer_review", "I'd prefer Postgres.", "request_changes")
	h.mcpSubmitReview(t, "peer_review", "Works for me.", "approve")
	h.mcpSubmitReviewAs(t, bob, "peer_review", "Concerns about tooling.", "approve")

	if got := h.taskGet("peer_review")["state"]; got != "accepted" {
		t.Fatalf("expected peer_review accepted (2-of-3 majority), got %v", got)
	}

	h.mcpClaimOK(t, "synthesize")
	inputs := h.mcpTaskInputs(t, "synthesize")
	resolved, _ := inputs["resolved_prompt"].(string)
	if resolved == "" {
		t.Fatal("expected resolved prompt on synthesize")
	}
	for _, want := range []string{
		"@" + h.username, "@" + bob.Username(), "@" + charlie.Username(),
		"approve", "request_changes",
		"Works for me.", "Concerns about tooling.", "I'd prefer Postgres.",
	} {
		if !strings.Contains(resolved, want) {
			t.Errorf("expected %q in resolved prompt, got: %s", want, resolved)
		}
	}
}

// TestMCPVoteDeadlineLazyResolve is the MCP-layer port. A vote
// with a short deadline + one submission below quorum should
// resolve when the next read fires past the deadline (lazy
// resolve).
func TestMCPVoteDeadlineLazyResolve(t *testing.T) {
	h := newMCPHarness(t, "DeadlineA")
	bob := h.newMCPClientAs(t, "DeadlineB")
	charlie := h.newMCPClientAs(t, "DeadlineC")

	projectID := h.createTestProject()
	yaml := `name: "Deadline test"
version: 1
tasks:
  - id: pick
    action: vote
    citizens: 3
    deadline: 100ms
    threshold: majority
    prompt: "Pick."
    options:
      - {id: a, label: "A"}
      - {id: b, label: "B"}
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "pick")
	h.mcpClaimAs(t, bob, "pick")
	h.mcpClaimAs(t, charlie, "pick")

	h.mcpSubmitVote(t, "pick", "A please.", "a")
	if got := h.taskGet("pick")["state"]; got != "collecting" {
		t.Fatalf("expected collecting after 1 vote, got %v", got)
	}

	// Wait past the deadline, then read — lazy resolver fires.
	time.Sleep(250 * time.Millisecond)
	pick := h.taskGet("pick")
	if got := pick["state"]; got != "accepted" {
		t.Fatalf("expected accepted after deadline lazy resolve, got %v", got)
	}
	if got := pick["vote_choice"]; got != "a" {
		t.Errorf("expected vote_choice=a, got %v", got)
	}
}

// TestMCPCreateRunWithParams is the MCP-layer port. A run YAML
// with a params: block can be submitted via enju_create_run with
// a `params` argument; the handler calls ParseWithParams and the
// resulting task has the substituted prompt.
func TestMCPCreateRunWithParams(t *testing.T) {
	h := newMCPHarness(t, "ParamsOK")
	projectID := h.createTestProject()

	yamlContent := `name: "GWAS recipe"
description: "Template for GWAS analysis"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "The disease to analyze"
  - name: tissue
    type: string
    default: "whole blood"
    description: "Primary tissue"
tasks:
  - id: gwas
    action: answer
    prompt: "Analyze GWAS data for {{disease}} in {{tissue}}"
`
	// The enju_create_run tool accepts yaml + params but has no
	// source_path argument — source_path is populated only when
	// the run is instantiated via `path` pointing at a
	// enju_templates/*.yaml file in the project clone. That
	// path is exercised by a dedicated template-mode test; here
	// we verify substitution via inline yaml + params.
	res := h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlContent,
		"params":     map[string]any{"disease": "PCOS"},
	})
	_ = res

	// Locate the run and verify substitution.
	runs := h.getList(fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	first, _ := runs[0].(map[string]interface{})
	seq, _ := first["seq"].(float64)
	h.lastProjectID = projectID
	h.lastRunSeq = int(seq)

	task := h.taskGet("gwas")
	want := "Analyze GWAS data for PCOS in whole blood"
	if got, _ := task["prompt"].(string); got != want {
		t.Errorf("prompt substitution wrong\n  got:  %q\n  want: %q", got, want)
	}
}

// TestMCPCreateRunWithParamsMissingRequired is the MCP-layer
// port. Missing a required parameter surfaces a natural-language
// error; no run is persisted.
func TestMCPCreateRunWithParamsMissingRequired(t *testing.T) {
	h := newMCPHarness(t, "ParamsMissing")
	projectID := h.createTestProject()

	yamlContent := `name: "GWAS recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "The disease to analyze (e.g. endometriosis, PCOS)"
tasks:
  - id: gwas
    action: answer
    prompt: "Analyze {{disease}}"
`
	// The MCP handler surfaces this as a prose rejection in a
	// non-error result (same pattern as other handler-wrapped
	// server errors).
	res := h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yamlContent,
		"params":     map[string]any{},
	})
	text := mcpText(res)
	if !strings.Contains(text, "missing required parameter") {
		t.Errorf("expected 'missing required parameter' in response, got: %s", text)
	}
	if !strings.Contains(text, "The disease to analyze") {
		t.Errorf("expected param description in response, got: %s", text)
	}

	// No run persisted.
	runs := h.getList(fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if len(runs) != 0 {
		t.Errorf("expected no runs after failed submit, got %d", len(runs))
	}
}

// TestMCPDynamicForEachMaterializes is the MCP-layer port.
// Dynamic for_each materializes instances on upstream accept;
// the submit path uses outputs_json with a list<string> value so
// the coordinator sees the output_lists payload.
func TestMCPDynamicForEachMaterializes(t *testing.T) {
	h := newMCPHarness(t, "DynamicA")
	projectID := h.createTestProject()

	yamlContent := `name: "Dynamic fan-out"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List 3 candidate genes."
    outputs:
      gene_symbols:
        description: "Genes to analyze."
        format: list<string>

  - id: analyze
    action: answer
    for_each:
      gene: "{{discover.gene_symbols}}"
    prompt: "Analyze {{gene}}"

  - id: synthesize
    action: answer
    prompt: "Combine: {{analyze.content}}"
`
	h.mcpCreateRunInline(t, projectID, yamlContent)

	// Pre-submit: only discover exists.
	before := h.runTasks(h.lastRunID)
	if len(before) != 1 {
		t.Fatalf("expected 1 task pre-materialization, got %d", len(before))
	}

	// Submit discover with a 3-gene list via outputs_json (the
	// handler routes list-valued fields to output_lists).
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverWithList(t, []string{"BRCA1", "TP53", "EGFR"})

	// Post: discover + 3 analyze + 1 synthesize = 5.
	after := h.runTasks(h.lastRunID)
	if len(after) != 5 {
		t.Errorf("expected 5 tasks post-materialization, got %d", len(after))
	}
	if got := mcpCountTasksByDef(after, "analyze"); got != 3 {
		t.Errorf("expected 3 analyze instances, got %d", got)
	}
	if got := mcpCountTasksByDef(after, "synthesize"); got != 1 {
		t.Errorf("expected 1 synthesize, got %d", got)
	}

	// Each analyze instance has {{gene}} substituted.
	runPrefix := fmt.Sprintf("%d:%d:", h.lastProjectID, h.lastRunSeq)
	byID := map[string]map[string]interface{}{}
	for _, raw := range after {
		tk, _ := raw.(map[string]interface{})
		id, _ := tk["id"].(string)
		byID[id] = tk
	}
	for _, gene := range []string{"BRCA1", "TP53", "EGFR"} {
		id := runPrefix + gene + ":analyze"
		tk, ok := byID[id]
		if !ok {
			t.Errorf("missing analyze instance %s", id)
			continue
		}
		want := "Analyze " + gene
		if got, _ := tk["prompt"].(string); got != want {
			t.Errorf("%s prompt: got %q, want %q", id, got, want)
		}
		if got, _ := tk["state"].(string); got != "ready" {
			t.Errorf("%s state: got %q, want ready", id, got)
		}
	}
}

// TestMCPDynamicForEachPerInstanceReviewChain is the MCP-layer
// port. Per-instance review targets must bind to the matching
// analyze:GENE, not the unscoped task_def_id.
func TestMCPDynamicForEachPerInstanceReviewChain(t *testing.T) {
	h := newMCPHarness(t, "DynReview")
	projectID := h.createTestProject()

	yamlContent := `name: "Dynamic review chain"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List 2 genes."
    outputs:
      gene_symbols:
        format: list<string>

  - id: analyze
    action: answer
    for_each:
      gene: "{{discover.gene_symbols}}"
    prompt: "Analyze {{gene}}"

  - id: check
    action: review
    reviews: analyze
    for_each:
      gene: "{{discover.gene_symbols}}"
    prompt: "Is the analysis of {{gene}} accurate?"
`
	h.mcpCreateRunInline(t, projectID, yamlContent)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverWithList(t, []string{"BRCA1", "MYC"})

	tasks := h.runTasks(h.lastRunID)
	byID := map[string]map[string]interface{}{}
	for _, raw := range tasks {
		tk, _ := raw.(map[string]interface{})
		id, _ := tk["id"].(string)
		byID[id] = tk
	}
	runPrefix := fmt.Sprintf("%d:%d:", h.lastProjectID, h.lastRunSeq)
	for _, gene := range []string{"BRCA1", "MYC"} {
		analyzeID := runPrefix + gene + ":analyze"
		checkID := runPrefix + gene + ":check"

		if tk, ok := byID[analyzeID]; !ok {
			t.Errorf("missing %s", analyzeID)
		} else if state, _ := tk["state"].(string); state != "ready" {
			t.Errorf("%s state: got %q, want ready", analyzeID, state)
		}

		check, ok := byID[checkID]
		if !ok {
			t.Errorf("missing %s", checkID)
			continue
		}
		if state, _ := check["state"].(string); state != "pending" {
			t.Errorf("%s state: got %q, want pending (should wait on %s)",
				checkID, state, analyzeID)
		}
		// reviews_target is stored in instance-short form
		// ("BRCA1:analyze") so consumers uniformly prepend the
		// run prefix. The previous full-id form ("1:1:BRCA1:
		// analyze") caused double-prefixing bugs in
		// submit_orchestrate.go + fetchAndResolveLocally.
		wantShort := gene + ":analyze"
		if rt, _ := check["reviews_target"].(string); rt != wantShort {
			t.Errorf("%s reviews_target: got %q, want %q", checkID, rt, wantShort)
		}
		depsStr, _ := check["depends_on"].(string)
		found := false
		for _, d := range strings.Split(depsStr, ",") {
			if strings.TrimSpace(d) == analyzeID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s depends_on should include %q, got %q", checkID, analyzeID, depsStr)
		}
	}
}

// TestMCPDynamicForEachInvalidationCascade is the MCP-layer port.
// Invalidating the dynamic source deletes every materialized
// descendant; a re-submit with a different list produces fresh
// instances matching the new list.
func TestMCPDynamicForEachInvalidationCascade(t *testing.T) {
	h := newMCPHarness(t, "DynInval")
	projectID := h.createTestProject()

	yamlContent := `name: "Invalidation test"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List genes."
    outputs:
      gene_symbols:
        format: list<string>

  - id: analyze
    action: answer
    for_each:
      gene: "{{discover.gene_symbols}}"
    prompt: "Analyze {{gene}}"
`
	h.mcpCreateRunInline(t, projectID, yamlContent)

	// Round 1.
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverWithList(t, []string{"BRCA1", "TP53"})
	round1 := h.runTasks(h.lastRunID)
	if got := mcpCountTasksByDef(round1, "analyze"); got != 2 {
		t.Fatalf("round 1: expected 2 analyze instances, got %d", got)
	}

	// Invalidate discover via MCP.
	h.mcpInvalidate(t, "discover", "re-run with different gene list")

	// Round 2: different list.
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverWithList(t, []string{"EGFR", "MYC", "KRAS"})
	round2 := h.runTasks(h.lastRunID)
	if got := mcpCountTasksByDef(round2, "analyze"); got != 3 {
		t.Errorf("round 2: expected 3 analyze instances, got %d", got)
	}

	// Stale instances from round 1 must be gone.
	runPrefix := fmt.Sprintf("%d:%d:", h.lastProjectID, h.lastRunSeq)
	for _, stale := range []string{"BRCA1", "TP53"} {
		staleID := runPrefix + stale + ":analyze"
		for _, raw := range round2 {
			tk, _ := raw.(map[string]interface{})
			if id, _ := tk["id"].(string); id == staleID {
				t.Errorf("stale instance %s still present", staleID)
			}
		}
	}
	// Fresh instances present + READY.
	for _, gene := range []string{"EGFR", "MYC", "KRAS"} {
		wantID := runPrefix + gene + ":analyze"
		found := false
		for _, raw := range round2 {
			tk, _ := raw.(map[string]interface{})
			if id, _ := tk["id"].(string); id == wantID {
				found = true
				if state, _ := tk["state"].(string); state != "ready" {
					t.Errorf("%s state: got %q, want ready", wantID, state)
				}
				break
			}
		}
		if !found {
			t.Errorf("expected new instance %s, not found", wantID)
		}
	}
}

// ==========================================================================
// Dynamic for_each runtime bugs (reported 2026-04-17)
//
// These tests lock in runtime behavior the earlier dynamic for_each
// tests don't exercise: upstream submits complete, downstream
// instances claim, and their resolved prompts must reflect the
// per-instance pairing / fan-in aggregation / review cascade. The
// earlier tests only verified materialization state (task rows,
// deps, states) — which passes while runtime resolution is broken.
// ==========================================================================

// TestMCPDynamicForEachPerInstancePromptResolves verifies bug 1:
// when a downstream task has the SAME dynamic for_each as its
// upstream, each downstream instance's resolved prompt must carry
// the matching upstream instance's content. tag:alpha reading
// {{expand.content}} should resolve to expand:alpha's result —
// not a raw placeholder, not expand:beta's content.
func TestMCPDynamicForEachPerInstancePromptResolves(t *testing.T) {
	h := newMCPHarness(t, "PerInstancePrompt")
	projectID := h.createTestProject()

	yaml := `name: "per-instance resolution"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List two values."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: tag
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Tag {{x}} based on: {{expand.content}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha", "beta"})

	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitText(t, "alpha:expand", "EXPAND-CONTENT-FOR-ALPHA")
	h.mcpClaimOK(t, "beta:expand")
	h.mcpSubmitText(t, "beta:expand", "EXPAND-CONTENT-FOR-BETA")

	inputs := h.mcpTaskInputs(t, "alpha:tag")
	resolved, _ := inputs["resolved_prompt"].(string)
	if strings.Contains(resolved, "{{expand.content}}") {
		t.Errorf("bug 1: {{expand.content}} left unresolved in tag:alpha:\n%s", resolved)
	}
	if !strings.Contains(resolved, "EXPAND-CONTENT-FOR-ALPHA") {
		t.Errorf("bug 1: tag:alpha should see expand:alpha content, got:\n%s", resolved)
	}
	if strings.Contains(resolved, "EXPAND-CONTENT-FOR-BETA") {
		t.Errorf("bug 1: tag:alpha should NOT see expand:beta content (cross-instance leak):\n%s", resolved)
	}

	betaInputs := h.mcpTaskInputs(t, "beta:tag")
	betaResolved, _ := betaInputs["resolved_prompt"].(string)
	if !strings.Contains(betaResolved, "EXPAND-CONTENT-FOR-BETA") {
		t.Errorf("bug 1: tag:beta should see expand:beta content, got:\n%s", betaResolved)
	}
	if strings.Contains(betaResolved, "EXPAND-CONTENT-FOR-ALPHA") {
		t.Errorf("bug 1: tag:beta should NOT see expand:alpha content:\n%s", betaResolved)
	}
}

// TestMCPDynamicForEachSingletonFanInAggregates verifies bug 2:
// a singleton consumer of a dynamic-for_each upstream via
// {{expand.content}} must receive every materialized instance's
// content as an Option 4 markdown block.
func TestMCPDynamicForEachSingletonFanInAggregates(t *testing.T) {
	h := newMCPHarness(t, "SingletonFanIn")
	projectID := h.createTestProject()

	yaml := `name: "singleton fan-in over dynamic"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List two values."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: aggregate
    action: answer
    prompt: "Aggregate: {{expand.content}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha", "beta"})

	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitText(t, "alpha:expand", "EXPAND-ALPHA-BODY")
	h.mcpClaimOK(t, "beta:expand")
	h.mcpSubmitText(t, "beta:expand", "EXPAND-BETA-BODY")

	inputs := h.mcpTaskInputs(t, "aggregate")
	resolved, _ := inputs["resolved_prompt"].(string)
	if strings.Contains(resolved, "{{expand.content}}") {
		t.Errorf("bug 2: {{expand.content}} left unresolved in singleton aggregate:\n%s", resolved)
	}
	if !strings.Contains(resolved, "EXPAND-ALPHA-BODY") {
		t.Errorf("bug 2: aggregate should see expand:alpha content, got:\n%s", resolved)
	}
	if !strings.Contains(resolved, "EXPAND-BETA-BODY") {
		t.Errorf("bug 2: aggregate should see expand:beta content, got:\n%s", resolved)
	}
	if !strings.Contains(resolved, "### iteration:") {
		t.Errorf("bug 2: fan-in should render Option 4 header (### iteration:), got:\n%s", resolved)
	}
}

// TestMCPDynamicForEachPerInstanceReviewCascades verifies bug 3:
// request_changes on check:alpha (per-instance review of
// expand:alpha) must cascade — expand:alpha bounces to READY,
// check:alpha re-invalidates through the dep edge, expand:beta
// stays accepted.
func TestMCPDynamicForEachPerInstanceReviewCascades(t *testing.T) {
	h := newMCPHarness(t, "CascadeDrafter")
	reviewer := h.newMCPClientAs(t, "CascadeReviewer")

	projectID := h.createTestProject()
	yaml := `name: "per-instance review cascade"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List values."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: check
    action: review
    reviews: expand
    for_each:
      x: "{{discover.items}}"
    prompt: "Review expansion of {{x}}."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha", "beta"})

	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitText(t, "alpha:expand", "first draft alpha")
	h.mcpClaimOK(t, "beta:expand")
	h.mcpSubmitText(t, "beta:expand", "first draft beta")

	h.mcpClaimAs(t, reviewer, "alpha:check")
	h.mcpSubmitReviewAs(t, reviewer, "alpha:check", "needs revision", "request_changes")

	if got := h.taskGet("alpha:expand")["state"]; got != "ready" {
		t.Errorf("bug 3: expand:alpha should be READY after request_changes, got %v", got)
	}
	if got := h.taskGet("beta:expand")["state"]; got != "accepted" {
		t.Errorf("bug 3: expand:beta should stay accepted (only alpha bounced), got %v", got)
	}
	if got := h.taskGet("alpha:check")["state"]; got == "accepted" {
		t.Errorf("bug 3: check:alpha should NOT stay accepted after cascade, got %v", got)
	}
}

// TestMCPDynamicForEachPerInstanceReviewShowsTargetContent
// verifies bug 4: claiming a per-instance review task must render
// the reviewed target's content inline in the ── Reviewing ──
// block, matching the singleton-review behavior.
func TestMCPDynamicForEachPerInstanceReviewShowsTargetContent(t *testing.T) {
	h := newMCPHarness(t, "InlineDrafter")
	reviewer := h.newMCPClientAs(t, "InlineReviewer")

	projectID := h.createTestProject()
	yaml := `name: "per-instance review inline"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: check
    action: review
    reviews: expand
    for_each:
      x: "{{discover.items}}"
    prompt: "Review {{x}}."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha"})

	h.mcpClaimOK(t, "alpha:expand")
	const distinctive = "INLINE-REVIEW-MARKER-alpha-body"
	h.mcpSubmitText(t, "alpha:expand", distinctive)

	res := h.mcpCallOKVia(t, reviewer, "enju_claim_task",
		map[string]any{"task_id": h.taskID("alpha:check")})
	text := mcpText(res)
	if !strings.Contains(text, "── Reviewing ──") {
		t.Errorf("bug 4: claim response missing ── Reviewing ── block, got:\n%s", text)
	}
	if !strings.Contains(text, distinctive) {
		t.Errorf("bug 4: Reviewing block missing target content %q, got:\n%s", distinctive, text)
	}
}

// TestMCPDynamicForEachUserReportedStressScenario replays the
// exact YAML the user filed the bug report against, following
// the reported step-by-step sequence. Locks in that the full
// pipeline (discover → 4 expand instances → 4 tag instances → 4
// review instances → aggregate with writes_artifacts) survives
// a per-instance request_changes without surfacing the
// originally-reported symptoms:
//   - alpha:expand must bounce to READY after review cascade.
//   - beta:tag's resolved prompt must include beta:expand's
//     paragraph (per-instance pairing), NOT raw {{expand.content}}.
//   - aggregate's resolved prompt must aggregate every
//     expand/tag instance via the Option 4 fan-in block, NOT
//     raw {{expand.content}} / {{tag.content}}.
func TestMCPDynamicForEachUserReportedStressScenario(t *testing.T) {
	h := newMCPHarness(t, "tamer")
	projectID := h.createTestProject()

	yaml := `name: "For_each + review + fan-in stress test"
description: "Toy stress test — dynamic for_each, per-instance review, multi-hop pairing, fan-in, artifact write."
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "Produce exactly 4 toy topic names, as a simple list. Use placeholder names like 'alpha', 'beta', 'gamma', 'delta'. Return them as a list<string> output."
    outputs:
      topics:
        format: list<string>

  - id: expand
    action: answer
    for_each:
      topic: "{{discover.topics}}"
    prompt: "Write a single short paragraph (~40 words) about the toy topic '{{topic}}'. Content can be nonsense — this is a mechanics test, not a content test."

  - id: tag
    action: answer
    for_each:
      topic: "{{discover.topics}}"
    prompt: "Given this paragraph about '{{topic}}':\n\n{{expand.content}}\n\nReturn exactly 2 keywords, comma-separated."

  - id: review
    action: review
    reviews: expand
    for_each:
      topic: "{{discover.topics}}"
    prompt: "Is the paragraph for '{{topic}}' acceptable?"

  - id: aggregate
    action: answer
    prompt: "Produce a single consolidated summary document combining all expansions and their tags.\n\nExpansions:\n{{expand.content}}\n\nTags:\n{{tag.content}}"
    writes_artifacts:
      - stress/summary.md
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// Step 2: discover → 4 topics.
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "topics", []string{"alpha", "beta", "gamma", "delta"})

	// Step 3: claim + submit each *:expand with distinctive content.
	for _, topic := range []string{"alpha", "beta", "gamma", "delta"} {
		h.mcpClaimOK(t, topic+":expand")
		h.mcpSubmitText(t, topic+":expand", "PARAGRAPH-FOR-"+topic)
	}

	// Step 4: alpha:review submits request_changes.
	h.mcpClaimOK(t, "alpha:review")
	h.mcpSubmitReview(t, "alpha:review", "needs more detail", "request_changes")

	// Step 5 (bug 3): alpha:expand should have bounced to READY;
	// the others stay accepted. alpha:tag should re-block on
	// alpha:expand (move back from READY to PENDING).
	if got := h.taskGet("alpha:expand")["state"]; got != "ready" {
		t.Errorf("bug 3: alpha:expand should be READY after request_changes, got %v", got)
	}
	for _, topic := range []string{"beta", "gamma", "delta"} {
		if got := h.taskGet(topic+":expand")["state"]; got != "accepted" {
			t.Errorf("bug 3: %s:expand should remain accepted, got %v", topic, got)
		}
	}
	// alpha:tag should no longer be ready (its upstream expand
	// just bounced). beta/gamma/delta tags should still be ready.
	if got := h.taskGet("alpha:tag")["state"]; got == "ready" || got == "accepted" {
		t.Errorf("bug 3: alpha:tag should re-block after alpha:expand bounce, got %v", got)
	}

	// Step 6: beta/gamma/delta reviews approve.
	for _, topic := range []string{"beta", "gamma", "delta"} {
		h.mcpClaimOK(t, topic+":review")
		h.mcpSubmitReview(t, topic+":review", "LGTM", "approve")
	}

	// Step 7 (bug 1): beta:tag's resolved prompt should include
	// PARAGRAPH-FOR-beta (per-instance pairing), NOT raw
	// {{expand.content}}.
	inputs := h.mcpTaskInputs(t, "beta:tag")
	resolved, _ := inputs["resolved_prompt"].(string)
	if strings.Contains(resolved, "{{expand.content}}") {
		t.Errorf("bug 1: beta:tag has unresolved {{expand.content}} — got:\n%s", resolved)
	}
	if !strings.Contains(resolved, "PARAGRAPH-FOR-beta") {
		t.Errorf("bug 1: beta:tag should include PARAGRAPH-FOR-beta, got:\n%s", resolved)
	}
	if strings.Contains(resolved, "PARAGRAPH-FOR-alpha") ||
		strings.Contains(resolved, "PARAGRAPH-FOR-gamma") {
		t.Errorf("bug 1: beta:tag should not leak other topics' paragraphs, got:\n%s", resolved)
	}

	// Step 8 (bug 2): aggregate is singleton. Since alpha:expand
	// is currently READY (rejected, awaiting revision), aggregate
	// should still be PENDING. But first drive the loop to
	// completion so we can claim aggregate: re-submit alpha:expand,
	// re-approve alpha:review.
	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitText(t, "alpha:expand", "PARAGRAPH-FOR-alpha-v2")
	h.mcpClaimOK(t, "alpha:review")
	h.mcpSubmitReview(t, "alpha:review", "fine now", "approve")

	// Complete every tag so aggregate unblocks.
	for _, topic := range []string{"alpha", "beta", "gamma", "delta"} {
		h.mcpClaimOK(t, topic+":tag")
		h.mcpSubmitText(t, topic+":tag", "TAGS-FOR-"+topic)
	}

	// Now aggregate — verify fan-in aggregation.
	aggInputs := h.mcpTaskInputs(t, "aggregate")
	aggResolved, _ := aggInputs["resolved_prompt"].(string)
	if strings.Contains(aggResolved, "{{expand.content}}") {
		t.Errorf("bug 2: aggregate has unresolved {{expand.content}}, got:\n%s", aggResolved)
	}
	if strings.Contains(aggResolved, "{{tag.content}}") {
		t.Errorf("bug 2: aggregate has unresolved {{tag.content}}, got:\n%s", aggResolved)
	}
	for _, topic := range []string{"alpha", "beta", "gamma", "delta"} {
		wantTag := "TAGS-FOR-" + topic
		if !strings.Contains(aggResolved, wantTag) {
			t.Errorf("bug 2: aggregate missing %q in fan-in, got:\n%s", wantTag, aggResolved)
		}
	}
	for _, topic := range []string{"beta", "gamma", "delta"} {
		wantPara := "PARAGRAPH-FOR-" + topic
		if !strings.Contains(aggResolved, wantPara) {
			t.Errorf("bug 2: aggregate missing %q in fan-in, got:\n%s", wantPara, aggResolved)
		}
	}
	if !strings.Contains(aggResolved, "PARAGRAPH-FOR-alpha-v2") {
		t.Errorf("bug 2: aggregate should include re-submitted alpha v2, got:\n%s", aggResolved)
	}
	if !strings.Contains(aggResolved, "### iteration:") {
		t.Errorf("bug 2: aggregate should render Option 4 header, got:\n%s", aggResolved)
	}
}

// TestMCPTemplateParamInAssignTo verifies that {{param}} refs
// in assign_to (and other validated per-field slots) are
// substituted BEFORE the fields reach their validators.
// Pre-fix, substituteParamsInPlace only walked task prompts
// and for_each values — AssignTo / RequireRole / Script /
// WritesArtifacts / ReadsArtifacts were ignored, so the
// engine's ValidateRunCreation saw the literal {{paramname}}
// and rejected it as a malformed username/role/path.
func TestMCPTemplateParamInAssignTo(t *testing.T) {
	h := newMCPHarness(t, "signoff-person")
	projectID := h.createTestProject()

	// Template parameterizes the reviewer username — the
	// canonical "who signs off on this batch" pattern.
	yaml := `name: "parameterized assign_to"
version: 1
params:
  - name: signoff_by
    type: string
    required: true
    description: "Reviewer username"
tasks:
  - id: draft
    action: answer
    prompt: "Write something."
  - id: check
    action: review
    reviews: draft
    assign_to: "{{signoff_by}}"
    prompt: "Review the draft."
`
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"params":     map[string]any{"signoff_by": h.username},
	})

	runs := h.getList(fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	first, _ := runs[0].(map[string]interface{})
	seq, _ := first["seq"].(float64)
	h.lastProjectID = projectID
	h.lastRunSeq = int(seq)
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, int(seq))

	// The check task's AssignTo should be the substituted
	// username, not the literal {{signoff_by}}.
	check := h.taskGet("check")
	assignees, _ := check["assign_to"].([]interface{})
	if len(assignees) != 1 {
		t.Fatalf("expected 1 assignee, got %v", assignees)
	}
	if got, _ := assignees[0].(string); got != h.username {
		t.Errorf("assign_to: got %q, want %q", got, h.username)
	}
}

// TestMCPTemplateParamInRunLevelForEach verifies the template
// parameterization pattern where the run-level for_each list
// comes from a top-level param. This is the most common shape
// for reusable templates ("iterate over each item in my
// gene/topic/file list"). Pre-fix, Run.ForEach was typed
// map[string][]string so YAML decode failed before validation
// or substitution could even run — the parser couldn't even
// load the template, which cascaded into enju_list_templates
// silently dropping it.
func TestMCPTemplateParamInRunLevelForEach(t *testing.T) {
	h := newMCPHarness(t, "RunLevelParamForEach")
	projectID := h.createTestProject()

	yaml := `name: "run-level param fan-out"
description: "One task template, fanned out by a caller list."
version: 1
params:
  - name: topics
    type: list<string>
    required: true
    description: "Topics to cover"
for_each:
  topic: "{{topics}}"
tasks:
  - id: cover
    action: answer
    prompt: "Cover topic: {{topic}}"
`
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"params":     map[string]any{"topics": []any{"red", "green", "blue"}},
	})

	runs := h.getList(fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	first, _ := runs[0].(map[string]interface{})
	seq, _ := first["seq"].(float64)
	h.lastProjectID = projectID
	h.lastRunSeq = int(seq)
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, int(seq))

	// Run-level for_each expands every task per iteration — 3
	// topics × 1 task = 3 cover instances.
	tasks := h.runTasks(h.lastRunID)
	if got := mcpCountTasksByDef(tasks, "cover"); got != 3 {
		t.Errorf("expected 3 cover instances, got %d", got)
	}
	runPrefix := fmt.Sprintf("%d:%d:", projectID, int(seq))
	byID := map[string]map[string]interface{}{}
	for _, raw := range tasks {
		tk, _ := raw.(map[string]interface{})
		id, _ := tk["id"].(string)
		byID[id] = tk
	}
	for _, topic := range []string{"red", "green", "blue"} {
		id := runPrefix + topic + ":cover"
		tk, ok := byID[id]
		if !ok {
			t.Errorf("missing cover:%s instance", topic)
			continue
		}
		want := "Cover topic: " + topic
		if got, _ := tk["prompt"].(string); got != want {
			t.Errorf("%s prompt: got %q, want %q", id, got, want)
		}
	}
}

// TestMCPTemplateParamInForEachList verifies a template can use
// a top-level {{paramname}} as the source of a for_each list.
// This is the canonical parameterized fan-out pattern:
// user supplies genes=["BRCA1","TP53"] at create_run time and
// the template's for_each: {gene: "{{genes}}"} expands into
// per-gene instances. Pre-fix the strict parser rejected this
// at describe_template and create_run time because the ref
// didn't contain a dot (so parseForEachRef refused it as a
// non-upstream-task reference) — even though it's a legitimate
// param reference.
func TestMCPTemplateParamInForEachList(t *testing.T) {
	h := newMCPHarness(t, "ParamForEach")
	projectID := h.createTestProject()

	yaml := `name: "parameterized fan-out"
description: "Fan out over a caller-supplied list."
version: 1
params:
  - name: genes
    type: list<string>
    required: true
    description: "Genes to analyze"
tasks:
  - id: analyze
    action: answer
    for_each:
      gene: "{{genes}}"
    prompt: "Analyze {{gene}}"
`
	// The describe_template path calls Parse() (no substitution)
	// — this must NOT error out on the {{genes}} ref. The
	// create_run path with a params map calls ParseWithParams
	// — this must substitute and then materialize per-instance.
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"params":     map[string]any{"genes": []any{"BRCA1", "TP53"}},
	})

	// Locate the run.
	runs := h.getList(fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	first, _ := runs[0].(map[string]interface{})
	seq, _ := first["seq"].(float64)
	h.lastProjectID = projectID
	h.lastRunSeq = int(seq)
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, int(seq))

	// Two analyze instances must have been materialized —
	// param expansion works the same way as a static literal
	// list would.
	tasks := h.runTasks(h.lastRunID)
	if got := mcpCountTasksByDef(tasks, "analyze"); got != 2 {
		t.Errorf("expected 2 analyze instances after param substitution, got %d", got)
	}
	// Per-instance prompts must have {{gene}} substituted.
	runPrefix := fmt.Sprintf("%d:%d:", projectID, h.lastRunSeq)
	byID := map[string]map[string]interface{}{}
	for _, raw := range tasks {
		tk, _ := raw.(map[string]interface{})
		id, _ := tk["id"].(string)
		byID[id] = tk
	}
	for _, gene := range []string{"BRCA1", "TP53"} {
		id := runPrefix + gene + ":analyze"
		tk, ok := byID[id]
		if !ok {
			t.Errorf("missing analyze:%s instance", gene)
			continue
		}
		want := "Analyze " + gene
		if got, _ := tk["prompt"].(string); got != want {
			t.Errorf("%s prompt: got %q, want %q", id, got, want)
		}
	}
}

// TestMCPClaimResolvesSkippedUpstream verifies that a
// downstream task whose depends_on reaches both an accepted and
// a skipped upstream can still be claimed cleanly. Skipped is a
// terminal state (losing vote branch, no result on disk). The
// resolver must surface it as a visible marker instead of
// failing on "no result file found" when the downstream
// references {{skipped_task.content}}.
func TestMCPClaimResolvesSkippedUpstream(t *testing.T) {
	h := newMCPHarness(t, "SkippedResolve")
	projectID := h.createTestProject()

	// Vote task picks one of two branches. Each branch has a
	// task. A finalize step references BOTH branches' content,
	// so one of the refs will resolve to a skipped upstream.
	yaml := `name: "skip-aware fan-in"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "Choose."
    options:
      - {id: a, label: "A", activates: [branch_a]}
      - {id: b, label: "B", activates: [branch_b]}
  - id: branch_a
    action: answer
    prompt: "Do A."
  - id: branch_b
    action: answer
    prompt: "Do B."
  - id: finalize
    action: answer
    depends_on: [branch_a, branch_b]
    prompt: "A said: {{branch_a.content}}\nB said: {{branch_b.content}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)

	h.mcpClaimOK(t, "pick")
	h.mcpSubmitVote(t, "pick", "going A", "a")

	// Branch A runs; branch B goes skipped.
	h.mcpClaimOK(t, "branch_a")
	h.mcpSubmitText(t, "branch_a", "A-RESULT-MARKER")

	if got, _ := h.taskGet("branch_b")["state"].(string); got != "skipped" {
		t.Fatalf("expected branch_b skipped after vote resolution, got %q", got)
	}

	// Claim finalize — must NOT error out on the skipped
	// upstream. Its resolved prompt should contain A's content
	// and a skip marker for B (anything non-empty, non-literal).
	inputs := h.mcpTaskInputs(t, "finalize")
	resolved, _ := inputs["resolved_prompt"].(string)
	if resolved == "" {
		t.Fatal("finalize resolved prompt is empty — resolver likely failed on skipped upstream")
	}
	if strings.Contains(resolved, "{{branch_b.content}}") {
		t.Errorf("{{branch_b.content}} left unresolved after skip, got:\n%s", resolved)
	}
	if !strings.Contains(resolved, "A-RESULT-MARKER") {
		t.Errorf("expected branch_a content in finalize prompt, got:\n%s", resolved)
	}
	// Skip marker: accept either "(skipped)" or "skipped" token
	// so the exact marker text can evolve without breaking this
	// test. What matters is it's a visible marker, not empty.
	if !strings.Contains(strings.ToLower(resolved), "skipped") {
		t.Errorf("expected 'skipped' marker for branch_b, got:\n%s", resolved)
	}
}

// TestMCPRunStatusShowsSkippedSeparately verifies the per-task
// summary in enju_run_status doesn't collapse skipped branches
// into the ✅ count. A vote that routes one branch and skips
// another was previously displayed as "N/N ✅" — hiding the
// lost branches from the reader. The summary must show ⚫
// skipped separately so vote outcomes are visible at a glance.
func TestMCPRunStatusShowsSkippedSeparately(t *testing.T) {
	h := newMCPHarness(t, "SkippedCountFix")
	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "vote-gate.yaml")

	// Drive the vote: pick the python branch → rust branch
	// (build_rust, ship_rust) goes SKIPPED.
	h.mcpClaimOK(t, "analysis")
	h.mcpSubmitText(t, "analysis", "analysis done")
	h.mcpClaimOK(t, "pick")
	h.mcpSubmitVote(t, "pick", "going python", "python")
	h.mcpClaimOK(t, "build_python")
	h.mcpSubmitText(t, "build_python", "built")
	h.mcpClaimOK(t, "ship_python")
	h.mcpSubmitText(t, "ship_python", "shipped")

	// Run is now terminal: analysis + pick + build_python +
	// ship_python accepted (4), build_rust + ship_rust skipped
	// (2). Summary must surface the skipped count, not roll it
	// under ✅.
	text := h.mcpRunStatusText(t)

	// build_rust and ship_rust should show up as skipped, not
	// as ✅.
	for _, defID := range []string{"build_rust", "ship_rust"} {
		// Grab the line for this def.
		var line string
		for _, l := range strings.Split(text, "\n") {
			if strings.Contains(l, defID) && !strings.Contains(l, "🟡") {
				line = l
				break
			}
		}
		if line == "" {
			t.Errorf("no per-def summary line found for %q in:\n%s", defID, text)
			continue
		}
		if !strings.Contains(line, "skipped") && !strings.Contains(line, "⚫") {
			t.Errorf("%s line should indicate skipped status, got: %q", defID, line)
		}
		if strings.Contains(line, "✅") {
			t.Errorf("%s was skipped but line shows ✅: %q", defID, line)
		}
	}
}

// TestMCPDynamicForEachVoteWinningOptionFanIn verifies that a
// singleton downstream task referencing {{pick.winning_option}}
// on a for_each vote upstream receives a markdown block
// aggregating every iteration's winner — not a raw literal
// placeholder. Parallel to the content fan-in behavior that
// already works for {{task.content}}.
func TestMCPDynamicForEachVoteWinningOptionFanIn(t *testing.T) {
	h := newMCPHarness(t, "WinningOptFanIn")
	projectID := h.createTestProject()

	yaml := `name: "vote winner fan-in"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List flavors."
    outputs:
      flavors:
        format: list<string>
  - id: pick
    action: vote
    for_each:
      f: "{{discover.flavors}}"
    prompt: "Pick for {{f}}"
    options:
      - {id: yes, label: "Yes"}
      - {id: no,  label: "No"}
  - id: summarize
    action: answer
    prompt: "Winners: {{pick.winning_option}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "flavors", []string{"alpha", "beta"})

	// Alpha votes yes, beta votes no.
	h.mcpClaimOK(t, "alpha:pick")
	h.mcpSubmitVote(t, "alpha:pick", "alpha rationale", "yes")
	h.mcpClaimOK(t, "beta:pick")
	h.mcpSubmitVote(t, "beta:pick", "beta rationale", "no")

	inputs := h.mcpTaskInputs(t, "summarize")
	resolved, _ := inputs["resolved_prompt"].(string)
	if strings.Contains(resolved, "{{pick.winning_option}}") {
		t.Errorf("winning_option fan-in bug: literal placeholder left unresolved:\n%s", resolved)
	}
	// Each iteration's winner must appear in the aggregated block,
	// tagged by its iteration label.
	if !strings.Contains(resolved, "yes") {
		t.Errorf("expected alpha's winner 'yes' in fan-in, got:\n%s", resolved)
	}
	if !strings.Contains(resolved, "no") {
		t.Errorf("expected beta's winner 'no' in fan-in, got:\n%s", resolved)
	}
	// The fan-in should use the same iteration-header format as
	// {{task.content}} fan-in so authors have a consistent shape
	// to parse.
	if !strings.Contains(resolved, "### iteration:") {
		t.Errorf("expected '### iteration:' header in winning_option fan-in, got:\n%s", resolved)
	}
}

// TestMCPDynamicForEachVoteResponsesFanIn verifies the same
// fan-in shape works for {{pick.responses}} on a multi-citizen
// for_each vote: the downstream prompt should receive a
// markdown block aggregating every iteration's per-citizen
// response list, NOT a Go map literal.
func TestMCPDynamicForEachVoteResponsesFanIn(t *testing.T) {
	h := newMCPHarness(t, "RespFanInA")
	bob := h.newMCPClientAs(t, "RespFanInB")

	projectID := h.createTestProject()
	yaml := `name: "vote responses fan-in"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List topics."
    outputs:
      topics:
        format: list<string>
  - id: pick
    action: vote
    citizens: 2
    min_quorum: 2
    threshold: plurality
    for_each:
      t: "{{discover.topics}}"
    prompt: "Vote on {{t}}"
    options:
      - {id: a, label: "A"}
      - {id: b, label: "B"}
  - id: synthesize
    action: answer
    prompt: "Per-iteration verdicts:\n{{pick.responses}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "topics", []string{"alpha", "beta"})

	// Two citizens vote on each iteration.
	for _, topic := range []string{"alpha", "beta"} {
		h.mcpClaimOK(t, topic+":pick")
		h.mcpClaimAs(t, bob, topic+":pick")
		h.mcpSubmitVote(t, topic+":pick", h.username+" prose for "+topic, "a")
		h.mcpSubmitVoteAs(t, bob, topic+":pick", bob.Username()+" prose for "+topic, "b")
	}

	inputs := h.mcpTaskInputs(t, "synthesize")
	resolved, _ := inputs["resolved_prompt"].(string)
	if strings.Contains(resolved, "{{pick.responses}}") {
		t.Errorf("responses fan-in bug: literal placeholder left unresolved:\n%s", resolved)
	}
	// Must NOT render as a Go map literal — that was the
	// symptom the bug report flagged.
	if strings.Contains(resolved, "map[") {
		t.Errorf("responses fan-in rendered as Go map literal (broken):\n%s", resolved)
	}
	// Each citizen's prose must appear.
	for _, topic := range []string{"alpha", "beta"} {
		wantAlice := h.username + " prose for " + topic
		wantBob := bob.Username() + " prose for " + topic
		if !strings.Contains(resolved, wantAlice) {
			t.Errorf("expected %q in fan-in, got:\n%s", wantAlice, resolved)
		}
		if !strings.Contains(resolved, wantBob) {
			t.Errorf("expected %q in fan-in, got:\n%s", wantBob, resolved)
		}
	}
	// Per-voter markdown header format (### @username — option).
	if !strings.Contains(resolved, "@"+h.username) || !strings.Contains(resolved, "@"+bob.Username()) {
		t.Errorf("expected @username markdown headers in responses fan-in, got:\n%s", resolved)
	}
	// Iteration headers too — one per for_each instance.
	if !strings.Contains(resolved, "### iteration:") {
		t.Errorf("expected '### iteration:' header in responses fan-in, got:\n%s", resolved)
	}
}

// TestMCPSubmitPreservesManualUserCommits is the regression
// guard for the "fat-client reset clobbers user work" bug.
// Adopt-mode users (and anyone editing files in the workspace
// clone between submits) need Enju to be a polite guest — a
// task submission must commit on top of whatever's currently
// there, not forcibly rewind HEAD to origin/main and discard
// intermediate commits.
//
// Scenario:
//  1. Submit task A (lands commit A on bare remote).
//  2. User manually commits "user_notes.md" to their workspace
//     clone on main. Commit exists locally, not yet pushed.
//  3. Submit task B.
//  4. Verify:
//       a. The user's commit is still reachable from HEAD in
//          the workspace clone.
//       b. user_notes.md exists in the bare remote (push sent
//          user's commit along with task B's).
//       c. Task B's submit succeeded (SubmitResult carried a
//          commit SHA, coordinator accepted).
func TestMCPSubmitPreservesManualUserCommits(t *testing.T) {
	h := newMCPHarness(t, "Polite Guest")
	projectID := h.createTestProject()

	yaml := `name: "submit preserves user commits"
version: 1
tasks:
  - id: one
    action: answer
    prompt: "first"
  - id: two
    action: answer
    prompt: "second"
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// Task A: submits normally. This populates the workspace
	// clone and pushes a commit to the bare remote.
	h.mcpClaimOK(t, "one")
	h.mcpSubmitText(t, "one", "task one result")

	// Locate the workspace clone directory so we can add a
	// manual user commit there, simulating "user did work in
	// the repo between submits."
	workspaceDir := ""
	matches, _ := filepath.Glob(filepath.Join(h.workspaceDir, "test-*"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly one project clone in workspace, got %d: %v", len(matches), matches)
	}
	workspaceDir = matches[0]

	// User writes a file and commits it on main.
	userFilePath := filepath.Join(workspaceDir, "user_notes.md")
	userFileContent := "manual user edit — should survive submit 2"
	if err := os.WriteFile(userFilePath, []byte(userFileContent), 0o644); err != nil {
		t.Fatalf("write user file: %v", err)
	}
	runGit := func(args ...string) (string, error) {
		cmd := execCommand("git", append([]string{"-C", workspaceDir}, args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	// Configure an author identity so the commit doesn't fail
	// on CI machines without global git config.
	if _, err := runGit("config", "user.email", "user@test.local"); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if _, err := runGit("config", "user.name", "Test User"); err != nil {
		t.Fatalf("git config name: %v", err)
	}
	if _, err := runGit("add", "user_notes.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if out, err := runGit("commit", "-m", "manual: add user notes"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	userCommitSHA, err := runGit("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}

	// Task B: submits through the fat-client path. Pre-fix,
	// this resets HEAD to origin/main and loses the user's
	// commit. Post-fix, it must commit on top of the user's
	// commit and push both commits to the remote.
	h.mcpClaimOK(t, "two")
	h.mcpSubmitText(t, "two", "task two result")

	// (a) The user's commit must still be reachable from HEAD
	// in the workspace clone.
	if out, err := runGit("cat-file", "-e", userCommitSHA); err != nil {
		t.Errorf("user commit %s not reachable after task B submit:\n%s", userCommitSHA, out)
	}
	// Belt-and-suspenders: verify it's in the HEAD chain, not
	// orphaned in the reflog.
	hist, err := runGit("log", "--format=%H", "HEAD")
	if err != nil {
		t.Fatalf("git log HEAD: %v", err)
	}
	if !strings.Contains(hist, userCommitSHA) {
		t.Errorf("user commit %s not in HEAD history (likely orphaned by hard reset):\nHEAD history:\n%s",
			userCommitSHA, hist)
	}

	// (b) user_notes.md must exist in the BARE remote too —
	// the submit's push should have carried the user's commit
	// along with task B's commit.
	if body, ok := h.readRepoFile(projectID, "user_notes.md"); !ok {
		t.Errorf("user_notes.md missing from bare remote — user commit was clobbered before push")
	} else if string(body) != userFileContent {
		t.Errorf("user_notes.md content in bare remote mismatch: %q", string(body))
	}

	// (c) Task B's result file must also have landed (the
	// submit itself succeeded, not just silently dropped).
	if got := h.mcpBareResultMD(t, "two"); got != "task two result" {
		t.Errorf("task two result missing from bare remote; got %q", got)
	}
}

// TestMCPDynamicForEachInvalidateSingleInstance verifies that
// invalidating one materialized instance (e.g. alpha:expand)
// cascades only that instance's descendants — siblings (beta:*)
// stay accepted. Also verifies recovery: re-submitting the
// invalidated instance re-unblocks its downstream, without
// re-materializing from scratch.
func TestMCPDynamicForEachInvalidateSingleInstance(t *testing.T) {
	h := newMCPHarness(t, "InvalSingleInst")
	projectID := h.createTestProject()

	yaml := `name: "per-instance invalidation"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: tag
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Tag {{x}} from {{expand.content}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha", "beta"})

	// Complete everything.
	for _, x := range []string{"alpha", "beta"} {
		h.mcpClaimOK(t, x+":expand")
		h.mcpSubmitText(t, x+":expand", "expand-"+x)
		h.mcpClaimOK(t, x+":tag")
		h.mcpSubmitText(t, x+":tag", "tag-"+x)
	}

	// Invalidate only alpha:expand. Only alpha:tag should cascade.
	h.mcpInvalidate(t, "alpha:expand", "per-instance invalidation")

	if got, _ := h.taskGet("alpha:expand")["state"].(string); got != "ready" {
		t.Errorf("alpha:expand should be READY after invalidate, got %q", got)
	}
	// Cascade: alpha:tag should no longer be accepted.
	if got, _ := h.taskGet("alpha:tag")["state"].(string); got == "accepted" {
		t.Errorf("alpha:tag should NOT be accepted after upstream invalidate, got %q", got)
	}
	// Siblings untouched.
	if got, _ := h.taskGet("beta:expand")["state"].(string); got != "accepted" {
		t.Errorf("beta:expand should stay accepted (sibling), got %q", got)
	}
	if got, _ := h.taskGet("beta:tag")["state"].(string); got != "accepted" {
		t.Errorf("beta:tag should stay accepted (sibling), got %q", got)
	}

	// Recovery: re-submit alpha:expand, re-do alpha:tag.
	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitText(t, "alpha:expand", "expand-alpha-v2")
	if got, _ := h.taskGet("alpha:tag")["state"].(string); got != "ready" {
		t.Errorf("alpha:tag should be READY after upstream re-submit, got %q", got)
	}
	h.mcpClaimOK(t, "alpha:tag")
	h.mcpSubmitText(t, "alpha:tag", "tag-alpha-v2")
	if got, _ := h.taskGet("alpha:tag")["state"].(string); got != "accepted" {
		t.Errorf("alpha:tag should be accepted after re-submit, got %q", got)
	}
}

// TestMCPDynamicForEachCrossRunArtifactInvalidation verifies
// cross-run cascade works when the artifact writer is a
// for_each instance. A run-1 instance writes an artifact; a
// separate run-2 task reads it. Invalidating the writer
// instance must bounce the cross-run reader too.
func TestMCPDynamicForEachCrossRunArtifactInvalidation(t *testing.T) {
	h := newMCPHarness(t, "CrossRunForEach")
	projectID := h.createTestProject()

	// Run 1: for_each writer. writes_artifacts isn't template-
	// resolved at materialization time, so we use a fixed path
	// and limit the test to a single instance (last-write-wins
	// would otherwise have instances race on the same file).
	writerYAML := `name: "writer run"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    writes_artifacts:
      - notes/alpha.md
    prompt: "Write for {{x}}"
`
	h.mcpCreateRunInline(t, projectID, writerYAML)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha"})

	// Each instance writes its own artifact.
	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitArtifacts(t, "alpha:expand", "",
		map[string]string{"notes/alpha.md": "alpha note v1"})

	// Artifact landed.
	if content, ok := h.readArtifactFile(projectID, "notes/alpha.md"); !ok || content != "alpha note v1" {
		t.Fatalf("artifact write failed: ok=%v content=%q", ok, content)
	}

	// Run 2: reader. Separate run reads the artifact.
	readerYAML := `name: "reader run"
version: 1
tasks:
  - id: summarize
    action: answer
    reads_artifacts: [notes/alpha.md]
    prompt: "Summarize {{artifact:notes/alpha.md}}"
`
	h.mcpCreateRunInline(t, projectID, readerYAML)
	h.mcpClaimOK(t, "summarize")
	h.mcpSubmitText(t, "summarize", "summary v1")

	// Point lastRunSeq back at run 1 before invalidating alpha.
	h.lastRunSeq = 1
	h.lastRunID = fmt.Sprintf("%d:1", projectID)
	h.mcpInvalidate(t, "alpha:expand", "cross-run cascade test")

	// Reader in run 2 must have cascaded to PENDING.
	h.lastRunSeq = 2
	h.lastRunID = fmt.Sprintf("%d:2", projectID)
	if got, _ := h.taskGet("summarize")["state"].(string); got != "pending" {
		t.Errorf("cross-run reader should be PENDING after per-instance writer invalidate, got %q", got)
	}

	// Recovery: re-submit alpha:expand with new artifact
	// content → reader should unblock.
	h.lastRunSeq = 1
	h.lastRunID = fmt.Sprintf("%d:1", projectID)
	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitArtifacts(t, "alpha:expand", "",
		map[string]string{"notes/alpha.md": "alpha note v2"})

	h.lastRunSeq = 2
	h.lastRunID = fmt.Sprintf("%d:2", projectID)
	if got, _ := h.taskGet("summarize")["state"].(string); got != "ready" {
		t.Errorf("cross-run reader should be READY after writer re-submit, got %q", got)
	}
}

// TestMCPDynamicForEachComputeAction verifies action:compute
// works inside a dynamic for_each. Each materialized compute
// instance runs the declared script via enju_execute_task, with
// ENJU_TASK_ID in its env so the script can distinguish
// iterations. The script is seeded into the project via a
// preceding artifact-writing task.
func TestMCPDynamicForEachComputeAction(t *testing.T) {
	h := newMCPHarness(t, "ComputeForEach")
	projectID := h.createTestProject()

	yaml := `name: "compute inside for_each"
version: 1
tasks:
  - id: setup
    action: answer
    prompt: "Seed the script."
    writes_artifacts:
      - scripts/echo_iter.sh

  - id: discover
    action: answer
    prompt: "List items."
    depends_on: [setup]
    outputs:
      items:
        format: list<string>

  - id: run
    action: compute
    script: scripts/echo_iter.sh
    for_each:
      x: "{{discover.items}}"
    prompt: "Run {{x}}"
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// setup seeds an executable script that echoes the env var
	// the handler injects. This is the iteration marker each
	// compute instance receives at runtime.
	h.mcpClaimOK(t, "setup")
	script := "#!/bin/bash\necho \"ran ${ENJU_TASK_ID}\"\n"
	h.mcpSubmitArtifacts(t, "setup", "seeded script",
		map[string]string{"scripts/echo_iter.sh": script})
	// The repository layer doesn't preserve the executable bit
	// through commits, and handleExecuteTask's pull restores
	// the committed mode. Apply chmod 755 right before each
	// execute call, reaching every local clone under the
	// workspace root.
	chmodScript := func() {
		matches, _ := filepath.Glob(filepath.Join(h.workspaceDir, "*", "scripts", "echo_iter.sh"))
		for _, m := range matches {
			_ = os.Chmod(m, 0o755)
		}
	}
	chmodScript()

	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha", "beta"})

	// Each compute instance runs via enju_execute_task. The
	// handler claims + runs + submits all in one call.
	for _, x := range []string{"alpha", "beta"} {
		chmodScript()
		res := h.call(t, "enju_execute_task", map[string]any{
			"task_id": h.taskID(x + ":run"),
		})
		if res.IsError {
			t.Fatalf("execute %s:run returned tool error: %s", x, mcpText(res))
		}
		text := mcpText(res)
		if !strings.Contains(text, "Script completed") {
			t.Errorf("%s:run should report script completed, got:\n%s", x, text)
		}
		if got, _ := h.taskGet(x + ":run")["state"].(string); got != "accepted" {
			t.Errorf("%s:run should be accepted after execute, got %q", x, got)
		}
	}

	// Each instance's result.md should carry the ENJU_TASK_ID
	// the handler injected (containing the instance key).
	for _, x := range []string{"alpha", "beta"} {
		body := h.mcpBareResultMD(t, x+":run")
		wantMarker := x + ":run"
		if !strings.Contains(body, wantMarker) {
			t.Errorf("%s:run result should contain %q (from ENJU_TASK_ID), got: %q",
				x, wantMarker, body)
		}
	}
}

// TestMCPDynamicForEachPerInstanceMultiVoter verifies citizens:N
// on a per-instance dynamic-for_each vote. Each materialized
// vote task independently collects N ballots and tallies in
// isolation.
func TestMCPDynamicForEachPerInstanceMultiVoter(t *testing.T) {
	h := newMCPHarness(t, "MultiVoteA")
	bob := h.newMCPClientAs(t, "MultiVoteB")
	carol := h.newMCPClientAs(t, "MultiVoteC")

	projectID := h.createTestProject()
	yaml := `name: "per-instance multi-voter"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List."
    outputs:
      items:
        format: list<string>
  - id: decide
    action: vote
    citizens: 3
    threshold: majority
    for_each:
      x: "{{discover.items}}"
    prompt: "Vote on {{x}}"
    options:
      - {id: yes, label: "Yes"}
      - {id: no,  label: "No"}
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha", "beta"})

	// alpha cycle: all 3 vote "yes" → resolves as yes.
	clients := []*mcpserver.TestClient{h.client, bob, carol}
	for _, c := range clients {
		h.mcpClaimAs(t, c, "alpha:decide")
	}
	for _, c := range clients {
		h.mcpSubmitVoteAs(t, c, "alpha:decide", "rationale from "+c.Username(), "yes")
	}
	if got, _ := h.taskGet("alpha:decide")["state"].(string); got != "accepted" {
		t.Errorf("alpha:decide should be accepted, got %q", got)
	}
	if got, _ := h.taskGet("alpha:decide")["vote_choice"].(string); got != "yes" {
		t.Errorf("alpha:decide winning choice should be 'yes', got %q", got)
	}

	// beta cycle: 2 yes + 1 no → majority yes.
	for _, c := range clients {
		h.mcpClaimAs(t, c, "beta:decide")
	}
	h.mcpSubmitVoteAs(t, h.client, "beta:decide", "yes please", "yes")
	h.mcpSubmitVoteAs(t, bob, "beta:decide", "yes too", "yes")
	h.mcpSubmitVoteAs(t, carol, "beta:decide", "I say no", "no")
	if got, _ := h.taskGet("beta:decide")["state"].(string); got != "accepted" {
		t.Errorf("beta:decide should be accepted, got %q", got)
	}
	if got, _ := h.taskGet("beta:decide")["vote_choice"].(string); got != "yes" {
		t.Errorf("beta:decide majority should be yes, got %q", got)
	}

	// Independent tallies: alpha's "all yes" should not have
	// leaked into beta's tally (and vice versa). Already
	// implicitly tested above — both resolved with their own
	// correct winners.
}

// TestMCPDynamicForEachPerInstanceMultiReviewer verifies that
// citizens:N on a per-instance dynamic-for_each review works —
// each materialized instance independently collects N reviews
// and tallies in isolation. alpha:check needs 3 reviewers,
// beta:check needs 3 reviewers; approvals on alpha don't count
// toward beta.
func TestMCPDynamicForEachPerInstanceMultiReviewer(t *testing.T) {
	h := newMCPHarness(t, "MultiRevA")
	bob := h.newMCPClientAs(t, "MultiRevB")
	carol := h.newMCPClientAs(t, "MultiRevC")

	projectID := h.createTestProject()
	yaml := `name: "per-instance multi-reviewer"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: check
    action: review
    reviews: expand
    citizens: 3
    for_each:
      x: "{{discover.items}}"
    prompt: "Review {{x}}."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha", "beta"})

	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitText(t, "alpha:expand", "alpha body")
	h.mcpClaimOK(t, "beta:expand")
	h.mcpSubmitText(t, "beta:expand", "beta body")

	// All three reviewers claim + approve alpha:check. Independent
	// tally: alpha should resolve, beta should still be idle.
	clients := []*mcpserver.TestClient{h.client, bob, carol}
	for _, c := range clients {
		h.mcpClaimAs(t, c, "alpha:check")
	}
	for _, c := range clients {
		h.mcpSubmitReviewAs(t, c, "alpha:check", "LGTM from "+c.Username(), "approve")
	}

	if got, _ := h.taskGet("alpha:check")["state"].(string); got != "accepted" {
		t.Errorf("alpha:check should be accepted after 3 approvals, got %q", got)
	}
	// beta:check — nobody reviewed it yet. Should be READY (open
	// for claims) or COLLECTING (not possible w/o claims). The
	// key invariant is: it is NOT accepted.
	if got, _ := h.taskGet("beta:check")["state"].(string); got == "accepted" {
		t.Errorf("beta:check must NOT be accepted (no reviews yet), got %q", got)
	}

	// Now run the beta cycle: only two reviewers approve, third
	// request_changes. any-reject-kills should bounce beta:expand.
	for _, c := range clients {
		h.mcpClaimAs(t, c, "beta:check")
	}
	h.mcpSubmitReviewAs(t, h.client, "beta:check", "fine", "approve")
	// carol's request_changes fires the any-reject rule
	// immediately; bob's late approve must be rejected.
	h.mcpSubmitReviewAs(t, carol, "beta:check", "needs revision", "request_changes")

	if got, _ := h.taskGet("beta:expand")["state"].(string); got != "ready" {
		t.Errorf("beta:expand should be READY after request_changes on beta:check, got %q", got)
	}
	// alpha side must be unaffected by beta's cascade.
	if got, _ := h.taskGet("alpha:expand")["state"].(string); got != "accepted" {
		t.Errorf("alpha:expand should remain accepted, got %q", got)
	}
	if got, _ := h.taskGet("alpha:check")["state"].(string); got != "accepted" {
		t.Errorf("alpha:check should remain accepted, got %q", got)
	}
}

// TestMCPDynamicForEachPerInstanceAllFourDecisions walks every
// valid review decision (approve, reject, request_changes,
// comment) against a per-instance dynamic-for_each review. The
// singleton-equivalent test TestMCPSubmitResultAllFourReviewDecisionsLand
// covers the non-for_each path. This test locks in the same
// four-way contract for dynamic per-instance reviews:
//
//   - approve:         target stays accepted, downstream unblocks
//   - reject (hard):   target goes FAILED, downstream blocked
//   - request_changes: target bounces to READY, cascade fires
//   - comment:         target stays accepted, non-blocking
func TestMCPDynamicForEachPerInstanceAllFourDecisions(t *testing.T) {
	cases := []struct {
		decision            string
		wantExpandStateAfter string
		wantTagReachable     bool // can the per-instance tag become ready?
	}{
		{"approve", "accepted", true},
		{"reject", "failed", false},
		{"request_changes", "ready", false},
		{"comment", "accepted", true},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			h := newMCPHarness(t, "PerInstance4Decisions-Drafter-"+tc.decision)
			reviewer := h.newMCPClientAs(t, "PerInstance4Decisions-Reviewer-"+tc.decision)

			projectID := h.createTestProject()
			yaml := `name: "per-instance 4 decisions"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: check
    action: review
    reviews: expand
    for_each:
      x: "{{discover.items}}"
    prompt: "Review {{x}}."
  - id: tag
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Tag {{x}} based on {{expand.content}}"
`
			h.mcpCreateRunInline(t, projectID, yaml)
			h.mcpClaimOK(t, "discover")
			h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha"})

			h.mcpClaimOK(t, "alpha:expand")
			h.mcpSubmitText(t, "alpha:expand", "alpha body")

			h.mcpClaimAs(t, reviewer, "alpha:check")
			h.mcpSubmitReviewAs(t, reviewer, "alpha:check", "feedback prose", tc.decision)

			if got, _ := h.taskGet("alpha:expand")["state"].(string); got != tc.wantExpandStateAfter {
				t.Errorf("decision %q: alpha:expand state = %q, want %q",
					tc.decision, got, tc.wantExpandStateAfter)
			}

			tagState, _ := h.taskGet("alpha:tag")["state"].(string)
			if tc.wantTagReachable {
				// approve / comment: tag should become ready
				// (its upstream expand is still accepted).
				if tagState != "ready" && tagState != "accepted" {
					t.Errorf("decision %q: alpha:tag should be reachable, got state %q",
						tc.decision, tagState)
				}
			} else {
				// reject: tag must be blocked (upstream failed).
				// request_changes: tag must be blocked (upstream ready).
				if tagState == "ready" || tagState == "accepted" {
					t.Errorf("decision %q: alpha:tag should NOT be reachable, got state %q",
						tc.decision, tagState)
				}
			}
		})
	}
}

// TestMCPDynamicForEachPerInstanceRevisionContext verifies that
// after request_changes on a per-instance review, re-claiming the
// bounced expand instance surfaces the "Previous submission" and
// "Reviewer feedback" blocks the singleton review path already
// provides. Before the fix, fetchReviewFeedback compared
// review.reviews_target (stored as "alpha:expand") against the
// bounced task's bare TaskDefID ("expand") — the match failed and
// neither block rendered.
func TestMCPDynamicForEachPerInstanceRevisionContext(t *testing.T) {
	h := newMCPHarness(t, "RevisionCtxDrafter")
	reviewer := h.newMCPClientAs(t, "RevisionCtxReviewer")

	projectID := h.createTestProject()
	yaml := `name: "per-instance revision context"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List."
    outputs:
      items:
        format: list<string>
  - id: expand
    action: answer
    for_each:
      x: "{{discover.items}}"
    prompt: "Expand {{x}}"
  - id: check
    action: review
    reviews: expand
    for_each:
      x: "{{discover.items}}"
    prompt: "Review {{x}}."
`
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverListAs(t, "items", []string{"alpha"})

	// Drafter submits an initial expand, reviewer bounces it.
	h.mcpClaimOK(t, "alpha:expand")
	h.mcpSubmitText(t, "alpha:expand", "First try on alpha")

	h.mcpClaimAs(t, reviewer, "alpha:check")
	h.mcpSubmitReviewAs(t, reviewer, "alpha:check", "please expand further", "request_changes")

	// Re-claim alpha:expand — the claim response must carry both
	// the "Previous submission" block (with the original content)
	// and the "Reviewer feedback" block (with reviewer username +
	// decision + feedback).
	reclaim := h.callOK(t, "enju_claim_task", map[string]any{
		"task_id": h.taskID("alpha:expand"),
	})
	text := mcpText(reclaim)

	if !strings.Contains(text, "Previous submission") {
		t.Errorf("#1: expected 'Previous submission' block on per-instance re-claim, got:\n%s", text)
	}
	if !strings.Contains(text, "First try on alpha") {
		t.Errorf("#1: previous-submission block should carry original content, got:\n%s", text)
	}
	if !strings.Contains(text, "Reviewer feedback") {
		t.Errorf("#1: expected 'Reviewer feedback' block, got:\n%s", text)
	}
	if !strings.Contains(text, "please expand further") {
		t.Errorf("#1: feedback block should carry reviewer prose, got:\n%s", text)
	}
	if !strings.Contains(text, reviewer.Username()) {
		t.Errorf("#1: feedback block should credit reviewer @%s, got:\n%s", reviewer.Username(), text)
	}
	if !strings.Contains(text, "request_changes") {
		t.Errorf("#1: feedback block should label the decision, got:\n%s", text)
	}
}

// mcpSubmitDiscoverListAs is a convenience for dynamic for_each
// tests: submits a result with one list<string> output field so
// the coordinator's output_lists materialization can fire.
func (h *mcpHarness) mcpSubmitDiscoverListAs(t *testing.T, field string, items []string) {
	t.Helper()
	asAny := make([]any, len(items))
	for i, v := range items {
		asAny[i] = v
	}
	outputs := map[string]any{field: asAny}
	outJSON, err := json.Marshal(outputs)
	if err != nil {
		t.Fatalf("marshal outputs: %v", err)
	}
	h.callOK(t, "enju_submit_result", map[string]any{
		"task_id":      h.taskID("discover"),
		"outputs_json": string(outJSON),
	})
}

// TestMCPDynamicForEachEagerDematerialization is the MCP-layer
// port. Invalidation deletes every descendant row; they're not
// just flipped to PENDING. enju_get_task on a dematerialized id
// returns a not-found error.
func TestMCPDynamicForEachEagerDematerialization(t *testing.T) {
	h := newMCPHarness(t, "DynDemat")
	projectID := h.createTestProject()

	yamlContent := `name: "Dematerialization test"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List genes."
    outputs:
      gene_symbols:
        format: list<string>

  - id: analyze
    action: answer
    for_each:
      gene: "{{discover.gene_symbols}}"
    prompt: "Analyze {{gene}}"

  - id: check
    action: review
    reviews: analyze
    for_each:
      gene: "{{discover.gene_symbols}}"
    prompt: "Check {{gene}}"

  - id: synthesize
    action: answer
    prompt: "Combine: {{analyze.content}}"
`
	h.mcpCreateRunInline(t, projectID, yamlContent)

	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverWithList(t, []string{"BRCA1", "TP53"})

	post := h.runTasks(h.lastRunID)
	if len(post) != 6 {
		t.Fatalf("expected 6 tasks post-materialization, got %d", len(post))
	}

	// Invalidate discover — dematerialize every descendant.
	h.mcpInvalidate(t, "discover", "test dematerialization")

	// Only discover survives.
	postInval := h.runTasks(h.lastRunID)
	if len(postInval) != 1 {
		t.Errorf("expected 1 task (discover) after invalidate, got %d", len(postInval))
	}
	if len(postInval) > 0 {
		tk, _ := postInval[0].(map[string]interface{})
		if def, _ := tk["task_def_id"].(string); def != "discover" {
			t.Errorf("expected surviving task to be discover, got %q", def)
		}
		if state, _ := tk["state"].(string); state != "ready" {
			t.Errorf("expected discover ready after invalidate, got %q", state)
		}
	}

	// enju_get_task on a dematerialized id must fail.
	runPrefix := fmt.Sprintf("%d:%d:", h.lastProjectID, h.lastRunSeq)
	for _, staleID := range []string{
		runPrefix + "BRCA1:analyze",
		runPrefix + "TP53:analyze",
		runPrefix + "BRCA1:check",
		runPrefix + "TP53:check",
		runPrefix + "synthesize",
	} {
		res := h.call(t, "enju_get_task", map[string]any{"task_id": staleID})
		text := mcpText(res)
		if !strings.Contains(strings.ToLower(text), "not found") &&
			!res.IsError {
			t.Errorf("stale task %s still retrievable: %s", staleID, text)
		}
	}

	// Re-submit with a different list — fresh instances.
	h.mcpClaimOK(t, "discover")
	h.mcpSubmitDiscoverWithList(t, []string{"EGFR"})
	reMat := h.runTasks(h.lastRunID)
	// discover + 1 analyze + 1 check + 1 synthesize.
	if len(reMat) != 4 {
		t.Errorf("expected 4 tasks after re-submit with 1 gene, got %d", len(reMat))
	}
}

// TestMCPReviewImplicitGating is the MCP-layer port of
// TestReviewImplicitGating. A publish task that {{draft.content}}
// references gets an auto-injected edge to the review, so it
// stays blocked until the review accepts.
func TestMCPReviewImplicitGating(t *testing.T) {
	h := newMCPHarness(t, "Implicit Drafter")
	reviewer := h.newMCPClientAs(t, "Implicit Reviewer")

	projectID := h.createTestProject()
	h.mcpCreateRunFromFixture(t, projectID, "review-implicit-gating.yaml")

	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "A summary.")

	// check must be ready; publish must still be blocked.
	readyDefs := map[string]bool{}
	for _, raw := range h.readyTasks(h.lastRunID) {
		m, _ := raw.(map[string]interface{})
		if td, _ := m["task_def_id"].(string); td != "" {
			readyDefs[td] = true
		}
	}
	if !readyDefs["check"] {
		t.Errorf("expected check to be ready, got: %v", readyDefs)
	}
	if readyDefs["publish"] {
		t.Fatal("publish should NOT be ready before review completes — implicit gating failed")
	}

	// Approve — publish unlocks.
	h.mcpClaimAs(t, reviewer, "check")
	h.mcpSubmitReviewAs(t, reviewer, "check", "Looks good.", "approve")

	if got := h.taskGet("publish")["state"]; got != "ready" {
		t.Fatalf("expected publish READY after approve, got %v", got)
	}
}
