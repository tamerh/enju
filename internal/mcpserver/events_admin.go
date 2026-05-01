package mcpserver

// operator-facing surface for the EventStore
// kill-switch. Wraps the coordinator's /admin/events HTTP
// endpoints so operators can flip the switch from their MCP
// session ("I see drops piling up — let me disable events
// while I investigate") without curl-ing the API directly.
//
// Auth: same Bearer token as every other write tool. A real
// admin tier with token rotation is hosted-mode work — see
// the pre-launch production-readiness section of TODO.md.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleEventsStatus calls GET /admin/events/status and
// returns enabled + Stats() snapshot as a human-readable
// summary plus structured JSON. Operators use this when
// triaging "is the audit log healthy?"
func (c *apiClient) handleEventsStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/admin/events/status")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	var status struct {
		Enabled    bool  `json:"enabled"`
		Enqueued   int64 `json:"enqueued"`
		Persisted  int64 `json:"persisted"`
		Dropped    int64 `json:"dropped"`
		QueueDepth int   `json:"queue_depth"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return mcp.NewToolResultError("decoding events status: " + err.Error()), nil
	}
	state := "ENABLED"
	if !status.Enabled {
		state = "DISABLED"
	}
	text := fmt.Sprintf(
		"Event store: %s\n"+
			"  Enqueued:    %d events\n"+
			"  Persisted:   %d events\n"+
			"  Dropped:     %d events\n"+
			"  Queue depth: %d (in-flight)\n",
		state, status.Enqueued, status.Persisted, status.Dropped, status.QueueDepth,
	)
	return mcp.NewToolResultText(text), nil
}

// handleSetEventsEnabled calls POST /admin/events/enabled
// with the requested boolean. Wide-blast operation — flips
// the runtime state for the whole coordinator, all tenants,
// all projects. Logged on the server side with the toggling
// citizen for after-the-fact attribution (the kill-switch
// event itself can't fire through a disabled store, so the
// log is the only audit trail).
func (c *apiClient) handleSetEventsEnabled(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	enabled := req.GetBool("enabled", true)
	body := map[string]interface{}{"enabled": enabled}
	data, err := c.post(ctx, "/api/v1/admin/events/enabled", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	var resp struct {
		Enabled bool `json:"enabled"`
		Prior   bool `json:"prior"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding response: " + err.Error()), nil
	}
	state := "ENABLED"
	if !resp.Enabled {
		state = "DISABLED"
	}
	if !resp.Changed {
		return mcp.NewToolResultText(fmt.Sprintf(
			"Event store already %s — no change.", state,
		)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		"Event store kill-switch flipped: %v → %v (now %s).\n"+
			"No backfill on re-enable: events that would have fired during\n"+
			"the disabled window are gone.",
		resp.Prior, resp.Enabled, state,
	)), nil
}
