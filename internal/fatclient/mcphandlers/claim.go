package mcphandlers

// Claim handlers. handleClaimTask is now a thin translator
// over service.FatClient.ClaimTask — orchestration (pre-claim
// reconcile, untracked-presence check, review-feedback +
// previous-submission gather) lives in
// internal/fatclient/service/claim.go. handleGetTaskInputs is
// the standalone descriptor fetcher; it still calls into
// service via session.FetchAndResolveLocally so the local-
// resolve path is shared.

import (
	"context"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	includeContext := req.GetBool("include_context", true)

	result, err := c.fc.ClaimTask(ctx, service.ClaimParams{
		TaskID:     taskID,
		IncludeContext: includeContext,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(format.ClaimResult(
		result.Data, result.Inputs, c.username(),
		result.ReviewFeedback, result.PreviousSubmission,
	)), nil
}

func (c *apiClient) handleGetTaskInputs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	meta, metaErr := c.fc.FetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		return mcp.NewToolResultError(metaErr.Error()), nil
	}

	if c.fc.UseFatClient(meta) {
		data, err := c.fc.FetchAndResolveLocally(ctx, meta)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(formatJSON(data)), nil
	}

	data, err := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}

