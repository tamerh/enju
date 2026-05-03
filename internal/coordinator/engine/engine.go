package engine

import (
	"log/slog"
)

// EngineVersion is checked by the coordinator on every
// /apply request. A mismatch means the client is running a
// different version of the engine than the coordinator, and
// the plan might be computed against assumptions the
// coordinator doesn't share. Hard reject + "update your
// enju binary" message.
const EngineVersion = "j1.0"

// Engine is the stateless computation core. It holds a
// read-only store reference for inspecting current state
// and a logger for diagnostics. Every public method takes
// inputs, reads state as needed, and returns a Plan —
// never writes state.
//
// Create one Engine per request or per MCP client lifetime;
// it's lightweight (no caches, no goroutines, no locks).
type Engine struct {
	store  ReadStore
	logger *slog.Logger
}

// New creates an Engine backed by the given read-only store.
func New(store ReadStore, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{store: store, logger: logger}
}
