// Package api provides the REST API for the Enju coordinator.
//
// This package is a THIN HTTP layer. It contains zero business
// logic — every computation (tallies, cascades, claim/submit
// validation, materialization, access control, fail checks)
// lives in internal/engine/. Every state mutation goes through
// store.ApplyPlan.
//
// A handler's job is strictly:
//  1. Parse the HTTP request
//  2. Call engine.ComputeX() for the decision
//  3. Call store.ApplyPlan() for the write
//  4. Record contribution events (best-effort)
//  5. Format and return the response
//
// If you find yourself writing an if-else that decides "what
// should happen" in this file, it belongs in the engine.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/dagcache"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server holds the coordinator state and dependencies. Post the
// iteration A orchestrator rewrite, the coordinator holds no git
// state of its own — clients own their own clones, and the
// coordinator is pure DAG/state/index metadata.
type Server struct {
	store store.CoordinatorStore
	// dagCache holds the parsed-run + DAG cache shared with the
	// service layer. Was two raw maps directly on Server until
	// the cascade handlers needed cache access from outside the
	// api package; now lifted to internal/coordinator/dagcache
	// with proper locking. Read/written via the cache's methods,
	// not direct map access.
	dagCache *dagcache.Cache
	// coord bundles the cross-cutting state cascade-touching
	// service functions need (cache + per-project triage
	// mutex + logger). Shared with mcphandlers so both
	// transports converge on identical cascade behavior.
	coord *service.Coordinator
	logger *slog.Logger

	// httpRequestTimeout caps per-request middleware latency.
	// Set via NewServerWithOptions; zero falls back to the
	// pre-config default of 30s in Router().
	httpRequestTimeout time.Duration
}

// NewServer creates a new API server with the default HTTP
// request timeout (30s). Use NewServerWithOptions to override.
func NewServer(st store.CoordinatorStore, logger *slog.Logger) *Server {
	return NewServerWithOptions(st, logger, ServerOptions{})
}

// ServerOptions tunes runtime knobs that operators surface via
// enju.conf. Zero values fall back to documented defaults.
type ServerOptions struct {
	// HTTPRequestTimeout caps the per-request middleware
	// timeout. Zero = 30s (the pre-config default).
	HTTPRequestTimeout time.Duration
}

// NewServerWithOptions creates an API server with operator-tuned
// runtime knobs.
func NewServerWithOptions(st store.CoordinatorStore, logger *slog.Logger, opts ServerOptions) *Server {
	cache := dagcache.New(st)
	return &Server{
		store:              st,
		dagCache:           cache,
		coord:              service.NewCoordinator(st, cache, logger),
		logger:             logger,
		httpRequestTimeout: opts.HTTPRequestTimeout,
	}
}

// Coordinator exposes the server's shared *service.Coordinator so
// out-of-band drivers (the reaper's citizen verify-fail backstop)
// converge on the SAME instance the HTTP handlers use — same cache,
// same per-project triage mutex. Constructing a second Coordinator
// would split triageMu and reintroduce the concurrent-spawn race
// it guards.
func (s *Server) Coordinator() *service.Coordinator { return s.coord }

// engine creates a lightweight Engine instance for
// pure-computation calls. One per request — no caching
// needed since Engine holds no state.
func (s *Server) engine() *engine.Engine {
	return engine.New(s.store, s.logger)
}

// Router returns the chi router with all endpoints registered.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	httpTimeout := s.httpRequestTimeout
	if httpTimeout <= 0 {
		httpTimeout = 30 * time.Second
	}
	r.Use(middleware.Timeout(httpTimeout))

	r.Get("/health", s.handleHealth)

	r.Route("/api/v1", func(r chi.Router) {
		// Auth middleware for write endpoints. Validates
		// the Bearer token from the Authorization header
		// against the citizens table. Soft-enforced: requests
		// without a token are allowed through (backwards
		// compat). Requests with an INVALID token are
		// rejected with 401.
		r.Use(s.authMiddleware)

		// Projects (long-lived containers)
		r.Post("/projects", s.handleCreateProject)
		r.Get("/projects", s.handleListProjects)
		r.Get("/projects/{projectID}", s.handleGetProject)
		r.Put("/projects/{projectID}/remote", s.handleSetProjectRemote)
		r.Put("/projects/{projectID}/default_branch", s.handleSetProjectDefaultBranch)
		r.Get("/projects/{projectID}/runs", s.handleListProjectRuns)
		r.Get("/projects/{projectID}/artifacts", s.handleListArtifacts)
		r.Get("/projects/{projectID}/artifacts/*", s.handleGetArtifact)

		// Project membership. Flat owner/member tiers; enforcement
		// lives in the handlers (add = any member, remove = owner
		// or self, list = any member, role change = owner).
		r.Get("/projects/{projectID}/members", s.handleListProjectMembers)
		r.Post("/projects/{projectID}/members", s.handleAddProjectMember)
		r.Delete("/projects/{projectID}/members/{citizenID}", s.handleRemoveProjectMember)
		r.Delete("/projects/{projectID}/members/by-username/{username}", s.handleRemoveProjectMemberByUsername)
		r.Put("/projects/{projectID}/members/{citizenID}/role", s.handleSetProjectMemberRole)
		r.Put("/projects/{projectID}/members/by-username/{username}/role", s.handleSetProjectMemberRoleByUsername)

		// Runs — hierarchical under projects
		// Address runs by project_id + run_seq (per-project numbering)
		r.Post("/projects/{projectID}/runs", s.handleCreateRun)
		r.Get("/projects/{projectID}/runs/{runSeq}", s.handleGetRun)
		r.Get("/projects/{projectID}/runs/{runSeq}/tasks", s.handleListRunTasks)
		r.Get("/projects/{projectID}/runs/{runSeq}/cost", s.handleGetRunCostSummary)
		r.Get("/projects/{projectID}/runs/{runSeq}/events", s.handleListRunEvents)
		r.Post("/projects/{projectID}/runs/{runSeq}/pause", s.handlePauseRun)
		r.Post("/projects/{projectID}/runs/{runSeq}/resume", s.handleResumeRun)
		r.Post("/projects/{projectID}/runs/{runSeq}/terminate", s.handleTerminateRun)
		r.Post("/projects/{projectID}/runs/{runSeq}/spawn", s.handleSpawnTask)
		r.Post("/projects/{projectID}/runs/{runSeq}/cycle_budget", s.handleSetCycleBudget)
		// fat-client (or any merge-driving consumer)
		// reports a successful FF-merge of a topic branch onto
		// the run branch. Coordinator emits a branch_merged
		// event so the audit timeline shows the moment main
		// advanced. Idempotent at the event layer (drops are
		// best-effort like every other event); duplicate POSTs
		// produce duplicate events but no state corruption.
		r.Post("/projects/{projectID}/runs/{runSeq}/merges", s.handleReportMerge)
		// Sibling of /merges for the parallel-merge non-FF
		// path: when the auto-merge of an ACCEPTED topic onto
		// the run branch hits a content conflict, the fat-
		// client reports it here. The accept already stood;
		// this signals "merge needs human resolution." Phase
		// 3 of the parallel-merge work converts this into a
		// merge_resolve task spawn.
		r.Post("/projects/{projectID}/runs/{runSeq}/merges/conflicts", s.handleReportMergeConflict)
		// Phase 8.4 — sibling of /merges for non-conflict
		// post-submit merge failures. Push rejected, transport
		// timeout, "object not found" on a freshly-added remote.
		// The accept at submit time stood, but the merge can't
		// land. Drives the underlying task to FAILED with a
		// merge_failed reason + fires the standard fail-cascade.
		r.Post("/projects/{projectID}/runs/{runSeq}/merges/failed", s.handleReportMergeFailed)
		// Audit hook for the verify-after-push fix: fat-client
		// posts here when its post-push verify catches a
		// silent-success state (push reported success but the
		// remote ref doesn't equal the local commit). Surfaces
		// the failure in run_status / event log so the bug is
		// visible without tailing daemon log files.
		r.Post("/projects/{projectID}/runs/{runSeq}/push-verify-failed", s.handleReportPushVerifyFailed)
		// Run-completion sync conflict (bug hunt B-1). The
		// run-branch → default-branch merge at run completion
		// hit a content conflict — the run's output never
		// reached the default branch. The run is already
		// terminal; this stamps a durable runs.sync_status flag
		// + run_sync_conflict event so the otherwise log-only
		// data-loss surfaces on run_status / runs / the event
		// log. Distinct from /merges/conflicts (post-submit
		// topic→run-branch auto-merge, which spawns merge_resolve).
		r.Post("/projects/{projectID}/runs/{runSeq}/sync-conflict", s.handleReportRunSyncConflict)
		r.Get("/projects/{projectID}/events", s.handleShowEvents)

		// Issues — project-level structured artifacts
		// (living-workflow phase 3). Outlive runs; filed by
		// any project member, triaged or closed by any member,
		// linked to fix-tasks once spawn arrives.
		r.Post("/projects/{projectID}/issues", s.handleFileIssue)
		r.Get("/projects/{projectID}/issues", s.handleListIssues)
		r.Get("/projects/{projectID}/issues/{issueSeq}", s.handleGetIssue)
		r.Post("/projects/{projectID}/issues/{issueSeq}/triage", s.handleTriageIssue)
		r.Post("/projects/{projectID}/issues/{issueSeq}/close", s.handleCloseIssue)

		// event-store status (read-only). Operators flip the
		// kill-switch by editing enju.conf and sending SIGHUP
		// to the coordinator, not via HTTP — no admin tier
		// exists yet, and exposing a write endpoint to any
		// authenticated citizen would let one tenant kill
		// audit for the whole deployment.
		r.Get("/events/status", s.handleEventsStatus)

		// Legacy flat listing — still useful for dashboards
		r.Get("/runs", s.handleListRuns)

		// Unified write endpoint — the coordinator's ONE
		// write path. Accepts a Plan (ordered mutations),
		// validates each against current DB state, and
		// applies atomically. Steps 7b-7g will migrate
		// existing tools to produce Plans client-side and
		// POST them here.
		r.Post("/apply", s.handleApply)

		// Tasks
		r.Get("/tasks/ready", s.handleListReadyTasks)
		r.Post("/tasks/{taskID}/claim", s.handleClaimTask)
		r.Get("/tasks/{taskID}", s.handleGetTask)
		r.Get("/tasks/{taskID}/inputs", s.handleGetTaskInputs)
		r.Get("/tasks/{taskID}/iterations", s.handleListIterations)
		r.Post("/tasks/{taskID}/started", s.handleMarkTaskStarted)
		r.Post("/tasks/{taskID}/result", s.handleSubmitResult)
		r.Post("/tasks/reconcile", s.handleReconcileTasks)
		r.Post("/tasks/{taskID}/release", s.handleReleaseTask)
		// Bulk release for daemon startup recovery — releases
		// every open claim held by the calling citizen across
		// all projects. Identity from the Bearer token.
		r.Post("/me/release-claims", s.handleReleaseAllOpenClaims)
		r.Post("/tasks/{taskID}/invalidate", s.handleInvalidateTask)
		r.Post("/tasks/{taskID}/retry", s.handleRetryTask)
		r.Post("/tasks/{taskID}/tally", s.handleTallyTask)
		r.Post("/tasks/{taskID}/fail", s.handleFailTask)
		r.Post("/tasks/{taskID}/citizen-verify-failed", s.handleReportCitizenVerifyFail)

		// Citizens
		r.Post("/citizens/register", s.handleRegisterCitizen)
		r.Get("/citizens/by-username/{username}/dashboard", s.handleCitizenDashboard)
		r.Get("/citizens/by-username/{username}/contributions", s.handleCitizenContributions)
		r.Put("/citizens/by-username/{username}/profile", s.handleUpdateProfile)
		r.Get("/citizens/by-username/{username}", s.handleGetCitizenByUsername)

		// Agent registration. An agent is an unattended citizen
		// (kind='agent') owned by the registering human, with its
		// own token, that claims and executes tasks. A model is
		// NOT a citizen — it has no identity and no registration;
		// it is a normalized name stamped as a label on the work
		// at submit time.
		r.Post("/citizens/me/agents", s.handleRegisterBot)
		r.Get("/citizens/me/agents", s.handleListMyBots)
		r.Post("/tokens/revoke", s.handleRevokeToken)
		r.Post("/citizens/me/agents/{username}/reissue", s.handleReissueBotToken)
	})

	return r
}



