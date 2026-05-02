package mcpserver

// enju_inbox — thin MCP wrapper over the coordinator's
// /projects/{id}/inbox endpoint. Renders the response as plain
// text sections so the assistant and the human both read the
// same surface; structured task IDs in `[brackets]` make it
// trivial for the assistant to extract them for follow-up calls
// (enju_claim_task, enju_review).
//
// See internal/store/inbox.go for the data shape and the
// known v1 limitation on compute/vote parent content.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// InboxRow mirrors the wire shape returned by the coordinator's
// /projects/{id}/inbox endpoint. Exported so the `enju inbox`
// CLI can share the decoder + formatter with the MCP tool —
// the two surfaces stay textually identical that way.
type InboxRow struct {
	TaskID          string             `json:"task_id"`
	Action          string             `json:"action"`
	Prompt          string             `json:"prompt"`
	PromptTruncated bool               `json:"prompt_truncated,omitempty"`
	Upstream        []InboxUpstreamRow `json:"upstream"`
}

// InboxUpstreamRow is one parent task's most recent submission
// surfaced inline with the inbox item.
type InboxUpstreamRow struct {
	TaskID    string `json:"task_id"`
	Action    string `json:"action"`
	CommitSHA string `json:"commit_sha"`
	Content   string `json:"content"`
}

func (c *apiClient) handleInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	endpoint := fmt.Sprintf("/api/v1/projects/%d/inbox", projectID)
	data, err := c.get(ctx, endpoint)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if errMsg := errorFromResponse(data); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	var rows []InboxRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return mcp.NewToolResultError("decoding inbox: " + err.Error()), nil
	}
	return mcp.NewToolResultText(FormatInbox(rows)), nil
}

// FormatInbox renders the inbox response as readable plain text.
// One section per task with the prompt inlined and each upstream
// submission below. Empty inbox renders as a single line so
// pattern-matching consumers can detect it. Shared between the
// MCP tool (enju_inbox) and the CLI (`enju inbox`) so both
// surfaces stay textually identical.
func FormatInbox(rows []InboxRow) string {
	if len(rows) == 0 {
		return "(no tasks waiting on you)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Inbox: %d task(s) waiting on you.\n", len(rows))
	for _, r := range rows {
		b.WriteString("\n")
		fmt.Fprintf(&b, "[%s] %s\n", r.TaskID, r.Action)
		if r.Prompt != "" {
			fmt.Fprintf(&b, "> %s\n", r.Prompt)
			if r.PromptTruncated {
				b.WriteString("  (prompt truncated — call enju_get_task for full text)\n")
			}
		}
		if len(r.Upstream) == 0 {
			b.WriteString("  (no upstream submissions)\n")
			continue
		}
		for _, up := range r.Upstream {
			fmt.Fprintf(&b, "\n  Upstream [%s] %s", up.TaskID, up.Action)
			if up.CommitSHA != "" {
				fmt.Fprintf(&b, " (commit %s)", up.CommitSHA)
			}
			b.WriteString(":\n")
			if up.Content == "" {
				b.WriteString("  (no inlined content — likely a compute or vote parent; pull from git via the commit_sha)\n")
				continue
			}
			// Indent each content line so the section structure
			// stays visible at a glance.
			for _, line := range strings.Split(strings.TrimRight(up.Content, "\n"), "\n") {
				b.WriteString("  ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
