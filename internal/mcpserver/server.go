// Package mcpserver implements the Enju MCP server for Claude Desktop/Code integration.
// It's a thin bridge: MCP tool calls → coordinator REST API calls.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Config holds the MCP server configuration.
type Config struct {
	CoordinatorURL string
	CitizenID  string
	CitizenName string
}

// New creates and configures the MCP server with all Enju tools.
func New(cfg Config) *server.MCPServer {
	s := server.NewMCPServer(
		"enju",
		"0.1.0",
		server.WithToolCapabilities(true),
	)

	client := &apiClient{
		baseURL:       cfg.CoordinatorURL,
		citizenID: cfg.CitizenID,
		httpClient:    &http.Client{},
	}

	// Register tools
	s.AddTool(toolListProjects(), client.handleListProjects)
	s.AddTool(toolListReadyTasks(), client.handleListReadyTasks)
	s.AddTool(toolClaimTask(), client.handleClaimTask)
	s.AddTool(toolGetTaskInputs(), client.handleGetTaskInputs)
	s.AddTool(toolSubmitResult(), client.handleSubmitResult)
	s.AddTool(toolReleaseTask(), client.handleReleaseTask)
	s.AddTool(toolGetTask(), client.handleGetTask)
	s.AddTool(toolProjectStatus(), client.handleProjectStatus)
	s.AddTool(toolCreateProject(), client.handleCreateProject)
	s.AddTool(toolMyDashboard(), client.handleMyDashboard)
	s.AddTool(toolUpdateProfile(), client.handleUpdateProfile)

	return s
}

// --- API Client ---

type apiClient struct {
	baseURL       string
	citizenID string
	httpClient    *http.Client
}

func (c *apiClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *apiClient) put(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *apiClient) post(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// --- Tool Definitions ---

func toolListProjects() mcp.Tool {
	return mcp.NewTool("enju_list_projects",
		mcp.WithDescription("List all available projects and their status."),
	)
}

func toolListReadyTasks() mcp.Tool {
	return mcp.NewTool("enju_list_ready_tasks",
		mcp.WithDescription("List tasks that are ready to be claimed. Optionally filter by project ID."),
		mcp.WithString("project_id",
			mcp.Description("Filter by project ID (optional)"),
		),
	)
}

func toolClaimTask() mcp.Tool {
	return mcp.NewTool("enju_claim_task",
		mcp.WithDescription("Claim a task to work on. Returns the task prompt and any upstream results needed."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to claim"),
		),
	)
}

func toolGetTaskInputs() mcp.Tool {
	return mcp.NewTool("enju_get_task_inputs",
		mcp.WithDescription("Get the upstream dependency results for a task. Use this to see what previous tasks produced."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func toolSubmitResult() mcp.Tool {
	return mcp.NewTool("enju_submit_result",
		mcp.WithDescription(`Submit a result for a claimed task. The task must be claimed by you first.

For simple tasks: provide 'content' as a string.
For tasks with named outputs: provide 'outputs' as a JSON object mapping output names to their values.
The task detail shows which format to use (check the 'outputs' schema in the task).`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
		mcp.WithString("content",
			mcp.Description("The result content as plain text (for simple tasks)"),
		),
		mcp.WithString("outputs_json",
			mcp.Description(`For tasks with named outputs: a JSON string of the outputs object. Example: '{"gene_list": "BRCA1, TP53", "pathways": "KEGG:hsa04110"}'`),
		),
	)
}

func toolReleaseTask() mcp.Tool {
	return mcp.NewTool("enju_release_task",
		mcp.WithDescription("Release a claimed task back to the pool if you can't complete it. No penalty for voluntary release."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to release"),
		),
	)
}

func toolGetTask() mcp.Tool {
	return mcp.NewTool("enju_get_task",
		mcp.WithDescription("Get details of a specific task including its state, prompt, and dependencies."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func toolProjectStatus() mcp.Tool {
	return mcp.NewTool("enju_project_status",
		mcp.WithDescription("Get the status of a project including all its tasks and their states."),
		mcp.WithString("project_id",
			mcp.Required(),
			mcp.Description("The ID of the project"),
		),
	)
}

func toolCreateProject() mcp.Tool {
	return mcp.NewTool("enju_create_project",
		mcp.WithDescription(`Create a new Enju project by submitting a YAML definition.

The YAML format:
  name: "Project name"
  version: 1
  ref: "https://github.com/..." (optional, link to issue/ticket)
  for_each:
    variable: [value1, value2] (optional, for parallel expansion)
  tasks:
    - id: task_name
      type: llm_prompt
      prompt: "The prompt text. Use {{other_task.content}} to reference upstream results."

Dependencies are inferred automatically from {{task_id.content}} references in prompts.
Tasks without references to other tasks run in parallel.`),
		mcp.WithString("yaml",
			mcp.Required(),
			mcp.Description("The project definition in YAML format"),
		),
	)
}

func toolUpdateProfile() mcp.Tool {
	return mcp.NewTool("enju_update_profile",
		mcp.WithDescription("Update your citizen profile — name and email."),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Your display name"),
		),
		mcp.WithString("email",
			mcp.Description("Your email (optional, must be unique)"),
		),
	)
}

func toolMyDashboard() mcp.Tool {
	return mcp.NewTool("enju_my_dashboard",
		mcp.WithDescription("Show your citizen dashboard: stats, active tasks, and recent completions."),
	)
}

// --- Tool Handlers ---

func (c *apiClient) handleUpdateProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	email := req.GetString("email", "")

	data, err := c.put(ctx, "/api/v1/citizens/"+c.citizenID+"/profile", map[string]string{
		"name":  name,
		"email": email,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if errMsg, ok := result["error"].(string); ok {
			return mcp.NewToolResultError(errMsg), nil
		}
	}

	// Update local credentials file
	updateLocalCredentials(name)

	return mcp.NewToolResultText(fmt.Sprintf("✓ Profile updated: %s", name)), nil
}

func (c *apiClient) handleMyDashboard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/citizens/"+c.citizenID+"/dashboard")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatDashboard(data)), nil
}

func (c *apiClient) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/projects")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatProjectList(data)), nil
}

func (c *apiClient) handleListReadyTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := "/api/v1/tasks/ready"
	if pid := req.GetString("project_id", ""); pid != "" {
		path += "?project_id=" + pid
	}
	data, err := c.get(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatReadyTasks(data)), nil
}

func (c *apiClient) handleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
		"citizen_id": c.citizenID,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Fetch inputs for resolved prompt
	inputs, _ := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")

	return mcp.NewToolResultText(formatClaimResult(data, inputs)), nil
}

func (c *apiClient) handleGetTaskInputs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}

func (c *apiClient) handleSubmitResult(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	content := req.GetString("content", "")
	outputsJSON := req.GetString("outputs_json", "")

	if content == "" && outputsJSON == "" {
		return mcp.NewToolResultError("either 'content' or 'outputs_json' is required"), nil
	}

	body := map[string]interface{}{
		"model": "claude",
	}

	if outputsJSON != "" {
		var outputs map[string]string
		if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
			return mcp.NewToolResultError("outputs_json must be valid JSON object: " + err.Error()), nil
		}
		body["outputs"] = outputs
	} else {
		body["content"] = content
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatSubmitResult(data, taskID)), nil
}

func (c *apiClient) handleReleaseTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/release", map[string]string{
		"citizen_id": c.citizenID,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}

func (c *apiClient) handleGetTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.get(ctx, "/api/v1/tasks/"+taskID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Also fetch inputs if task has dependencies
	inputs, _ := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")

	return mcp.NewToolResultText(formatTaskDetail(data, inputs)), nil
}

func (c *apiClient) handleProjectStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}

	project, err := c.get(ctx, "/api/v1/projects/"+projectID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	tasks, err := c.get(ctx, "/api/v1/projects/"+projectID+"/tasks")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(formatProjectStatus(project, tasks)), nil
}

func (c *apiClient) handleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	yamlContent, err := req.RequireString("yaml")
	if err != nil {
		return mcp.NewToolResultError("yaml is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/projects", map[string]string{
		"yaml": yamlContent,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(formatCreateProject(data)), nil
}

// --- Helpers ---

// updateLocalCredentials updates the name in ~/.enju/credentials.json
func updateLocalCredentials(name string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := home + "/.enju/credentials.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var creds map[string]interface{}
	if json.Unmarshal(data, &creds) != nil {
		return
	}
	creds["name"] = name
	updated, _ := json.MarshalIndent(creds, "", "  ")
	os.WriteFile(path, updated, 0600)
}

func formatJSON(data []byte) string {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return string(data)
	}
	return pretty.String()
}
