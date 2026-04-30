// Package api provides the REST API for the Enju coordinator.
//
// This package is a THIN HTTP layer. It contains zero business
// logic — every computation (tallies, cascades, claim/submit
// validation, materialization, access control, fail checks)
// lives in internal/engine/. Every state mutation goes through
// store.ApplyPlan.
//
// A handler's job is strictly:
//   1. Parse the HTTP request
//   2. Call engine.ComputeX() for the decision
//   3. Call store.ApplyPlan() for the write
//   4. Record contribution events (best-effort)
//   5. Format and return the response
//
// If you find yourself writing an if-else that decides "what
// should happen" in this file, it belongs in the engine.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enju-ai/enju/internal/dag"
	"github.com/enju-ai/enju/internal/engine"
	"github.com/enju-ai/enju/internal/store"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// ctxKey is a private type for context keys so package-external
// callers can't collide. Only the authenticated-citizen key is
// stored this way today.
type ctxKey int

const (
	ctxKeyCitizen ctxKey = iota
)

// citizenFromRequest returns the authenticated citizen that
// authMiddleware stashed into the request context, or nil when
// the request arrived without a valid Bearer token (soft-auth
// backwards-compat path). Handlers that need to know who is
// asking — read gating, ownership checks, creator auto-add —
// call this; handlers that don't care skip it.
func citizenFromRequest(r *http.Request) *store.CitizenRecord {
	if v, ok := r.Context().Value(ctxKeyCitizen).(*store.CitizenRecord); ok {
		return v
	}
	return nil
}

// Server holds the coordinator state and dependencies. Post the
// iteration A orchestrator rewrite, the coordinator holds no git
// state of its own — clients own their own clones, and the
// coordinator is pure DAG/state/index metadata.
type Server struct {
	store  *store.Store
	dags   map[int64]*dag.DAG // runID -> DAG (in-memory for fast queries)
	runs   map[int64]*enjuYaml.ParsedRun
	logger *slog.Logger

	// triageMu serializes maybeAutoTriage per project to close
	// the bounded race where two concurrent submits both pass
	// the open-issue check, both spawn a fix task, and one
	// becomes an orphan. Loads-or-stores per project — the
	// map is small (one entry per active project), keyed by
	// projectID. See the maybeAutoTriage doc comment.
	triageMu sync.Map // projectID(int64) -> *sync.Mutex
}

// NewServer creates a new API server.
func NewServer(st *store.Store, logger *slog.Logger) *Server {
	return &Server{
		store:  st,
		dags:   make(map[int64]*dag.DAG),
		runs:   make(map[int64]*enjuYaml.ParsedRun),
		logger: logger,
	}
}

// projectTriageMutex returns the per-project mutex used by
// maybeAutoTriage. Idempotent: first caller for a projectID
// allocates, subsequent callers find the same mutex.
func (s *Server) projectTriageMutex(projectID int64) *sync.Mutex {
	if m, ok := s.triageMu.Load(projectID); ok {
		return m.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := s.triageMu.LoadOrStore(projectID, m)
	return actual.(*sync.Mutex)
}

// engine creates a lightweight Engine instance for
// pure-computation calls. One per request — no caching
// needed since Engine holds no state.
func (s *Server) engine() *engine.Engine {
	return engine.New(s.store, s.logger)
}

// authenticateCitizen extracts the Bearer token from the
// Authorization header and verifies it matches a registered
// citizen. Returns the citizen record on success, or writes
// an HTTP error and returns nil on failure. Write endpoints
// call this; read endpoints don't need it.
//
// If no Authorization header is present, the request is
// allowed through (backwards compatibility for the
// transition period). Once all clients send tokens, this
// can be tightened to reject unauthenticated writes.
func (s *Server) authenticateCitizen(w http.ResponseWriter, r *http.Request) *store.CitizenRecord {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil // no auth — allowed for now
	}
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "invalid Authorization header — expected 'Bearer <token>'")
		return nil
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	citizen, err := s.store.GetCitizenByToken(token)
	if err != nil || citizen == nil {
		writeError(w, http.StatusUnauthorized, "invalid or expired token — re-register with enju mcp")
		return nil
	}
	return citizen
}

// authMiddleware validates the Bearer token on every request.
// Hard-enforced: missing OR invalid token → 401. The only
// un-authenticated endpoint is /citizens/register (bootstrap),
// which is explicitly whitelisted below.
//
// Prior iterations let missing-token requests fall through for
// backwards-compat. That was removed after Phase J: a coordinator
// that silently ignores "no auth header" leaks project data to
// anyone who just forgets to send one, and the coordinator
// already rejects INVALID tokens — the asymmetry was a pure
// footgun.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bootstrap exception: /citizens/register is the only
		// endpoint that legitimately has no token — that's
		// how a new citizen gets one.
		if r.URL.Path == "/api/v1/citizens/register" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeError(w, http.StatusUnauthorized, "Authorization header is required — send 'Bearer <token>' from your registered citizen")
			return
		}
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "invalid Authorization header — expected 'Bearer <token>'")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		citizen, err := s.store.GetCitizenByToken(token)
		if err != nil || citizen == nil {
			s.logger.Warn("auth: invalid token rejected",
				"method", r.Method, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "invalid or expired token — delete ~/.enju/credentials.json and re-register")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyCitizen, citizen)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireProjectMembershipForTask gates a task-scoped endpoint by
// resolving the task's run → project, then deferring to
// requireProjectMembership. Convenience wrapper so each task
// handler doesn't re-implement the lookup.
func (s *Server) requireProjectMembershipForTask(w http.ResponseWriter, r *http.Request, taskID string) (*store.ProjectMemberRecord, bool) {
	task, err := s.store.GetTask(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task lookup failed: "+err.Error())
		return nil, false
	}
	if task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return nil, false
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run lookup failed")
		return nil, false
	}
	return s.requireProjectMembership(w, r, run.ProjectID)
}

// checkProjectMembershipForTask is the write-free variant of
// requireProjectMembershipForTask: returns the caller's
// membership row or an error message without touching the
// response writer. Used by batch endpoints (e.g.
// /tasks/reconcile) that need to emit per-entry membership
// errors as part of a larger JSON response envelope, not as
// standalone HTTP errors. Mixing both forms in one request
// would corrupt the response (headers already written, then a
// second writeJSON) — see reconcileOne.
func (s *Server) checkProjectMembershipForTask(r *http.Request, taskID string) (*store.ProjectMemberRecord, string) {
	task, err := s.store.GetTask(taskID)
	if err != nil {
		return nil, "task lookup failed: " + err.Error()
	}
	if task == nil {
		return nil, "task not found"
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, "run lookup failed"
	}
	return s.checkProjectMembership(r, run.ProjectID)
}

// checkProjectMembership is the write-free variant of
// requireProjectMembership. Same gating rules, but returns
// (memb, errMsg) instead of writing to w. Empty errMsg with
// nil memb means "legacy pre-membership project, caller is
// accepted" — matches the membership-bypass branch in
// requireProjectMembership.
func (s *Server) checkProjectMembership(r *http.Request, projectID int64) (*store.ProjectMemberRecord, string) {
	caller := citizenFromRequest(r)
	if caller == nil {
		return nil, "authentication required"
	}
	total, err := s.store.CountProjectMembers(projectID)
	if err != nil {
		return nil, "membership lookup failed: " + err.Error()
	}
	if total == 0 {
		// Legacy project — same bypass as requireProjectMembership.
		return nil, ""
	}
	memb, err := s.store.GetProjectMember(projectID, caller.ID)
	if err != nil {
		return nil, "membership lookup failed: " + err.Error()
	}
	if memb == nil {
		return nil, fmt.Sprintf("not a member of project %d", projectID)
	}
	return memb, ""
}

// requireProjectMembership returns the caller's membership row
// for a project, writing the appropriate error and returning
// false when gating blocks the request.
//
// Since authMiddleware hard-enforces Bearer tokens, the caller is
// always non-nil by the time this runs. The only gray area is
// pre-membership legacy projects with zero rows in
// project_members — those remain open so databases migrated from
// the pre-Phase-J schema don't lose access overnight. Every new
// project seeds its creator as owner so it never lands there.
func (s *Server) requireProjectMembership(w http.ResponseWriter, r *http.Request, projectID int64) (*store.ProjectMemberRecord, bool) {
	caller := citizenFromRequest(r)
	if caller == nil {
		// Belt-and-suspenders: authMiddleware should have
		// already rejected this. Treat as 401 if we ever
		// somehow reach here.
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	total, err := s.store.CountProjectMembers(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "membership lookup failed: "+err.Error())
		return nil, false
	}
	if total == 0 {
		// Pre-membership legacy project — no rows means "not
		// migrated yet", not "empty." Keep open so reading
		// the DB before any member is seeded still works.
		return nil, true
	}
	m, err := s.store.GetProjectMember(projectID, caller.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "membership lookup failed: "+err.Error())
		return nil, false
	}
	if m == nil {
		writeError(w, http.StatusForbidden, fmt.Sprintf("not a member of project %d — ask an existing member to add you", projectID))
		return nil, false
	}
	return m, true
}

// Router returns the chi router with all endpoints registered.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

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
		r.Post("/projects/{projectID}/runs/{runSeq}/spawn", s.handleSpawnTask)
		r.Post("/projects/{projectID}/runs/{runSeq}/cycle_budget", s.handleSetCycleBudget)
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
		r.Post("/tasks/{taskID}/result", s.handleSubmitResult)
		r.Post("/tasks/reconcile", s.handleReconcileTasks)
		r.Post("/tasks/{taskID}/release", s.handleReleaseTask)
		r.Post("/tasks/{taskID}/invalidate", s.handleInvalidateTask)
		r.Post("/tasks/{taskID}/tally", s.handleTallyTask)
		r.Post("/tasks/{taskID}/fail", s.handleFailTask)

		// Citizens
		r.Post("/citizens/register", s.handleRegisterCitizen)
		r.Get("/citizens/by-username/{username}/dashboard", s.handleCitizenDashboard)
		r.Get("/citizens/by-username/{username}/contributions", s.handleCitizenContributions)
		r.Put("/citizens/by-username/{username}/profile", s.handleUpdateProfile)
		r.Get("/citizens/by-username/{username}", s.handleGetCitizenByUsername)

		// operator/model design — bot + model
		// registration tools. Bots are kind='bot' citizens
		// owned by a parent (the registering human). Models
		// are kind='model' catalog entries with no token,
		// referenced for per-submit attribution. See
		// docs/operator-model-design.md.
		r.Post("/citizens/me/bots", s.handleRegisterBot)
		r.Get("/citizens/me/bots", s.handleListMyBots)
		r.Post("/tokens/revoke", s.handleRevokeToken)
		r.Get("/models", s.handleListModels)
		r.Post("/models", s.handleRegisterModel)
	})

	return r
}

// --- Health ---

// handleApply is the unified write endpoint. Accepts a
// serialized Plan, validates the engine version, and calls
// store.ApplyPlan to execute all mutations atomically.
// Returns the ApplyResult as JSON.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var plan store.Plan
	if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
		writeError(w, http.StatusBadRequest, "invalid plan: "+err.Error())
		return
	}

	// Version gate: reject plans from mismatched engine
	// versions so a stale client can't submit plans the
	// coordinator doesn't understand.
	if plan.Version != engine.EngineVersion {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("engine version mismatch: client=%q, coordinator=%q — update your enju binary",
				plan.Version, engine.EngineVersion))
		return
	}

	result, err := s.store.ApplyPlan(plan)
	if err != nil {
		writeError(w, http.StatusBadRequest, "plan rejected: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Projects (long-lived containers) ---

type createProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	RemoteURL   string `json:"remote_url,omitempty"`
	// DefaultBranch is the git branch new runs land on by
	// default. Optional — falls back to "main" when unset or
	// empty. Orgs that want Enju activity to stay off their
	// repo's main branch set this to e.g. "enju/work" at
	// create-project time. Validated against the same loose
	// git-ref grammar as branch= on create_run.
	DefaultBranch string `json:"default_branch,omitempty"`
}

type setProjectRemoteRequest struct {
	RemoteURL string `json:"remote_url"`
}

type projectResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	RemoteURL     string `json:"remote_url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	RunCount      int    `json:"run_count"`
	CreatedAt     string `json:"created_at"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Check uniqueness
	existing, _ := s.store.GetProjectByName(req.Name)
	if existing != nil {
		writeError(w, http.StatusConflict, "a project with this name already exists")
		return
	}

	// Iteration A: the coordinator never creates a git repo. The
	// project metadata goes into the DB and that's it — clients
	// own their local clones and the project's data lives in the
	// citizen's configured remote (remote_url). If remote_url is
	// empty, the project is a local-only project handled entirely
	// by the MCP client's workspace.
	now := time.Now()
	creator := citizenFromRequest(r)
	if creator == nil {
		// authMiddleware requires a token; this is defense in
		// depth for future refactors.
		writeError(w, http.StatusUnauthorized, "authentication required to create a project")
		return
	}
	defaultBranch := strings.TrimSpace(req.DefaultBranch)
	if defaultBranch != "" {
		if err := validateBranchName(defaultBranch); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	id, err := s.store.CreateProject(&store.ProjectRecord{
		Name:          req.Name,
		Description:   req.Description,
		CreatedBy:     creator.Username,
		RemoteURL:     req.RemoteURL,
		DefaultBranch: defaultBranch,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create project: "+err.Error())
		return
	}

	// Seed the creator as owner. Every project has at least
	// this one member from birth — no legacy zero-members
	// branch on the new-project path.
	if err := s.store.AddProjectMember(id, creator.ID, store.ProjectRoleOwner, 0); err != nil {
		s.logger.Warn("creator auto-add failed",
			"project_id", id, "citizen_id", creator.ID, "error", err)
	}

	effectiveBranch := defaultBranch
	if effectiveBranch == "" {
		effectiveBranch = "main"
	}
	writeJSON(w, http.StatusCreated, projectResponse{
		ID:            id,
		Name:          req.Name,
		RemoteURL:     req.RemoteURL,
		DefaultBranch: effectiveBranch,
		CreatedAt:     now.Format(time.RFC3339),
	})
}

// handleSetProjectRemote updates the project's remote URL in the DB.
// Reconfiguring the MCP client's local clone to point at the new
// remote happens on the client side (see the MCP tool handler in
// internal/mcpserver/server.go).
func (s *Server) handleSetProjectRemote(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	// Owner-only: changing the remote URL moves the whole
	// project's git home, which determines where task commits
	// land. Members can push to the remote their owner set, but
	// only owners can redirect it.
	m, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}
	if m != nil && m.Role != store.ProjectRoleOwner {
		writeError(w, http.StatusForbidden, "only project owners can change the remote URL")
		return
	}

	var req setProjectRemoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Reject empty remote_url. Defense-in-depth complement to the
	// MCP-handler validation: a direct API call would otherwise
	// store an empty remote and silently fork the team on a
	// multi-machine project (Alice's commits stop pushing
	// anywhere; Bob's machine can't see them). The legitimate
	// way to clear a remote from coordinator state is to leave
	// the project entirely (DELETE membership); migration to a
	// new remote uses this endpoint with the new URL directly.
	// Note: POST /projects still accepts empty remote_url for
	// local-only project creation — that's the create-time entry
	// point for solo work, deliberate, not in scope here.
	if strings.TrimSpace(req.RemoteURL) == "" {
		writeError(w, http.StatusBadRequest,
			"remote_url cannot be empty — clearing a remote silently forks multi-machine projects. Pass the new URL directly to migrate, or leave the project to stop using it.")
		return
	}

	p, err := s.store.GetProject(projectID)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	if err := s.store.SetProjectRemoteURL(projectID, req.RemoteURL); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist remote url")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project_id": projectID,
		"remote_url": req.RemoteURL,
	})
}

// handleSetProjectDefaultBranch updates a project's
// default_branch column. Owner-only: the default branch is
// where new runs land when no explicit branch is specified, so
// flipping it is the sort of project-wide configuration change
// that should sit with the admin tier.
func (s *Server) handleSetProjectDefaultBranch(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	m, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}
	if m != nil && m.Role != store.ProjectRoleOwner {
		writeError(w, http.StatusForbidden, "only project owners can change the default branch")
		return
	}
	var req struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if err := validateBranchName(branch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetProjectDefaultBranch(projectID, branch); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update default branch: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project_id":     projectID,
		"default_branch": branch,
	})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Callers see only projects they're a member of.
	projects, err := s.store.ListProjectsForCitizen(caller.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list projects")
		return
	}

	resp := make([]projectResponse, 0, len(projects))
	for _, p := range projects {
		runs, _ := s.store.ListRunsByProject(p.ID)
		resp = append(resp, toProjectResponse(p, len(runs)))
	}
	writeJSON(w, http.StatusOK, resp)
}

// toProjectResponse builds the wire projectResponse from a store
// record + a pre-computed run count.
func toProjectResponse(p store.ProjectRecord, runCount int) projectResponse {
	return projectResponse{
		ID:            p.ID,
		Name:          p.Name,
		Description:   p.Description,
		RemoteURL:     p.RemoteURL,
		DefaultBranch: p.DefaultBranch,
		RunCount:      runCount,
		CreatedAt:     p.CreatedAt.Format(time.RFC3339),
	}
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	p, err := s.store.GetProject(projectID)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	runs, _ := s.store.ListRunsByProject(p.ID)
	writeJSON(w, http.StatusOK, toProjectResponse(*p, len(runs)))
}

// handleProjectRemoteStatus / handleProjectSync were deleted during
// the iteration A orchestrator rewrite. The coordinator no longer
// owns a clone to compare or push from — the MCP client runs these
// diagnostics against its own local clone via mcpgit. The MCP tool
// names are unchanged; see internal/mcpserver/server.go for the new
// implementations.

func (s *Server) handleListProjectRuns(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	runs, err := s.store.ListRunsByProject(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}

	resp := make([]runResponse, 0, len(runs))
	for _, run := range runs {
		tasks, _ := s.store.ListTasksByRun(run.ID)
		resp = append(resp, runResponse{
			ID:         run.ID,
			ProjectID:  run.ProjectID,
			Seq:        run.Seq,
			Name:       run.Name,
			State:      string(run.State),
			TaskCount:  len(tasks),
			Branch:     run.Branch,
			Slug:       run.Slug,
			CreatedAt:  run.CreatedAt.Format(time.RFC3339),
			SourcePath: run.SourcePath,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Project members ---

type addProjectMemberRequest struct {
	Username string `json:"username"`
	Role     string `json:"role,omitempty"` // optional; defaults to "member"
}

type projectMemberResponse struct {
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role"`
	AddedAt  string `json:"added_at"`
	AddedBy  string `json:"added_by,omitempty"` // username of adder; empty for the creator row
}

type setProjectMemberRoleRequest struct {
	Role string `json:"role"`
}

// handleListProjectMembers returns every member on the project,
// gated on caller membership. Response rows expose usernames (not
// citizen IDs) so all external identifiers match the rest of the
// API surface.
func (s *Server) handleListProjectMembers(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	rows, err := s.store.ListProjectMembers(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list members: "+err.Error())
		return
	}
	resp := make([]projectMemberResponse, 0, len(rows))
	for _, m := range rows {
		cz, _ := s.store.GetCitizen(m.CitizenID)
		username := ""
		name := ""
		if cz != nil {
			username = cz.Username
			name = cz.Name
		}
		resp = append(resp, projectMemberResponse{
			Username: username,
			Name:     name,
			Role:     string(m.Role),
			AddedAt:  m.AddedAt.Format(time.RFC3339),
			AddedBy:  s.citizenUsername(m.AddedBy),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddProjectMember grants membership to a citizen. Any
// existing member can add — role-free delegation is the
// GitHub-style trust the user asked for.
func (s *Server) handleAddProjectMember(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	m, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}
	var req addProjectMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	target := s.resolveCitizen(w, req.Username)
	if target == nil {
		return
	}
	if existing, _ := s.store.GetProjectMember(projectID, target.ID); existing != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("%q is already a member of this project", req.Username))
		return
	}
	role := store.ProjectRole(req.Role)
	switch role {
	case "":
		role = store.ProjectRoleMember
	case store.ProjectRoleOwner:
		// Promoting someone to owner on the add path is a
		// shortcut for "add + promote" — gate it to owners so
		// the shortcut doesn't quietly bypass the promote-
		// only-by-owner rule. Members wanting to invite
		// another owner have to ask an existing owner.
		if m == nil || m.Role != store.ProjectRoleOwner {
			writeError(w, http.StatusForbidden, "only project owners can add members as 'owner' — ask an owner, or add as 'member' and request promotion")
			return
		}
	case store.ProjectRoleMember:
		// default path
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown role %q (expected 'member' or 'owner')", req.Role))
		return
	}
	var adder int64
	if caller := citizenFromRequest(r); caller != nil {
		adder = caller.ID
	}
	if err := s.store.AddProjectMember(projectID, target.ID, role, adder); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to add member: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, projectMemberResponse{
		Username: target.Username,
		Name:     target.Name,
		Role:     string(role),
		AddedAt:  time.Now().Format(time.RFC3339),
		AddedBy:  s.citizenUsername(adder),
	})
}

// handleRemoveProjectMember removes a citizen from the project.
// The removed citizen must be the caller (self-leave) or the
// caller must be an owner. Enforces the ≥1-owner invariant.
func (s *Server) handleRemoveProjectMember(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	targetID, _ := strconv.ParseInt(chi.URLParam(r, "citizenID"), 10, 64)
	if targetID == 0 {
		writeError(w, http.StatusBadRequest, "invalid citizen ID")
		return
	}
	callerMember, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}
	caller := citizenFromRequest(r)
	isSelf := caller != nil && caller.ID == targetID
	// Only owners or the subject themselves can remove.
	if !isSelf && (callerMember == nil || callerMember.Role != store.ProjectRoleOwner) {
		writeError(w, http.StatusForbidden, "only project owners can remove other members — or remove yourself to leave")
		return
	}
	target, err := s.store.GetProjectMember(projectID, targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "member lookup failed: "+err.Error())
		return
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "member not found on this project")
		return
	}
	// Last-owner invariant: block a remove that would drop the
	// owner count to zero. The subject (or owner doing the
	// remove) must promote a successor first.
	if target.Role == store.ProjectRoleOwner {
		owners, _ := s.store.CountProjectOwners(projectID)
		if owners <= 1 {
			if isSelf {
				writeError(w, http.StatusConflict, "you are the last owner — promote another member to owner first, then leave")
			} else {
				writeError(w, http.StatusConflict, "cannot remove the last owner — promote another member to owner first")
			}
			return
		}
	}
	if err := s.store.RemoveProjectMember(projectID, targetID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove member: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project_id": projectID,
		"citizen":    s.citizenUsername(targetID),
		"removed":    true,
		"self_leave": isSelf,
	})
}

// handleRemoveProjectMemberByUsername resolves the username to a
// citizen ID and delegates to handleRemoveProjectMember. Thin
// convenience alias so the MCP layer doesn't have to round-trip
// through /citizens/by-username first.
func (s *Server) handleRemoveProjectMemberByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target := s.resolveCitizen(w, username)
	if target == nil {
		return
	}
	rctx := chi.RouteContext(r.Context())
	rctx.URLParams.Add("citizenID", strconv.FormatInt(target.ID, 10))
	s.handleRemoveProjectMember(w, r)
}

// handleSetProjectMemberRoleByUsername mirrors handleRemoveProjectMemberByUsername
// for the role-change endpoint — resolves username to citizen ID
// and hands off to the canonical handler.
func (s *Server) handleSetProjectMemberRoleByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target := s.resolveCitizen(w, username)
	if target == nil {
		return
	}
	rctx := chi.RouteContext(r.Context())
	rctx.URLParams.Add("citizenID", strconv.FormatInt(target.ID, 10))
	s.handleSetProjectMemberRole(w, r)
}

// handleSetProjectMemberRole promotes or demotes a member.
// Owner-only. Enforces the ≥1-owner invariant when demoting the
// last owner.
func (s *Server) handleSetProjectMemberRole(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	targetID, _ := strconv.ParseInt(chi.URLParam(r, "citizenID"), 10, 64)
	if targetID == 0 {
		writeError(w, http.StatusBadRequest, "invalid citizen ID")
		return
	}
	callerMember, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}
	if callerMember == nil || callerMember.Role != store.ProjectRoleOwner {
		writeError(w, http.StatusForbidden, "only project owners can change member roles")
		return
	}
	var req setProjectMemberRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	newRole := store.ProjectRole(req.Role)
	if newRole != store.ProjectRoleOwner && newRole != store.ProjectRoleMember {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown role %q (expected 'owner' or 'member')", req.Role))
		return
	}
	target, err := s.store.GetProjectMember(projectID, targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "member lookup failed: "+err.Error())
		return
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "member not found on this project")
		return
	}
	if target.Role == newRole {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"project_id": projectID,
			"citizen":    s.citizenUsername(targetID),
			"role":       string(newRole),
			"changed":    false,
		})
		return
	}
	// Demoting the last owner would leave the project
	// ownerless. Refuse with guidance.
	if target.Role == store.ProjectRoleOwner && newRole == store.ProjectRoleMember {
		owners, _ := s.store.CountProjectOwners(projectID)
		if owners <= 1 {
			writeError(w, http.StatusConflict, "cannot demote the last owner — promote another member to owner first")
			return
		}
	}
	if err := s.store.SetProjectMemberRole(projectID, targetID, newRole); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to change role: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project_id": projectID,
		"citizen":    s.citizenUsername(targetID),
		"role":       string(newRole),
		"changed":    true,
	})
}

// --- Artifacts ---

type artifactResponse struct {
	Path       string `json:"path"`
	LastWriter string `json:"last_writer,omitempty"` // username of the last writer
	LastTaskID string `json:"last_task_id,omitempty"`
	LastRunID  int64  `json:"last_run_id,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"` // empty iff tracked=false
	// Tracked reflects whether the artifact's bytes live in git.
	// Defaults to true for every entry in pre-untracked DB rows;
	// new untracked entries (writes_artifacts: track: false) land
	// with Tracked=false and CommitSHA="". Serialized as a pointer
	// so `false` is distinguishable from omitted on older clients.
	Tracked   *bool  `json:"tracked,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// trackedPtr renders the ArtifactRecord.Tracked bool for the
// wire: returns a pointer so json.Marshal emits the field
// unconditionally (including the false case). Keeps the same
// omitempty behavior of Go's bool-pointer convention.
func trackedPtr(b bool) *bool { return &b }

// citizenUsername looks up the username for an internal citizen ID.
// Returns the empty string if the citizen isn't found (e.g. id is 0).
// This is the centralized point for translating backstage IDs into
// user-facing handles.
func (s *Server) citizenUsername(id int64) string {
	if id == 0 {
		return ""
	}
	c, _ := s.store.GetCitizen(id)
	if c == nil {
		return ""
	}
	return c.Username
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	prefix := r.URL.Query().Get("prefix")
	// Branch filter: callers can narrow to a specific branch
	// via ?branch=<name>; empty defaults to the project's
	// configured default branch so the common case ("show me
	// artifacts on main") just works.
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		if p, _ := s.store.GetProject(projectID); p != nil {
			branch = p.DefaultBranch
		}
	}
	rows, err := s.store.ListArtifactsByProject(projectID, branch, prefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list artifacts")
		return
	}

	resp := make([]artifactResponse, 0, len(rows))
	for _, a := range rows {
		resp = append(resp, artifactResponse{
			Path:       a.Path,
			LastWriter: s.citizenUsername(a.LastWriter),
			LastTaskID: a.LastTaskID,
			LastRunID:  a.LastRunID,
			CommitSHA:  a.CommitSHA,
			Tracked:    trackedPtr(a.Tracked),
			UpdatedAt:  a.UpdatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetArtifact returns the artifacts index metadata for one
// artifact path: who wrote it, in what task, at what commit SHA.
// File content reading has moved to the MCP client side, which
// reads directly from its local clone at the commit SHA this
// endpoint returns.
func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	// chi's wildcard captures everything after "artifacts/" — that IS
	// the user-facing artifact path.
	path := chi.URLParam(r, "*")
	if err := validateArtifactPath(path); err != nil {
		writeError(w, http.StatusBadRequest, "invalid artifact path: "+err.Error())
		return
	}

	// Branch filter defaults to the project's configured
	// default — single-branch projects get the expected
	// behavior, multi-branch projects can query with
	// ?branch=<name>.
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		if p, _ := s.store.GetProject(projectID); p != nil {
			branch = p.DefaultBranch
		}
	}
	meta, err := s.store.GetArtifact(projectID, branch, path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read artifact index")
		return
	}
	if meta == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":         path,
		"last_writer":  s.citizenUsername(meta.LastWriter),
		"last_task_id": meta.LastTaskID,
		"last_run_id":  meta.LastRunID,
		"commit_sha":   meta.CommitSHA,
		"tracked":      meta.Tracked,
		"updated_at":   meta.UpdatedAt.Format(time.RFC3339),
	})
}

// --- Runs ---

// resolveRunBranch turns a caller's `branch:` request into a
// concrete branch name, honoring the project's default and the
// "auto" sugar form. Returns an error when the explicit name is
// malformed (validator matches git's ref grammar loosely enough
// to accept "experiment-2", "enju/work", etc. but rejects empty
// segments, leading "-", and reserved forms).
//
// For branch="auto", the generated name uses `<slug>-N` where the
// slug comes from engine.ComputeRunSlug(sourcePath, runName) — the
// same rule that stamps the `enju/runs/{seq}-{slug}/` directory
// name. Keeping both on the same slugger means the user never
// sees `git checkout quick-inline-1` pointing at a run whose dir
// is `2-Quick_Inline/` (style drift that surfaced on early
// testing).
func (s *Server) resolveRunBranch(projectID int64, defaultBranch, requested, sourcePath, runName string) (string, error) {
	if requested == "" {
		if defaultBranch == "" {
			return "main", nil
		}
		return defaultBranch, nil
	}
	if requested == "auto" {
		// Walk <slug>-1, <slug>-2, ... picking the first one
		// that doesn't already appear on an existing run in
		// this project. Bounded to 10_000 so a misbehaving
		// caller can't stall the endpoint forever.
		used := map[string]bool{}
		branches, err := s.store.ListRunBranches(projectID)
		if err != nil {
			return "", fmt.Errorf("allocating auto branch name: %w", err)
		}
		for _, b := range branches {
			used[b] = true
		}
		slug := engine.ComputeRunSlug(sourcePath, runName)
		// Defense in depth: a slug that slips past the kebab
		// slugger into something git would reject (shouldn't
		// happen — all outputs are [a-z0-9-]+ — but the
		// check is cheap) falls back to "run" so we still
		// produce a usable branch name.
		if validateBranchName(slug) != nil {
			slug = "run"
		}
		for n := 1; n <= 10000; n++ {
			name := fmt.Sprintf("%s-%d", slug, n)
			if !used[name] {
				return name, nil
			}
		}
		return "", fmt.Errorf("unable to allocate an auto branch name after 10000 tries — pass branch=\"<name>\" explicitly")
	}
	if err := validateBranchName(requested); err != nil {
		return "", err
	}
	return requested, nil
}

// validateBranchName rejects shapes that git would refuse to
// store as a ref. Deliberately loose — matches the subset of
// git-check-ref-format rules that matter at create_run time.
// The full rules are enforced by git itself when the fat client
// pushes; this is a fast-fail upfront validation so a typo
// doesn't leave us with a half-created run.
func validateBranchName(s string) error {
	if s == "" {
		return fmt.Errorf("branch name cannot be empty")
	}
	if s == "HEAD" {
		return fmt.Errorf("branch name %q is reserved", s)
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return fmt.Errorf("branch name %q: must not start with '-' or '/' and must not end with '/'", s)
	}
	if strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{") {
		return fmt.Errorf("branch name %q contains a forbidden sequence (.., //, or @{)", s)
	}
	for _, r := range s {
		switch r {
		case ' ', '\t', '~', '^', ':', '?', '*', '[', '\\', '\x7f':
			return fmt.Errorf("branch name %q contains a forbidden character %q", s, r)
		}
		if r < 0x20 {
			return fmt.Errorf("branch name %q contains a control character", s)
		}
	}
	return nil
}

type createRunRequest struct {
	YAML            string                 `json:"yaml"`
	RepoURL         string                 `json:"repo_url,omitempty"`
	Params          map[string]interface{} `json:"params,omitempty"`
	SourcePath      string                 `json:"source_path,omitempty"`
	SourceCommitSHA string                 `json:"source_commit_sha,omitempty"`
	Username        string                 `json:"username,omitempty"` // citizen who created this run, for contribution tracking
	// Branch is the git branch this run should commit to.
	// Three forms:
	//   - empty → fall back to the project's DefaultBranch
	//   - "auto" → the coordinator picks an unused branch name
	//     of the shape "run-N" so parallel variants don't force
	//     the caller to invent names
	//   - explicit name → use it verbatim
	// Refused when there's already an active run on the resolved
	// branch (serial-per-branch invariant).
	Branch string `json:"branch,omitempty"`
}

type runResponse struct {
	ID              int64    `json:"id"`                   // global DB ID
	ProjectID       int64    `json:"project_id,omitempty"` // parent project
	Seq             int      `json:"seq"`                  // sequence within project (this is the user-facing run #)
	Name            string   `json:"name"`
	State           string   `json:"state"`
	TaskCount       int      `json:"task_count"`
	Branch          string   `json:"branch,omitempty"`            // git branch this run commits to
	Slug            string   `json:"slug,omitempty"`              // per-run slug used in enju/runs/{seq}-{slug}/
	CreatedAt       string   `json:"created_at"`
	SourcePath      string   `json:"source_path,omitempty"`       // Phase H.1 — template this run came from, if any
	SourceCommitSHA string   `json:"source_commit_sha,omitempty"` // Phase H.1 — project HEAD at instantiation time
	Warnings        []string `json:"warnings,omitempty"`          // non-fatal advisories from the parser
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}

	// Verify project exists
	proj, err := s.store.GetProject(projectID)
	if err != nil || proj == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("project %d not found", projectID))
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.YAML == "" {
		writeError(w, http.StatusBadRequest, "yaml is required")
		return
	}

	// Always route through ParseWithParams so declared defaults
	// fire even when the caller supplied no params. Passing nil
	// is equivalent to "caller supplied nothing" — defaults
	// still apply, and a required-no-default param raises the
	// natural-language error. Previously the plain-Parse branch
	// skipped substitution entirely and {{placeholder}} refs
	// leaked through as literal text whenever callers leaned on
	// defaults, which defeated the whole point of the default:
	// field.
	//
	// Parsing runs BEFORE branch resolution so the auto-branch
	// slug can source `parsed.Run.Name` (previously, branch:auto
	// fell back to "run-N" for inline YAML even when the run
	// carried a perfectly good name: field).
	parsed, err := enjuYaml.ParseWithParams([]byte(req.YAML), req.Params)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run definition: "+err.Error())
		return
	}

	// Branch resolution — three paths:
	//   - empty → project default
	//   - "auto" → pick an unused "<slug>-N" name sharing the
	//     slug with the run directory
	//   - explicit → use verbatim, just validate shape
	branch, err := s.resolveRunBranch(projectID, proj.DefaultBranch, req.Branch, req.SourcePath, parsed.Run.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Serial-runs-per-branch invariant: refuse a second active
	// run on the same branch. Concurrent variants MUST use
	// distinct branches. Error points at the existing run so
	// the caller can wait, switch to branch="auto", or pick a
	// specific name. "auto" skips this check because
	// resolveRunBranch already guarantees the result is unused.
	if req.Branch != "auto" {
		if existing, _ := s.store.ActiveRunOnBranch(projectID, branch); existing != nil {
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"branch %q already has an active run (#%d %q) — wait for it to finish, use branch=\"auto\" for an auto-named branch, or pass branch=\"<name>\" to isolate this run",
				branch, existing.Seq, existing.Name,
			))
			return
		}
	}

	// Pre-flight validation via engine (artifact paths +
	// citizen usernames). Runs before CreateRun so a failed
	// validation never leaves a ghost run behind.
	if err := s.engine().ValidateRunCreation(parsed); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Create the run record.
	now := time.Now()
	// Persist the MERGED params (declared defaults + caller-
	// supplied values, supplied wins) so the compute executor
	// rehydrates them into ENJU_PARAM_* env vars on task
	// execution. Using the merged map here ensures defaults
	// reach scripts too — persisting only req.Params would
	// drop defaults, causing `{{param}}` refs to substitute
	// correctly at parse time but ENJU_PARAM_<name> to come up
	// empty for any param the caller didn't type.
	var paramsJSON string
	if len(parsed.MergedParams) > 0 {
		if b, merr := json.Marshal(parsed.MergedParams); merr == nil {
			paramsJSON = string(b)
		}
	}
	runSlug := engine.ComputeRunSlug(req.SourcePath, parsed.Run.Name)
	runID, runSeq, err := s.store.CreateRun(&store.RunRecord{
		ProjectID:       projectID,
		Name:            parsed.Run.Name,
		Ref:             parsed.Run.Ref,
		YAMLData:        req.YAML,
		RepoURL:         req.RepoURL,
		State:           store.RunActive,
		SourcePath:      req.SourcePath,
		SourceCommitSHA: req.SourceCommitSHA,
		Params:          paramsJSON,
		Branch:          branch,
		Slug:            runSlug,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		// SQLite's partial unique index on (project_id, branch)
		// WHERE state = 'active' fires when a concurrent request
		// wins the race past ActiveRunOnBranch but before our
		// INSERT. Translate the raw constraint error into the
		// same 409 + helpful message the application-level
		// refusal produces, so both paths surface an identical
		// user experience.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "idx_runs_active_branch") {
			msg := fmt.Sprintf("branch %q already has an active run — wait for it to finish, use branch=\"auto\" for an auto-named branch, or pass branch=\"<name>\" to isolate this run", branch)
			if existing, _ := s.store.ActiveRunOnBranch(projectID, branch); existing != nil {
				msg = fmt.Sprintf("branch %q already has an active run (#%d %q) — wait for it to finish, use branch=\"auto\" for an auto-named branch, or pass branch=\"<name>\" to isolate this run", branch, existing.Seq, existing.Name)
			}
			writeError(w, http.StatusConflict, msg)
			return
		}
		s.logger.Error("creating run", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create run: "+err.Error())
		return
	}

	// Living-workflow phase 4c — persist the run-level
	// auto_triage rule (if any) before tasks are inserted, so
	// the auto-triage hook can read it the moment the run lands
	// on idle. Marshalled to JSON; empty when not declared OR
	// when declared empty (`auto_triage: {}`) so static
	// workflows and "rule present but empty" are both treated
	// uniformly. Without the empty check, an empty block would
	// land as `{}` in the column and the maybeAutoTriage hook
	// would log "missing action" warnings every idle tick.
	if t := parsed.Run.AutoTriage; t != nil &&
		(t.Action != "" || t.Prompt != "" || len(t.AssignTo) > 0 || t.RequireRole != "") {
		if data, err := json.Marshal(t); err == nil {
			if err := s.store.SetAutoTriageTemplate(runID, string(data)); err != nil {
				s.logger.Error("setting auto_triage_template", "run_id", runID, "error", err)
			}
		}
	}

	// Build task records via engine and apply atomically.
	taskRecords := engine.BuildRunTasks(parsed, runID, projectID, runSeq, runSlug)
	var mutations []store.Mutation
	for i := range taskRecords {
		mutations = append(mutations, store.CreateTask{Task: taskRecords[i]})
	}
	if len(mutations) > 0 {
		plan := store.Plan{
			Version:   engine.EngineVersion,
			Mutations: mutations,
		}
		if _, err := s.store.ApplyPlan(plan); err != nil {
			s.logger.Error("creating tasks", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to create tasks: "+err.Error())
			return
		}
	}
	taskCount := len(taskRecords)

	// Cache DAG and parsed run in memory.
	s.dags[runID] = parsed.DAG
	s.runs[runID] = parsed

	s.logger.Info("run created", "id", runID, "project_id", projectID, "seq", runSeq, "name", parsed.Run.Name, "tasks", taskCount)

	// Record run_created contribution event.
	if req.Username != "" {
		if citizen, _ := s.store.GetCitizenByUsername(req.Username); citizen != nil {
			s.store.RecordContributionEvent(&store.ContributionEvent{
				CitizenID:    citizen.ID,
				EventType:    "run_created",
				RunID:        runID,
				ProjectID:    projectID,
				Metadata:     fmt.Sprintf(`{"tasks":%d}`, taskCount),
				CreatedAt:    now,
			})
		}
	}

	if len(parsed.Warnings) > 0 {
		s.logger.Info("run created with warnings",
			"id", runID, "warnings", parsed.Warnings)
	}

	writeJSON(w, http.StatusCreated, runResponse{
		ID:              runID,
		ProjectID:       projectID,
		Seq:             runSeq,
		Name:            parsed.Run.Name,
		State:           string(store.RunActive),
		TaskCount:       taskCount,
		Branch:          branch,
		Slug:            runSlug,
		CreatedAt:       now.Format(time.RFC3339),
		SourcePath:      req.SourcePath,
		SourceCommitSHA: req.SourceCommitSHA,
		Warnings:        parsed.Warnings,
	})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}

	// Filter runs to projects the caller is a member of.
	// Legacy zero-members projects still flow through — they're
	// the pre-Phase-J open set.
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	allowed := map[int64]bool{}
	memberProjects, _ := s.store.ListProjectsForCitizen(caller.ID)
	for _, p := range memberProjects {
		allowed[p.ID] = true
	}

	var resp []runResponse
	for _, p := range runs {
		total, _ := s.store.CountProjectMembers(p.ProjectID)
		if total > 0 && !allowed[p.ProjectID] {
			continue
		}
		tasks, _ := s.store.ListTasksByRun(p.ID)
		resp = append(resp, runResponse{
			ID:         p.ID,
			ProjectID:  p.ProjectID,
			Seq:        p.Seq,
			Name:       p.Name,
			State:      string(p.State),
			TaskCount:  len(tasks),
			Branch:     p.Branch,
			Slug:       p.Slug,
			CreatedAt:  p.CreatedAt.Format(time.RFC3339),
			SourcePath: p.SourcePath,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetRunCostSummary(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	tasks, _ := s.store.ListTasksByRun(run.ID)

	var citizenSet = map[int64]bool{}
	var firstClaim, lastAccept time.Time
	for _, t := range tasks {
		if store.TaskState(t.State) == store.TaskAccepted {
			if t.ClaimedBy > 0 {
				citizenSet[t.ClaimedBy] = true
			}
			if t.ClaimedAt != nil && (firstClaim.IsZero() || t.ClaimedAt.Before(firstClaim)) {
				firstClaim = *t.ClaimedAt
			}
			if t.SubmittedAt != nil && (lastAccept.IsZero() || t.SubmittedAt.After(lastAccept)) {
				lastAccept = *t.SubmittedAt
			}
		}
	}
	// Pull char counts + estimated tokens from contribution
	// events metadata (the task record doesn't store content
	// length — content lives in git, but the events log has
	// both prompt_chars and content_chars from submit time).
	var totalPromptChars, totalContentChars, totalEstTokens int64
	taskIDs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		taskIDs = append(taskIDs, t.ID)
	}
	for _, tid := range taskIDs {
		metadata, err := s.store.GetEventMetadataForTask(tid, "task_completed")
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal([]byte(metadata), &m) == nil {
			if v, ok := m["prompt_chars"].(float64); ok {
				totalPromptChars += int64(v)
			}
			if v, ok := m["content_chars"].(float64); ok {
				totalContentChars += int64(v)
			}
			if v, ok := m["estimated_tokens"].(float64); ok {
				totalEstTokens += int64(v)
			}
		}
	}

	var wallClock string
	if !firstClaim.IsZero() && !lastAccept.IsZero() {
		wallClock = lastAccept.Sub(firstClaim).Round(time.Second).String()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project_id":       projectID,
		"run_seq":          runSeq,
		"tasks_total":      len(tasks),
		"tasks_accepted":   countByState(tasks, store.TaskAccepted),
		"prompt_chars":     totalPromptChars,
		"content_chars":    totalContentChars,
		"estimated_tokens": totalEstTokens,
		"citizen_count":    len(citizenSet),
		"wall_clock":       wallClock,
	})
}

// handleListRunEvents returns the synthesized event timeline
// for one run — chronological JSON list built from
// contribution_events + task_claims. Consumed by the fat-
// client's enju_export_run_events tool which materializes
// the list into `enju/runs/{seq}/events/{phase}.jsonl`
// on demand. Authoritative data stays in the coordinator DB;
// git gets a snapshot only when the user asks for one.
func (s *Server) handleListRunEvents(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	events, err := s.store.ListRunEvents(run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing events: "+err.Error())
		return
	}
	// Flatten into JSON-friendly shape with ts as RFC3339
	// (JSONL consumers parse this trivially) and metadata
	// as raw JSON (not a quoted string) when parseable.
	out := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		row := map[string]interface{}{
			"ts":   e.Timestamp.UTC().Format(time.RFC3339Nano),
			"type": e.Type,
		}
		if e.Subtype != "" {
			row["subtype"] = e.Subtype
		}
		if e.TaskID != "" {
			row["task_id"] = e.TaskID
		}
		if e.Citizen != "" {
			row["citizen"] = e.Citizen
		}
		if e.Metadata != "" {
			var md interface{}
			if json.Unmarshal([]byte(e.Metadata), &md) == nil {
				row["metadata"] = md
			} else {
				row["metadata"] = e.Metadata
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePauseRun moves a run to the `paused` state. Idempotent on
// already-paused runs; refuses on terminal (completed / failed)
// runs. Member-gated. Living-workflow phase 1 — see
// docs/living-workflow.md § 5 (out-of-band human interrupt).
func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	member, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}
	changed, err := s.store.PauseRun(run.ID, member.CitizenID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _ := s.store.GetRun(run.ID)
	status := "paused"
	if !changed {
		status = "already_paused"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  status,
		"run_id":  fmt.Sprintf("%d:%d", projectID, runSeq),
		"state":   string(updated.State),
		"changed": changed,
		"message": "run paused — SpawnTask now refuses on paused runs, but claims and submits still pass through.",
	})
}

// handleResumeRun moves a paused run back to active or idle,
// depending on whether ready work exists. No-op on already-alive
// runs; refuses on terminal runs.
func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	member, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}
	next, err := s.store.ResumeRun(run.ID, member.CitizenID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "resumed",
		"run_id": fmt.Sprintf("%d:%d", projectID, runSeq),
		"state":  string(next),
	})
}

// handleShowEvents is the read-only projection over
// contribution_events — the JSONL-shaped event log view.
// Distinct from /events (run-scoped) and from
// enju_export_run_events (which writes git-tracked snapshots).
// This endpoint is for ad-hoc queries: "what happened in this
// project / run / by this citizen, of these types, since when."
//
// Query params: run_seq (optional, narrows to one run),
// citizen (username, optional), event_types (comma-separated),
// since (RFC3339), limit (default 100, max 1000).
//
// Living-workflow phase 2 — see docs/living-workflow-design-notes.md
// § "The event log is the central data primitive."
func (s *Server) handleShowEvents(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	q := store.EventQuery{ProjectID: projectID}

	if rs := r.URL.Query().Get("run_seq"); rs != "" {
		seq, err := strconv.Atoi(rs)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_seq")
			return
		}
		run, err := s.store.GetRunByProjectSeq(projectID, seq)
		if err != nil || run == nil {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		q.RunID = run.ID
	}
	if u := r.URL.Query().Get("citizen"); u != "" {
		c, err := s.store.GetCitizenByUsername(u)
		if err == nil && c != nil {
			q.CitizenID = c.ID
		} else {
			// Unknown username → empty result, not an error.
			writeJSON(w, http.StatusOK, []map[string]interface{}{})
			return
		}
	}
	if et := r.URL.Query().Get("event_types"); et != "" {
		q.EventTypes = strings.Split(et, ",")
	}
	if since := r.URL.Query().Get("since"); since != "" {
		ts, err := time.Parse(time.RFC3339, since)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since (expected RFC3339): "+err.Error())
			return
		}
		q.Since = ts
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		q.Limit = n
	}

	events, err := s.store.ListEvents(q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing events: "+err.Error())
		return
	}

	out := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		row := map[string]interface{}{
			"ts":   e.Timestamp.UTC().Format(time.RFC3339Nano),
			"type": e.Type,
		}
		if e.Subtype != "" {
			row["subtype"] = e.Subtype
		}
		if e.TaskID != "" {
			row["task_id"] = e.TaskID
		}
		if e.Citizen != "" {
			row["citizen"] = e.Citizen
		}
		if e.Metadata != "" {
			var md interface{}
			if json.Unmarshal([]byte(e.Metadata), &md) == nil {
				row["metadata"] = md
			} else {
				row["metadata"] = e.Metadata
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// maybeAutoTriage handles the living-workflow phase 4c idle
// trigger: when a run lands on `idle` AND the run carries an
// auto_triage_template AND the project has at least one open
// issue, the engine spawns a fix task for the oldest open
// issue (by per-project seq). The spawned task lifts the run
// back to active; when it accepts, the close-on-accept hook
// transitions the issue to "closed".
//
// Best-effort: failures are logged and don't surface to
// callers — the run is still genuinely idle even if triage
// can't pick it up. Idempotent per (run, issue): the issue is
// transitioned to in_progress on spawn, so a second call finds
// no `open` issue and is a no-op.
//
// Concurrency: serialized per project via projectTriageMutex.
// Without the mutex, two concurrent submits in the same
// project could each pass FindOldestOpenIssue before either
// got to MarkIssueInProgress, both spawn a fix task, and one
// would become an orphan (correct issue link but no
// corresponding in_progress claim). The mutex closes that
// window — only one auto-triage per project can be mid-flight
// at any moment. Across projects there's no contention.
//
// Caller invokes after every state evaluation that can land on
// idle (submit, invalidate, fail-cascade).
func (s *Server) maybeAutoTriage(runID int64) {
	tmpl, err := s.store.GetAutoTriageTemplate(runID)
	if err != nil || tmpl == "" {
		return
	}
	run, err := s.store.GetRun(runID)
	if err != nil || run == nil {
		return
	}
	mu := s.projectTriageMutex(run.ProjectID)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the mutex: another goroutine may have
	// just spawned for this issue and transitioned it. If so,
	// FindOldestOpenIssue returns nil and we no-op.
	issue, err := s.store.FindOldestOpenIssue(run.ProjectID)
	if err != nil || issue == nil {
		return
	}

	var spec enjuYaml.RemediationTemplate
	if err := json.Unmarshal([]byte(tmpl), &spec); err != nil {
		s.logger.Error("auto_triage_template malformed", "run", runID, "error", err)
		return
	}
	if spec.Action == "" {
		s.logger.Error("auto_triage_template missing action", "run", runID)
		return
	}

	// Substitute issue context at spawn time. Mirrors the
	// {{review.feedback}} pattern from maybeSpawnRemediation
	// so the prompt captures the issue snapshot — even if the
	// underlying issue is later updated, the spawned task's
	// prompt remembers what triggered it.
	prompt := spec.Prompt
	prompt = strings.ReplaceAll(prompt, "{{issue.title}}", issue.Title)
	prompt = strings.ReplaceAll(prompt, "{{issue.body}}", issue.Body)
	prompt = strings.ReplaceAll(prompt, "{{issue.severity}}", issue.Severity)
	prompt = strings.ReplaceAll(prompt, "{{issue.id}}", fmt.Sprintf("ISSUE-%03d", issue.Seq))

	// Per-issue task_def_id pattern: fix_ISSUE_<seq>_<n> where
	// n is the next-available index. Mirrors the remediation
	// naming so multiple fix attempts on the same issue (e.g.
	// after a previous fix-task failed and the issue was
	// re-opened) don't collide.
	base := fmt.Sprintf("fix_ISSUE_%03d", issue.Seq)
	count, _ := s.store.CountTasksWithDefIDPrefix(runID, base+"_")
	defID := fmt.Sprintf("%s_%d", base, count+1)

	var assignTo []string
	if len(spec.AssignTo) > 0 {
		assignTo = []string(spec.AssignTo)
	}

	taskID, err := s.store.SpawnTask(store.SpawnSpec{
		RunID:          runID,
		TaskDefID:      defID,
		Action:         spec.Action,
		Prompt:         prompt,
		AssignTo:       assignTo,
		RequireRole:    spec.RequireRole,
		Trigger:        "auto_triage",
		ClosesIssueSeq: issue.Seq,
		// SpawnedBy = 0 — system-initiated, not a specific
		// citizen. The audit trail records this as
		// trigger=auto_triage which is the better attribution
		// signal anyway.
	})
	if err != nil {
		s.logger.Error("auto-triage spawn failed", "run", runID, "issue", issue.Seq, "error", err)
		return
	}

	// Move the issue to in_progress + link to the fix task.
	// MarkIssueInProgress emits issue_in_progress event for
	// the audit log.
	if err := s.store.MarkIssueInProgress(issue.ID, 0, taskID); err != nil {
		s.logger.Warn("auto-triage in_progress transition failed", "issue", issue.ID, "task", taskID, "error", err)
	}
}

// evaluateRunStateAndMaybeTriage wraps EvaluateRunState with
// the post-evaluation auto-triage hook. Used at every site
// that re-evaluates a run's state after a task transition.
func (s *Server) evaluateRunStateAndMaybeTriage(runID int64) {
	next, err := s.store.EvaluateRunState(runID)
	if err != nil {
		return
	}
	if next == store.RunIdle {
		s.maybeAutoTriage(runID)
	}
}

// maybeAutoTriageIfIdle is the submit-path variant: the state
// was already re-evaluated by CheckAndCompleteRun (which
// applyCompleteRun runs inside the submit Plan tx). We just
// need to read the current state and fire the trigger if
// idle. Cheap.
func (s *Server) maybeAutoTriageIfIdle(runID int64) {
	r, err := s.store.GetRun(runID)
	if err != nil || r == nil {
		return
	}
	if r.State == store.RunIdle {
		s.maybeAutoTriage(runID)
	}
}

// maybeAutoCloseIssue closes the issue linked to a submitted
// fix task. Triggered when a task with closes_issue_seq > 0
// reaches accepted. Best-effort — a CloseIssue refusal (issue
// already terminal somehow, e.g. closed manually mid-flight)
// is logged and absorbed.
func (s *Server) maybeAutoCloseIssue(task *store.TaskRecord) {
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		return
	}
	issue, err := s.store.GetIssueBySeq(run.ProjectID, task.ClosesIssueSeq)
	if err != nil || issue == nil {
		return
	}
	// Pass citizen_id = 0 (system close on auto-triage). The
	// audit event records closed_by_task_id pointing at the
	// fix task, which is the more useful attribution.
	if err := s.store.CloseIssue(issue.ID, 0, store.IssueStatusClosed, task.ID); err != nil {
		s.logger.Warn("auto-close issue failed", "issue", issue.ID, "task", task.ID, "error", err)
	}
}

// maybeSpawnRemediation handles the living-workflow phase 4b
// auto-spawn rule. Returns (result, true) when the target
// declared spawn_remediation for the given decision and a
// remediation task was successfully spawned; (nil, false)
// otherwise so the caller falls through to default cascade
// behavior.
//
// The spawned remediation:
//   - Carries the reviewer's feedback in metadata so an audit
//     reader can reconstruct the why
//   - Has the reviewer's content substituted into prompt via
//     {{review.feedback}} and {{review.decision}}
//   - depends_on names the original target so any future
//     re-claim chain naturally waits for the remediation
//   - Trigger = "template_rule" — distinguishes auto-spawned
//     remediations from human/bot-initiated spawns in the audit log
//
// Failure modes: rule unset, target not found, malformed
// remediation_template JSON, or SpawnTask error (cycle budget
// exhausted etc.) all return (nil, false). The caller falls
// back to the default cascade. We log but don't surface — a
// remediation-spawn failure must not stop a review submission
// from being recorded.
func (s *Server) maybeSpawnRemediation(reviewTaskID, targetTaskID, eventKind, decision, feedback string, submitterID int64) (*invalidationResult, bool) {
	target, err := s.store.GetTask(targetTaskID)
	if err != nil || target == nil {
		return nil, false
	}
	var rule string
	switch eventKind {
	case "reject":
		rule = target.OnReviewReject
	case "request_changes":
		rule = target.OnReviewRequestChanges
	}
	if rule != "spawn_remediation" || target.RemediationTemplate == "" {
		return nil, false
	}

	var tmpl enjuYaml.RemediationTemplate
	if err := json.Unmarshal([]byte(target.RemediationTemplate), &tmpl); err != nil {
		s.logger.Error("remediation_template malformed", "target", targetTaskID, "error", err)
		return nil, false
	}
	if tmpl.Action == "" {
		s.logger.Error("remediation_template missing action", "target", targetTaskID)
		return nil, false
	}

	// Substitute reviewer feedback into the prompt. Done at
	// spawn time (not claim time) so the remediation task
	// captures the feedback text immutably even if the review
	// task is later edited or invalidated.
	prompt := tmpl.Prompt
	prompt = strings.ReplaceAll(prompt, "{{review.feedback}}", feedback)
	prompt = strings.ReplaceAll(prompt, "{{review.decision}}", decision)

	// Pick a unique task_def_id for the remediation. The
	// pattern <target>_remediation_<n> keeps lineage readable;
	// the counter handles the case where the same target gets
	// rejected multiple times. Empty target_def_id falls back
	// to a generic seq via task_spawned events.
	remediationDefID := s.nextRemediationDefID(target)

	var assignTo []string
	if len(tmpl.AssignTo) > 0 {
		assignTo = []string(tmpl.AssignTo)
	}

	taskID, err := s.store.SpawnTask(store.SpawnSpec{
		RunID:        target.RunID,
		ParentTaskID: targetTaskID,
		TaskDefID:    remediationDefID,
		Action:       tmpl.Action,
		Prompt:       prompt,
		DependsOn:    []string{targetTaskID},
		AssignTo:     assignTo,
		RequireRole:  tmpl.RequireRole,
		Trigger:      "template_rule",
		SpawnedBy:    submitterID,
	})
	if err != nil {
		s.logger.Error("auto-spawn remediation failed", "target", targetTaskID, "error", err)
		return nil, false
	}

	updated, _ := s.store.GetTask(taskID)
	return &invalidationResult{
		Task: updated,
	}, true
}

// nextRemediationDefID picks a fresh task_def_id for an
// auto-spawned remediation by counting existing remediation
// tasks for the target. Pattern: <target_def_id>_remediation_<N>
// where N is the next-available index.
//
// Counts directly from the tasks table via a LIKE prefix
// match — bounded query (one COUNT, indexed by run_id) and
// precise (no false-positive substring traps that an
// event-metadata scan would have).
func (s *Server) nextRemediationDefID(target *store.TaskRecord) string {
	base := target.TaskDefID + "_remediation"
	count, err := s.store.CountTasksWithDefIDPrefix(target.RunID, base+"_")
	if err != nil {
		return fmt.Sprintf("%s_1", base)
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}

// --- Spawn primitive (living-workflow phase 4a) ---

type spawnTaskRequest struct {
	ParentTaskID string   `json:"parent_task_id,omitempty"`
	TaskDefID    string   `json:"task_def_id"`
	Action       string   `json:"action"`
	Prompt       string   `json:"prompt,omitempty"`
	UserPrompt   string   `json:"user_prompt,omitempty"`
	Citizens     int      `json:"citizens,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`
	AssignTo     []string `json:"assign_to,omitempty"`
	RequireRole  string   `json:"require_role,omitempty"`
	ResultType   string   `json:"result_type,omitempty"`
	Trigger      string   `json:"trigger,omitempty"`
}

// handleSpawnTask creates a new task in an existing run at
// runtime. Member-gated; the spawning citizen is the
// authenticated caller. Subject to the per-run cycle budget —
// budget exhaustion auto-pauses the run and returns 409 Conflict
// so callers can distinguish "you tried to spawn into a stopped
// run" from generic 400 validation errors.
//
// Living-workflow phase 4a. The YAML-rule sugar
// (on_review_reject, on_idle) lands in 4b/4c on top of this
// primitive.
func (s *Server) handleSpawnTask(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	runSeq, err := strconv.Atoi(chi.URLParam(r, "runSeq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run_seq")
		return
	}
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	member, ok := s.requireProjectMembership(w, r, projectID)
	if !ok {
		return
	}

	var req spawnTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TaskDefID == "" {
		writeError(w, http.StatusBadRequest, "task_def_id is required")
		return
	}
	if req.Action == "" {
		writeError(w, http.StatusBadRequest, "action is required")
		return
	}

	taskID, err := s.store.SpawnTask(store.SpawnSpec{
		RunID:        run.ID,
		ParentTaskID: req.ParentTaskID,
		TaskDefID:    req.TaskDefID,
		Action:       req.Action,
		Prompt:       req.Prompt,
		UserPrompt:   req.UserPrompt,
		Citizens:     req.Citizens,
		DependsOn:    req.DependsOn,
		AssignTo:     req.AssignTo,
		RequireRole:  req.RequireRole,
		ResultType:   req.ResultType,
		Trigger:      req.Trigger,
		SpawnedBy:    member.CitizenID,
	})
	if err != nil {
		// Cycle-budget exhaustion is a distinct condition —
		// the run is now paused, the caller should know to
		// resume after extending budget. 409 Conflict matches
		// the "request can't be fulfilled in current state"
		// REST convention.
		if strings.Contains(err.Error(), "cycle budget exhausted") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	used, max, _ := s.store.GetCycleBudget(run.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":           "spawned",
		"task_id":          taskID,
		"task_def_id":      req.TaskDefID,
		"parent_task_id":   req.ParentTaskID,
		"trigger":          req.Trigger,
		"cycle_budget":     map[string]int{"used": used, "max": max},
	})
}

type setCycleBudgetRequest struct {
	Max int `json:"max"`
}

// handleSetCycleBudget bumps the cycle-budget cap on a run.
// Used by operators to extend room after a runaway has been
// triaged and the underlying loop fixed. Member-gated.
func (s *Server) handleSetCycleBudget(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	runSeq, err := strconv.Atoi(chi.URLParam(r, "runSeq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run_seq")
		return
	}
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setCycleBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Max <= 0 {
		writeError(w, http.StatusBadRequest, "max must be positive")
		return
	}
	if err := s.store.SetCycleBudgetMax(run.ID, caller.ID, req.Max); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	used, max, _ := s.store.GetCycleBudget(run.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "updated",
		"cycle_budget": map[string]int{"used": used, "max": max},
	})
}

// --- Issues (living-workflow phase 3) ---

type fileIssueRequest struct {
	Title         string `json:"title"`
	Body          string `json:"body"`
	Severity      string `json:"severity"`
	FoundInRunSeq int    `json:"found_in_run_seq,omitempty"`
	FoundInTaskID string `json:"found_in_task_id,omitempty"`
}

// handleFileIssue creates a new issue under a project. Member-
// gated; the filer is the authenticated citizen. Emits an
// `issue_filed` contribution event so the issue appears in the
// project's event log. Body and severity are optional; status
// defaults to "open."
//
// Living-workflow phase 3 — see
// docs/living-workflow-design-notes.md § 6.
func (s *Server) handleFileIssue(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	var req fileIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	// Filer attribution comes from the auth context — works
	// uniformly for both regular projects (where the caller is
	// also a member) and legacy zero-member projects (where
	// requireProjectMembership returns nil-member-ok-true under
	// the open-access fallback). Falling back to caller via
	// citizenFromRequest keeps the audit trail meaningful in
	// both cases.
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rec := &store.IssueRecord{
		ProjectID:     projectID,
		Title:         req.Title,
		Body:          req.Body,
		Severity:      req.Severity,
		FoundInTaskID: req.FoundInTaskID,
		FiledBy:       caller.ID,
	}
	// found_in_run_seq is project-scoped; resolve to the run's
	// global ID before storing. Hard-fail on lookup miss so the
	// audit trail can't accumulate issues that point at runs
	// that never existed — silent drops were the bug ISSUE-007
	// flagged.
	if req.FoundInRunSeq > 0 {
		run, err := s.store.GetRunByProjectSeq(projectID, req.FoundInRunSeq)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "looking up found_in_run_seq: "+err.Error())
			return
		}
		if run == nil {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("found_in_run_seq %d does not exist in project %d", req.FoundInRunSeq, projectID))
			return
		}
		rec.FoundInRunID = run.ID
	}

	// CreateIssue emits issue_filed in the same tx as the
	// INSERT, so no follow-up event recording here.
	id, seq, err := s.store.CreateIssue(rec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "creating issue: "+err.Error())
		return
	}

	// Living-workflow phase 4c — file-against-completed-run
	// gap. A run that opted into auto_triage and reached
	// "completed" because no issue existed at the time should
	// re-evaluate now that one does: completed → idle, then
	// the auto-triage hook spawns a fix. Without this, the
	// natural pattern "dev task finishes, THEN tester files
	// issue" silently drops the trigger and the issue sits
	// open against a dead run.
	if runIDs, err := s.store.ListRunsWithAutoTriage(projectID); err == nil {
		for _, rid := range runIDs {
			s.evaluateRunStateAndMaybeTriage(rid)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":        id,
		"seq":       seq,
		"slug":      fmt.Sprintf("ISSUE-%03d", seq),
		"status":    rec.Status,
		"severity":  rec.Severity,
		"title":     rec.Title,
	})
}

// handleListIssues returns all issues in a project, newest-first.
// Optional query params: status (comma-separated, OR-matched),
// severity (comma-separated), limit (default 100, max 1000).
func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	f := store.IssueFilter{ProjectID: projectID}
	if st := r.URL.Query().Get("status"); st != "" {
		f.Status = strings.Split(st, ",")
	}
	if sv := r.URL.Query().Get("severity"); sv != "" {
		f.Severity = strings.Split(sv, ",")
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = n
	}

	issues, err := s.store.ListIssues(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing issues: "+err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(issues))
	for _, it := range issues {
		out = append(out, s.issueToMap(&it))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetIssue returns one issue by its (project, seq) pair.
func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "issueSeq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid issue_seq")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	it, err := s.store.GetIssueBySeq(projectID, seq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if it == nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	writeJSON(w, http.StatusOK, s.issueToMap(it))
}

type triageIssueRequest struct {
	Severity string `json:"severity,omitempty"` // optional severity update
}

func (s *Server) handleTriageIssue(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "issueSeq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid issue_seq")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req triageIssueRequest
	// Body is optional (severity-only update). Decode and
	// tolerate io.EOF — that covers Content-Length: 0,
	// Transfer-Encoding: chunked with empty body, and a nil
	// body equally. Any other decode error means the caller
	// sent malformed JSON and should hear about it.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	it, err := s.store.GetIssueBySeq(projectID, seq)
	if err != nil || it == nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	// TriageIssue emits issue_triaged in the same tx as the
	// UPDATE.
	if err := s.store.TriageIssue(it.ID, caller.ID, req.Severity); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _ := s.store.GetIssue(it.ID)
	writeJSON(w, http.StatusOK, s.issueToMap(updated))
}

type closeIssueRequest struct {
	Status         string `json:"status"`            // "closed" | "wontfix"
	ClosedByTaskID string `json:"closed_by_task_id"` // optional
}

func (s *Server) handleCloseIssue(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "issueSeq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid issue_seq")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req closeIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Status == "" {
		req.Status = store.IssueStatusClosed
	}

	it, err := s.store.GetIssueBySeq(projectID, seq)
	if err != nil || it == nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	// CloseIssue emits issue_closed in the same tx as the UPDATE.
	if err := s.store.CloseIssue(it.ID, caller.ID, req.Status, req.ClosedByTaskID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _ := s.store.GetIssue(it.ID)
	writeJSON(w, http.StatusOK, s.issueToMap(updated))
}

// issueToMap is the shared JSON shape for every issue endpoint.
// Keys mirror the YAML frontmatter from the design notes (id,
// title, status, severity, ...) so a future fat-client mirror
// can dump the map straight into ISSUE-<NNN>.md.
//
// Citizen ids are resolved to usernames so humans (and the
// markdown frontmatter) read names, not numbers — matches the
// vote/review submission rendering.
func (s *Server) issueToMap(it *store.IssueRecord) map[string]interface{} {
	m := map[string]interface{}{
		"id":         fmt.Sprintf("ISSUE-%03d", it.Seq),
		"db_id":      it.ID,
		"seq":        it.Seq,
		"project_id": it.ProjectID,
		"title":      it.Title,
		"body":       it.Body,
		"status":     it.Status,
		"severity":   it.Severity,
		"filed_by":   s.citizenUsername(it.FiledBy),
		"filed_at":   it.FiledAt.UTC().Format(time.RFC3339),
		"updated_at": it.UpdatedAt.UTC().Format(time.RFC3339),
	}
	// Surface the per-project run seq (#1, #2, ...) — the
	// citizen-facing identity. The internal DB id stays out
	// of the response shape so a future filesystem mirror
	// writes the human-meaningful number into ISSUE-NNN.md
	// frontmatter. Lookup is best-effort: if the run was
	// hard-deleted the field falls off silently rather than
	// blocking the issue render.
	if it.FoundInRunID > 0 {
		if run, err := s.store.GetRun(it.FoundInRunID); err == nil && run != nil {
			m["found_in_run_seq"] = run.Seq
		}
	}
	if it.FoundInTaskID != "" {
		m["found_in_task_id"] = it.FoundInTaskID
	}
	if it.TriagedBy > 0 {
		m["triaged_by"] = s.citizenUsername(it.TriagedBy)
	}
	if it.TriagedAt != nil {
		m["triaged_at"] = it.TriagedAt.UTC().Format(time.RFC3339)
	}
	if it.ClosedByTaskID != "" {
		m["closed_by_task_id"] = it.ClosedByTaskID
	}
	if it.ClosedAt != nil {
		m["closed_at"] = it.ClosedAt.UTC().Format(time.RFC3339)
	}
	return m
}

func countByState(tasks []store.TaskRecord, state store.TaskState) int {
	n := 0
	for _, t := range tasks {
		if store.TaskState(t.State) == state {
			n++
		}
	}
	return n
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))

	p, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	tasks, _ := s.store.ListTasksByRun(p.ID)

	resp := map[string]interface{}{
		"id":         p.ID,
		"project_id": p.ProjectID,
		"seq":        p.Seq,
		"name":       p.Name,
		"state":      p.State,
		"repo_url":   p.RepoURL,
		"branch":     p.Branch,
		"slug":       p.Slug,
		"task_count": len(tasks),
		"created_at": p.CreatedAt.Format(time.RFC3339),
	}
	if p.SourcePath != "" {
		resp["source_path"] = p.SourcePath
	}
	if p.SourceCommitSHA != "" {
		resp["source_commit_sha"] = p.SourceCommitSHA
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListRunTasks(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))

	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	tasks, err := s.store.ListTasksByRun(run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}
	writeJSON(w, http.StatusOK, s.toTaskResponses(tasks))
}

// --- Tasks ---

type taskResponse struct {
	ID               string `json:"id"`
	RunID            int64  `json:"run_id"`                       // global run ID
	RunSeq           int    `json:"run_seq"`                      // per-project run sequence
	ProjectID        int64  `json:"project_id"`                   // parent project
	ProjectRemoteURL string `json:"project_remote_url,omitempty"` // parent project's git remote (for fat clients)
	ProjectName      string `json:"project_name,omitempty"`       // human-readable project name (for workspace dirs)
	Seq              int    `json:"seq"`                          // task sequence within run
	TaskDefID       string   `json:"task_def_id"`
	InstanceKey     string   `json:"instance_key,omitempty"`
	IterationLabel  string   `json:"iteration_label,omitempty"` // "gene=BRCA1, tissue=breast" — human-readable for_each context
	// ResultDir is the pre-computed repo-relative path for
	// this task's result files. Layout is a coordinator-
	// owned schema (see engine.ComputeResultDir); clients
	// consume the string directly rather than rebuilding it
	// from (runSeq, instanceKey, taskDefID). Keeps future
	// layout changes to one function edit.
	ResultDir       string   `json:"result_dir,omitempty"`
	// RunSlug is the per-run slug that appears in ResultDir
	// (enju/runs/{seq}-{slug}/). Surfaced on the wire so the
	// fat-client executor can locate the template-snapshot
	// dir without duplicating the slug rule client-side.
	RunSlug         string   `json:"run_slug,omitempty"`
	Ref             string   `json:"ref,omitempty"`
	Action          string   `json:"action"`
	Prompt          string   `json:"prompt,omitempty"`
	UserPrompt      string   `json:"user_prompt,omitempty"`
	Script          string   `json:"script,omitempty"`
	Outputs         string   `json:"outputs,omitempty"`
	Requirements    string   `json:"requirements,omitempty"`
	ResultType      string   `json:"result_type"`
	State           string   `json:"state"`
	ClaimedBy       string   `json:"claimed_by,omitempty"` // username of the claimer
	// Model is the model citizen username credited for the most
	// recent completed submission on this task. Populated for
	// single-citizen tasks once they reach a terminal/submitted
	// state; multi-citizen tasks expose per-voter models via
	// VoteSubmissions[].Model instead. Empty when no submission
	// has happened yet, when the operator was a human submitting
	// unaided, or for pre-1.4 rows that never recorded a model.
	Model string `json:"model,omitempty"`
	ResultPath      string   `json:"result_path,omitempty"`
	CommitSHA       string   `json:"commit_sha,omitempty"` // git SHA of the accepted result (iteration A+)
	DependsOn       string   `json:"depends_on,omitempty"`
	ReadsArtifacts  []string               `json:"reads_artifacts,omitempty"`
	WritesArtifacts enjuYaml.WriteArtifacts `json:"writes_artifacts,omitempty"`
	AssignTo        []string `json:"assign_to,omitempty"` // usernames
	RequireRole     string   `json:"require_role,omitempty"`
	ReviewsTarget   string   `json:"reviews_target,omitempty"`   // Phase E: target task id this review evaluates
	ReviewDecision  string   `json:"review_decision,omitempty"`  // Phase E: approve/reject once submitted
	VoteOptions     string   `json:"vote_options,omitempty"`     // Phase E.2: declared options JSON
	VoteChoice      string   `json:"vote_choice,omitempty"`      // Phase E.2: winning option id
	Citizens        int      `json:"citizens,omitempty"`         // Phase E.2: invited voter count
	MinQuorum       int      `json:"min_quorum,omitempty"`       // Phase E.2: required submitted count
	VoteThreshold   string   `json:"vote_threshold,omitempty"`   // Phase E.2: agreement rule
	VoteDeadline    string   `json:"vote_deadline,omitempty"`    // Phase E.2: voting-closes duration
	VoteDeadlineAt  string   `json:"vote_deadline_at,omitempty"` // Phase E.2: absolute expiry (ISO), empty until first claim
	Anonymize       bool     `json:"anonymize,omitempty"`        // Phase E.2: hide citizen usernames
	Visibility      string   `json:"visibility,omitempty"`       // Phase E.2: open|blind during collection
	FailReason      string   `json:"fail_reason,omitempty"`     // reason for FAILED state
	SkipReason      string   `json:"skip_reason,omitempty"`     // reason for SKIPPED via fail-cascade, e.g. "upstream failed: 1:4:write_data"
	ParkedFromState string   `json:"parked_from_state,omitempty"` // stashed prior state for a parked task; empty otherwise
	// RunSourcePath mirrors run.source_path so the fat-client
	// executor can resolve a compute task's `script:` field
	// against the run's per-run template snapshot
	// (enju/runs/{seq}/template-snapshot/) instead of the live
	// enju/templates/ path. Empty for inline-YAML runs.
	RunSourcePath   string   `json:"run_source_path,omitempty"`
	// RunBranch is the git branch this task's run commits to.
	// Fat-client submit/execute paths feed this into
	// mcpgit.SubmitRequest so parallel runs on distinct
	// branches don't stomp on each other's files.
	RunBranch string `json:"run_branch,omitempty"`
	// RunParams is the parsed map of run-level params the
	// caller supplied at create_run, after defaults filled
	// in. The executor exposes these to compute scripts as
	// ENJU_PARAM_<name> env vars (lists comma-joined).
	RunParams map[string]interface{} `json:"run_params,omitempty"`
	// InstanceParams is the parsed per-iteration for_each
	// variable map (e.g. {"stem": "alpha"} for alpha:describe).
	// Surfaced alongside RunParams so the executor can expose
	// both as ENJU_PARAM_<name>.
	InstanceParamsMap map[string]interface{} `json:"instance_params_map,omitempty"`
	// Env is the task-definition-level env: block for compute
	// tasks — already {{param}}-substituted at parse time.
	// Surfaced on the task record so the compute executor on
	// the fat-client side can inject these into the script's
	// process environment. Empty/absent for non-compute tasks.
	Env map[string]string `json:"env,omitempty"`
	// Mode carries the compute-task execution mode — "sync"
	// or "async" (phase 4 async compute). Empty for non-
	// compute tasks. The fat-client's enju_execute_task
	// handler reads this to pick the sync vs detached-
	// subprocess code path.
	Mode string `json:"mode,omitempty"`
	// Container is the Docker image reference this compute
	// task runs inside (empty = run the script directly on the
	// host as before). The fat-client's enju_execute_task
	// handler feeds it into the compute wrapper, which builds
	// a `docker run ...` invocation instead of exec'ing the
	// script directly.
	Container string `json:"container,omitempty"`
	// VoteSubmissions is the per-citizen voting history for
	// multi-citizen vote tasks — one entry per submitted vote,
	// in submission order. Populated lazily only for citizens>1
	// vote tasks; empty for single-citizen tasks or non-vote
	// actions.
	VoteSubmissions []voteSubmissionRef `json:"vote_submissions,omitempty"`
	// ActiveClaimants lists citizens who currently hold open
	// claim slots on a multi-citizen task (claimed but not yet
	// submitted). Empty for citizens=1 tasks — those use the
	// ClaimedBy field.
	ActiveClaimants []string `json:"active_claimants,omitempty"`
	// IterationBranches maps each active claimant's username to
	// the per-iteration topic branch they should commit on
	// (e.g. "myrun/expand/iter-1"). The fat client picks its
	// own entry — coordinator stays branch-agnostic. Empty when
	// no active claim has a branch (vote/review actions, or
	// pre-phase-5 rows).
	IterationBranches map[string]string `json:"iteration_branches,omitempty"`
	// ArtifactProvenance shows who last wrote each artifact
	// this task reads.
	ArtifactProvenance []artifactProvenance `json:"artifact_provenance,omitempty"`
	// TaskHistory shows previous claim/submit/invalidation
	// attempts on this task. Populated when the task has
	// more than one claim record (indicates re-runs after
	// invalidation).
	TaskHistory []taskHistoryEntry `json:"task_history,omitempty"`
	// PreviousIterationCommit is the commit SHA of the most
	// recent COMPLETED claim that's no longer the active one.
	// Surfaced for the phase 6b.1 topic-branch flow: when the
	// fat-client re-claims after request_changes, the prior
	// iteration's content lives on its (now stale) topic
	// branch; the previous-submission UI uses this SHA to
	// read the content via ReadFileAtCommit instead of from
	// the current workspace (which was switched to the run
	// branch by the pre-claim reconcile). Empty when there's
	// no prior completed claim, or when the prior claim
	// wrote no commit (vote/review without content).
	PreviousIterationCommit string `json:"previous_iteration_commit,omitempty"`
	// UpstreamIterationBranch is the topic branch of the task
	// this one is reviewing — populated only for action:review
	// tasks whose reviews_target has a completed claim with a
	// topic branch. The fat-client uses it as the fork point
	// for the review's own topic branch so that, when the
	// review approves, the merged review topic carries the
	// upstream's content forward to the run branch in one FF
	// step. Empty for non-review tasks and for reviews whose
	// targets predate the topic-branch flow (legacy claim
	// rows on the run branch).
	UpstreamIterationBranch string `json:"upstream_iteration_branch,omitempty"`
	// LatestCompletedCommitSHA is the most recent submission
	// commit on this task, regardless of whether the task is
	// currently in a terminal state. Useful to readers that
	// need to recover content after a state-clearing transition
	// (request_changes, invalidate) where t.CommitSHA gets
	// blanked but the underlying git commit still exists. Empty
	// when no claim ever completed.
	LatestCompletedCommitSHA string `json:"latest_completed_commit_sha,omitempty"`
	// LatestCompletedBranch is the topic branch of the same
	// claim referenced by LatestCompletedCommitSHA. Pair with
	// LatestCompletedCommitSHA for ReadFileAtCommit lookups
	// against historic content that's no longer on the
	// workspace's current branch.
	LatestCompletedBranch string `json:"latest_completed_branch,omitempty"`
}

type taskHistoryEntry struct {
	Citizen     string  `json:"citizen"`
	ClaimedAt   string  `json:"claimed_at"`
	SubmittedAt string  `json:"submitted_at,omitempty"`
	Outcome     string  `json:"outcome"` // completed, invalidated, released, timed_out
	Decision    string  `json:"decision,omitempty"`
}

type artifactProvenance struct {
	Path       string `json:"path"`
	LastWriter string `json:"last_writer,omitempty"` // username
	LastTaskID string `json:"last_task_id,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"`
}

// voteSubmissionRef is one citizen's submitted vote on a
// multi-citizen task, rendered for the task response so
// formatters can show the tally without a separate fetch.
type voteSubmissionRef struct {
	Username    string `json:"username"`
	Option      string `json:"option"`
	SubmittedAt string `json:"submitted_at,omitempty"`
	// Model is the model citizen username credited for this
	// submission's words (operator/model design — the operator
	// is Username, the model is who PRODUCED the prose). Empty
	// when the operator is a human submitting unaided. Pre-1.4
	// rows have empty model; new submits populate it from the
	// per-call -model flag or the per-call override.
	Model string `json:"model,omitempty"`
}

func (s *Server) handleListReadyTasks(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	runSeq, _ := strconv.Atoi(r.URL.Query().Get("run_id"))

	// When the caller scopes to a specific project, gate on
	// membership here. When they don't (project_id=0), filter
	// the returned task list down to runs whose project they're
	// a member of — see below.
	if projectID > 0 {
		if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
			return
		}
	}

	var runGlobalID int64
	if projectID > 0 && runSeq > 0 {
		run, _ := s.store.GetRunByProjectSeq(projectID, runSeq)
		if run != nil {
			runGlobalID = run.ID
		}
	}

	tasks, err := s.store.ListReadyTasks(runGlobalID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}

	// Cross-project listings: filter to tasks whose run's
	// project the caller is a member of. Pre-membership legacy
	// projects (zero members) stay visible.
	if projectID == 0 {
		caller := citizenFromRequest(r)
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		allowed := map[int64]bool{}
		member, _ := s.store.ListProjectsForCitizen(caller.ID)
		for _, p := range member {
			allowed[p.ID] = true
		}
		filtered := tasks[:0]
		for _, t := range tasks {
			run, _ := s.store.GetRun(t.RunID)
			if run == nil {
				continue
			}
			total, _ := s.store.CountProjectMembers(run.ProjectID)
			if total == 0 || allowed[run.ProjectID] {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}
	writeJSON(w, http.StatusOK, s.toTaskResponses(tasks))
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}
	// Lazy deadline check: if this is a collecting vote task
	// whose deadline has passed, evaluate the tally now — no
	// new submission is needed for the polls to close. When
	// the tally resolves, transition to ACCEPTED and fire the
	// skip cascade so downstream consumers unblock without
	// waiting for an external nudge.
	s.maybeResolveDeadlineVote(task)
	// Re-read the task in case the resolution above mutated
	// state. Cheap (one DB query) and keeps the response
	// consistent with the new state.
	if updated, _ := s.store.GetTask(taskID); updated != nil {
		task = updated
	}
	writeJSON(w, http.StatusOK, s.toTaskResponse(*task))
}

// maybeResolveDeadlineVote runs the deadline-triggered tally
// resolution for a collecting vote task whose deadline has
// passed. Called from GET paths as a lazy sweep — we don't run
// a background reaper for this in R1; the task resolves the
// next time anyone looks at it. Soft-failures are logged, not
// surfaced: if the resolution can't run, the task stays in
// COLLECTING and the next touch retries.
func (s *Server) maybeResolveDeadlineVote(task *store.TaskRecord) {
	if task == nil {
		return
	}
	if task.Action != "vote" || store.TaskState(task.State) != store.TaskCollecting {
		return
	}
	if task.VoteDeadline == "" {
		return
	}
	passed, err := s.engine().DeadlinePassed(task)
	if err != nil || !passed {
		return
	}
	outcome, err := s.engine().EvaluateVoteTally(task)
	if err != nil || outcome == nil || !outcome.Resolved {
		return
	}
	if _, err := s.store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetTaskState{
				TaskID:     task.ID,
				NewState:   store.TaskAccepted,
				VoteChoice: outcome.WinningOption,
				CommitSHA:  task.CommitSHA,
			},
		},
	}); err != nil {
		s.logger.Warn("deadline-triggered vote resolve failed",
			"task_id", task.ID, "error", err)
		return
	}
	// Re-load so the skip cascade sees the winning option in
	// the task record.
	updated, _ := s.store.GetTask(task.ID)
	if updated != nil {
		if _, err := s.performSkipCascade(updated, outcome.WinningOption); err != nil {
			s.logger.Warn("deadline-triggered skip cascade failed",
				"task_id", task.ID, "error", err)
		}
	}
	// Re-run readiness so downstream tasks unblock.
	_, _ = s.store.UpdateReadyTasks(task.RunID)
	s.logger.Info("vote resolved via deadline sweep",
		"task_id", task.ID, "winning_option", outcome.WinningOption)
}

// claimRequest identifies the caller by username. Internally the
// server resolves it to an int64 citizen ID.
type claimRequest struct {
	Username string `json:"username"`
	// Model is the LLM citizen username producing the
	// words for this claim (operator/model design, layer B). Empty
	// when the operator is a human submitting unaided. Bots MUST
	// supply a non-empty value or the apply path rejects the claim.
	// Server resolves the username to a model citizen ID via
	// resolveModelByUsername.
	Model string `json:"model,omitempty"`
}

// resolveModelByUsername looks up a model citizen by username and
// returns its ID. Empty input → (nil, nil), the unaided-human case
// (apply-layer enforces "bots must have model" so empty for a bot
// fails downstream with a clear message).
//
// Per the operator/model design doc's "free-form + soft validation"
// stance, unknown-but-valid model names are AUTO-REGISTERED as new
// kind='model' catalog entries on first use. This matches local-mode
// philosophy: someone running Ollama with a custom finetune
// shouldn't have to ceremonially pre-register before they can submit.
// Hosted-mode policy gating (require pre-registration / admin
// approval) is deferred — see the design doc.
//
// The one defense kept: reject if the username resolves to a
// non-model citizen (kind ∈ {human, bot}). Without this, a caller
// could attribute their submit to a teammate's account, blurring
// who-did-what.
func (s *Server) resolveModelByUsername(modelName string) (*int64, error) {
	if modelName == "" {
		return nil, nil
	}
	c, err := s.store.GetCitizenByUsername(modelName)
	if err != nil {
		return nil, fmt.Errorf("look up model %q: %w", modelName, err)
	}
	if c != nil {
		if c.Kind != "model" {
			return nil, fmt.Errorf("citizen %q has kind %q, not %q — operators cannot be self-attributed as their own model", modelName, c.Kind, "model")
		}
		return &c.ID, nil
	}
	// Unknown model — auto-register. Display name defaults to the
	// username; explicit registration via enju_register_model gives
	// callers a chance to set a prettier display name.
	//
	// Known limitation: typos pollute the catalog. A submit with
	// model=clude-opus-4-7 (typo) creates a permanent ghost entry.
	// No cleanup tool ships today. Acceptable for now since
	// (a) the catalog is small, (b) typos surface in
	// enju_list_models so the user can spot them, (c) ghost models
	// don't authenticate or grant access. A "delete unused model"
	// admin tool can land later if catalog hygiene becomes a real
	// problem in hosted mode.
	id, err := s.store.CreateModelCitizen(modelName, modelName)
	if err != nil {
		return nil, fmt.Errorf("auto-register model %q: %w", modelName, err)
	}
	return &id, nil
}

// resolveCitizen looks up a caller by username and returns the
// CitizenRecord, or writes an error response and returns nil if the
// citizen doesn't exist.
func (s *Server) resolveCitizen(w http.ResponseWriter, username string) *store.CitizenRecord {
	if username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return nil
	}
	c, err := s.store.GetCitizenByUsername(username)
	if err != nil || c == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("citizen %q not found", username))
		return nil
	}
	return c
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	caller := s.resolveCitizen(w, req.Username)
	if caller == nil {
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}

	// Get task to determine timeout and enforce access control
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	// Access control: assign_to and require_role are optional.
	if err := engine.CheckTaskAccess(task, caller); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	// Parse timeout (default 30 minutes)
	timeout := 30 * time.Minute
	if task.Timeout != "" {
		if d, err := time.ParseDuration(task.Timeout); err == nil {
			timeout = d
		}
	}
	deadline := time.Now().Add(timeout)

	// Resolve optional model attribution. Empty model
	// is fine for human operators (unaided submit); bots that
	// arrive here without a model are rejected by the apply layer.
	modelID, err := s.resolveModelByUsername(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Engine validates (state, slots, cap) → returns Plan.
	plan, err := s.engine().ComputeClaim(taskID, caller.ID, deadline, modelID)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if _, err := s.store.ApplyPlan(*plan); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.store.TouchCitizen(caller.ID)

	// Return task with full details. The per-citizen
	// iteration_branch is surfaced via toTaskResponse's
	// IterationBranches map (read by the fat-client through
	// fetchTaskMeta), so no separate top-level field is
	// needed here.
	updatedTask, _ := s.store.GetTask(taskID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"task":     s.toTaskResponse(*updatedTask),
		"deadline": deadline.Format(time.RFC3339),
	})
}

// handleGetTaskInputs returns the structured dependency descriptor
// that client-side template resolvers (mcpgit.Project.Resolve)
// consume. The coordinator never reads files — it just reads DB rows
// and emits commit SHAs + paths. This replaces the legacy path that
// handleListIterations is the living-workflow phase 5 surface
// for the iteration projection. One row per task_claims row,
// in claim-order, with the seq counter computed and the
// claimant + commit_sha + review_decision joined in.
//
// Member-gated through the task's project. Citizens render as
// usernames; ModelID resolves to model citizen username when
// present.
func (s *Server) handleListIterations(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, run.ProjectID); !ok {
		return
	}
	iters, err := s.store.ListTaskIterations(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing iterations: "+err.Error())
		return
	}

	out := make([]map[string]interface{}, 0, len(iters))
	for _, it := range iters {
		row := map[string]interface{}{
			"seq":        it.Seq,
			"citizen":    it.Username,
			"claimed_at": it.ClaimedAt.UTC().Format(time.RFC3339),
		}
		// Outcome — render the active iteration explicitly so
		// the consumer doesn't have to interpret "" as "still
		// running."
		if it.Outcome == "" {
			row["outcome"] = "active"
		} else {
			row["outcome"] = it.Outcome
		}
		if it.SubmittedAt != nil {
			row["submitted_at"] = it.SubmittedAt.UTC().Format(time.RFC3339)
		}
		if it.CommitSHA != "" {
			row["commit_sha"] = it.CommitSHA
		}
		if it.Branch != "" {
			row["branch"] = it.Branch
		}
		if it.ReviewDecision != "" {
			row["review_decision"] = it.ReviewDecision
		}
		if it.Option != "" {
			row["option"] = it.Option
		}
		if it.Content != "" {
			row["content"] = it.Content
		}
		if it.ModelID != nil {
			if model := s.citizenUsername(*it.ModelID); model != "" {
				row["model"] = model
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// resolved templates server-side by reading upstream result files
// from a coordinator-owned working tree.
func (s *Server) handleGetTaskInputs(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, run.ProjectID); !ok {
		return
	}
	s.handleGetTaskInputsDescriptor(w, r, task, run)
}

// submitResultRequest is the iteration A shape: the client has
// already written the result + artifact files to its local clone,
// committed atomically, and pushed to the project's remote. The
// coordinator only receives metadata — no file content crosses the
// wire.
type submitResultRequest struct {
	// CommitSHA identifies the commit the client pushed to the
	// project's remote. Required.
	CommitSHA string `json:"commit_sha"`
	// ResultPath is the repo-relative directory holding this
	// task's result files. Must match the expected layout for the
	// task (runs/{seq}/{instance_key}/{task_def_id}) — the
	// coordinator validates this.
	ResultPath string `json:"result_path"`
	// ArtifactsWritten lists the user-facing artifact paths the
	// client wrote in the same commit. All share CommitSHA.
	ArtifactsWritten []string `json:"artifacts_written,omitempty"`

	TokensUsed int64  `json:"tokens_used,omitempty"`
	Model      string `json:"model,omitempty"`

	// Username identifies the submitting citizen. Required for
	// multi-citizen tasks so the server can credit the right
	// task_claims slot; optional for single-citizen tasks where
	// tasks.claimed_by is already the implicit claimer.
	Username string `json:"username,omitempty"`

	// Decision is the review verdict for action:review tasks. One
	// of "approve" / "reject". Ignored on non-review tasks. An empty
	// string on a review task is rejected up front — the reviewer
	// has to say something.
	Decision string `json:"decision,omitempty"`

	// Option is the chosen option id for action:vote tasks. Must
	// match one of the declared options' ids. Ignored on
	// non-vote tasks. Session 1 ships single-voter vote so one
	// submit resolves the task; session 2 multi-voter will tally
	// across N submissions.
	Option string `json:"option,omitempty"`

	// Content is the citizen's prose commentary for multi-
	// citizen tasks (vote/review). Stored on task_claims.content
	// so {{task.responses}} can render it without a git read.
	// The fat-client path also writes this to
	// citizen-<username>/result.md, but the DB column is the
	// authoritative source for responses rendering.
	Content string `json:"content,omitempty"`

	// OutputLists carries the *values* of named outputs that
	// are declared as format: list<string> on the submitting
	// task. Populated by the fat client at submit time so the
	// coordinator can use them for dynamic for_each
	// materialization (Phase J.1) without having to read git.
	// Keyed by output field name.
	//
	// Other named-output values (plain strings) stay in the
	// task's git-committed result files — only list<string>
	// outputs need to round-trip through the coordinator.
	OutputLists map[string][]string `json:"output_lists,omitempty"`
}

// handleSubmitResult is the metadata-only submit path. The client
// has already done the git work; the coordinator just validates the
// report, updates the state machine, updates the artifact index,
// runs the scheduler, and checks run completion.
//
// This is coordinator protocol, not a bot SDK — the wire format
// fat clients use to report a completed submission. There is no
// coordinator-side git worker (trust-the-client); bots calling
// this directly take on the fat client's git responsibilities.
//
// Per-action contract:
//
//   - compute / answer / contribute → commit_sha REQUIRED. The
//     400 fires below if missing. Submission lives in git
//     (metadata.json + result.md + writes_artifacts paths) and
//     the DB stores commit_sha + result_path as the pointer.
//
//   - vote / review → commit_sha OPTIONAL. The DB row
//     (task_claims.content + tasks.vote_choice / review_decision)
//     is the load-bearing record; the state machine, scheduler,
//     and tally engine all read from it. A direct-HTTP submit
//     without commit_sha is state-machine-correct but loses the
//     immutable git audit artifact (metadata.json with action +
//     option/decision + model + timestamp) that the MCP fat
//     client produces.
//
// See docs/coordinator.md § REST API § Tasks for the full
// per-action table and the two-tier (DB-mutable, git-immutable)
// rationale.
// handleTallyTask forces a tally evaluation on a collecting
// vote or review task. Any user can trigger it; it runs the
// same tally logic as a submission would, resolves if the
// threshold + quorum permit, and reports the outcome. Useful
// when a vote is stuck past its deadline or has enough
// submissions to short-circuit but nobody has submitted lately
// to re-trigger the evaluation.
func (s *Server) handleTallyTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("task %q not found", taskID))
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}
	if task.Action != "vote" && task.Action != "review" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("tally is only valid on action:vote or action:review tasks (got %q)", task.Action))
		return
	}
	if store.TaskState(task.State) == store.TaskAccepted {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "already_resolved",
			"task_id": taskID,
			"state":   task.State,
			"message": "task is already accepted — nothing to tally",
		})
		return
	}
	if store.TaskState(task.State) != store.TaskCollecting {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q is not collecting submissions yet (state: %s) — nothing to tally", taskID, engine.StateLabel(store.TaskState(task.State))))
		return
	}

	resp := map[string]interface{}{
		"task_id": taskID,
		"state":   task.State,
	}

	if task.Action == "vote" {
		outcome, err := s.engine().EvaluateVoteTally(task)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "tally failed: "+err.Error())
			return
		}
		resp["tally"] = map[string]interface{}{
			"resolved":    outcome.Resolved,
			"total_votes": outcome.TotalVotes,
			"counts":      outcome.Counts,
			"reason":      outcome.Reason,
		}
		if outcome.Resolved {
			// Build a Plan and apply atomically.
			plan := store.Plan{
				Version: engine.EngineVersion,
				Mutations: []store.Mutation{
					store.SetTaskState{
						TaskID:     taskID,
						NewState:   store.TaskAccepted,
						VoteChoice: outcome.WinningOption,
						CommitSHA:  task.CommitSHA,
					},
					store.UpdateReadyTasks{RunID: task.RunID},
					store.CompleteRun{RunID: task.RunID},
				},
			}
			result, err := s.store.ApplyPlan(plan)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "resolve failed: "+err.Error())
				return
			}
			// Skip cascade still uses the old path for now
			// (it's complex and touches the DAG cache).
			updated, _ := s.store.GetTask(taskID)
			if updated != nil {
				if skipRes, err := s.performSkipCascade(updated, outcome.WinningOption); err == nil && skipRes != nil {
					resp["skipped"] = skipRes.Skipped
				}
			}
			resp["status"] = "resolved"
			resp["winning_option"] = outcome.WinningOption
			resp["newly_ready"] = result.TasksReadied
		} else {
			resp["status"] = "collecting"
		}
	} else { // review
		outcome, err := s.engine().EvaluateReviewTally(task)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "tally failed: "+err.Error())
			return
		}
		resp["tally"] = map[string]interface{}{
			"resolved":      outcome.Resolved,
			"verdict":       outcome.Verdict,
			"approves":      outcome.Approves,
			"rejects":       outcome.Rejects,
			"total_reviews": outcome.TotalReviews,
			"reason":        outcome.Reason,
		}
		if outcome.Resolved {
			// Build a Plan and apply atomically.
			plan := store.Plan{
				Version: engine.EngineVersion,
				Mutations: []store.Mutation{
					store.SetTaskState{
						TaskID:   taskID,
						NewState: store.TaskAccepted,
					},
					store.UpdateReadyTasks{RunID: task.RunID},
					store.CompleteRun{RunID: task.RunID},
				},
			}
			_, err := s.store.ApplyPlan(plan)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "resolve failed: "+err.Error())
				return
			}
			// Review-reject cascade still uses the old path.
			if outcome.Verdict == "reject" && task.ReviewsTarget != "" {
				run, _ := s.store.GetRun(task.RunID)
				if run != nil {
					targetFullID := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq) + task.ReviewsTarget
					if _, err := s.performInvalidate(targetFullID); err != nil {
						s.logger.Warn("tally review-reject cascade failed",
							"review_task", taskID, "target", targetFullID, "error", err)
					}
				}
			}
			readied, _ := s.store.UpdateReadyTasks(task.RunID)
			resp["status"] = "resolved"
			resp["verdict"] = outcome.Verdict
			resp["newly_ready"] = readied
		} else {
			resp["status"] = "collecting"
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSubmitResult(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req submitResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Task existence check FIRST — otherwise a submit to a
	// deleted/wiped task falls through the commit_sha validator
	// and surfaces "commit_sha is required" as if that were the
	// root cause.
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("task %q not found", taskID))
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}

	// Claim validity check comes next — a submit from someone
	// who never claimed the task has no legitimate path, and
	// reporting "commit_sha is required" would hide the actual
	// "you don't own a slot on this task" error. For
	// single-citizen tasks the claimant is tasks.claimed_by; for
	// multi-citizen tasks it's any row in task_claims for the
	// submitting username.
	if task.Citizens > 1 {
		if req.Username == "" {
			writeError(w, http.StatusBadRequest, "username is required on multi-citizen task submissions")
			return
		}
		citizen, cerr := s.store.GetCitizenByUsername(req.Username)
		if cerr != nil || citizen == nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown citizen %q", req.Username))
			return
		}
		hasClaim, _ := s.store.HasActiveClaim(taskID, citizen.ID)
		if !hasClaim {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no open claim on task %q for user %q — claim it with enju_claim_task first", taskID, req.Username))
			return
		}
	} else if task.ClaimedBy == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q has no active claimant — claim it with enju_claim_task first", taskID))
		return
	}

	// commit_sha is required for action types that actually
	// ship prose/code/data to git (answer / contribute /
	// compute). Vote and review are decisions — the
	// authoritative storage for their submissions is the
	// task_claims row's content column, not a git file.
	// Making commit_sha optional for vote/review removes a
	// class of ordering bugs and matches the tools' real
	// contracts: "git is how tasks ship their work; votes and
	// reviews have nothing to ship."
	if req.CommitSHA == "" && task.Action != "vote" && task.Action != "review" {
		writeError(w, http.StatusBadRequest, "commit_sha is required — the coordinator no longer writes result files, clients must write + push + report")
		return
	}
	// Shape-level validation on commit_sha. Trust-the-client
	// architecture says we don't fetch the commit to verify it
	// exists on the remote (that's an optional future mode —
	// see ARCHITECTURE.md principle 7 + Open Question #4). But
	// even under trust-the-client, a commit_sha of the wrong
	// SHAPE is always a client bug — a buggy client sending
	// "not-a-sha" corrupts the artifact index for its own
	// project and makes downstream template resolution fail
	// mysteriously. Shape-check catches that cheaply. Accept
	// both SHA-1 (40 hex) and SHA-256 (64 hex) lengths so the
	// check doesn't block git's future hash transition.
	if req.CommitSHA != "" && !isValidCommitSHAShape(req.CommitSHA) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("commit_sha %q is not a valid git SHA (expected 40 or 64 hex characters)", req.CommitSHA))
		return
	}

	if task.Action == "vote" || task.Action == "review" {
		passed, derr := s.engine().DeadlinePassed(task)
		if derr == nil && passed {
			writeError(w, http.StatusConflict, fmt.Sprintf("task %q voting deadline has expired — submission rejected, run enju_tally_task to resolve", taskID))
			return
		}
	}

	s.handleSubmitResultReport(w, r, task, &req)
}

// reconcileEntry is one item in a POST /tasks/reconcile batch.
// Same semantics as submitResultRequest fields but flattened for
// batch transport. All fields are optional on the wire except
// TaskID + CommitSHA + ExitCode — the fetch-path scanner extracts
// them from commit trailers and forwards whatever it parsed.
type reconcileEntry struct {
	TaskID           string   `json:"task_id"`
	CommitSHA        string   `json:"commit_sha"`
	ExitCode         int      `json:"exit_code"`
	ResultPath       string   `json:"result_path,omitempty"`
	ArtifactsWritten []string `json:"artifacts_written,omitempty"`
	Content          string   `json:"content,omitempty"`
	FailReason       string   `json:"fail_reason,omitempty"` // optional override when ExitCode != 0
	Username         string   `json:"username,omitempty"`
	Model            string   `json:"model,omitempty"`
}

// reconcileBatchRequest is the top-level shape posted to
// /tasks/reconcile. Accepts either a batch or a single entry
// inline — scanners send batches, individual callers (wrapper's
// future async path) send one entry at a time.
type reconcileBatchRequest struct {
	Tasks []reconcileEntry `json:"tasks"`
}

// reconcileResult is the per-entry outcome returned in the
// response. Status is one of "accepted", "failed", "noop"
// (already terminal at this commit), or "error" — the scanner
// advances its cursor past entries regardless of status so a
// persistent error doesn't wedge the queue, but surfaces the
// error text so humans can diagnose.
type reconcileResult struct {
	TaskID    string `json:"task_id"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// handleReconcileTasks processes a batch of task-completion
// assertions from a fat-client fetch-path scanner. Each entry
// says "commit X landed on task Y's branch; these are the
// trailer-encoded results." The coordinator advances the task's
// state (accepted on exit 0, failed otherwise) and runs the
// downstream cascade — exactly as if the caller had POSTed to
// /tasks/{taskID}/result, but idempotent and batchable.
//
// Phase 2 scope: endpoint + idempotency + single-task delegation
// to the existing submit path. Phase 3 wires it into the fat
// client's fetch-path scanner. Phase 4 uses it as the completion
// signal for async compute tasks.
//
// Trust model: matches today's submit — the coordinator takes
// the client's word for the commit contents. The shape-check
// on commit_sha still fires (garbage-in-garbage-out gets
// rejected), and the task/project membership gate stays in
// place so a client can only reconcile tasks it's allowed to
// see.
func (s *Server) handleReconcileTasks(w http.ResponseWriter, r *http.Request) {
	var req reconcileBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Tasks) == 0 {
		writeError(w, http.StatusBadRequest, "tasks is required and must be non-empty")
		return
	}

	results := make([]reconcileResult, 0, len(req.Tasks))
	for _, entry := range req.Tasks {
		results = append(results, s.reconcileOne(r, entry))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

// reconcileOne handles a single reconcile entry. Returns the
// per-entry result; never writes to the ResponseWriter — every
// gate/error path surfaces via res.Error so the batch handler
// can aggregate and emit one writeJSON at the end. An earlier
// revision used the write-to-w requireProjectMembership*
// helper inside this loop, which would double-write the
// response on any membership failure (headers already sent +
// final writeJSON). Fixed by routing through
// checkProjectMembershipForTask, which returns an errMsg
// string instead. r is still needed to extract the
// citizen-from-request principal.
func (s *Server) reconcileOne(r *http.Request, entry reconcileEntry) reconcileResult {
	res := reconcileResult{TaskID: entry.TaskID, CommitSHA: entry.CommitSHA}

	if entry.TaskID == "" {
		res.Status = "error"
		res.Error = "task_id is required"
		return res
	}
	if entry.CommitSHA == "" {
		res.Status = "error"
		res.Error = "commit_sha is required"
		return res
	}
	if !isValidCommitSHAShape(entry.CommitSHA) {
		res.Status = "error"
		res.Error = fmt.Sprintf("commit_sha %q is not a valid git SHA (expected 40 or 64 hex characters)", entry.CommitSHA)
		return res
	}

	task, err := s.store.GetTask(entry.TaskID)
	if err != nil || task == nil {
		res.Status = "error"
		res.Error = fmt.Sprintf("task %q not found", entry.TaskID)
		return res
	}

	// Idempotency: a task that already reached a terminal state
	// at THIS commit is a no-op success. Different commit at
	// terminal state means the caller is trying to rewrite
	// history — return an error, not a silent overwrite.
	if task.State == "accepted" || task.State == "failed" {
		if task.CommitSHA == entry.CommitSHA {
			res.Status = "noop"
			return res
		}
		res.Status = "error"
		res.Error = fmt.Sprintf("task already %s at commit %s — cannot reconcile a different commit %s",
			task.State, shortCommit(task.CommitSHA), shortCommit(entry.CommitSHA))
		return res
	}
	// Reconcile only advances tasks from in-flight states
	// (claimed / running). Anything else — pending, ready,
	// skipped, invalidated, parked — is a stale trailer: e.g.
	// the scanner re-reads an old Enju-Task-Complete commit
	// from a task that has since been invalidated and is
	// waiting to be re-claimed. Advancing that would silently
	// resurrect the old completion and clobber any in-progress
	// re-run, so treat it as a no-op and move on.
	if task.State != "claimed" && task.State != "running" {
		res.Status = "noop"
		return res
	}

	// Route exit != 0 to the fail cascade — same path the sync
	// submit handler uses for compute-script failures. The fail
	// reason defaults to a synthetic "script exited with code N"
	// when the caller didn't supply one, matching the user-
	// facing string from the inline path for consistency.
	if entry.ExitCode != 0 {
		reason := entry.FailReason
		if reason == "" {
			reason = fmt.Sprintf("script exited with code %d", entry.ExitCode)
		}
		if _, err := s.engine().ComputeFailTask(entry.TaskID, reason); err != nil {
			res.Status = "error"
			res.Error = "fail precondition: " + err.Error()
			return res
		}
		if _, err := s.performFailCascade(entry.TaskID, reason); err != nil {
			res.Status = "error"
			res.Error = "fail cascade: " + err.Error()
			return res
		}
		res.Status = "failed"
		return res
	}

	// Exit 0 — delegate to the existing submit reporter.
	// Membership check via the write-free variant: a batch
	// handler must NOT let the per-entry gate touch the
	// ResponseWriter, or the final writeJSON on the batch
	// would be the second write to the same stream (headers
	// already committed). checkProjectMembershipForTask
	// returns (memb, errMsg) for us to surface inline on the
	// result entry instead.
	if _, errMsg := s.checkProjectMembershipForTask(r, entry.TaskID); errMsg != "" {
		res.Status = "error"
		res.Error = errMsg
		return res
	}
	req := &submitResultRequest{
		CommitSHA:        entry.CommitSHA,
		ResultPath:       entry.ResultPath,
		ArtifactsWritten: entry.ArtifactsWritten,
		Content:          entry.Content,
		Username:         entry.Username,
		Model:            entry.Model,
	}
	if err := s.reconcileAcceptTask(task, req); err != nil {
		res.Status = "error"
		res.Error = err.Error()
		return res
	}
	res.Status = "accepted"
	return res
}

// shortCommit formats a commit SHA for display in error
// messages (7 chars, git's standard abbreviation).
func shortCommit(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// reconcileAcceptTask runs the submit flow for a compute/answer
// task that reconciled successfully. Reimplements the meaningful
// slice of handleSubmitResultReport (engine validate → plan →
// apply → cascade) without the HTTP plumbing, so the batch
// handler can call it per-entry and get a plain `error` back.
// The caller has already verified membership and task state.
func (s *Server) reconcileAcceptTask(task *store.TaskRecord, req *submitResultRequest) error {
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		return fmt.Errorf("run not found for task %q", task.ID)
	}

	// Reconcile carries only the compute-submit subset — no
	// decisions, options, tokens, or output lists (async flows
	// are compute-only; reviews and votes go through the sync
	// endpoint). The shared core accepts a full SubmitRequest
	// so sync can pass its richer payload without branching.
	engineReq := &engine.SubmitRequest{
		TaskID:           task.ID,
		ResultPath:       req.ResultPath,
		CommitSHA:        req.CommitSHA,
		Username:         req.Username,
		Content:          req.Content,
		ArtifactsWritten: req.ArtifactsWritten,
	}
	_, err = s.acceptComputeTaskCore(task, run, engineReq, req.Model)
	if err != nil {
		return err
	}

	// Ready-task sweep + run completion. Without this, any
	// downstream whose only remaining blocker was this task
	// stays in PENDING forever — the claim gate rejects it
	// with "blocked". Mirrors step 7 of handleSubmitResultReport;
	// an earlier revision of reconcileAcceptTask omitted the
	// sweep, so async compute's downstream chain never unlocked.
	// Errors are logged-and-swallowed (the same pattern the
	// sync path uses) since a sweep failure mid-flight still
	// leaves the submission applied correctly.
	if _, err := s.store.UpdateReadyTasks(task.RunID); err != nil {
		s.logger.Warn("reconcile ready-sweep", "task_id", task.ID, "run_id", task.RunID, "error", err)
	}
	if _, err := s.store.CheckAndCompleteRun(task.RunID); err != nil {
		s.logger.Warn("reconcile run-complete check", "run_id", task.RunID, "error", err)
	}

	return nil
}

// acceptComputeTaskCoreResult carries the engine products both
// callers of acceptComputeTaskCore need to continue their work.
// Sync-submit uses them to drive review/vote/reject cascades,
// materialization, and its HTTP response shape; async-reconcile
// ignores most of them and just does the ready sweep +
// run-complete check.
type acceptComputeTaskCoreResult struct {
	Outcome     *engine.SubmissionOutcome
	Actions     *engine.PostSubmitActions
	ResultPath  string
	Decision    string
	VoteChoice  string
	SubmitterID int64
}

// acceptComputeTaskCore runs the "task completed, apply the
// consequences" dance that both sync submit-result and async
// reconcile paths need:
//
//  1. ValidateSubmitRequest (artifacts, paths, citizen).
//  2. ComputeSubmission → state-transition plan.
//  3. ApplyPlan(submit plan).
//  4. Record contribution events (best-effort, logged).
//  5. ComputePostSubmitActions (artifacts + tally + resolution).
//  6. ApplyPlan(artifact mutations).
//
// Each step mirrors one named phase in handleSubmitResultReport.
// Factoring them here closes the "reconcile forgot step X" bug
// class that already hit us once (ready-sweep omission, called
// out in reconcileAcceptTask's surrounding comment). New
// coordination steps added at this boundary affect both
// entry points automatically.
//
// Deliberately does NOT include:
//
//   - The ready-task sweep + run-complete check (callers run
//     those at different positions in their own flow — sync
//     after cascades, reconcile right after core — so leaving
//     them to the caller preserves current ordering byte-for-
//     byte).
//   - Review/vote/reject cascades, materialization, HTTP
//     response — those only apply to the sync path.
//
// Returns (&result, nil) on success. On engine validation or
// apply failure, returns an error with the submit state
// partially applied (the submit plan may have landed even if
// later steps fail — same semantics the old monolithic handler
// had). Caller logs or surfaces the error as HTTP.
func (s *Server) acceptComputeTaskCore(
	task *store.TaskRecord,
	run *store.RunRecord,
	req *engine.SubmitRequest,
	model string,
) (*acceptComputeTaskCoreResult, error) {
	eng := s.engine()

	resultPath, decision, voteChoice, submitterID, err := eng.ValidateSubmitRequest(task, run, req)
	if err != nil {
		return nil, err
	}
	// Resolve optional model attribution. The model
	// string already arrives via SubmitRequest.Model from the
	// fat-client submit path; here we resolve it to a model
	// citizen ID so it can ride the RecordSubmission mutation.
	// Empty model is fine for human operators (unaided); bots
	// without a model are rejected at apply time.
	modelID, err := s.resolveModelByUsername(model)
	if err != nil {
		return nil, err
	}
	submitOutcome, err := eng.ComputeSubmission(
		task.ID, submitterID, resultPath, req.CommitSHA,
		decision, voteChoice, req.Content, req.TokensUsed, modelID,
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.ApplyPlan(submitOutcome.Plan); err != nil {
		return nil, fmt.Errorf("applying submit plan: %w", err)
	}

	// Contribution events are append-only and best-effort:
	// a recording failure must not fail the submit (same
	// contract the old sync path promised). Model injection
	// is a per-submit client concern the engine doesn't know
	// about, so we stamp it here before writing.
	for i := range submitOutcome.Events {
		evt := &submitOutcome.Events[i]
		if evt.ProjectID == 0 {
			evt.ProjectID = run.ProjectID
		}
		if model != "" && evt.Metadata != "" {
			evt.Metadata = strings.TrimSuffix(evt.Metadata, "}") + fmt.Sprintf(`,"model":%q}`, model)
		}
		if err := s.store.RecordContributionEvent(evt); err != nil {
			s.logger.Warn("recording contribution event", "task_id", task.ID, "error", err)
		}
	}

	actions, err := eng.ComputePostSubmitActions(task, run, submitOutcome, req, decision, voteChoice)
	if err != nil {
		// Log but don't error — partial state is still
		// applied (events recorded, state transitioned), and
		// a missing post-submit-actions computation shouldn't
		// mask the accepted submission from the caller. Same
		// soft-fail pattern the pre-refactor sync path used
		// at this step.
		s.logger.Error("post-submit actions failed", "task_id", task.ID, "error", err)
	}

	if actions != nil && len(actions.ArtifactMutations) > 0 {
		if _, err := s.store.ApplyPlan(store.Plan{
			Version:   engine.EngineVersion,
			Mutations: actions.ArtifactMutations,
		}); err != nil {
			// Log-and-continue rather than hard-fail: the
			// submission has already been accepted at the
			// task level, and an artifact-index write
			// failure is recoverable (the next submit or
			// invalidation rebuilds the row). Matches the
			// pre-refactor sync behavior.
			s.logger.Error("upserting artifact index", "task_id", task.ID, "error", err)
		}
	}

	return &acceptComputeTaskCoreResult{
		Outcome:     submitOutcome,
		Actions:     actions,
		ResultPath:  resultPath,
		Decision:    decision,
		VoteChoice:  voteChoice,
		SubmitterID: submitterID,
	}, nil
}

// handleGetTaskInputsDescriptor is the client-writes claim-time
// endpoint (iteration A.2). It returns the structured dependency
// descriptor that the client-side template resolver in mcpgit
// consumes. No file reads happen on the coordinator side.
//
// Response shape mirrors mcpgit.ResolveInput:
//
//	{
//	  "task_id": "1:2:analyze",
//	  "prompt_template": "Analyze {{gather.content}} for {{gene}}",
//	  "user_prompt_template": "",
//	  "for_each_params": {"gene": "BRCA1"},
//	  "dependencies": [
//	    {"task_def_id": "gather", "instance_key": "",
//	     "instance_params": {}, "commit_sha": "abc...",
//	     "result_path": "runs/1/gather"}
//	  ],
//	  "artifact_reads": [
//	    {"path": "notes/intro.md", "commit_sha": "def..."}
//	  ]
//	}
func (s *Server) handleFailTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	var req struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}

	// Validate the target is failable via the engine (preserves
	// the state-precondition check) before running the cascade.
	if _, err := s.engine().ComputeFailTask(taskID, req.Reason); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Full reject-cascade: target→FAILED + artifact rollback +
	// descendants→SKIPPED + cross-run readers→PENDING. See
	// performFailCascade godoc for the rationale.
	res, err := s.performFailCascade(taskID, req.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	task, _ := s.store.GetTask(taskID)

	// Record contribution event.
	if task != nil && task.ClaimedBy > 0 {
		run, _ := s.store.GetRun(task.RunID)
		projectID := int64(0)
		if run != nil {
			projectID = run.ProjectID
		}
		s.store.RecordContributionEvent(&store.ContributionEvent{
			CitizenID: task.ClaimedBy,
			EventType: "task_failed",
			TaskID:    taskID,
			RunID:     task.RunID,
			ProjectID: projectID,
			Metadata:  fmt.Sprintf(`{"reason":%q}`, req.Reason),
			CreatedAt: time.Now(),
		})
	}

	resp := map[string]interface{}{
		"status":              "failed",
		"task_id":             taskID,
		"reason":              req.Reason,
		"skipped_descendants": res.SkippedDescendants,
		"rollbacks":           res.Rollbacks,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetTaskInputsDescriptor(w http.ResponseWriter, r *http.Request, task *store.TaskRecord, run *store.RunRecord) {
	desc, err := s.engine().BuildInputsDescriptor(task, run)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "building descriptor: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, desc)
}

// handleSubmitResultReport is the client-writes submit path
// (iteration A.2). The client has already written result files +
// artifacts and pushed them to the project's remote. We just update
// metadata: result_path, commit_sha, state machine, artifact index,
// scheduler re-evaluation, run completion. No git operations here.
func (s *Server) handleSubmitResultReport(w http.ResponseWriter, r *http.Request, task *store.TaskRecord, req *submitResultRequest) {
	taskID := task.ID

	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}

	// Steps 1–6 (validate → submit plan → events → post-actions
	// → artifact mutations) are shared with the async reconcile
	// path; see acceptComputeTaskCore. This function owns the
	// rest: review/vote/reject cascades, dynamic materialization,
	// tail ready-sweep, and HTTP response shape.
	engineReq := &engine.SubmitRequest{
		TaskID:           taskID,
		ResultPath:       req.ResultPath,
		CommitSHA:        req.CommitSHA,
		Decision:         req.Decision,
		Option:           req.Option,
		Username:         req.Username,
		Content:          req.Content,
		TokensUsed:       req.TokensUsed,
		ArtifactsWritten: req.ArtifactsWritten,
		OutputLists:      req.OutputLists,
	}
	core, err := s.acceptComputeTaskCore(task, run, engineReq, req.Model)
	if err != nil {
		// Validation errors land as 400; anything else (apply
		// plan failures) as 500. The core preserves the same
		// split the old monolithic handler enforced —
		// "applying submit plan" is the only error form that
		// wraps its underlying cause.
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "applying submit plan:") {
			status = http.StatusInternalServerError
		}
		writeError(w, status, err.Error())
		return
	}
	submitOutcome := core.Outcome
	actions := core.Actions
	resultPath := core.ResultPath
	decision := core.Decision
	voteChoice := core.VoteChoice
	submitterID := core.SubmitterID

	// 5. Apply review/vote resolution + fire cascades.
	var rejectResult *invalidationResult
	var skipResult *skipCascadeResult
	if actions != nil {
		if actions.ReviewResolvePlan != nil {
			s.store.ApplyPlan(*actions.ReviewResolvePlan)
		}
		if actions.ShouldRejectTarget && actions.RejectTargetID != "" {
			// Living-workflow phase 4b — if the target opted into
			// spawn_remediation on request_changes, skip the
			// invalidate cascade and spawn the remediation
			// instead. Default behavior (continue_iteration via
			// invalidate cascade) is preserved when the rule is
			// unset.
			if spawned, ok := s.maybeSpawnRemediation(taskID, actions.RejectTargetID, "request_changes", req.Decision, req.Content, submitterID); ok {
				rejectResult = spawned
			} else {
				res, err := s.performInvalidate(actions.RejectTargetID)
				if err != nil {
					s.logger.Error("review-request_changes cascade", "target", actions.RejectTargetID, "error", err)
				} else {
					rejectResult = res
				}
				// Phase 6b.2 / ISSUE-003: relabel the rejected
				// iteration's claim row from "completed" to
				// "rejected" so the audit trail reflects the
				// verdict, not just the bare fact that bytes
				// were submitted. Best-effort — a labeling
				// failure shouldn't undo the cascade.
				if _, err := s.store.MarkLatestCompletedClaimOutcome(actions.RejectTargetID, "rejected"); err != nil {
					s.logger.Warn("relabel claim outcome on request_changes",
						"target", actions.RejectTargetID, "error", err)
				}
			}
		}
		if actions.ShouldFailTarget && actions.RejectTargetID != "" {
			// Living-workflow phase 4b — if the target opted into
			// spawn_remediation on reject, skip the fail-cascade
			// and spawn the remediation instead. The author of
			// the YAML opted in to "reject = soft fork, not hard
			// kill." Default behavior (cascade-fail) preserved
			// when the rule is unset.
			if spawned, ok := s.maybeSpawnRemediation(taskID, actions.RejectTargetID, "reject", req.Decision, req.Content, submitterID); ok {
				rejectResult = spawned
			} else {
				// Validate fail-ability (engine precondition check),
				// then run the full cascade: target→FAILED + artifact
				// rollback + descendants→SKIPPED + cross-run readers→
				// PENDING. A raw ComputeFailTask here would leave the
				// artifact index pointing at the rejected commit and
				// leave DAG descendants stalled in PENDING forever —
				// both were the bugs that motivated this path.
				if _, err := s.engine().ComputeFailTask(actions.RejectTargetID, "rejected by reviewer"); err != nil {
					s.logger.Error("review-reject fail: compute", "target", actions.RejectTargetID, "error", err)
				} else if res, err := s.performFailCascade(actions.RejectTargetID, "rejected by reviewer"); err != nil {
					s.logger.Error("review-reject fail: cascade", "target", actions.RejectTargetID, "error", err)
				} else {
					rejectResult = &invalidationResult{
						Task:           res.Task,
						Descendants:    res.SkippedDescendants,
						Dematerialized: res.Dematerialized,
						Changed:        res.Changed,
						Rollbacks:      res.Rollbacks,
					}
				}
				// Phase 6b.2 / ISSUE-003: same relabel for the
				// terminal-reject path — the rejected iteration
				// should read "rejected" in the audit log, not
				// "completed."
				if _, err := s.store.MarkLatestCompletedClaimOutcome(actions.RejectTargetID, "rejected"); err != nil {
					s.logger.Warn("relabel claim outcome on review reject",
						"target", actions.RejectTargetID, "error", err)
				}
			}
		}
		if actions.VoteResolvePlan != nil {
			s.store.ApplyPlan(*actions.VoteResolvePlan)
		}
		if actions.ShouldSkipCascade {
			updated, _ := s.store.GetTask(taskID)
			if updated != nil {
				res, err := s.performSkipCascade(updated, actions.WinningOption)
				if err != nil {
					s.logger.Error("skip cascade", "error", err)
				} else {
					skipResult = res
				}
			}
		}
	}

	// 6. Dynamic materialization.
	if submitOutcome.Resolved && len(req.OutputLists) > 0 {
		if err := s.materializeDeferredTasks(task, run, req.OutputLists); err != nil {
			s.logger.Error("materializing deferred tasks", "task_id", taskID, "error", err)
		}
	}

	// 7. Ready-task sweep + run completion. No cross-run
	// fan-out — branch isolation means other runs' readiness
	// is unaffected by this submission.
	readied, _ := s.store.UpdateReadyTasks(task.RunID)
	completed, _ := s.store.CheckAndCompleteRun(task.RunID)

	// 7b. Living-workflow phase 4c — auto-triage hook.
	// CheckAndCompleteRun returns true only on the
	// active|idle → completed edge; we still need to fire the
	// idle hook on every transition that lands on idle. Cheap
	// post-evaluation: load the current state and fire if
	// idle. No-op when no auto_triage_template / no open
	// issues.
	if !completed {
		s.maybeAutoTriageIfIdle(task.RunID)
	}

	// 7c. Auto-close on accept (living-workflow phase 4c).
	// If this submitted task was spawned by auto-triage to fix
	// an issue, transition that issue to "closed" now that the
	// fix landed. CountTasks-by-prefix already protects against
	// duplicate auto-closes; CloseIssue refuses on already-
	// terminal issues.
	if task.ClosesIssueSeq > 0 && submitOutcome.Resolved {
		s.maybeAutoCloseIssue(task)
	}

	// 8. Build response.
	s.logger.Info("result reported", "task_id", taskID, "path", resultPath, "commit", req.CommitSHA, "newly_ready", readied)

	status := "accepted"
	reviewTally := actions.ReviewTally
	tallyOutcome := actions.VoteTally
	voteStillCollecting := tallyOutcome != nil && !tallyOutcome.Resolved
	reviewStillCollecting := reviewTally != nil && !reviewTally.Resolved
	if submitOutcome.Collecting && (voteStillCollecting || reviewStillCollecting || (tallyOutcome == nil && reviewTally == nil)) {
		status = "collecting"
	}
	resp := map[string]interface{}{
		"status":      status,
		"result_path": resultPath,
		"commit_sha":  req.CommitSHA,
		"newly_ready": readied,
	}
	// Contribution counter — "Contribution #N".
	if submitterID > 0 {
		contribCount, _ := s.store.CountContributionEvents(submitterID)
		projectsThisMonth, _ := s.store.CountProjectsThisMonth(submitterID)
		resp["contribution_number"] = contribCount
		resp["projects_this_month"] = projectsThisMonth
	}
	if decision != "" {
		resp["decision"] = decision
	}
	if rejectResult != nil {
		// Formatter keys off `decision` on the outer response
		// (request_changes vs reject → different phrasing),
		// so the cascade block itself doesn't need to carry
		// a verdict field.
		resp["review_cascade"] = map[string]interface{}{
			"target":          task.ReviewsTarget,
			"descendants":     rejectResult.Descendants,
			"changed":         rejectResult.Changed,
			"rollbacks_count": len(rejectResult.Rollbacks),
		}
	}
	if reviewTally != nil {
		resp["review_tally"] = map[string]interface{}{
			"resolved": reviewTally.Resolved, "verdict": reviewTally.Verdict,
			"approves": reviewTally.Approves, "rejects": reviewTally.Rejects,
			"total_reviews": reviewTally.TotalReviews, "reason": reviewTally.Reason,
		}
	}
	if task.Action == "vote" {
		voteResp := map[string]interface{}{}
		if tallyOutcome != nil && tallyOutcome.Resolved {
			voteResp["winning_option"] = tallyOutcome.WinningOption
			voteResp["votes_tallied"] = tallyOutcome.TotalVotes
			voteResp["counts"] = tallyOutcome.Counts
		} else if submitOutcome.Resolved && voteChoice != "" {
			voteResp["winning_option"] = voteChoice
		} else if tallyOutcome != nil {
			voteResp["collecting"] = true
			voteResp["votes_so_far"] = tallyOutcome.TotalVotes
			voteResp["counts"] = tallyOutcome.Counts
			voteResp["reason"] = tallyOutcome.Reason
		}
		if skipResult != nil {
			voteResp["skipped"] = skipResult.Skipped
			voteResp["skipped_count"] = len(skipResult.Skipped)
		}
		if len(voteResp) > 0 {
			resp["vote_resolution"] = voteResp
		}
	}
	if len(req.ArtifactsWritten) > 0 {
		resp["artifacts_written"] = req.ArtifactsWritten
	}
	if completed {
		resp["run_completed"] = true
	}

	// Living-workflow phase 6b foundational v1: surface
	// "tasks that just transitioned to ACCEPTED and need their
	// topic branch fast-forwarded onto the run branch." The
	// fat-client iterates these and pushes <topic-sha> :
	// refs/heads/<run-branch> for each one. Two cases:
	//
	//   - The submitter's own task auto-accepted (single-citizen
	//     answer/compute, no review).
	//   - A review's target transitioned to accepted because the
	//     reviewer approved.
	//
	// Vote tasks have no topic branch (generateIterationBranch
	// returns "" for vote/review actions), so they don't appear
	// here even when the vote resolves on this submit. Same for
	// reviews themselves — only the targets they approved are
	// candidates.
	if merges := s.collectAcceptedMerges(taskID, task, actions, run.Branch); len(merges) > 0 {
		resp["accepted_merges"] = merges
	}
	writeJSON(w, http.StatusOK, resp)
}

// acceptedMergeTarget identifies one (topic-branch, run-branch,
// commit-sha) tuple that the fat-client must FF-push to advance
// the run branch onto the accepted iteration's tip. Phase 6b.1
// + auto-merge wedge.
type acceptedMergeTarget struct {
	TaskID      string `json:"task_id"`
	TopicBranch string `json:"topic_branch"`
	RunBranch   string `json:"run_branch"`
	CommitSHA   string `json:"commit_sha"`
}

// collectAcceptedMerges builds the post-submit list of tasks
// whose accepted topic branch needs FF-merging onto the run
// branch. Caller surfaces these in the submit response so the
// fat-client can drive the merges (trust-the-client — the
// coordinator never touches git).
func (s *Server) collectAcceptedMerges(
	submittedTaskID string,
	submittedTask *store.TaskRecord,
	actions *engine.PostSubmitActions,
	runBranch string,
) []acceptedMergeTarget {
	if runBranch == "" {
		return nil
	}
	var out []acceptedMergeTarget

	// Case 1: the submitter's own task accepted on this submit
	// (single-citizen answer/compute, no review gate, or a
	// multi-citizen vote that resolved on this submit). Re-read
	// state — the submit plan we applied above may have
	// transitioned the task without us looking again.
	//
	// Three suppression rules, all variants of "this commit
	// doesn't yet belong on main":
	//
	//  (a) Review that did NOT approve. Per the design:
	//      "Reject → branch stays where it is, main untouched."
	//      A request_changes / reject review's topic stays as
	//      audit, never merged.
	//
	//  (b) Task with a downstream review (phase 6b.2 fix for
	//      ISSUE-001). The existing engine auto-accepts answer/
	//      compute tasks on submit even when a reviewer is
	//      pending; without this guard those topics merged to
	//      main BEFORE the reviewer ever saw them, polluting
	//      main with rejected work. The merge moment must be
	//      "the review approves," not "the task transitions to
	//      accepted." The review's own topic was forked from
	//      this task's topic and carries its content forward —
	//      so on approve, Case 1 fires for the review, and the
	//      single FF push lands both upstream content and
	//      verdict prose on main.
	//
	//  (c) Whatever future state-machine cases land here —
	//      keep this gate the only place merge eligibility is
	//      decided so nothing slips around it.
	skipMergeOfSelf := false
	if submittedTask != nil && submittedTask.Action == "review" &&
		actions != nil && (actions.ShouldRejectTarget || actions.ShouldFailTarget) {
		skipMergeOfSelf = true
	}
	if !skipMergeOfSelf && submittedTask != nil && s.taskHasDownstreamReview(submittedTask) {
		skipMergeOfSelf = true
	}
	if !skipMergeOfSelf {
		if cur, err := s.store.GetTask(submittedTaskID); err == nil && cur != nil &&
			store.TaskState(cur.State) == store.TaskAccepted {
			if t := s.acceptedMergeForTask(cur.ID, runBranch); t != nil {
				out = append(out, *t)
			}
		}
	}

	// Case 2: review approved the upstream. The review's own
	// topic branch was forked from the upstream's topic at
	// claim time (see UpstreamIterationBranch in the claim
	// response), so the review_topic SHA already carries the
	// upstream's commits. The case-1 merge above (which emits
	// the review's own topic) is sufficient — emitting the
	// upstream's topic separately would just attempt a second
	// FF whose target isn't a descendant of the now-advanced
	// run branch tip and would fail the FF check.
	//
	// The only situation where we DO need to emit an upstream
	// merge is if the review HAS no topic branch of its own
	// (legacy claim row, multi-citizen review where the
	// reviewer that pushed it over the threshold also has an
	// empty IterationBranch). In that case the review's
	// commit landed directly on the run branch and the
	// upstream's topic still needs to advance separately.
	if submittedTask != nil && submittedTask.Action == "review" && submittedTask.ReviewsTarget != "" &&
		actions != nil && !actions.ShouldRejectTarget && !actions.ShouldFailTarget {
		reviewHadTopic := false
		for _, m := range out {
			if m.TaskID == submittedTaskID {
				reviewHadTopic = true
				break
			}
		}
		if !reviewHadTopic {
			targetDef, targetInstance := parseReviewsTargetForMerge(submittedTask.ReviewsTarget)
			runTasks, _ := s.store.ListTasksByRun(submittedTask.RunID)
			for _, rt := range runTasks {
				if rt.TaskDefID != targetDef {
					continue
				}
				if rt.InstanceKey != targetInstance {
					continue
				}
				if store.TaskState(rt.State) != store.TaskAccepted {
					continue
				}
				if t := s.acceptedMergeForTask(rt.ID, runBranch); t != nil {
					out = append(out, *t)
				}
				break
			}
		}
	}

	return out
}

// taskHasDownstreamReview reports whether any task in the same
// run is an action:review that targets `t` (matched on
// (TaskDefID, InstanceKey)). Used by the merge gate to suppress
// auto-merge of a task whose verdict is still pending.
//
// Phase 6b.2 design contract: "merge fires on the review's
// approve, not on the upstream's submit." Without this gate,
// the engine's state-level auto-accept (answer/compute tasks
// transition to ACCEPTED on submit even when a reviewer is
// pending) would pollute main with content the reviewer hasn't
// seen — exactly the launch-blocking ISSUE-001 from the v1
// sanity report.
//
// O(log N) via the idx_tasks_reviews_target partial index —
// previously this was O(tasks-in-run) because we walked
// ListTasksByRun and string-matched per row, which gets
// expensive on for_each-heavy runs that fan out to dozens of
// instances. Multi-distinct-reviewer-per-target is rejected at
// parse time (yaml.validateNoDuplicateReviewTargets), so even
// the index lookup returns at most one row.
func (s *Server) taskHasDownstreamReview(t *store.TaskRecord) bool {
	if t == nil {
		return false
	}
	has, err := s.store.HasReviewerOfTarget(t.RunID, t.TaskDefID, t.InstanceKey)
	if err != nil {
		return false
	}
	return has
}

// acceptedMergeForTask renders one merge target for a task that
// is currently in state=accepted. Returns nil when the task
// has no topic branch (vote/review action) or no commit_sha
// on its latest completed claim (untracked-only outputs).
func (s *Server) acceptedMergeForTask(taskID, runBranch string) *acceptedMergeTarget {
	hist, err := s.store.ListTaskHistory(taskID)
	if err != nil {
		return nil
	}
	for i := len(hist) - 1; i >= 0; i-- {
		c := hist[i]
		if c.Outcome != "completed" {
			continue
		}
		if c.Branch == "" || c.CommitSHA == "" {
			return nil
		}
		return &acceptedMergeTarget{
			TaskID:      taskID,
			TopicBranch: c.Branch,
			RunBranch:   runBranch,
			CommitSHA:   c.CommitSHA,
		}
	}
	return nil
}

// parseReviewsTargetForMerge mirrors mcpserver.parseReviewsTarget
// for the merge-targets path: split a reviews_target value
// (either "defID" or "instanceKey:defID") into (defID,
// instanceKey). Kept local to avoid a server→mcpserver import
// cycle; the two implementations must stay in sync.
func parseReviewsTargetForMerge(target string) (string, string) {
	if idx := strings.Index(target, ":"); idx > 0 {
		return target[idx+1:], target[:idx]
	}
	return target, ""
}

func (s *Server) handleReleaseTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	caller := s.resolveCitizen(w, req.Username)
	if caller == nil {
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}

	if err := s.store.ReleaseTask(taskID, caller.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to release task")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

type invalidateRequest struct {
	Reason string `json:"reason"`
}

// getOrLoadDAG returns the in-memory DAG for a run, loading it from the
// run's stored YAML if it's not cached. This makes DAG-backed features
// like cascade invalidation restart-safe: the coordinator doesn't need
// to remember every DAG across process restarts.
func (s *Server) getOrLoadDAG(runID int64) (*dag.DAG, error) {
	if d, ok := s.dags[runID]; ok {
		return d, nil
	}
	if _, err := s.getOrLoadParsedRun(runID); err != nil {
		return nil, err
	}
	return s.dags[runID], nil
}

// getOrLoadParsedRun is getOrLoadDAG's richer sibling: it
// returns the full ParsedRun so callers that need the
// original YAML's task defs, warnings, or Phase J.1 deferred
// task metadata can reach them. Loads + caches lazily on
// first access.
func (s *Server) getOrLoadParsedRun(runID int64) (*enjuYaml.ParsedRun, error) {
	if p, ok := s.runs[runID]; ok {
		return p, nil
	}
	run, err := s.store.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("loading run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %d not found", runID)
	}
	parsed, err := enjuYaml.Parse([]byte(run.YAMLData))
	if err != nil {
		return nil, fmt.Errorf("parsing stored YAML for run %d: %w", runID, err)
	}
	s.dags[runID] = parsed.DAG
	s.runs[runID] = parsed
	return parsed, nil
}

// invalidationResult summarizes what performInvalidate actually
// changed on a single invocation. Used by the HTTP handler to
// render the response body and by the review-reject path in
// handleSubmitResultReport to log what happened.
type invalidationResult struct {
	Task        *store.TaskRecord
	Descendants []string
	// Dematerialized lists task IDs that were deleted
	// rather than flipped to PENDING. Populated for
	// invalidations of dynamic-for_each sources — the
	// materialized descendants can't preserve their
	// instance keys across a re-accept, so they're removed
	// entirely and re-created on the next accept. See
	// [docs/dynamic-outputs.md] for the rationale.
	Dematerialized []string
	Changed        int
	Rollbacks      []rollbackOutcome
}

type rollbackOutcome struct {
	Path              string
	Deleted           bool
	RestoredFromTask  string
	RestoredCommitSHA string
}

// performInvalidate is the shared cascade-invalidation implementation
// used by handleInvalidateTask (external API) and the review-reject
// path inside handleSubmitResultReport.
//
// The computation (DAG walk, artifact rollback decisions, dynamic
// descendant identification) is delegated to engine.ComputeInvalidation.
// This function applies the outcome's mutations to the store and
// manages the in-memory DAG cache.
func (s *Server) performInvalidate(taskID string) (*invalidationResult, error) {
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	d, err := s.getOrLoadDAG(task.RunID)
	if err != nil {
		return nil, fmt.Errorf("loading DAG for run %d: %w", task.RunID, err)
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("run not found for task %q", taskID)
	}
	parsed, _ := s.getOrLoadParsedRun(task.RunID)

	// Engine computes — reads state, never writes.
	outcome, err := s.engine().ComputeInvalidation(task, run, d, parsed)
	if err != nil {
		return nil, err
	}

	// Build a Plan from the engine's outcome.
	var mutations []store.Mutation

	// 1. Artifact rollbacks.
	var rollbacks []rollbackOutcome
	for _, rb := range outcome.ArtifactRollbacks {
		if rb.Delete {
			mutations = append(mutations, store.DeleteArtifact{
				ProjectID: rb.ProjectID,
				Branch:    rb.Branch,
				Path:      rb.Path,
			})
			rollbacks = append(rollbacks, rollbackOutcome{Path: rb.Path, Deleted: true})
		} else if rb.RestoreTo != nil {
			mutations = append(mutations, store.MoveArtifact{
				Artifact: *rb.RestoreTo,
			})
			rollbacks = append(rollbacks, rollbackOutcome{
				Path:              rb.Path,
				RestoredFromTask:  rb.RestoreTo.LastTaskID,
				RestoredCommitSHA: rb.RestoreTo.CommitSHA,
			})
		}
	}

	// 2. Target: ACCEPTED → READY with claim clear.
	mutations = append(mutations, store.SetTaskState{
		TaskID:     taskID,
		NewState:   store.TaskReady,
		ClearClaim: true,
	})

	// 3. Regular descendants → PENDING with claim clear.
	for _, descID := range outcome.RegularDescendants {
		mutations = append(mutations, store.SetTaskState{
			TaskID:     descID,
			NewState:   store.TaskPending,
			ClearClaim: true,
		})
	}

	// 4. Cross-run reader cascade was removed with the branch-
	// per-run model. Runs on distinct branches are isolated
	// by design, so an invalidation on branch X can't affect
	// readers on branch Y — the artifact index is keyed by
	// (project, branch, path) and the serial-per-branch
	// invariant means only one run is active on any branch.

	// 5. Dynamic descendants → PARK (J.2 partial re-mat
	//    Phase 1). Previously these were deleted outright,
	//    destroying any in-flight reviews / ballots / accepted
	//    work on a re-accept with a near-identical list. Parking
	//    preserves the row (state flips to 'parked', prior state
	//    stashed in parked_from_state) so the Phase 2
	//    reconciliation pass on re-accept can restore matched
	//    keys losslessly. Stale keys still get deleted at that
	//    point — but the judgment is deferred to when we have
	//    the new output list in hand.
	//
	//    Fail-cascade keeps deleting (see performFailCascade
	//    below, D5 in PARTIAL_REMAT_PLAN.md): a terminally
	//    failed source will never re-accept, so its parked
	//    descendants would orphan forever.
	for _, descID := range outcome.DematerializedIDs {
		dt, err := s.store.GetTask(descID)
		if err != nil || dt == nil {
			// Row vanished between the engine's read and this
			// write — nothing to park. Unlikely under the
			// project lock but handled defensively.
			continue
		}
		mutations = append(mutations, store.SetTaskState{
			TaskID:          descID,
			NewState:        store.TaskParked,
			ParkedFromState: store.TaskState(dt.State),
		})
	}

	plan := store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	}
	result, err := s.store.ApplyPlan(plan)
	if err != nil {
		return nil, err
	}

	// No DAG cache wipe: parked rows keep their nodes + edges
	// intact so reconciliation can diff the current DAG
	// against the incoming output list without rebuilding.
	// (Previous deletion path wiped the cache because nodes
	// disappeared.)

	// Ready-task sweep + run state re-evaluation for the
	// target's own run. No cross-run fan-out any more — branch
	// isolation means other runs are unaffected by this
	// invalidation. EvaluateRunState lands on active / idle /
	// completed based on the current task counts; previously we
	// just force-flipped to active, which is wrong now that idle
	// is observable (a run with only pending work after
	// invalidation is genuinely idle, not active).
	_, _ = s.store.UpdateReadyTasks(task.RunID)
	s.evaluateRunStateAndMaybeTriage(task.RunID)

	changed := result.Changed + result.TasksDeleted

	return &invalidationResult{
		Task:           task,
		Descendants:    outcome.RegularDescendants,
		Dematerialized: outcome.DematerializedIDs,
		Changed:        changed,
		Rollbacks:      rollbacks,
	}, nil
}

// failCascadeResult summarizes the outcome of performFailCascade
// for logging and response rendering. Mirrors invalidationResult
// but with terminal semantics — the target is FAILED (not back to
// READY) and intra-run descendants are SKIPPED (not PENDING).
type failCascadeResult struct {
	Task               *store.TaskRecord
	Reason             string
	SkippedDescendants []string
	Dematerialized     []string
	Changed            int
	Rollbacks          []rollbackOutcome
}

// performFailCascade is the reject/fail analogue of performInvalidate.
// It's used when a writer task terminates unsuccessfully (review
// `reject` verdict, enju_fail_task, compute script error) and any
// downstream consumers or artifact readers must be told the data
// they were going to consume is not coming.
//
// Semantic contract (see docs/rollback.md § Rejection vs invalidation):
//
//  1. Target → FAILED with the supplied reason. Terminal — unlike
//     request_changes which bounces back to READY.
//  2. Intra-run DAG descendants of the target → SKIPPED with
//     skip_reason = "upstream failed: <targetID>". Terminal, and
//     carries the reason so run_status can render ⊘ "(upstream
//     failed: X)" distinctly from vote-cascade skips.
//  3. Artifact rollback — identical to invalidation. The target's
//     writes roll back to the prior accepted writer (or delete if
//     none). Otherwise the artifact index would silently keep
//     pointing at a rejected commit, which downstream readers would
//     then consume.
//  4. Cross-run artifact readers that were ACCEPTED against the
//     rolled-back path → PENDING (not SKIPPED). They weren't DAG
//     descendants of the rejected task; they're independent runs
//     whose basis moved. Put them back on the queue to re-run with
//     the restored content.
//  5. Dynamic-for_each descendants of the target → DELETE (same as
//     invalidation — their instance keys are tied to the rejected
//     output list and can't survive).
//
// The computation (DAG walk + rollback decisions) reuses
// engine.ComputeInvalidation; only the Plan that gets applied
// differs (terminal states vs. reset-to-retry).
func (s *Server) performFailCascade(taskID, reason string) (*failCascadeResult, error) {
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	d, err := s.getOrLoadDAG(task.RunID)
	if err != nil {
		return nil, fmt.Errorf("loading DAG for run %d: %w", task.RunID, err)
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("run not found for task %q", taskID)
	}
	parsed, _ := s.getOrLoadParsedRun(task.RunID)

	outcome, err := s.engine().ComputeInvalidation(task, run, d, parsed)
	if err != nil {
		return nil, err
	}

	var mutations []store.Mutation

	// 1. Artifact rollbacks (same as invalidation — the
	//    artifact index shouldn't point at rejected content).
	var rollbacks []rollbackOutcome
	for _, rb := range outcome.ArtifactRollbacks {
		if rb.Delete {
			mutations = append(mutations, store.DeleteArtifact{
				ProjectID: rb.ProjectID,
				Branch:    rb.Branch,
				Path:      rb.Path,
			})
			rollbacks = append(rollbacks, rollbackOutcome{Path: rb.Path, Deleted: true})
		} else if rb.RestoreTo != nil {
			mutations = append(mutations, store.MoveArtifact{
				Artifact: *rb.RestoreTo,
			})
			rollbacks = append(rollbacks, rollbackOutcome{
				Path:              rb.Path,
				RestoredFromTask:  rb.RestoreTo.LastTaskID,
				RestoredCommitSHA: rb.RestoreTo.CommitSHA,
			})
		}
	}

	// 2. Target → FAILED (terminal). Preserve commit_sha and
	//    claim info for audit; applySetTaskState without
	//    ClearClaim writes the fail_reason as-is.
	mutations = append(mutations, store.SetTaskState{
		TaskID:     taskID,
		NewState:   store.TaskFailed,
		FailReason: reason,
	})

	// 3. Intra-run descendants → SKIPPED with reason. ClearClaim
	//    so any in-flight claims are invalidated and per-claim
	//    state wiped — a skipped task should not look half-claimed.
	//
	//    Filter to non-terminal descendants only. Terminal states
	//    stay terminal:
	//      - ACCEPTED review/vote that *caused* this failure
	//        already did its job; overwriting it as "skipped
	//        because upstream failed" is semantically backwards.
	//      - Already-FAILED/SKIPPED descendants are no-ops; no
	//        need to re-flip them and lose their original reason.
	skipReason := fmt.Sprintf("upstream failed: %s", taskID)
	var skippedDescendants []string
	for _, descID := range outcome.RegularDescendants {
		dt, err := s.store.GetTask(descID)
		if err != nil || dt == nil {
			continue
		}
		if isTerminalTaskState(dt.State) {
			continue
		}
		mutations = append(mutations, store.SetTaskState{
			TaskID:     descID,
			NewState:   store.TaskSkipped,
			ClearClaim: true,
			SkipReason: skipReason,
		})
		skippedDescendants = append(skippedDescendants, descID)
	}

	// 4. Cross-run readers → PENDING. They're in other runs, not
	//    Cross-run reader cascade was removed with the branch-
	//    per-run model — see performInvalidate comment for the
	//    rationale. A branch owns its artifact history; other
	//    branches are unaffected.

	// 5. Dematerialized dynamic descendants → delete.
	for _, descID := range outcome.DematerializedIDs {
		mutations = append(mutations, store.DeleteTask{TaskID: descID})
	}

	plan := store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	}
	result, err := s.store.ApplyPlan(plan)
	if err != nil {
		return nil, err
	}

	if len(outcome.DematerializedDefs) > 0 {
		delete(s.dags, task.RunID)
		delete(s.runs, task.RunID)
	}

	// Ready-task sweep + state re-evaluation. See the matching
	// comment in performInvalidate for the rationale.
	_, _ = s.store.UpdateReadyTasks(task.RunID)
	s.evaluateRunStateAndMaybeTriage(task.RunID)

	return &failCascadeResult{
		Task:               task,
		Reason:             reason,
		SkippedDescendants: skippedDescendants,
		Dematerialized:     outcome.DematerializedIDs,
		Changed:            result.Changed + result.TasksDeleted,
		Rollbacks:          rollbacks,
	}, nil
}

// isTerminalTaskState reports whether a task state is terminal
// (work is done, one way or another) and should not be rewritten
// by a cascade. Used by performFailCascade to avoid overwriting
// the ACCEPTED review that caused the failure, or descendants
// that already landed in their own terminal state.
func isTerminalTaskState(s store.TaskState) bool {
	switch s {
	case store.TaskAccepted, store.TaskFailed, store.TaskSkipped:
		return true
	}
	return false
}


// skipCascadeResult summarizes the outcome of performSkipCascade
// for logging and response rendering.
type skipCascadeResult struct {
	WinningOption string
	// Skipped is the list of full task ids that transitioned to
	// SKIPPED as a result of this vote's resolution.
	Skipped []string
}

// performSkipCascade applies Phase E.2's gate routing after a vote
// task has been accepted. The rule:
//
//	winning_set = winning_activates ∪ descendants(winning_activates)
//	losing_set  = ⋃ losing_activates ∪ descendants(losing_activates)
//	skip_set    = losing_set − winning_set
//
// Tasks in skip_set transition to SKIPPED. Tasks in winning_set
// stay alive. Tasks in neither set are unrelated to any branch
// materializeDeferredTasks is the Phase J.1 entry point for
// dynamic for_each fan-out. When a task with list<string>
// outputs accepts and the run has deferred downstream tasks
// whose for_each lists reference those outputs, this creates
// the concrete task rows for every resolved instance.
//
// The algorithm:
//
//  1. Load the ParsedRun (cached from run creation or
//     lazily re-parsed from stored YAML).
//  2. For each DeferredTaskDef whose for_each refs point at
//     this accepting task, resolve the list values from
//     req.OutputLists and run for_each expansion.
//  3. Insert one task row per expanded instance via
//     store.CreateTask, with depends_on computed against
//     the instance key (matching siblings for per-instance
//     chaining).
//  4. For transitively-deferred tasks (singletons that
//     consume the dynamic upstream via fan-in), materialize
//     them with depends_on listing every newly-inserted
//     instance ID for the upstream.
//  5. Add nodes + edges to the cached in-memory DAG so
//     cascade-invalidation walks see the new rows.
//
// Non-atomic: runs after the upstream's SubmitTaskResult
// transaction has committed. A failure here leaves the
// upstream accepted and the deferred downstream un-
// materialized — recoverable by invalidate + re-accept. For
// first slice this is acceptable; a future iteration can
// move it inside the submit transaction.
func (s *Server) materializeDeferredTasks(task *store.TaskRecord, run *store.RunRecord, outputLists map[string][]string) error {
	parsed, err := s.getOrLoadParsedRun(task.RunID)
	if err != nil {
		return fmt.Errorf("loading parsed run: %w", err)
	}

	// Engine computes the materialization plan.
	outcome, err := s.engine().ComputeMaterialization(parsed, task, run, outputLists)
	if err != nil {
		return err
	}
	if outcome == nil {
		return nil
	}

	// Phase 2 reconciliation apply pass. All four buckets go
	// through a single ApplyPlan transaction so a failure
	// midway rolls back cleanly — we never want a run stuck
	// with half-restored / half-deleted rows.
	//
	// Ordering within the plan:
	//   1. Restore (unpark to stashed state) — safe to apply
	//      before deletes because restored rows have matching
	//      keys that won't collide with anything else.
	//   2. Singleton re-opens (state → PENDING, new deps).
	//   3. Delete stale subtrees.
	//   4. Create new-only instances.
	var muts []store.Mutation

	for _, r := range outcome.TasksToRestore {
		muts = append(muts, store.SetTaskState{
			TaskID:   r.TaskID,
			NewState: r.ToState,
		})
	}
	for _, so := range outcome.SingletonReopens {
		// Re-open carries an update to depends_on too. Both
		// ride the same SetTaskState mutation via the
		// NewDependsOn field so state flip + edge-set rewrite
		// land in one transaction — a mid-crash can't leave
		// the singleton at PENDING with stale parents.
		newDeps := strings.Split(so.NewDependsOn, ",")
		muts = append(muts, store.SetTaskState{
			TaskID:       so.TaskID,
			NewState:     store.TaskPending,
			ClearClaim:   true,
			NewDependsOn: &newDeps,
		})
	}
	for _, delID := range outcome.TasksToDelete {
		muts = append(muts, store.DeleteTask{TaskID: delID})
	}
	for i := range outcome.TasksToCreate {
		muts = append(muts, store.CreateTask{Task: outcome.TasksToCreate[i]})
	}

	if len(muts) > 0 {
		if _, err := s.store.ApplyPlan(store.Plan{
			Version:   engine.EngineVersion,
			Mutations: muts,
		}); err != nil {
			return fmt.Errorf("applying materialization plan: %w", err)
		}
	}

	// DAG cache management. Any deletion or singleton re-open
	// invalidates edges the cache knows about; easier to wipe
	// and let the next access rebuild than to surgically
	// remove nodes/edges. Matches the performInvalidate
	// pattern.
	if len(outcome.TasksToDelete) > 0 || len(outcome.SingletonReopens) > 0 {
		delete(s.dags, task.RunID)
		delete(s.runs, task.RunID)
	} else {
		// No structural churn — safe to incrementally add the
		// new nodes/edges to the cached DAG (the fast path
		// for first-time materialization).
		for _, node := range outcome.DAGNodes {
			if err := parsed.DAG.AddNode(node.ShortID, node.Action, node.Data); err != nil {
				s.logger.Warn("DAG AddNode", "id", node.ShortID, "err", err)
			}
		}
		for _, edge := range outcome.DAGEdges {
			if err := parsed.DAG.AddEdge(edge.From, edge.To); err != nil {
				s.logger.Warn("DAG AddEdge", "from", edge.From, "to", edge.To, "err", err)
			}
		}
	}
	return nil
}
// and are untouched. Merge points reachable from both sides stay
// alive because the winning path still reaches them.
//
// Called from handleSubmitResultReport after the DB state flip to
// ACCEPTED has landed. Returns nil with no error when the vote
// has no activates (pure-decision votes); callers can still use
// the decision as data but no routing happens.
func (s *Server) performSkipCascade(task *store.TaskRecord, winningOptionID string) (*skipCascadeResult, error) {
	// Decode the declared options.
	var declared []struct {
		ID        string   `json:"id"`
		Label     string   `json:"label,omitempty"`
		Activates []string `json:"activates,omitempty"`
	}
	if task.VoteOptions == "" {
		return nil, fmt.Errorf("task %q has no vote_options", task.ID)
	}
	if err := json.Unmarshal([]byte(task.VoteOptions), &declared); err != nil {
		return nil, fmt.Errorf("decoding vote_options: %w", err)
	}

	// Short-circuit: if no option has activates, there's no
	// skip cascade to run — pure decision vote.
	anyActivates := false
	for _, o := range declared {
		if len(o.Activates) > 0 {
			anyActivates = true
			break
		}
	}
	if !anyActivates {
		return &skipCascadeResult{WinningOption: winningOptionID}, nil
	}

	// Load the DAG so we can walk descendants of activates roots.
	d, err := s.getOrLoadDAG(task.RunID)
	if err != nil {
		return nil, fmt.Errorf("loading DAG: %w", err)
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("loading run: %w", err)
	}
	runPrefix := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq)

	// Build the winning set and the losing set. Both are
	// expressed as full task ids ({projectID:runSeq:...}) since
	// that's what the store uses.
	//
	// Iteration qualification: for a vote in iteration K
	// (task.InstanceKey == "K"), each activates short id
	// `"human_adjudicate"` refers to the same-iteration
	// counterpart `"K:human_adjudicate"` — NOT the bare
	// `"human_adjudicate"` DAG node (which doesn't exist in
	// fanned-out templates, so the cascade used to silently
	// no-op). Try the iteration-qualified form first; fall
	// back to the bare form when the qualified node isn't in
	// the DAG, which handles the rarer case of an iteration-
	// level vote activating a singleton downstream.
	resolveActivatesNode := func(shortID string) string {
		if task.InstanceKey == "" {
			return shortID
		}
		qualified := enjuYaml.MakeFullID(task.InstanceKey, shortID)
		if _, ok := d.GetNode(qualified); ok {
			return qualified
		}
		// Fall back to bare — the iteration-level vote is
		// naming a singleton (run-wide) task. The inScope
		// filter below treats the bare node's empty
		// instance_key as out-of-scope for an iteration
		// vote, so the singleton is excluded from BOTH the
		// winning and losing sets: a single iteration's
		// decision doesn't unilaterally skip a run-wide
		// resource. Other iterations' votes reach the same
		// singleton via the same fan-in edges, and the
		// ready-task sweep promotes it once every iteration
		// is terminal — exact same partial-tolerance
		// semantics as cross-iteration aggregators.
		//
		// Kept live (not removed) because for a SINGLETON
		// vote (the early-return above), this branch isn't
		// reached and the bare form is still the correct
		// lookup. The test
		// TestMCPVoteActivatesSingletonActivateeSurvives
		// locks in the fanned-vote → singleton shape.
		return shortID
	}

	// inScope restricts the activates-reachability walk to
	// same-iteration descendants of a fanned-out vote. A
	// cross-iteration aggregator below the activated tasks
	// (singleton consumer of the fanned-out leaf) is
	// reachable from the current iteration's losing branch
	// but ALSO from every other iteration's winning branch —
	// pulling it into the single-iteration losing_set would
	// skip the aggregator even when another iteration keeps
	// it reachable. Same iteration-scope filter as the
	// fail-cascade path: the walk stays inside the vote's
	// iteration, cross-iteration merge points are left alone
	// and promote via UpdateReadyTasks once all cohort
	// instances are terminal.
	//
	// Singleton votes (InstanceKey == "") fan unrestricted —
	// their scope is run-wide, and there's no other iteration
	// to partition against.
	inScope := func(descShortID string) bool {
		if task.InstanceKey == "" {
			return true
		}
		n, ok := d.GetNode(descShortID)
		if !ok {
			return false
		}
		descKey, _ := n.Data["instance_key"].(string)
		return descKey == task.InstanceKey
	}

	winningSet := make(map[string]bool)
	losingSet := make(map[string]bool)
	for _, o := range declared {
		target := winningSet
		if o.ID != winningOptionID {
			target = losingSet
		}
		for _, shortID := range o.Activates {
			nodeID := resolveActivatesNode(shortID)
			if !inScope(nodeID) {
				continue
			}
			target[runPrefix+nodeID] = true
			for _, desc := range d.Descendants(nodeID) {
				if !inScope(desc) {
					continue
				}
				target[runPrefix+desc] = true
			}
		}
	}

	// skip_set = losing_set − winning_set
	skipIDs := make([]string, 0, len(losingSet))
	for id := range losingSet {
		if winningSet[id] {
			continue
		}
		skipIDs = append(skipIDs, id)
	}
	if len(skipIDs) == 0 {
		return &skipCascadeResult{WinningOption: winningOptionID}, nil
	}

	// Flip them to SKIPPED via ApplyPlan.
	var skipMuts []store.Mutation
	for _, id := range skipIDs {
		skipMuts = append(skipMuts, store.SetTaskState{
			TaskID:   id,
			NewState: store.TaskSkipped,
		})
	}
	if _, err := s.store.ApplyPlan(store.Plan{
		Version:   engine.EngineVersion,
		Mutations: skipMuts,
	}); err != nil {
		return nil, fmt.Errorf("marking tasks skipped: %w", err)
	}
	return &skipCascadeResult{
		WinningOption: winningOptionID,
		Skipped:       skipIDs,
	}, nil
}

func (s *Server) handleInvalidateTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req invalidateRequest
	json.NewDecoder(r.Body).Decode(&req)

	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}

	result, err := s.performInvalidate(taskID)
	if err != nil {
		// Not-found vs bad-state vs internal are indistinguishable
		// from the helper's single error return. Use 400 as the
		// generic "can't invalidate this" code; the message carries
		// the specific reason.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.logger.Info("task invalidated",
		"task_id", taskID,
		"descendants", len(result.Descendants),
		"changed", result.Changed,
		"artifacts_rolled_back", len(result.Rollbacks),
		"reason", req.Reason,
	)

	// Record the invalidation in the contribution event log
	// so the audit timeline (enju_export_run_events) shows
	// the moment the cascade fired. Best-effort — failure to
	// record shouldn't un-invalidate the task.
	if result.Task != nil {
		metaJSON := fmt.Sprintf(`{"reason":%q,"descendants":%d,"rollbacks":%d}`,
			req.Reason, len(result.Descendants), len(result.Rollbacks))
		s.store.RecordContributionEvent(&store.ContributionEvent{
			EventType: "task_invalidated",
			TaskID:    taskID,
			RunID:     result.Task.RunID,
			ProjectID: func() int64 {
				if run, _ := s.store.GetRun(result.Task.RunID); run != nil {
					return run.ProjectID
				}
				return 0
			}(),
			Metadata:  metaJSON,
			CreatedAt: time.Now(),
		})
	}

	resp := map[string]interface{}{
		"status":      "invalidated",
		"task_id":     taskID,
		"descendants": result.Descendants,
		"changed":     result.Changed,
		"reason":      req.Reason,
	}
	if len(result.Dematerialized) > 0 {
		// J.2 renamed the semantic from "deleted" to
		// "parked": the rows stay, waiting for a matching
		// re-accept to restore or a non-matching one to
		// delete. Single `parked` key on the wire.
		resp["parked"] = result.Dematerialized
	}
	if len(result.Rollbacks) > 0 {
		rbView := make([]map[string]interface{}, 0, len(result.Rollbacks))
		for _, rb := range result.Rollbacks {
			item := map[string]interface{}{
				"path": rb.Path,
			}
			if rb.Deleted {
				item["deleted"] = true
			} else {
				item["restored_from_task"] = rb.RestoredFromTask
				item["restored_from_commit"] = rb.RestoredCommitSHA
			}
			rbView = append(rbView, item)
		}
		resp["artifacts_rolled_back"] = rbView
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Citizens ---

type registerRequest struct {
	Name     string `json:"name"`
	Username string `json:"username,omitempty"` // optional, auto-generated from name if omitted
	Email    string `json:"email,omitempty"`
}

// generateUniqueUsername picks an unused username based on the display
// name. It slugifies the name, falls back to "user" if the slug is
// empty, and appends -2, -3, etc. on collision.
func (s *Server) generateUniqueUsername(displayName string) string {
	base := store.SlugifyName(displayName)
	if base == "" {
		base = "user"
	}
	candidate := base
	for i := 2; ; i++ {
		c, _ := s.store.GetCitizenByUsername(candidate)
		if c == nil {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
		// Safety valve — after way too many collisions, fall back to a
		// random suffix. Shouldn't ever happen in practice.
		if i > 1000 {
			return fmt.Sprintf("%s-%s", base, uuid.New().String()[:6])
		}
	}
}

func (s *Server) handleRegisterCitizen(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Validate or generate username. An explicit username is required
	// to match the GitHub rules; an auto-generated one comes from
	// slugifying the display name and is unique by construction.
	username := req.Username
	if username != "" {
		if err := store.ValidateUsername(username); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		username = s.generateUniqueUsername(req.Name)
	}

	token := uuid.New().String()
	now := time.Now()

	id, err := s.store.CreateCitizen(&store.CitizenRecord{
		Username:     username,
		Name:         req.Name,
		Email:        req.Email,
		Role:         "citizen",
		Token:        token,
		RegisteredAt: now,
		LastSeen:     now,
	})
	if err != nil {
		if strings.Contains(err.Error(), "email already exists") ||
			strings.Contains(err.Error(), "already taken") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to register: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":       id,
		"username": username,
		"name":     req.Name,
		"email":    req.Email,
		"role":     "citizen",
		"token":    token,
	})
}

// handleGetCitizenByUsername is the user-facing citizen lookup.
func (s *Server) handleGetCitizenByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")

	citizen, err := s.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}
	writeJSON(w, http.StatusOK, citizenToMap(citizen))
}

// --- operator/model design: bot + model registration ---
//
// Five handlers covering bot lifecycle (register, list, revoke
// token) and model catalog (list, register). All require Bearer
// auth — bots inherit the same auth surface as humans, models
// require an authenticated caller in local mode (hosted-mode
// gating deferred per the design doc).

type registerBotRequest struct {
	Name     string `json:"name"`               // display name, required
	Username string `json:"username,omitempty"` // optional — auto-slugified
	Role     string `json:"role,omitempty"`     // optional — defaults to 'citizen'
	Label    string `json:"label,omitempty"`    // optional initial-token label
}

// handleRegisterBot creates a new kind='bot' citizen owned by the
// authenticated caller, plus an initial token returned ONCE in the
// response. The caller is responsible for stashing the token where
// the bot will run from (CI env var, daemon config, etc.) — there
// is no recovery path. To rotate, issue a new token via
// (future) tools and revoke the old one.
func (s *Server) handleRegisterBot(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req registerBotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	username := req.Username
	if username != "" {
		if err := store.ValidateUsername(username); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		username = s.generateUniqueUsername(req.Name)
	}
	role := req.Role
	if role == "" {
		role = "citizen"
	}
	token := uuid.New().String()
	now := time.Now()

	id, err := s.store.CreateCitizen(&store.CitizenRecord{
		Username:     username,
		Name:         req.Name,
		Role:         role,
		Token:        token,
		RegisteredAt: now,
		LastSeen:     now,
		Kind:         "bot",
		ParentID:     &caller.ID,
	})
	if err != nil {
		if strings.Contains(err.Error(), "already taken") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "register bot: "+err.Error())
		return
	}
	// CreateCitizen already inserted the initial token under
	// label=''. If the caller named a label, retag it via a
	// fresh issue + revoke of the auto-issued one. Cheaper than
	// adding a label parameter to CreateCitizen for the one
	// caller that needs it.
	if req.Label != "" {
		// Look up the auto-issued token and update its label
		// in place — no re-issue, just metadata.
		_, _ = s.store.DB().Exec(`UPDATE tokens SET label = ? WHERE citizen_id = ? AND token = ?`, req.Label, id, token)
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":          id,
		"username":    username,
		"name":        req.Name,
		"kind":        "bot",
		"parent_id":   caller.ID,
		"parent_name": caller.Username,
		"token":       token,
		"label":       req.Label,
		"warning":     "Stash this token now — it cannot be retrieved later. Revoke + re-issue if lost.",
	})
}

// handleListMyBots returns every bot the authenticated caller owns
// (parent_id = caller.id), with each bot's active token labels but
// NOT the token values (those were shown once at registration).
func (s *Server) handleListMyBots(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	bots, err := s.store.ListBotsByParent(caller.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list bots: "+err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(bots))
	for _, b := range bots {
		tokens, _ := s.store.ListTokensByCitizen(b.ID)
		tokenInfo := make([]map[string]interface{}, 0, len(tokens))
		for _, t := range tokens {
			info := map[string]interface{}{
				"id":        t.ID,
				"label":     t.Label,
				"issued_at": t.IssuedAt.Format(time.RFC3339),
			}
			if t.RevokedAt != nil {
				info["revoked_at"] = t.RevokedAt.Format(time.RFC3339)
			}
			tokenInfo = append(tokenInfo, info)
		}
		out = append(out, map[string]interface{}{
			"id":         b.ID,
			"username":   b.Username,
			"name":       b.Name,
			"role":       b.Role,
			"registered": b.RegisteredAt.Format(time.RFC3339),
			"tokens":     tokenInfo,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"bots": out})
}

type revokeTokenRequest struct {
	Token   string `json:"token,omitempty"`    // revoke by value (caller already holds it)
	TokenID int64  `json:"token_id,omitempty"` // revoke by row id (from list endpoint)
}

// handleRevokeToken marks a token as revoked. The caller must own
// the token: either it's their own (humans rotating their session
// token) or it belongs to a bot they parent. Without that check, a
// member could revoke another member's session — auth bypass via
// denial.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req revokeTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" && req.TokenID == 0 {
		writeError(w, http.StatusBadRequest, "either token or token_id is required")
		return
	}
	// Resolve the citizen this token belongs to so we can verify
	// ownership before revoking.
	ownerID, err := s.store.LookupTokenOwner(req.Token, req.TokenID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ownerID == 0 {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	// Ownership check: caller must own the token directly OR
	// parent the bot that owns it.
	if ownerID != caller.ID {
		owner, _ := s.store.GetCitizen(ownerID)
		if owner == nil || owner.Kind != "bot" || owner.ParentID == nil || *owner.ParentID != caller.ID {
			writeError(w, http.StatusForbidden, "you don't own this token")
			return
		}
	}
	if req.TokenID != 0 {
		err = s.store.RevokeToken(req.TokenID)
	} else {
		err = s.store.RevokeTokenByValue(req.Token)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "revoke: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"revoked": true})
}

// handleListModels returns the model catalog. Open to any
// authenticated citizen — the catalog is public information; you
// need to know which models exist before you can attribute work to
// them.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.store.ListModelCitizens()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list models: "+err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]interface{}{
			"id":       m.ID,
			"username": m.Username,
			"name":     m.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": out})
}

type registerModelRequest struct {
	Username    string `json:"username"`               // required, slug-form
	DisplayName string `json:"display_name,omitempty"` // optional, defaults to username
}

// handleRegisterModel creates a new kind='model' citizen in the
// catalog. Per the design doc's "free-form + soft validation"
// stance, any authenticated citizen can register in local mode;
// hosted-mode policy gating is deferred. Idempotent on duplicate
// (returns 409 with a helpful error).
func (s *Server) handleRegisterModel(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req registerModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" {
		writeError(w, http.StatusBadRequest, "username is required (e.g. 'ollama-llama-3-1-70b')")
		return
	}
	displayName := req.DisplayName
	if displayName == "" {
		displayName = req.Username
	}
	id, err := s.store.CreateModelCitizen(req.Username, displayName)
	if err != nil {
		if strings.Contains(err.Error(), "already taken") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":           id,
		"username":     req.Username,
		"display_name": displayName,
		"kind":         "model",
	})
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	citizen, err := s.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}

	// Merge semantics: omitted fields are nil pointers and must be
	// left untouched. An explicit empty string is respected (the
	// caller wants to clear the value). Requires json.Decoder with
	// a pointer-typed struct.
	var req struct {
		Name  *string `json:"name"`
		Email *string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Name can't be cleared — enforcing this matches the register
	// flow where name is required.
	if req.Name != nil && *req.Name == "" {
		writeError(w, http.StatusBadRequest, "name cannot be empty")
		return
	}

	if err := s.store.UpdateCitizenProfile(citizen.ID, req.Name, req.Email); err != nil {
		if strings.Contains(err.Error(), "email already exists") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update profile")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleCitizenContributions(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	citizen, err := s.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}
	summary, err := s.store.GetContributionSummary(citizen.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get contributions")
		return
	}
	totalEvents, _ := s.store.CountContributionEvents(citizen.ID)
	projectsThisMonth, _ := s.store.CountProjectsThisMonth(citizen.ID)
	downstreamTasks, downstreamProjects, _ := s.store.GetDownstreamImpact(citizen.ID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"username":           username,
		"tasks_completed":    summary.TasksCompleted,
		"tasks_rejected":     summary.TasksRejected,
		"tasks_timed_out":    summary.TasksTimedOut,
		"tasks_released":     summary.TasksReleased,
		"reviews_given":      summary.ReviewsGiven,
		"review_approves":    summary.ReviewApproves,
		"review_rejects":     summary.ReviewRejects,
		"votes_cast":         summary.VotesCast,
		"runs_created":       summary.RunsCreated,
		"tokens_total":       summary.TokensTotal,
		"project_count":      summary.ProjectCount,
		"total_contributions": totalEvents,
		"projects_this_month": projectsThisMonth,
		"downstream_impact": map[string]interface{}{
			"tasks":    downstreamTasks,
			"projects": downstreamProjects,
		},
	})
}

func (s *Server) handleCitizenDashboard(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")

	citizen, err := s.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}

	active, _ := s.store.ListCitizenActiveTasks(citizen.ID)
	recent, _ := s.store.ListCitizenCompletedTasks(citizen.ID, 5)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"citizen":      citizenToMap(citizen),
		"active_tasks": s.toTaskResponses(active),
		"recent_tasks": s.toTaskResponses(recent),
	})
}

// citizenToMap renders a CitizenRecord as a map suitable for JSON
// responses. The internal int id is omitted — callers who need it can
// read `citizen.id` directly from the store.
func citizenToMap(c *store.CitizenRecord) map[string]interface{} {
	return map[string]interface{}{
		"username":           c.Username,
		"name":               c.Name,
		"email":              c.Email,
		"role":               c.Role,
		"score":              c.Score,
		"tasks_completed":    c.TasksCompleted,
		"tasks_timed_out":    c.TasksTimedOut,
		"tasks_released":     c.TasksReleased,
		"tokens_contributed": c.TokensContrib,
		"registered_at":      c.RegisteredAt.Format(time.RFC3339),
	}
}

// --- Helpers ---

// marshalOutputs serializes named outputs to JSON.
func marshalOutputs(outputs map[string]enjuYaml.OutputSpec) string {
	if len(outputs) == 0 {
		return ""
	}
	data, err := json.Marshal(outputs)
	if err != nil {
		return ""
	}
	return string(data)
}

// marshalRequirements serializes task environment requirements to JSON.
func marshalRequirements(reqs map[string]interface{}) string {
	if len(reqs) == 0 {
		return ""
	}
	data, err := json.Marshal(reqs)
	if err != nil {
		return ""
	}
	return string(data)
}

// checkTaskAccess enforces a task's assign_to and require_role
// restrictions for a given citizen. Returns nil when the citizen is
// allowed to claim, or an error with a user-friendly message when they
// aren't.
//
// Both fields are optional. An unset field imposes no restriction — the
// default is that any registered citizen can claim any task.
//
// task.AssignTo stores citizen usernames (resolved at run submission
// time), so we compare against caller.Username directly.

func (s *Server) toTaskResponse(t store.TaskRecord) taskResponse {
	// Look up the run to get project_id and run_seq, and the
	// project to get the remote URL so fat clients know whether
	// this task's content should be written locally (client-side)
	// or via the legacy coordinator-writes path.
	var projectID int64
	var runSeq int
	var remoteURL string
	var projectName string
	var runSourcePath string
	var runBranch string
	var runParams map[string]interface{}
	if run, _ := s.store.GetRun(t.RunID); run != nil {
		projectID = run.ProjectID
		runSeq = run.Seq
		runSourcePath = run.SourcePath
		runBranch = run.Branch
		if run.Params != "" {
			// Best-effort decode; a malformed params JSON
			// (shouldn't happen since we encoded it ourselves
			// at create_run) shouldn't block the task response.
			_ = json.Unmarshal([]byte(run.Params), &runParams)
		}
		if p, _ := s.store.GetProject(projectID); p != nil {
			remoteURL = p.RemoteURL
			projectName = p.Name
		}
	}
	// Per-iteration for_each variables are stored as JSON on
	// the task row itself. Decode so the fat-client can hand
	// them to scripts alongside run-level params.
	var instanceParams map[string]interface{}
	if t.InstanceParams != "" {
		_ = json.Unmarshal([]byte(t.InstanceParams), &instanceParams)
	}
	iterationLabel := ""
	if t.InstanceKey != "" {
		iterationLabel = formatIterationLabel(t.InstanceParams, t.InstanceKey)
	}
	resp := taskResponse{
		ID:               t.ID,
		RunID:            t.RunID,
		RunSeq:           runSeq,
		ProjectID:        projectID,
		ProjectRemoteURL: remoteURL,
		ProjectName:      projectName,
		Seq:              t.Seq,
		TaskDefID:        t.TaskDefID,
		InstanceKey:      t.InstanceKey,
		ResultDir:        engine.ComputeResultDir(&t),
		RunSlug:          t.RunSlug,
		IterationLabel:   iterationLabel,
		Ref:              t.Ref,
		Action:          t.Action,
		Prompt:          t.Prompt,
		UserPrompt:      t.UserPrompt,
		Script:          t.Script,
		Outputs:         t.Outputs,
		Requirements:    t.Requirements,
		ResultType:      t.ResultType,
		State:           string(t.State),
		ClaimedBy:       s.citizenUsername(t.ClaimedBy),
		ResultPath:      t.ResultPath,
		CommitSHA:       t.CommitSHA,
		DependsOn:       t.DependsOn,
		ReadsArtifacts:  unmarshalStringSlice(t.ReadsArtifacts),
		WritesArtifacts: unmarshalWriteArtifacts(t.WritesArtifacts),
		AssignTo:        unmarshalStringSlice(t.AssignTo),
		RequireRole:     t.RequireRole,
		ReviewsTarget:   t.ReviewsTarget,
		ReviewDecision:  t.ReviewDecision,
		VoteOptions:     t.VoteOptions,
		VoteChoice:      t.VoteChoice,
		Citizens:        t.Citizens,
		MinQuorum:       t.MinQuorum,
		VoteThreshold:   t.VoteThreshold,
		VoteDeadline:    t.VoteDeadline,
		Anonymize:       t.Anonymize,
		Visibility:      t.Visibility,
		FailReason:        t.FailReason,
		SkipReason:        t.SkipReason,
		ParkedFromState:   t.ParkedFromState,
		RunSourcePath:     runSourcePath,
		RunBranch:         runBranch,
		RunParams:         runParams,
		InstanceParamsMap: instanceParams,
		Env:               unmarshalStringMapField(t.Env),
		Mode:              t.Mode,
		Container:         t.Container,
	}
	// Single-citizen task model attribution. For multi-citizen
	// tasks we expose per-voter models on VoteSubmissions[].Model
	// (below); for single-citizen tasks there's one (or after
	// invalidate+resubmit, multiple) task_claims row, so a single
	// top-level resp.Model field is the natural surface for the
	// formatter to render alongside "Claimed by".
	//
	// Gate on state == accepted: no completed claim row exists
	// before a task lands in a terminal-success state, so for a
	// 100-task run-listing query most tasks skip this round-trip
	// entirely. Without the gate, every toTaskResponse call pays
	// an extra task_claims query plus a citizens lookup — N+1 on
	// every run-listing endpoint.
	//
	// Take the LAST element, not the first: ListVoteSubmissions
	// orders by submitted_at ASC. After invalidate → re-claim →
	// re-submit, the original (stale) claim row stays at
	// subs[0] because invalidation only flips outcome=NULL rows
	// to 'invalidated', leaving 'completed' rows untouched. The
	// freshly-submitted model lives at subs[len-1] — that's what
	// the formatter and any caller cares about.
	if t.Citizens <= 1 && store.TaskState(t.State) == store.TaskAccepted {
		if subs, err := s.store.ListVoteSubmissions(t.ID); err == nil && len(subs) > 0 {
			latest := subs[len(subs)-1]
			if latest.ModelID != nil {
				resp.Model = s.citizenUsername(*latest.ModelID)
			}
		}
	}

	// Phase E.2 session 2a/2b — surface per-citizen claim and
	// submission state for multi-citizen vote AND review tasks
	// so MCP formatters can render the Voting / Review block
	// without an extra round trip.
	if (t.Action == "vote" || t.Action == "review") && t.Citizens > 1 {
		if t.VoteDeadline != "" {
			if d, derr := time.ParseDuration(t.VoteDeadline); derr == nil {
				if first, ferr := s.store.EarliestClaimTime(t.ID); ferr == nil && !first.IsZero() {
					resp.VoteDeadlineAt = first.Add(d).Format(time.RFC3339)
				}
			}
		}
		if submissions, err := s.store.ListVoteSubmissions(t.ID); err == nil {
			for idx, sub := range submissions {
				uname := s.citizenUsername(sub.CitizenID)
				if t.Anonymize {
					uname = fmt.Sprintf("citizen-%d", idx+1)
				}
				ref := voteSubmissionRef{
					Username: uname,
					Option:   sub.Option,
				}
				if sub.SubmittedAt != nil {
					ref.SubmittedAt = sub.SubmittedAt.Format(time.RFC3339)
				}
				// Resolve the model citizen username if this
				// submit recorded one. citizenUsername returns
				// "" for nil id or unknown citizen — both render
				// as no model field, which is correct for
				// pre-1.4 rows and unaided humans.
				if sub.ModelID != nil {
					ref.Model = s.citizenUsername(*sub.ModelID)
				}
				resp.VoteSubmissions = append(resp.VoteSubmissions, ref)
			}
		}
		if claims, err := s.store.ListActiveClaims(t.ID); err == nil {
			for idx, c := range claims {
				uname := s.citizenUsername(c.CitizenID)
				if uname == "" {
					continue
				}
				if t.Anonymize {
					uname = fmt.Sprintf("active-citizen-%d", idx+1)
				}
				resp.ActiveClaimants = append(resp.ActiveClaimants, uname)
				if c.Branch != "" {
					if resp.IterationBranches == nil {
						resp.IterationBranches = map[string]string{}
					}
					resp.IterationBranches[uname] = c.Branch
				}
			}
		}
	}

	// Phase 6b foundational v1 + 6b.2: upstream_iteration_branch
	// for action:review tasks. The fat-client uses this as the
	// fork base for the review's own topic — review_topic is
	// then a descendant of upstream_topic, so the eventual
	// approve merge advances main to a tip that contains BOTH
	// the upstream's commit and the review's verdict prose in
	// one FF step.
	//
	// Always set when the upstream has a completed claim with
	// a topic. Phase 6b.1 used to clear this when the upstream
	// was state=accepted on the assumption that "accepted means
	// merged" — false after 6b.2's merge gate, where reviewed
	// tasks stay accepted but unmerged until the review approves.
	// The previous gate would have produced fork-from-main
	// here, which under linear progression equals fork-from-
	// upstream-topic SHA (main hasn't moved past upstream's
	// base) — but under any concurrent-task scenario the SHAs
	// diverge and the FF refuses. Always forking from the
	// upstream topic keeps the merge invariant correct in both
	// linear and divergent layouts.
	if t.Action == "review" && t.ReviewsTarget != "" {
		targetDef, targetInstance := parseReviewsTargetForMerge(t.ReviewsTarget)
		runTasks, _ := s.store.ListTasksByRun(t.RunID)
		for _, ut := range runTasks {
			if ut.TaskDefID != targetDef || ut.InstanceKey != targetInstance {
				continue
			}
			hist, _ := s.store.ListTaskHistory(ut.ID)
			for i := len(hist) - 1; i >= 0; i-- {
				if hist[i].Outcome == "completed" && hist[i].Branch != "" {
					resp.UpstreamIterationBranch = hist[i].Branch
					break
				}
			}
			break
		}
	}

	// Phase 6b.1: previous_iteration_commit. The most recent
	// claim that submitted (outcome=completed) AND isn't the
	// current active one. After a request_changes re-claim,
	// the active claim has no commit yet but the prior closed
	// claim's commit SHA lets the fat-client surface the prior
	// content via ReadFileAtCommit — the workspace is on the
	// run branch by then, so a plain ReadFile of the workspace
	// path can't see the prior topic-branch-only content.
	//
	// Also populate latest_completed_commit_sha + branch from
	// the same scan (foundational v1 fan-out): readers that
	// need a task's last submission content even after the
	// task's DB state was cleared (request_changes cascade
	// blanks t.CommitSHA on the task row but the commit
	// itself persists on the topic branch) use these for a
	// branch-safe ReadFileAtCommit.
	// Match any claim that produced a commit, regardless of
	// final verdict. Phase 6b.2's outcome relabel (ISSUE-003)
	// flips the row from "completed" to "rejected" when a
	// review request_changes / reject fires, but the commit
	// itself still exists on the topic branch and the
	// previous-submission UI / merge collector still need to
	// reach it. Match on `CommitSHA != ""` rather than the
	// outcome string so the lookup survives the relabel.
	if hist, err := s.store.ListTaskHistory(t.ID); err == nil {
		for i := len(hist) - 1; i >= 0; i-- {
			c := hist[i]
			if c.CommitSHA == "" {
				continue
			}
			if c.Outcome != "completed" && c.Outcome != "rejected" {
				continue
			}
			if resp.LatestCompletedCommitSHA == "" {
				resp.LatestCompletedCommitSHA = c.CommitSHA
				resp.LatestCompletedBranch = c.Branch
			}
			if resp.PreviousIterationCommit == "" {
				resp.PreviousIterationCommit = c.CommitSHA
			}
			if resp.LatestCompletedCommitSHA != "" && resp.PreviousIterationCommit != "" {
				break
			}
		}
	}

	// Phase 6b.1: surface per-citizen iteration_branches for
	// EVERY task that has an active claim, not just multi-
	// citizen vote/review. Single-citizen action:answer tasks
	// also need their topic branch on the wire so the fat-
	// client submit handler can pick it up via fetchTaskMeta
	// after the claim. Anonymized tasks remain anonymized —
	// fall through to the multi-citizen block above which
	// already handles that case.
	if resp.IterationBranches == nil && !t.Anonymize {
		if claims, err := s.store.ListActiveClaims(t.ID); err == nil {
			for _, c := range claims {
				if c.Branch == "" {
					continue
				}
				uname := s.citizenUsername(c.CitizenID)
				if uname == "" {
					continue
				}
				if resp.IterationBranches == nil {
					resp.IterationBranches = map[string]string{}
				}
				resp.IterationBranches[uname] = c.Branch
			}
		}
	}

	// Task history: show previous attempts when the task has
	// been invalidated and re-run. Only populated when there
	// are multiple claim records (single-claim tasks skip
	// this to avoid noise).
	if history, err := s.store.ListTaskHistory(t.ID); err == nil && len(history) > 1 {
		for _, h := range history {
			entry := taskHistoryEntry{
				Citizen:   s.citizenUsername(h.CitizenID),
				ClaimedAt: h.ClaimedAt.Format(time.RFC3339),
				Outcome:   h.Outcome,
				Decision:  h.Option,
			}
			if h.SubmittedAt != nil {
				entry.SubmittedAt = h.SubmittedAt.Format(time.RFC3339)
			}
			resp.TaskHistory = append(resp.TaskHistory, entry)
		}
	}

	// Artifact provenance: for each artifact this task reads,
	// show who last wrote it on the task's own branch. Using
	// runBranch (captured above) keeps provenance scoped to
	// the branch this task actually consumes — parallel runs
	// on other branches have their own index rows.
	for _, path := range unmarshalStringSlice(t.ReadsArtifacts) {
		prov := artifactProvenance{Path: path}
		if art, err := s.store.GetArtifact(projectID, runBranch, path); err == nil && art != nil {
			prov.LastWriter = s.citizenUsername(art.LastWriter)
			prov.LastTaskID = art.LastTaskID
			prov.CommitSHA = art.CommitSHA
		}
		resp.ArtifactProvenance = append(resp.ArtifactProvenance, prov)
	}

	return resp
}

func (s *Server) toTaskResponses(tasks []store.TaskRecord) []taskResponse {
	resp := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		resp = append(resp, s.toTaskResponse(t))
	}
	return resp
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// isValidCommitSHAShape checks whether s is shaped like a git
// commit SHA (40 hex for SHA-1, 64 hex for SHA-256) AND isn't
// one of the obviously-phantom patterns (all zeros, all same
// digit, etc.) that a buggy or test-harness client would
// submit by accident.
//
// This is still a shape sanity check, not a content
// verification — under ARCHITECTURE principle 7
// (trust-the-client), existence verification is a client
// responsibility. Full server-side verify-by-fetching the
// commit is tracked as pre-launch work for hosted mode, where
// the trust-the-client assumption no longer holds.
//
// Rejecting well-known phantoms (all-zero especially) closes
// the "I sent '0000...000' manually and it was accepted"
// class of reports without requiring the architectural shift
// that would let the coordinator actually clone and verify
// remotes.
func isValidCommitSHAShape(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	// All hex?
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	// All-same-digit phantoms: "0000..." is the empty-SHA
	// sentinel go-git uses as a nil-ref marker; "ffff..." and
	// other repeats are common test-garbage values. A real
	// commit SHA has entropy — accidental all-same-char is
	// cryptographically impossible.
	firstChar := s[0]
	allSame := true
	for i := 1; i < len(s); i++ {
		if s[i] != firstChar {
			allSame = false
			break
		}
	}
	if allSame {
		return false
	}
	return true
}
