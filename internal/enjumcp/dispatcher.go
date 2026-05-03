// Package enjumcp wraps the MCP dispatcher and owns the tool
// schema registry. Zero business logic — no git, no DB, no store
// calls. It only:
//   - wraps the underlying MCP runtime (mark3labs/mcp-go)
//   - owns the tool registry (schemas in schemas.go, list in
//     registry.go)
//   - registers the caller-supplied handler for each tool
//
// Today the only consumer is internal/fatclient/mcphandlers,
// which builds a handler map for every tool in Registry and
// hands it to enjumcp.New(). If hosted-mode adds a coord-side
// MCP transport later, it'd register its own handlers via the
// same Config.Handlers map.
//
// Package name: "enjumcp" rather than "mcp" because the upstream
// library github.com/mark3labs/mcp-go/mcp already owns the short
// name; collision-free is worth the four extra letters.
package enjumcp

import "github.com/mark3labs/mcp-go/server"

// Handler is the canonical MCP tool handler signature. Aliased
// from mcp-go so callers don't need to import that package
// directly when populating handler maps.
type Handler = server.ToolHandlerFunc

// Config configures the dispatcher. Minimal by design — the
// only knobs are the agent-facing identity strings and the
// handler map.
type Config struct {
	// Name advertised to the agent (e.g. "enju").
	Name string
	// Version advertised to the agent.
	Version string
	// Instructions: the long agent-prompt passed at construction.
	// Tells the agent how to use the tools, what the workflow is,
	// what icons mean, etc.
	Instructions string
	// Handlers maps tool name → handler. Every entry MUST
	// correspond to a tool in Registry; an entry with an
	// unknown name panics at New(). Missing entries (a tool
	// in the registry with no handler) are not flagged here
	// — the caller's Register function is expected to be
	// exhaustive.
	Handlers map[string]Handler
}

// Server is the configured dispatcher. Embeds the underlying
// MCP runtime so transport-layer code (stdio, SSE) can drive
// it via the standard mcp-go entry points.
type Server struct {
	mcp *server.MCPServer
}

// New constructs a dispatcher that has every (name, handler)
// in cfg.Handlers registered with the underlying MCP runtime,
// using the tool schema from Registry.
//
// Panics on a handler whose name isn't in the registry — that
// indicates a programmer error in the calling Register function
// (registered a name that doesn't exist) and is loud-and-early
// rather than silent-and-late.
func New(cfg Config) *Server {
	s := server.NewMCPServer(
		cfg.Name,
		cfg.Version,
		server.WithToolCapabilities(true),
		server.WithInstructions(cfg.Instructions),
	)
	for name, h := range cfg.Handlers {
		t, ok := ByName(name)
		if !ok {
			panic("mcp: handler registered for unknown tool: " + name)
		}
		s.AddTool(t, h)
	}
	return &Server{mcp: s}
}

// MCPServer exposes the underlying mcp-go runtime so transport
// wrappers (stdio in cmd/enju, SSE in the future hosted mode)
// can serve it via mcp-go's standard ServeStdio / NewSSEServer
// entry points.
//
// Direct access is intentional: this dispatcher's job is
// dispatch, not transport. Wrapping every transport here would
// duplicate mcp-go's API for no gain.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcp
}
