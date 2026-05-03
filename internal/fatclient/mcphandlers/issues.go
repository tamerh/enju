package mcphandlers

// Issues — project-level structured artifacts. Filed by any
// project member, triaged or closed by any member, linked to
// fix-tasks once spawn arrives (phase 4). DB-only in phase 3;
// the enju/issues/ISSUE-<NNN>.md filesystem mirror lands as a
// follow-up. See docs/living-workflow-design-notes.md § 6.

import (
	"context"
	"fmt"
	"net/url"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleFileIssue(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError("title is required"), nil
	}
	body := map[string]interface{}{
		"title":             title,
		"body":              req.GetString("body", ""),
		"severity":          req.GetString("severity", ""),
		"found_in_run_seq":  req.GetInt("found_in_run_seq", 0),
		"found_in_task_id":  req.GetString("found_in_task_id", ""),
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/issues", projectID), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.FileIssueResult(data)), nil
}

func (c *apiClient) handleListIssues(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	q := url.Values{}
	if st := req.GetString("status", ""); st != "" {
		q.Set("status", st)
	}
	if sv := req.GetString("severity", ""); sv != "" {
		q.Set("severity", sv)
	}
	if limit := req.GetInt("limit", 0); limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}

	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues", projectID)
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	data, err := c.get(ctx, endpoint)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(format.IssueList(data)), nil
}

func (c *apiClient) handleGetIssue(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	seq, err := req.RequireInt("issue_seq")
	if err != nil {
		return mcp.NewToolResultError("issue_seq is required"), nil
	}
	data, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/issues/%d", projectID, seq))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(format.IssueDetail(data)), nil
}

func (c *apiClient) handleTriageIssue(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	seq, err := req.RequireInt("issue_seq")
	if err != nil {
		return mcp.NewToolResultError("issue_seq is required"), nil
	}
	body := map[string]string{}
	if sv := req.GetString("severity", ""); sv != "" {
		body["severity"] = sv
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/issues/%d/triage", projectID, seq), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.TriageIssueResult(data)), nil
}

func (c *apiClient) handleCloseIssue(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	seq, err := req.RequireInt("issue_seq")
	if err != nil {
		return mcp.NewToolResultError("issue_seq is required"), nil
	}
	status := req.GetString("status", "closed")
	body := map[string]string{
		"status":            status,
		"closed_by_task_id": req.GetString("closed_by_task_id", ""),
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/issues/%d/close", projectID, seq), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.CloseIssueResult(data)), nil
}
