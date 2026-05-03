package mcphandlers

// enju_notifications — read/unread notification list for the
// active project. The actual log-walk + cursor IO lives in
// internal/fatclient/service/notifications_ops.go; this file
// is the transport-layer translator: parse args, dispatch to
// service, render unread/read markers in newest-first order.

import (
	"context"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

// handleNotifications returns the list with read/unread markers.
//
// Args:
//
//	project_id  (required) the project to surface
//	limit       optional — default 20, max 100 (clamped in service)
//	mark_read   optional — default true; advances the read cursor
//	            to the highest seq returned
func (c *apiClient) handleNotifications(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	limit := req.GetInt("limit", 20)
	markRead := req.GetBool("mark_read", true)

	res, err := c.session.ReadNotifications(int64(projectID), c.username(), limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read live log: %v", err)), nil
	}
	if !res.ProjectClonePresent {
		return mcp.NewToolResultText("(no notifications — project has no local clone yet; will populate after first task work)"), nil
	}

	out := formatNotifications(res.Matches, res.LastReadSeq)

	if markRead {
		if err := c.session.MarkNotificationsRead(int64(projectID), res.Matches); err != nil {
			c.session.Logger().Warn("notifications: failed to persist read-seq", "err", err)
		}
	}

	return mcp.NewToolResultText(out), nil
}

// formatNotifications renders the user-visible list. Plain ASCII
// — leading "*" for unread, two spaces for read. Newest first.
func formatNotifications(matches []service.Notification, lastReadSeq int64) string {
	if len(matches) == 0 {
		return "(no notifications)"
	}
	var b strings.Builder
	for _, m := range matches {
		marker := "  "
		if m.Seq > lastReadSeq {
			marker = "* "
		}
		fmt.Fprintf(&b, "%s%s  %s\n",
			marker,
			m.Ts.Local().Format("2006-01-02 15:04:05"),
			m.Message,
		)
	}
	return strings.TrimRight(b.String(), "\n")
}
