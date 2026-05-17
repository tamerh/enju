package mcphandlers

// End-to-end integration tests for the bot + model
// registration tools. Spins up a REAL coordinator (api.NewServer
// over store.New), registers a citizen, and exercises the
// register → list → revoke flow through the full HTTP stack.
//
// The previous ship had a critical bug — CreateCitizen
// dropped Kind and ParentID — that no test caught because every
// existing bot test went through raw db.Exec rather than the
// MCP tool path. This file closes that gap.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/api"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

// startTestCoordinator spins up a real coordinator backed by a
// file-backed store in t.TempDir() and returns the URL plus a
// registered citizen's token. Used by the end-to-end tests below
// to verify the whole auth + registration stack against a DB
// configured the same way production runs it.
//
// File-backed (not :memory:) deliberately. modernc serializes all
// connections to a :memory: database onto a single internal
// connection, so the parallel-write DSN config
// (?_pragma=busy_timeout(5000)&_txlock=immediate) is NEVER
// exercised under :memory:. Real production DBs are file-backed,
// so these tests should be too — anything subtle that depends on
// per-connection PRAGMAs (which is everything in the parallel-
// write story) only shows up against files. t.TempDir() handles
// cleanup automatically (DB file + WAL + shm at test end).
func startTestCoordinator(t *testing.T) (string, string, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.NewServer(st, logger)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	// Register a citizen via the bootstrap endpoint (the only
	// unauthenticated route).
	resp, err := ts.Client().Post(ts.URL+"/api/v1/citizens/register", "application/json",
		strings.NewReader(`{"name":"Tamer","username":"tamer"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var registered map[string]interface{}
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode register response: %v (body=%s)", err, body)
	}
	token, _ := registered["token"].(string)
	if token == "" {
		t.Fatalf("no token in register response: %s", body)
	}
	return ts.URL, "tamer", token
}

// TestBotRegisterListFlowE2E is the integration test that would
// have caught the CreateCitizen bug. Goes through the full MCP
// tool surface: register a bot, list bots, verify the bot shows
// up with kind='bot' and the right parent. If CreateCitizen drops
// Kind/ParentID, list_my_bots returns empty here and the test
// fails loudly.
func TestBotRegisterListFlowE2E(t *testing.T) {
	url, username, token := startTestCoordinator(t)

	c := newE2EClient(url, username, token)

	// 1. Register a bot.
	registerResp, err := c.handleRegisterBot(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_register_agent",
			Arguments: map[string]interface{}{
				"name":  "Tamer's Reviewer Bot",
				"label": "laptop",
			},
		},
	})
	if err != nil {
		t.Fatalf("register_bot: %v", err)
	}
	regText := mcpResultText(t, registerResp)
	if !strings.Contains(regText, "Agent registered") {
		t.Fatalf("register output missing success marker: %s", regText)
	}
	if !strings.Contains(regText, "TOKEN") {
		t.Fatalf("register output missing token (caller can't use the bot): %s", regText)
	}
	// Sanity check: bot username should be slugified from name.
	// SlugifyName strips apostrophes, so "Tamer's Reviewer Bot"
	// becomes "tamers-reviewer-bot" (no extra hyphen at the
	// stripped-apostrophe position).
	if !strings.Contains(regText, "@tamers-reviewer-bot") {
		t.Errorf("expected slugified bot username; got: %s", regText)
	}

	// 2. List bots — this is THE assertion that catches the
	// CreateCitizen bug. Buggy code returns 0 bots.
	listResp, err := c.handleListMyBots(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "enju_list_my_agents"},
	})
	if err != nil {
		t.Fatalf("list_my_bots: %v", err)
	}
	listText := mcpResultText(t, listResp)
	if strings.Contains(listText, "don't own any agents") {
		t.Fatal("list_my_bots returned empty — CreateCitizen dropped Kind/ParentID, so the bot doesn't show up under its parent")
	}
	if !strings.Contains(listText, "tamers-reviewer-bot") {
		t.Errorf("listed bots don't include the one we just registered: %s", listText)
	}
	if !strings.Contains(listText, "laptop") {
		t.Errorf("token label 'laptop' not surfaced in list output: %s", listText)
	}
}

// TestBotRevokeFlowE2E exercises the revoke path: register a bot,
// take its token, revoke it, verify the token is dead. The
// ownership check (caller must parent the bot whose token they
// revoke) is exercised because we go through the real
// authentication middleware.
func TestBotRevokeFlowE2E(t *testing.T) {
	url, username, token := startTestCoordinator(t)
	c := newE2EClient(url, username, token)

	// Register the bot and capture its token from the response.
	registerResp, err := c.handleRegisterBot(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_register_agent",
			Arguments: map[string]interface{}{"name": "Test Bot"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	botToken := extractBotToken(t, mcpResultText(t, registerResp))

	// The bot's token should authenticate (we route a list_models
	// call through it as a probe — it's the cheapest authenticated
	// endpoint). Handlers return tool-error results, not Go errors,
	// for auth failures, so we check IsError on the result.
	botClient := newE2EClient(url, "", botToken)
	preResp, err := botClient.handleListModels(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "enju_list_models"},
	})
	if err != nil {
		t.Fatalf("transport error before revoke: %v", err)
	}
	if preResp.IsError {
		t.Fatalf("bot token didn't authenticate before revoke: %s", mcpResultText(t, preResp))
	}

	// Revoke the bot's token via the parent's session.
	revokeResp, err := c.handleRevokeToken(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_revoke_token",
			Arguments: map[string]interface{}{"token": botToken},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if revokeResp.IsError || !strings.Contains(mcpResultText(t, revokeResp), "Token revoked") {
		t.Fatalf("revoke didn't succeed: %s", mcpResultText(t, revokeResp))
	}

	// After revoke: the bot's token must NOT authenticate. The
	// handler returns IsError=true and the surfaced text contains
	// the 401 "invalid or expired token" message from the auth
	// middleware. If IsError is false, leaked tokens stay live
	// forever — the security regression.
	postResp, err := botClient.handleListModels(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "enju_list_models"},
	})
	if err != nil {
		// Transport error is also fine — auth failed and the
		// caller saw it as an error rather than a tool result.
		return
	}
	if !postResp.IsError {
		t.Fatalf("revoked bot token still authenticated — auth bypass! response: %s", mcpResultText(t, postResp))
	}
}

// TestEffectiveModelPrecedence pins the override-vs-default rule
// the per-call model param relies on. Without this test, a refactor
// that flips the precedence (e.g. session always wins) would
// silently break attribution for mixed-model workflows: every submit
// would end up credited to the -model session value, regardless of
// what the caller passed.
func TestEffectiveModelPrecedence(t *testing.T) {
	fc := service.New(service.Config{ModelName: "session-default"})
	c := &apiClient{fc: fc}

	// Override empty → fall back to session default.
	if got := c.fc.EffectiveModel(""); got != "session-default" {
		t.Errorf("empty override: got %q, want session-default", got)
	}
	// Override non-empty → win over session default.
	if got := c.fc.EffectiveModel("call-override"); got != "call-override" {
		t.Errorf("non-empty override: got %q, want call-override", got)
	}
	// Override non-empty even when session default is empty.
	fc2 := service.New(service.Config{ModelName: ""})
	c2 := &apiClient{fc: fc2}
	if got := c2.fc.EffectiveModel("call-override"); got != "call-override" {
		t.Errorf("override with empty session: got %q, want call-override", got)
	}
	// Both empty → empty (the unaided-human case).
	if got := c2.fc.EffectiveModel(""); got != "" {
		t.Errorf("both empty: got %q, want empty (unaided human)", got)
	}
}

// TestModelRegisterAndListE2E covers the catalog extension path:
// list shows the seed; register adds; list shows the new entry.
func TestModelRegisterAndListE2E(t *testing.T) {
	url, username, token := startTestCoordinator(t)
	c := newE2EClient(url, username, token)

	// Initial list — must include the seed catalog.
	listResp, _ := c.handleListModels(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "enju_list_models"},
	})
	initial := mcpResultText(t, listResp)
	if !strings.Contains(initial, "claude-opus-4-7") {
		t.Errorf("seeded catalog missing claude-opus-4-7: %s", initial)
	}

	// Register a custom model.
	regResp, err := c.handleRegisterModel(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_register_model",
			Arguments: map[string]interface{}{
				"username":     "ollama-llama-3-1-70b",
				"display_name": "Llama 3.1 70B (local Ollama)",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpResultText(t, regResp), "Model registered") {
		t.Fatalf("register failed: %s", mcpResultText(t, regResp))
	}

	// List again — new entry must appear alongside the seed.
	listResp, _ = c.handleListModels(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "enju_list_models"},
	})
	updated := mcpResultText(t, listResp)
	if !strings.Contains(updated, "ollama-llama-3-1-70b") {
		t.Errorf("custom model not in catalog after register: %s", updated)
	}
}

// --- helpers ---

// newE2EClient builds an apiClient that talks to a real coordinator.
// Empty username skips username-binding (used for tests that
// authenticate as a bot, where the bot's username doesn't match
// the parent's).
func newE2EClient(url, username, token string) *apiClient {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	coordClient := coord.New(coord.Config{
		BaseURL:   url,
		Username:  username,
		AuthToken: token,
		Logger:    logger,
	})
	return newClient(coordClient, "", logger)
}

// mcpResultText extracts the text content from an MCP tool result.
// Tool handlers return either text-success or text-error; both
// arrive as TextContent on result.Content. Tests assert against
// the surfaced string, not the internal IsError flag.
func mcpResultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if r == nil {
		t.Fatal("nil tool result")
	}
	for _, c := range r.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// extractBotToken pulls the UUID-ish token line out of the
// register_bot response text. The handler renders the token after
// the "TOKEN" header; we grab the first token-shaped string after
// that marker.
func extractBotToken(t *testing.T, text string) string {
	t.Helper()
	idx := strings.Index(text, "TOKEN")
	if idx < 0 {
		t.Fatalf("no TOKEN marker in register output: %s", text)
	}
	tail := text[idx:]
	// Token is on its own line, indented two spaces, ~36-char UUID.
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		// UUIDs are 36 chars with 4 hyphens — close enough to
		// disambiguate from header lines.
		if len(line) >= 32 && strings.Count(line, "-") >= 4 {
			return line
		}
	}
	t.Fatalf("couldn't find token in register output: %s", text)
	return ""
}
