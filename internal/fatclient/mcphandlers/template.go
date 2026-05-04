package mcphandlers

// Template-discovery handlers. enju_list_templates enumerates
// enju/templates/*.yaml files in the project's local clone;
// enju_describe_template returns the full YAML + param schema
// for a single template. Both are read-only client-side
// operations — the coordinator doesn't know about templates
// beyond a run's source_path provenance column. Workspace
// open + best-effort pull live in
// internal/fatclient/service/template_ops.go.

import (
	"context"
	"encoding/json"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListTemplates(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	templates, err := c.fc.ListTemplates(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(map[string]interface{}{
		"project_id": projectID,
		"templates":  templates,
	})
	return mcp.NewToolResultText(format.ListTemplates(data)), nil
}

// handleDescribeTemplate — pure client-side tool. Loads one
// template file from the local clone and returns its full
// metadata + param documentation.
func (c *apiClient) handleDescribeTemplate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	templatePath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required (e.g. 'enju/templates/gwas.yaml')"), nil
	}
	loaded, err := c.fc.DescribeTemplate(ctx, int64(projectID), templatePath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(loaded.Summary)
	return mcp.NewToolResultText(format.DescribeTemplate(data)), nil
}
