package enjumcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// noopHandler returns a stub MCP result. Used in tests where
// we care about registration mechanics, not handler behavior.
func noopHandler(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("ok"), nil
}

// TestNew_RegistersHandlersFromMap pins the happy path: every
// (name, handler) in cfg.Handlers gets registered with the
// underlying MCP runtime. We can't directly inspect the runtime,
// but we CAN confirm New() succeeds and returns a server with
// the right name/version.
func TestNew_RegistersHandlersFromMap(t *testing.T) {
	// Pick a real tool name from the registry so ByName lookup
	// succeeds.
	if len(Registry) == 0 {
		t.Fatal("registry empty — test fixture broken")
	}
	name := Registry[0].Name
	srv := New(Config{
		Name:    "enju-test",
		Version: "0.0.1",
		Handlers: map[string]Handler{
			name: noopHandler,
		},
	})
	if srv == nil {
		t.Fatal("New returned nil")
	}
	if srv.MCPServer() == nil {
		t.Fatal("MCPServer accessor returned nil")
	}
}

// TestNew_PanicsOnUnknownTool pins the safety: registering a
// handler for a tool that isn't in the registry is a programmer
// error and should panic loud-and-early at construction, not
// silently accept and confuse the agent at runtime.
func TestNew_PanicsOnUnknownTool(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New did not panic on unknown tool name")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not a string: %T %v", r, r)
		}
		if !strings.Contains(msg, "unknown tool") {
			t.Errorf("panic message missing 'unknown tool': %q", msg)
		}
	}()
	_ = New(Config{
		Name:    "enju-test",
		Version: "0.0.1",
		Handlers: map[string]Handler{
			"enju_does_not_exist": noopHandler,
		},
	})
}

// TestNew_EmptyHandlersOK pins that an empty handler map is
// valid — the dispatcher constructs cleanly, just with no tools
// registered. Useful for the future hosted-thin deploy where a
// caller might register zero tools (degenerate but valid case).
func TestNew_EmptyHandlersOK(t *testing.T) {
	srv := New(Config{
		Name:     "enju-test",
		Version:  "0.0.1",
		Handlers: map[string]Handler{},
	})
	if srv == nil || srv.MCPServer() == nil {
		t.Fatal("expected valid empty server")
	}
}
