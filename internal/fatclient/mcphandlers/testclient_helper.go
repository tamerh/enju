package mcphandlers

// Test fixture helpers for constructing apiClient with the same
// session-wiring shape production uses (server.New). Keeps the
// "two ways to construct" surface from drifting — production and
// tests both build the apiClient with an explicit session.
//
// Tests that build `&apiClient{}` literals must always set
// `session:` explicitly. The plain accessor pattern (no lazy
// fallback) makes nil sessions fail loudly at first use rather
// than silently auto-construct a parallel session that diverges
// from production wiring.

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// TestClientConfig is the test-only construction shape for
// apiClient. Mirrors the production Config but exposes the
// pre-built coord client + workspace + logger so tests can
// inject httptest stubs without going through the full
// Register() boot sequence.
type TestClientConfig struct {
	Coord           *coord.Client
	Workspace       *workspace.Workspace
	ModelName       string
	Logger          *slog.Logger
	ProjectRegistry *projectreg.Registry
}

// newAPIClientForTest constructs an apiClient + FatClient in
// lock-step using the same wiring shape mcphandlers.Register
// uses in production. Returns the apiClient so tests can call
// handler methods directly.
//
// Defaults:
//   - Logger → slog.Default() when unset.
//   - ProjectRegistry → a fresh temp-file registry when a
//     workspace is provided but no registry. Post-Phase-F
//     ForProject's path resolution consults the registry, so
//     tests that exercise workspace-touching handlers need one
//     attached. The temp file is cleaned up by Go's tempdir on
//     test exit.
func newAPIClientForTest(cfg TestClientConfig) *apiClient {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	reg := cfg.ProjectRegistry
	if reg == nil && cfg.Workspace != nil {
		// Test-only: synthesize a registry at a unique temp
		// path so handlers that EagerInitProjectClone can
		// register paths without colliding with the real user's
		// ~/.enju/projects.json. Production constructs a real
		// projectreg.Open(DefaultPath()) in cmd/enju.
		dir, err := os.MkdirTemp("", "enju-test-reg-*")
		if err == nil {
			reg = projectreg.Open(filepath.Join(dir, "projects.json"))
		}
	}
	fc := service.New(service.Config{
		Coord:           cfg.Coord,
		Workspace:       cfg.Workspace,
		ModelName:       cfg.ModelName,
		Logger:          logger,
		ProjectRegistry: reg,
	})
	return &apiClient{fc: fc}
}

// newClient is the shorter test fixture form. Tests construct
// `c := newClient(coord.New(...), ws, logger)` instead of
// hand-rolling `&apiClient{...}` with the right fields. ws may
// be nil for tests that don't touch the workspace.
func newClient(coordCli *coord.Client, ws *workspace.Workspace, logger *slog.Logger) *apiClient {
	return newAPIClientForTest(TestClientConfig{
		Coord:     coordCli,
		Workspace: ws,
		Logger:    logger,
	})
}
