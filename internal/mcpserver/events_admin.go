package mcpserver

// MCP wrapper for the EventStore status endpoint. Read-only —
// operators flip the kill-switch by editing enju.conf and sending
// SIGHUP to the coordinator, not via MCP.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleEventsStatus calls GET /events/status and returns enabled
// + Stats() snapshot as a human-readable summary. Operators use
// this when triaging "is the audit log healthy?"
func (c *apiClient) handleEventsStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/events/status")
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
