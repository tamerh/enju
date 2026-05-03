package mcphandlers

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/mark3labs/mcp-go/mcp"
)

// Template handlers take the client down three short pre-check
// branches — missing args or absent workspace — before ever hitting
// git or the coordinator. Covering those here keeps template.go's
// coverage nonzero and pins the "tool-level errors don't panic"
// contract without booting a real workspace.

func newClientNoWorkspace() *apiClient {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newClient(coord.New(coord.Config{
			BaseURL:  "http://unused",
			Username: "tester",
			Logger:  logger,
		}), nil, logger)
}

func callTool(fn func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) (*mcp.CallToolResult, error) {
	return fn(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: args},
	})
}

// Reuses toolResultText from server_test.go.

func TestHandleListTemplatesMissingProjectID(t *testing.T) {
	c := newClientNoWorkspace()
	res, err := callTool(c.handleListTemplates, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected tool-level error, got %+v", res)
	}
	if !strings.Contains(toolResultText(res), "project_id") {
		t.Errorf("expected project_id error, got %q", toolResultText(res))
	}
}

func TestHandleListTemplatesWithoutWorkspace(t *testing.T) {
	c := newClientNoWorkspace()
	res, err := callTool(c.handleListTemplates, map[string]any{"project_id": float64(1)})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected tool-level error, got %+v", res)
	}
	if !strings.Contains(toolResultText(res), "local workspace") {
		t.Errorf("expected workspace error, got %q", toolResultText(res))
	}
}

func TestHandleDescribeTemplateMissingProjectID(t *testing.T) {
	c := newClientNoWorkspace()
	res, err := callTool(c.handleDescribeTemplate, map[string]any{"path": "enju/templates/x.yaml"})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected tool-level error, got %+v", res)
	}
	if !strings.Contains(toolResultText(res), "project_id") {
		t.Errorf("expected project_id error, got %q", toolResultText(res))
	}
}

func TestHandleDescribeTemplateMissingPath(t *testing.T) {
	c := newClientNoWorkspace()
	res, err := callTool(c.handleDescribeTemplate, map[string]any{"project_id": float64(1)})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected tool-level error, got %+v", res)
	}
	msg := toolResultText(res)
	if !strings.Contains(msg, "path is required") {
		t.Errorf("expected path-required error, got %q", msg)
	}
	// The hint that points the LLM at the right directory.
	if !strings.Contains(msg, "enju/templates/") {
		t.Errorf("expected path error to include enju/templates/ hint, got %q", msg)
	}
}

func TestHandleDescribeTemplateWithoutWorkspace(t *testing.T) {
	c := newClientNoWorkspace()
	res, err := callTool(c.handleDescribeTemplate, map[string]any{
		"project_id": float64(1),
		"path":       "enju/templates/x.yaml",
	})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected tool-level error, got %+v", res)
	}
	if !strings.Contains(toolResultText(res), "local workspace") {
		t.Errorf("expected workspace error, got %q", toolResultText(res))
	}
}
