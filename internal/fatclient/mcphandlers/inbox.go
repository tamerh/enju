package mcphandlers

// enju_inbox MCP tool — thin wrapper over internal/inbox. The
// projection logic (event replay, parent walk, content read,
// formatting) lives in internal/inbox; this file adapts the
// project clone to inbox.Deps. Zero coordinator round-trips —
// inbox renders entirely from local live.jsonl + git.

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/enju-ai/enju/internal/fatclient/inbox"
	"github.com/enju-ai/enju/internal/fatclient/mcpgit"
	"github.com/mark3labs/mcp-go/mcp"
)

// FormatInbox / InboxRow re-exports keep cmd/enju review.go and
// any other in-repo callers stable without forcing them to import
// internal/inbox directly.
var FormatInbox = inbox.FormatInbox

type InboxRow = inbox.InboxRow
type InboxUpstreamRow = inbox.InboxUpstreamRow

func (c *apiClient) handleInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("workspace not configured"), nil
	}
	projectDir := c.workspace.ProjectDir(int64(projectID))
	if projectDir == "" {
		return mcp.NewToolResultText("(no tasks waiting on you — project clone not yet materialized; run a task in this project once to populate)"), nil
	}

	remoteURL, projName, _ := c.fetchProjectMetaFull(ctx, int64(projectID))
	proj, err := c.workspace.ForProject(int64(projectID), remoteURL, projName)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("opening project clone: %v", err)), nil
	}

	livePath := filepath.Join(projectDir, "enju", "events", "live.jsonl")
	rows, err := inbox.BuildInbox(livePath, c.username, &gitOnlyDeps{proj: proj})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(inbox.FormatInbox(rows)), nil
}

// gitOnlyDeps adapts a project clone to inbox.Deps. The full
// Deps surface today is just one method (git read at commit) —
// the projection is otherwise self-contained over live.jsonl.
type gitOnlyDeps struct {
	proj *mcpgit.Project
}

func (d *gitOnlyDeps) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
	return d.proj.ReadFileAtCommit(commitSHA, repoRelPath)
}
