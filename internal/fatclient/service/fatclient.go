// Package service is the fat-client orchestration layer between
// per-tool handlers (mcphandlers/*) and the underlying primitives
// (coord HTTP client + workspace git/fs). Methods on FatClient bundle
// the dependencies handlers need — coord client, local workspace,
// citizen identity, model attribution, logger — so each tool's
// orchestration can be expressed without rebuilding the wiring at
// every call site.
//
// The service layer is intentionally thin: it owns the helpers that
// every per-tool flow uses (fetch task meta, open workspace project,
// pull-with-reconcile, commit author cache) and the per-tool service
// methods. It does NOT own MCP transport concerns (parameter
// parsing, response formatting) — those stay in mcphandlers, which
// calls into FatClient.
//
// Mirrors internal/coordinator/service/ on the coord side: same
// "extract orchestration from transport" shape, same "construct
// once, share for the life of the process" lifecycle.
package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/enju-ai/enju/internal/common/types"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/project"
)

// Config is the constructor input for New. Coord and Workspace are
// the load-bearing dependencies; ModelName + Logger are
// process-scoped attribution / diagnostics.
type Config struct {
	Coord     *coord.Client
	Workspace *project.Opener
	ModelName string
	Logger    *slog.Logger

	// ProjectRegistry tracks the projects this fat-client knows
	// about (standard clones + externally adopted dirs). Optional
	// — when nil, ListMaterializedProjects falls back to walking
	// the workspace root, and Register/Touch/Unregister are
	// no-ops. Production wiring (`enju mcp`, `enju ui`) supplies
	// projectreg.Open(projectreg.DefaultPath()); tests can inject
	// a temp-path registry or omit it entirely.
	ProjectRegistry *projectreg.Registry
}

// FatClient is the published consumer handle for the fat-client
// orchestration layer. Constructed once at process boot
// (mcphandlers.Register for `enju mcp`, the analogous wiring in
// `enju ui`, etc.) and shared across every consumer that calls
// into service.* . The methods on FatClient are the contract
// in-process consumers (MCP handlers, web handlers, CLI) program
// against; out-of-process consumers go through an MCP transport
// that wraps the same surface.
//
// Safe for concurrent use — all underlying dependencies are
// themselves goroutine-safe; the profile-cache load is gated by
// sync.Once.
type FatClient struct {
	coord       *coord.Client
	project     *project.Opener
	// enjugit is the new workspace handle, opened in parallel to
	// `project` during the project→enjugit migration. Both point
	// at the same on-disk root, but reach it through different
	// abstractions. Each call site is ported one at a time:
	// pre-port sites still go through `project`; post-port sites
	// open Workflow / View handles via `enjugit`. After the last
	// site flips, the old `project` field gets removed.
	enjugit *enjugit.Workspace
	modelName   string
	logger      *slog.Logger
	projectRegistry  *projectreg.Registry

	// Cached citizen profile (name + email + kind) used to
	// populate git commit author fields on the fat-client submit
	// path and to classify the calling citizen ("human" / "bot" /
	// "model"). Fetched lazily on first use and held for the
	// life of the FatClient.
	profileOnce  sync.Once
	profileName  string
	profileEmail string
	profileKind  string

	// botClones holds bot-resolved clones keyed by projectID.
	// Populated by ResolveBotWorkspace; consulted by OpenProject
	// so subsequent submit / claim / reset calls within the bot
	// daemon route to its own clone instead of falling through
	// to the operator-side ForProject lookup. One FatClient =
	// one citizen identity, so a single map covers every project
	// the daemon polls without bleeding across bot identities.
	//
	// Operator-side FatClients leave this nil and OpenProject
	// flows to the operator's working tree as before.
	botClonesMu sync.Mutex
	botClones   map[int64]*project.Clone

	// botWorkflows mirrors botClones for the new enjugit-based
	// path. Populated by ResolveBotWorkspace alongside botClones
	// so OpenWorkflow (the enjugit-side analog of OpenProject)
	// routes to the same per-bot clone that the project path
	// uses. Two maps until the project package is fully retired;
	// at that point this collapses to one.
	botWorkflows map[int64]*enjugit.Workflow
}

// New constructs a FatClient. Logger defaults to slog.Default() when
// the caller didn't supply one — service helpers always have
// somewhere to log without a nil check at every call site.
//
// When both Workspace and ProjectRegistry are configured, the
// registry is attached to the workspace at construction so that
// ForProject / ProjectDir / OpenExisting can resolve project
// paths directly. The registry is the durable per-machine
// "project N → home path" record; the workspace consults it
// each call rather than maintaining its own mirror.
// AttachRegistry is a nil-safe no-op when either side is
// unconfigured (test fixtures, embeddings without a registry).
func New(cfg Config) *FatClient {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	fc := &FatClient{
		coord:      cfg.Coord,
		project:    cfg.Workspace,
		modelName:  cfg.ModelName,
		logger:     logger,
		projectRegistry: cfg.ProjectRegistry,
	}
	if cfg.Workspace != nil && cfg.ProjectRegistry != nil {
		cfg.Workspace.AttachRegistry(cfg.ProjectRegistry)
	}
	// Open the parallel enjugit workspace at the same root. Logs
	// (but doesn't fail) when construction errors — pre-port call
	// sites still work via `s.project`; only post-port sites need
	// the enjugit handle.
	if cfg.Workspace != nil {
		opts := []enjugit.Option{enjugit.WithLogger(logger)}
		if cfg.ProjectRegistry != nil {
			opts = append(opts, enjugit.WithRegistry(cfg.ProjectRegistry))
		}
		ws, err := enjugit.NewWorkspace(cfg.Workspace.RootDir(),
			enjugit.NewProductionConventions(), opts...)
		if err != nil {
			logger.Warn("enjugit workspace init failed; new-API call sites will be unavailable",
				"root", cfg.Workspace.RootDir(), "error", err)
		} else {
			fc.enjugit = ws
		}
	}
	return fc
}

// ProjectRegistry returns the per-machine registry the FatClient
// reads/writes for project-machine bindings. Nil when no
// registry was supplied at construction (test fixtures, hosted
// read-only setups).
func (s *FatClient) ProjectRegistry() *projectreg.Registry { return s.projectRegistry }

// RegisterProject upserts a registry entry. Called from the
// project-creation paths (EagerInitProjectClone for standard
// clones, RegisterAdoptedDir for externally adopted dirs) so
// the UI's cross-project landing finds the project on next
// render, including external dirs that aren't discoverable from
// the workspace root.
//
// No-op when no registry is configured.
func (s *FatClient) RegisterProject(e projectreg.Entry) {
	if s.projectRegistry == nil {
		return
	}
	if err := s.projectRegistry.Upsert(e); err != nil {
		s.logger.Warn("project registry upsert failed",
			"id", e.ID, "path", e.LocalPath, "error", err)
	}
}

// TouchProject bumps LastTouched for an existing entry.
// Idempotent — no-op if the entry doesn't exist or no registry
// is configured. Wired into ClaimTask (claim.go) and
// SubmitTaskResult (submit.go), and from the handleCreateRun
// MCP handler. Drives "recently active project" sorting on the
// cross-project landing.
func (s *FatClient) TouchProject(id int64) {
	if s.projectRegistry == nil {
		return
	}
	if err := s.projectRegistry.Touch(id); err != nil {
		s.logger.Warn("project registry touch failed",
			"id", id, "error", err)
	}
}

// UnregisterProject drops the entry. Called from
// LocalLeaveProject after the local clone has been removed.
// No-op when no registry is configured.
func (s *FatClient) UnregisterProject(id int64) {
	if s.projectRegistry == nil {
		return
	}
	if err := s.projectRegistry.Remove(id); err != nil {
		s.logger.Warn("project registry remove failed",
			"id", id, "error", err)
	}
}

// Coord returns the underlying coord HTTP client. Exposed for
// callers that need to issue raw requests not yet wrapped by a
// FatClient method.
func (s *FatClient) Coord() *coord.Client { return s.coord }

// Workspace returns the underlying project. Exposed for callers
// that need direct access to workspace primitives (project resolve,
// scan, etc.).
func (s *FatClient) Workspace() *project.Opener { return s.project }

// Username delegates to the coord client so callers see live values
// across auto-reregister rotations.
func (s *FatClient) Username() string { return s.coord.Username() }

// ModelName returns the process-default model identifier (the
// `-model` flag the MCP client was launched with).
func (s *FatClient) ModelName() string { return s.modelName }

// Logger returns the FatClient's logger. Service helpers and the
// handlers that wrap them share this logger.
func (s *FatClient) Logger() *slog.Logger { return s.logger }

// EffectiveModel returns the model identifier to attribute a single
// action to. If the caller passed an explicit override (the per-call
// `model` argument on submit / submit_results_batch), use it.
// Otherwise fall back to the process default — the `-model` flag the
// MCP client was launched with.
//
// The override path is what makes mixed-model workflows work without
// restarting MCP.
func (s *FatClient) EffectiveModel(override string) string {
	if override != "" {
		return override
	}
	return s.modelName
}

// CommitAuthor returns the `name email` pair to use as git commit
// author for submits made on this citizen's behalf. Fetches the
// citizen profile from the coordinator once and caches it for the
// life of the FatClient. Falls back to the configured display name
// when no profile is available, and to a synthetic
// `{username}@enju.local` address when no real email is set.
//
// Real email addresses attribute commits to the right GitHub user
// when they match the citizen's GitHub email; synthetic ones at
// least make different citizens' commits distinguishable in
// contributor graphs instead of collapsing to one bot identity.
func (s *FatClient) CommitAuthor(ctx context.Context) (name, email string) {
	s.loadProfile(ctx)
	return s.profileName, s.profileEmail
}

// CitizenKind returns the calling citizen's kind ("human" | "bot" |
// "model"), populated lazily through the same one-shot fetch as
// CommitAuthor. Defaults to "human" on lookup failure or unmigrated
// rows where Kind is empty server-side.
func (s *FatClient) CitizenKind(ctx context.Context) string {
	s.loadProfile(ctx)
	if s.profileKind == "" {
		return string(types.CitizenKindHuman)
	}
	return s.profileKind
}

// loadProfile fetches the citizen profile once and stashes the
// fields we care about on FatClient. Shared by CommitAuthor and
// CitizenKind so a single GET populates both. Safe to call
// repeatedly — sync.Once gates the network.
func (s *FatClient) loadProfile(ctx context.Context) {
	s.profileOnce.Do(func() {
		username := s.coord.Username()
		s.profileName = username
		s.profileEmail = username + "@enju.local"
		s.profileKind = string(types.CitizenKindHuman)

		data, err := s.coord.Get(ctx, "/api/v1/citizens/by-username/"+username)
		if err != nil {
			s.logger.Warn("loadProfile: failed to fetch profile, using defaults",
				"username", username, "error", err)
			return
		}
		var p map[string]interface{}
		if err := json.Unmarshal(data, &p); err != nil {
			return
		}
		if n, ok := p["name"].(string); ok && n != "" {
			s.profileName = n
		}
		if e, ok := p["email"].(string); ok && e != "" {
			s.profileEmail = e
		}
		if k, ok := p["kind"].(string); ok && k != "" {
			s.profileKind = k
		}
	})
}
