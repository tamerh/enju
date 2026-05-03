package mcphandlers

// enju_review — narrow MCP wrapper around the existing submit
// path for action:review tasks. Counterpart action verb to
// enju_inbox: read what's waiting, then decide.
//
// Why a separate tool instead of just reusing enju_submit_result:
// the submit tool's surface is wide (content, outputs_json,
// artifacts_json, decision, option, model) so the assistant
// has to reason about which fields matter for the action at hand.
// enju_review collapses that to (task_id, decision, content,
// model?) — the only fields a review submitter ever touches.
// Same coordinator endpoint, same validation, same trailer
// metadata; just a constrained schema.

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleReview(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	decision, err := req.RequireString("decision")
	if err != nil {
		return mcp.NewToolResultError("decision is required"), nil
	}
	if msg := validateReviewDecision(decision); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	content := req.GetString("content", "")
	modelOverride := req.GetString("model", "")

	// Fetch the task to confirm it's a review and to drive the
	// fat-client/legacy split. Mirrors handleSubmitResult's
	// preflight — surface "task not found" cleanly instead of
	// letting the legacy path POST into a void.
	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("task %q not found: %v", taskID, metaErr)), nil
	}
	if meta != nil && meta.Action != "" && meta.Action != "review" {
		return mcp.NewToolResultError(fmt.Sprintf("task %q is action:%s, not action:review — use enju_submit_result", taskID, meta.Action)), nil
	}

	if !c.useFatClient(meta) {
		// Git is a hard prerequisite for review submissions:
		// the verdict is committed to the iteration topic
		// branch and the commit_sha is what the coordinator
		// records. No local clone = no place to write.
		return mcp.NewToolResultError("enju_review requires a local workspace; run via `enju mcp` so the verdict is committed to git"), nil
	}
	return c.submitResultFatClient(ctx, taskID, meta, content, nil, nil, nil, decision, "", modelOverride)
}
