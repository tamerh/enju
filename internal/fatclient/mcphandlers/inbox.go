package mcphandlers

// enju_inbox MCP tool — thin wrapper over service.FatClient.BuildInbox,
// which in turn delegates the projection logic (event replay,
// parent walk, content read, formatting) to internal/inbox.
// Zero coordinator round-trips beyond the project-meta fetch
// inside the service layer; inbox renders entirely from local
// live.jsonl + git.

import (
	"context"

	"github.com/enju-ai/enju/internal/fatclient/inbox"
	"github.com/mark3labs/mcp-go/mcp"
)

// FormatInbox / InboxRow re-exports keep cmd/enju review.go
// and any other in-repo callers stable without forcing them
// to import internal/inbox directly.
var FormatInbox = inbox.FormatInbox

type InboxRow = inbox.InboxRow
type InboxUpstreamRow = inbox.InboxUpstreamRow

func (c *apiClient) handleInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	res, err := c.fc.BuildInbox(ctx, int64(projectID), c.username())
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !res.ProjectClonePresent {
		return mcp.NewToolResultText("(no tasks waiting on you — project clone not yet materialized; run a task in this project once to populate)"), nil
	}
	return mcp.NewToolResultText(inbox.FormatInbox(res.Rows)), nil
}
