package mcphandlers

// Workflow-discovery handlers. enju_list_workflows enumerates every
// *.yaml/*.yml in the project's local clone outside hidden dirs;
// enju_describe_workflow returns the full YAML + param schema for
// one workflow. Both are read-only client-side operations — the
// coordinator doesn't know about workflows beyond a run's
// source_path provenance column. Workspace open + best-effort pull
// live in internal/fatclient/service/template_ops.go.

import (
	"context"
	"encoding/json"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListWorkflows(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	workflows, err := c.fc.ListWorkflows(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(map[string]interface{}{
		"project_id": projectID,
		"workflows":  workflows,
	})
	return mcp.NewToolResultText(format.ListWorkflows(data)), nil
}

// handleDescribeWorkflow — pure client-side tool. Loads one
// workflow YAML from the local clone and returns its full
// metadata + param documentation. Unlike list, this DOES parse
// the file.
func (c *apiClient) handleDescribeWorkflow(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	workflowPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required (path to a workflow YAML file inside the project's repo — anywhere, e.g. 'enju.yaml', 'workflows/scan-deps/enju.yaml')"), nil
	}
	loaded, err := c.fc.DescribeWorkflow(ctx, int64(projectID), workflowPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(loaded.Details)
	return mcp.NewToolResultText(format.DescribeWorkflow(data)), nil
}
