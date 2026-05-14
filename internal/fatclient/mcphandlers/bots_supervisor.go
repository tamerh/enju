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
	"path/filepath"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/mark3labs/mcp-go/mcp"
)

// resolveWorkflowPath returns the absolute path to the workflow
// YAML the caller passed. Required — there is no sensible
// cwd-based default when each workflow is its own self-contained
// unit; the operator must name the YAML explicitly.
func resolveWorkflowPath(arg string) (string, error) {
	if arg == "" {
		return "", fmt.Errorf("workflow argument is required (path to the workflow YAML whose inline bots: section declares the bot)")
	}
	return filepath.Abs(arg)
}

func (c *apiClient) handleBotStart(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	botName := req.GetString("bot", "")
	workflowPath, err := resolveWorkflowPath(req.GetString("workflow", ""))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("resolve workflow: %v", err)), nil
	}
	// project_id is optional; mcp-go returns 0 when absent.
	projectID := int64(req.GetFloat("project_id", 0))

	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}

	// Read the workflow YAML so we can pull this bot's tool
	// allowlist (mcp_tools.allow). The supervisor passes it
	// through to the daemon as --allow-tools so the trust
	// model — manifest declares, runner pins, audit log
	// records — is wired end-to-end. Missing inline bots is
	// fatal here (start needs the declaration to know the bot
	// exists at all); the runner re-validates downstream.
	//
	// We don't pass the human's auth token to the daemon —
	// the daemon authenticates via its own credentials file
	// written by `enju bot setup` and resolved from the
	// manifest's credentials path.
	manifest, err := bots.LoadFromWorkflow(workflowPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("loading workflow: %v", err)), nil
	}
	if manifest == nil {
		return mcp.NewToolResultError(fmt.Sprintf("no bots declared inline in %s", workflowPath)), nil
	}
	// Auto-discover when the operator didn't name a bot AND
	// the workflow declares exactly one. Cuts redundant typing
	// for the dominant single-bot case without breaking the
	// multi-bot path — there the operator must still
	// disambiguate by name.
	if botName == "" {
		switch len(manifest.Bots) {
		case 0:
			return mcp.NewToolResultError(fmt.Sprintf("no bots declared inline in %s", workflowPath)), nil
		case 1:
			botName = manifest.Bots[0].Name
		default:
			names := make([]string, 0, len(manifest.Bots))
			for i := range manifest.Bots {
				names = append(names, manifest.Bots[i].Name)
			}
			return mcp.NewToolResultError(fmt.Sprintf("workflow declares %d bots — pick one: %v", len(manifest.Bots), names)), nil
		}
	}
	bot := manifest.ByName(botName)
	if bot == nil {
		return mcp.NewToolResultError(fmt.Sprintf("bot %q not found in %s", botName, workflowPath)), nil
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

	rb, outcome, err := sup.Start(ctx, bots.StartParams{
		BotName:      botName,
		WorkflowPath: workflowPath,
		Coordinator:  coordURL,
		ProjectID:    projectID,
		AllowTools:   allowTools,
	})
	if err != nil {
		return mcp.NewToolResultError("✗ " + err.Error()), nil
	}
	body, _ := json.MarshalIndent(rb, "", "  ")
	verb := "started"
	if outcome == bots.AlreadyRunning {
		verb = "already running"
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ bot %q %s (pid=%d, log=%s)\n%s",
		rb.Name, verb, rb.PID, rb.LogPath, string(body))), nil
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

// handleBotStartAll iterates the workflow's inline bots,
// starting every one not already running. Continues past
// per-bot failures so a single misconfigured bot doesn't
// block the rest of the fleet from coming up.
func (c *apiClient) handleBotStartAll(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	workflowPath, err := resolveWorkflowPath(req.GetString("workflow", ""))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("resolve workflow: %v", err)), nil
	}
	projectID := int64(req.GetFloat("project_id", 0))

	manifest, err := bots.LoadFromWorkflow(workflowPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("loading workflow: %v", err)), nil
	}
	if manifest == nil || len(manifest.Bots) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("no bots declared inline in %s", workflowPath)), nil
	}
	sup, err := c.botSupervisor()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("supervisor init: %v", err)), nil
	}
	coordURL := c.fc.Coord().BaseURL()

	// Per-bot status reflects the supervisor's idempotent Start
	// surface: "started" = fresh process spawned this call,
	// "already_running" = no-op success (daemon was up from a
	// prior start), "failed" = Start returned an error. The
	// first two satisfy the post-condition "fleet is up"; only
	// "failed" is a real problem.
	type result struct {
		Name   string `json:"name"`
		Status string `json:"status"` // started | already_running | failed
		PID    int    `json:"pid,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(manifest.Bots))
	for _, b := range manifest.Bots {
		var allow []string
		if b.MCPTools != nil {
			allow = b.MCPTools.Allow
		}
		rb, outcome, err := sup.Start(ctx, bots.StartParams{
			BotName:      b.Name,
			WorkflowPath: workflowPath,
			Coordinator:  coordURL,
			ProjectID:    projectID,
			AllowTools:   allow,
		})
		if err != nil {
			results = append(results, result{Name: b.Name, Status: "failed", Error: err.Error()})
			continue
		}
		results = append(results, result{Name: b.Name, Status: outcome.String(), PID: rb.PID})
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
