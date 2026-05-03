package mcphandlers

// MCP wrapper for the EventStore status endpoint. Read-only —
// operators flip the kill-switch by editing enju.conf and sending
// SIGHUP to the coordinator, not via MCP.

import (
	"context"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/mark3labs/mcp-go/mcp"
)

// handleEventsStatus calls GET /events/status and renders the
// stats via the shared formatter. Operators use this when
// triaging "is the audit log healthy?"
func (c *apiClient) handleEventsStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/events/status")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.EventsStatus(data)), nil
}
