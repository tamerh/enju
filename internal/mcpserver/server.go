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

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Config holds the MCP server configuration.
type Config struct {
	CoordinatorURL string
	ParticipantID  string
	ParticipantName string
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
		participantID: cfg.ParticipantID,
		httpClient:    &http.Client{},
	}

	// Register tools
	s.AddTool(toolListProblems(), client.handleListProblems)
	s.AddTool(toolListReadyTasks(), client.handleListReadyTasks)
	s.AddTool(toolClaimTask(), client.handleClaimTask)
	s.AddTool(toolGetTaskInputs(), client.handleGetTaskInputs)
	s.AddTool(toolSubmitResult(), client.handleSubmitResult)
	s.AddTool(toolReleaseTask(), client.handleReleaseTask)
	s.AddTool(toolGetTask(), client.handleGetTask)
	s.AddTool(toolProblemStatus(), client.handleProblemStatus)

	return s
}

// --- API Client ---

type apiClient struct {
	baseURL       string
	participantID string
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

func toolListProblems() mcp.Tool {
	return mcp.NewTool("enju_list_problems",
		mcp.WithDescription("List all available problems and their status."),
	)
}

func toolListReadyTasks() mcp.Tool {
	return mcp.NewTool("enju_list_ready_tasks",
		mcp.WithDescription("List tasks that are ready to be claimed. Optionally filter by problem ID."),
		mcp.WithString("problem_id",
			mcp.Description("Filter by problem ID (optional)"),
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
		mcp.WithDescription("Submit a result for a claimed task. The task must be claimed by you first."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("The result content"),
		),
		mcp.WithString("result_type",
			mcp.Description("Result type: 'text' (default) or 'json'"),
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

func toolProblemStatus() mcp.Tool {
	return mcp.NewTool("enju_problem_status",
		mcp.WithDescription("Get the status of a problem including all its tasks and their states."),
		mcp.WithString("problem_id",
			mcp.Required(),
			mcp.Description("The ID of the problem"),
		),
	)
}

// --- Tool Handlers ---

func (c *apiClient) handleListProblems(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/problems")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}

func (c *apiClient) handleListReadyTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := "/api/v1/tasks/ready"
	if pid := req.GetString("problem_id", ""); pid != "" {
		path += "?problem_id=" + pid
	}
	data, err := c.get(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}

func (c *apiClient) handleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
		"participant_id": c.participantID,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Also fetch inputs for convenience
	inputs, _ := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")

	result := fmt.Sprintf("Task claimed successfully.\n\n**Task Details:**\n%s", formatJSON(data))
	if inputs != nil && len(inputs) > 0 {
		result += fmt.Sprintf("\n\n**Upstream Inputs:**\n%s", formatJSON(inputs))
	}

	return mcp.NewToolResultText(result), nil
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
	content, err := req.RequireString("content")
	if err != nil {
		return mcp.NewToolResultError("content is required"), nil
	}
	resultType := req.GetString("result_type", "text")

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", map[string]interface{}{
		"content":     content,
		"result_type": resultType,
		"model":       "claude",
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}

func (c *apiClient) handleReleaseTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/release", map[string]string{
		"participant_id": c.participantID,
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
	return mcp.NewToolResultText(formatJSON(data)), nil
}

func (c *apiClient) handleProblemStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	problemID, err := req.RequireString("problem_id")
	if err != nil {
		return mcp.NewToolResultError("problem_id is required"), nil
	}

	// Get problem info
	problem, err := c.get(ctx, "/api/v1/problems/"+problemID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Get all tasks
	tasks, err := c.get(ctx, "/api/v1/problems/"+problemID+"/tasks")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result := fmt.Sprintf("**Problem:**\n%s\n\n**Tasks:**\n%s", formatJSON(problem), formatJSON(tasks))
	return mcp.NewToolResultText(result), nil
}

// --- Helpers ---

func formatJSON(data []byte) string {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return string(data)
	}
	return pretty.String()
}
