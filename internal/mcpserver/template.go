package mcpserver

// Template-discovery handlers. enju_list_templates enumerates
// enju_templates/*.yaml files in the project's local clone;
// enju_describe_template returns the full YAML + param schema
// for a single template. Both are read-only client-side
// operations — the coordinator doesn't know about templates
// beyond a run's source_path provenance column.

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListTemplates(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("enju_list_templates requires a local workspace (MCP client mode)"), nil
	}
	proj, _, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Best-effort pull so templates pushed by other citizens
	// since the local clone was last updated show up in the
	// menu. A failed pull (offline, diverged, auth, etc.) is
	// logged and we scan whatever's currently on disk — the
	// user still gets a menu, and the error surfaces on the
	// next tool call if it's load-bearing.
	proj.Lock()
	if perr := proj.Pull(); perr != nil {
		c.logger.Debug("list_templates pull failed, scanning local state", "err", perr)
	}
	proj.Unlock()
	templates, err := proj.ListTemplates()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(map[string]interface{}{
		"project_id": projectID,
		"templates":  templates,
	})
	return mcp.NewToolResultText(formatListTemplates(data)), nil
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
		return mcp.NewToolResultError("path is required (e.g. 'enju_templates/gwas.yaml')"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("enju_describe_template requires a local workspace (MCP client mode)"), nil
	}
	proj, _, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Best-effort pull: surface templates pushed after the
	// clone was last updated. Same fallback as list_templates
	// — on failure, read whatever's on disk.
	proj.Lock()
	if perr := proj.Pull(); perr != nil {
		c.logger.Debug("describe_template pull failed, reading local state", "err", perr)
	}
	proj.Unlock()
	loaded, err := proj.LoadTemplate(templatePath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(loaded.Summary)
	return mcp.NewToolResultText(formatDescribeTemplate(data)), nil
}
