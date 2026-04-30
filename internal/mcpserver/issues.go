package mcpserver

// Issues — project-level structured artifacts. Filed by any
// project member, triaged or closed by any member, linked to
// fix-tasks once spawn arrives (phase 4). DB-only in phase 3;
// the enju/issues/ISSUE-<NNN>.md filesystem mirror lands as a
// follow-up. See docs/living-workflow-design-notes.md § 6.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

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
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding response: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	id, _ := resp["slug"].(string)
	severity, _ := resp["severity"].(string)
	return mcp.NewToolResultText(fmt.Sprintf("✓ Filed %s [%s]: %s", id, severity, title)), nil
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
	var issues []map[string]interface{}
	if err := json.Unmarshal(data, &issues); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if len(issues) == 0 {
		return mcp.NewToolResultText("(no issues match)"), nil
	}
	out := ""
	for _, it := range issues {
		id, _ := it["id"].(string)
		title, _ := it["title"].(string)
		status, _ := it["status"].(string)
		severity, _ := it["severity"].(string)
		out += fmt.Sprintf("• %s [%s/%s] %s\n", id, status, severity, title)
	}
	return mcp.NewToolResultText(out), nil
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
	// Pretty-print as YAML-ish frontmatter + body. Same shape
	// the future filesystem mirror will write to disk.
	var it map[string]interface{}
	if err := json.Unmarshal(data, &it); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	out := fmt.Sprintf("---\nid: %v\ntitle: %v\nstatus: %v\nseverity: %v\nfiled_by: %v\nfiled_at: %v\n",
		it["id"], it["title"], it["status"], it["severity"], it["filed_by"], it["filed_at"])
	if it["found_in_run_id"] != nil {
		out += fmt.Sprintf("found_in_run_id: %v\n", it["found_in_run_id"])
	}
	if it["found_in_task_id"] != nil {
		out += fmt.Sprintf("found_in_task_id: %v\n", it["found_in_task_id"])
	}
	if it["triaged_at"] != nil {
		out += fmt.Sprintf("triaged_at: %v\ntriaged_by: %v\n", it["triaged_at"], it["triaged_by"])
	}
	if it["closed_at"] != nil {
		out += fmt.Sprintf("closed_at: %v\nclosed_by_task_id: %v\n", it["closed_at"], it["closed_by_task_id"])
	}
	out += "---\n"
	if body, _ := it["body"].(string); body != "" {
		out += "\n" + body + "\n"
	}
	return mcp.NewToolResultText(out), nil
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
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Triaged ISSUE-%03d [severity=%v]", seq, resp["severity"])), nil
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
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Closed ISSUE-%03d [status=%s]", seq, status)), nil
}
