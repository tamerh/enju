// Bot supervisor MCP handlers — Phase 4.
//
// Six tools wrap the bots.Supervisor: start, stop, status,
// logs, start_all, stop_all. Each handler is a thin
// translator between the MCP wire shape and the supervisor
// API; supervisor errors surface as MCP tool errors with the
// coord-style "✗ <reason>" prefix the LLM is trained to
// recognize.
//
// Layering note: the supervisor manages OS processes only —
// it spawns `enju bot run --bot=<name> ...`, captures the
// PID, tails the log, and signals on shutdown. The daemon's
// internals (FatClient consumer, Handler interface, all the
// Phase 7 architecture) are opaque to the supervisor; from
// here the daemon is just a binary that takes argv and
// streams stdout/stderr. That separation is intentional —
// the supervisor doesn't import internal/bots beyond the
// manifest reader and the shared Supervisor type, and it
// would still work if the daemon were re-implemented in a
// completely different shape.

package mcphandlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/mark3labs/mcp-go/mcp"
)

// resolveProjectDir returns the absolute path to the project
// directory. Defaults to the fatclient's cwd when the caller
// omits the field — same convention as the cmdBotRun CLI.
//
// MCP-side rationale: the operator's MCP host (claude-code,
// web UI, etc.) typically runs in the project directory the
// user is working in; defaulting to cwd matches that
// expectation. Explicit --project / project= argument lets a
// supervisor running under a different cwd point at the right
// place without environment gymnastics.
func resolveProjectDir(arg string) (string, error) {
	if arg == "" {
		return os.Getwd()
	}
	return filepath.Abs(arg)
}

func (c *apiClient) handleBotStart(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	botName := req.GetString("bot", "")
	projectDir, err := resolveProjectDir(req.GetString("project", ""))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("resolve project: %v", err)), nil
	}
	// project_id is optional; mcp-go returns 0 when absent.
	projectID := int64(req.GetFloat("project_id", 0))

	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}

	// Read the manifest so we can pull this bot's tool
	// allowlist (mcp_tools.allow). The supervisor passes it
	// through to the daemon as --allow-tools so the trust
	// model — manifest declares, runner pins, audit log
	// records — is wired end-to-end. Missing manifest is
	// fatal here (start needs the manifest to know the bot
	// exists at all); the runner re-validates downstream.
	//
	// We don't pass the human's auth token to the daemon —
	// the daemon authenticates via its own credentials file
	// written by `enju bot setup` and resolved from the
	// manifest's credentials path.
	manifest, err := bots.Load(projectDir)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("loading manifest: %v", err)), nil
	}
	if manifest == nil {
		return mcp.NewToolResultError(fmt.Sprintf("no enju/bots.yaml found at %s", projectDir)), nil
	}
	// Auto-discover when the operator didn't name a bot AND
	// the manifest has exactly one. Cuts redundant typing for
	// the dominant solo-project case (one bot, one project)
	// without breaking the multi-bot path — there the operator
	// must still disambiguate by name.
	if botName == "" {
		switch len(manifest.Bots) {
		case 0:
			return mcp.NewToolResultError(fmt.Sprintf("no bots declared in %s/enju/bots.yaml", projectDir)), nil
		case 1:
			botName = manifest.Bots[0].Name
		default:
			names := make([]string, 0, len(manifest.Bots))
			for i := range manifest.Bots {
				names = append(names, manifest.Bots[i].Name)
			}
			return mcp.NewToolResultError(fmt.Sprintf("manifest has %d bots — pick one: %v", len(manifest.Bots), names)), nil
		}
	}
	bot := manifest.ByName(botName)
	if bot == nil {
		return mcp.NewToolResultError(fmt.Sprintf("bot %q not found in %s/enju/bots.yaml", botName, projectDir)), nil
	}
	// Fall back to the manifest's project_id when the caller
	// didn't supply one. Mirrors `enju bot setup`'s precedence
	// rule (caller > manifest > 0). 0 is still valid — the
	// daemon treats it as "poll every project the bot is a
	// member of" — but solo projects with a single coord
	// shouldn't have to repeat the id on every start call.
	if projectID == 0 && manifest.ProjectID > 0 {
		projectID = manifest.ProjectID
	}
	var allowTools []string
	if bot.MCPTools != nil {
		allowTools = bot.MCPTools.Allow
	}

	// Coordinator URL is a fatclient-wide setting — the bot
	// daemon needs to talk to the same coord this MCP session
	// is talking to. We grab it from the long-lived coord
	// client the apiClient already owns.
	coordURL := c.fc.Coord().BaseURL()

	rb, err := sup.Start(ctx, bots.StartParams{
		BotName:     botName,
		ProjectDir:  projectDir,
		Coordinator: coordURL,
		ProjectID:   projectID,
		AllowTools:  allowTools,
	})
	if err != nil {
		return mcp.NewToolResultError("✗ " + err.Error()), nil
	}
	body, _ := json.MarshalIndent(rb, "", "  ")
	return mcp.NewToolResultText(fmt.Sprintf("✓ bot %q started (pid=%d, log=%s)\n%s",
		rb.Name, rb.PID, rb.LogPath, string(body))), nil
}

func (c *apiClient) handleBotStop(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	botName := req.GetString("bot", "")
	if botName == "" {
		return mcp.NewToolResultError("bot is required"), nil
	}
	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}
	res, err := sup.Stop(ctx, botName)
	if err != nil {
		return mcp.NewToolResultError("✗ " + err.Error()), nil
	}
	if res.Graceful {
		return mcp.NewToolResultText(fmt.Sprintf("✓ bot %q stopped gracefully", botName)), nil
	}
	// Not graceful — surface this so operators know the
	// daemon was holding work it couldn't release. Phase 5's
	// stress tests will key off this signal.
	return mcp.NewToolResultText(fmt.Sprintf("⚠ bot %q hard-killed (graceful timeout exceeded — daemon was likely mid-LLM-call or hung; check the log)", botName)), nil
}

func (c *apiClient) handleBotStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}
	running := sup.Status()
	exits := sup.RecentExits()

	// Compose the status report. Both buckets matter — running
	// answers "what's alive?", exits answer "did anything just
	// crash?" Empty-on-empty surfaces a hint pointing the
	// operator at enju_bot_logs for history.
	switch {
	case len(running) == 0 && len(exits) == 0:
		return mcp.NewToolResultText("(no bots running and no recent exits in this fatclient session — use enju_bot_logs <name> if you want to inspect a bot's prior session)"), nil
	case len(running) == 0:
		body, _ := json.MarshalIndent(exits, "", "  ")
		return mcp.NewToolResultText(fmt.Sprintf("0 bots running.\n%d recent exit(s):\n%s", len(exits), string(body))), nil
	case len(exits) == 0:
		body, _ := json.MarshalIndent(running, "", "  ")
		return mcp.NewToolResultText(fmt.Sprintf("%d bot(s) running:\n%s", len(running), string(body))), nil
	}
	combo := map[string]interface{}{
		"running":       running,
		"recent_exits":  exits,
	}
	body, _ := json.MarshalIndent(combo, "", "  ")
	return mcp.NewToolResultText(fmt.Sprintf("%d bot(s) running, %d recent exit(s):\n%s", len(running), len(exits), string(body))), nil
}

func (c *apiClient) handleBotLogs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	botName := req.GetString("bot", "")
	if botName == "" {
		return mcp.NewToolResultError("bot is required"), nil
	}
	lines := int(req.GetFloat("lines", 50))
	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}
	got, err := sup.Logs(botName, lines)
	if err != nil {
		return mcp.NewToolResultError("✗ " + err.Error()), nil
	}
	if len(got) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("(no log file for bot %q yet)", botName)), nil
	}
	body := ""
	for _, line := range got {
		body += line + "\n"
	}
	return mcp.NewToolResultText(fmt.Sprintf("--- last %d lines from bot %q ---\n%s", len(got), botName, body)), nil
}

// handleBotStartAll iterates the manifest, starting every bot
// not already running. Continues past per-bot failures so a
// single misconfigured bot doesn't block the rest of the
// fleet from coming up.
func (c *apiClient) handleBotStartAll(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectDir, err := resolveProjectDir(req.GetString("project", ""))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("resolve project: %v", err)), nil
	}
	projectID := int64(req.GetFloat("project_id", 0))

	manifest, err := bots.Load(projectDir)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("loading manifest: %v", err)), nil
	}
	if manifest == nil || len(manifest.Bots) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("no bots declared at %s/enju/bots.yaml", projectDir)), nil
	}
	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}
	coordURL := c.fc.Coord().BaseURL()

	type result struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		PID   int    `json:"pid,omitempty"`
		Error string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(manifest.Bots))
	for _, b := range manifest.Bots {
		var allow []string
		if b.MCPTools != nil {
			allow = b.MCPTools.Allow
		}
		rb, err := sup.Start(ctx, bots.StartParams{
			BotName:     b.Name,
			ProjectDir:  projectDir,
			Coordinator: coordURL,
			ProjectID:   projectID,
			AllowTools:  allow,
		})
		if err != nil {
			results = append(results, result{Name: b.Name, OK: false, Error: err.Error()})
			continue
		}
		results = append(results, result{Name: b.Name, OK: true, PID: rb.PID})
	}
	body, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(body)), nil
}

func (c *apiClient) handleBotStopAll(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}
	errs := sup.StopAll(ctx)
	if len(errs) == 0 {
		return mcp.NewToolResultText("✓ all bots stopped"), nil
	}
	msg := fmt.Sprintf("%d stop error(s):\n", len(errs))
	for _, e := range errs {
		msg += "  - " + e.Error() + "\n"
	}
	return mcp.NewToolResultError(msg), nil
}
