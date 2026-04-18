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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/dag"
	"github.com/enju-ai/enju/internal/engine"
	"github.com/enju-ai/enju/internal/store"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// Server holds the coordinator state and dependencies. Post the
// iteration A orchestrator rewrite, the coordinator holds no git
// state of its own — clients own their own clones, and the
// coordinator is pure DAG/state/index metadata.
type Server struct {
	store  *store.Store
	dags   map[int64]*dag.DAG // runID -> DAG (in-memory for fast queries)
	runs   map[int64]*enjuYaml.ParsedRun
	logger *slog.Logger
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
// Soft-enforced: missing token → allowed through (backwards
// compat for clients that haven't re-registered yet).
// Invalid token → 401. The validated citizen record is
// stashed in the request context for handlers to use.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// Soft enforcement: no token → allowed through.
			// This is intentional for the transition period
			// while clients upgrade. Monitor these in logs
			// and tighten to hard-reject after battle tests.
			if r.Method != "GET" {
				s.logger.Debug("auth: no token on write request",
					"method", r.Method, "path", r.URL.Path)
			}
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "invalid Authorization header")
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
		next.ServeHTTP(w, r)
	})
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
		r.Get("/projects/{projectID}/runs", s.handleListProjectRuns)
		r.Get("/projects/{projectID}/artifacts", s.handleListArtifacts)
		r.Get("/projects/{projectID}/artifacts/*", s.handleGetArtifact)

		// Runs — hierarchical under projects
		// Address runs by project_id + run_seq (per-project numbering)
		r.Post("/projects/{projectID}/runs", s.handleCreateRun)
		r.Get("/projects/{projectID}/runs/{runSeq}", s.handleGetRun)
		r.Get("/projects/{projectID}/runs/{runSeq}/tasks", s.handleListRunTasks)
		r.Get("/projects/{projectID}/runs/{runSeq}/cost", s.handleGetRunCostSummary)

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
		r.Post("/tasks/{taskID}/result", s.handleSubmitResult)
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
}

type setProjectRemoteRequest struct {
	RemoteURL string `json:"remote_url"`
}

type projectResponse struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	RemoteURL   string `json:"remote_url,omitempty"`
	RunCount    int    `json:"run_count"`
	CreatedAt   string `json:"created_at"`
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
	id, err := s.store.CreateProject(&store.ProjectRecord{
		Name:        req.Name,
		Description: req.Description,
		RemoteURL:   req.RemoteURL,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create project: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, projectResponse{
		ID:        id,
		Name:      req.Name,
		RemoteURL: req.RemoteURL,
		CreatedAt: now.Format(time.RFC3339),
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

	var req setProjectRemoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
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

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
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
		ID:          p.ID,
		Name:        p.Name,
		Description: p.Description,
		RemoteURL:   p.RemoteURL,
		RunCount:    runCount,
		CreatedAt:   p.CreatedAt.Format(time.RFC3339),
	}
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	p, err := s.store.GetProject(projectID)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "project not found")
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
			CreatedAt:  run.CreatedAt.Format(time.RFC3339),
			SourcePath: run.SourcePath,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Artifacts ---

type artifactResponse struct {
	Path       string `json:"path"`
	LastWriter string `json:"last_writer,omitempty"` // username of the last writer
	LastTaskID string `json:"last_task_id,omitempty"`
	LastRunID  int64  `json:"last_run_id,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

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

	prefix := r.URL.Query().Get("prefix")
	rows, err := s.store.ListArtifactsByProject(projectID, prefix)
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

	// chi's wildcard captures everything after "artifacts/" — that IS
	// the user-facing artifact path.
	path := chi.URLParam(r, "*")
	if err := validateArtifactPath(path); err != nil {
		writeError(w, http.StatusBadRequest, "invalid artifact path: "+err.Error())
		return
	}

	meta, err := s.store.GetArtifact(projectID, path)
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
		"updated_at":   meta.UpdatedAt.Format(time.RFC3339),
	})
}

// --- Runs ---

type createRunRequest struct {
	YAML            string                 `json:"yaml"`
	RepoURL         string                 `json:"repo_url,omitempty"`
	Params          map[string]interface{} `json:"params,omitempty"`
	SourcePath      string                 `json:"source_path,omitempty"`
	SourceCommitSHA string                 `json:"source_commit_sha,omitempty"`
	Username        string                 `json:"username,omitempty"` // citizen who created this run, for contribution tracking
}

type runResponse struct {
	ID              int64    `json:"id"`                   // global DB ID
	ProjectID       int64    `json:"project_id,omitempty"` // parent project
	Seq             int      `json:"seq"`                  // sequence within project (this is the user-facing run #)
	Name            string   `json:"name"`
	State           string   `json:"state"`
	TaskCount       int      `json:"task_count"`
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

	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.YAML == "" {
		writeError(w, http.StatusBadRequest, "yaml is required")
		return
	}

	// Parse and validate the YAML. If the caller supplied
	// top-level params (Phase H.1), use ParseWithParams to
	// substitute them into task prompts before validation/DAG
	// construction. Calls with no params take the plain Parse
	// path so inline-YAML submissions keep their existing
	// semantics.
	var parsed *enjuYaml.ParsedRun
	if req.Params != nil {
		parsed, err = enjuYaml.ParseWithParams([]byte(req.YAML), req.Params)
	} else {
		parsed, err = enjuYaml.Parse([]byte(req.YAML))
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run definition: "+err.Error())
		return
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
	runID, runSeq, err := s.store.CreateRun(&store.RunRecord{
		ProjectID:       projectID,
		Name:            parsed.Run.Name,
		Ref:             parsed.Run.Ref,
		YAMLData:        req.YAML,
		RepoURL:         req.RepoURL,
		State:           store.RunActive,
		SourcePath:      req.SourcePath,
		SourceCommitSHA: req.SourceCommitSHA,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		s.logger.Error("creating run", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create run: "+err.Error())
		return
	}

	// Build task records via engine and apply atomically.
	taskRecords := engine.BuildRunTasks(parsed, runID, projectID, runSeq)
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

	var resp []runResponse
	for _, p := range runs {
		tasks, _ := s.store.ListTasksByRun(p.ID)
		resp = append(resp, runResponse{
			ID:         p.ID,
			ProjectID:  p.ProjectID,
			Seq:        p.Seq,
			Name:       p.Name,
			State:      string(p.State),
			TaskCount:  len(tasks),
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

	tasks, _ := s.store.ListTasksByRun(p.ID)

	resp := map[string]interface{}{
		"id":         p.ID,
		"project_id": p.ProjectID,
		"seq":        p.Seq,
		"name":       p.Name,
		"state":      p.State,
		"repo_url":   p.RepoURL,
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
	ResultPath      string   `json:"result_path,omitempty"`
	CommitSHA       string   `json:"commit_sha,omitempty"` // git SHA of the accepted result (iteration A+)
	DependsOn       string   `json:"depends_on,omitempty"`
	ReadsArtifacts  []string `json:"reads_artifacts,omitempty"`
	WritesArtifacts []string `json:"writes_artifacts,omitempty"`
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
	// ArtifactProvenance shows who last wrote each artifact
	// this task reads.
	ArtifactProvenance []artifactProvenance `json:"artifact_provenance,omitempty"`
	// TaskHistory shows previous claim/submit/invalidation
	// attempts on this task. Populated when the task has
	// more than one claim record (indicates re-runs after
	// invalidation).
	TaskHistory []taskHistoryEntry `json:"task_history,omitempty"`
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
}

func (s *Server) handleListReadyTasks(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	runSeq, _ := strconv.Atoi(r.URL.Query().Get("run_id"))

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

	// Engine validates (state, slots, cap) → returns Plan.
	plan, err := s.engine().ComputeClaim(taskID, caller.ID, deadline)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if _, err := s.store.ApplyPlan(*plan); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.store.TouchCitizen(caller.ID)

	// Return task with full details
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
		"cross_run_readers":   res.CrossRunReaders,
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
	eng := s.engine()

	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}

	// 1. Engine validates request (artifacts, paths, decision,
	//    option, citizen).
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
	resultPath, decision, voteChoice, submitterID, err := eng.ValidateSubmitRequest(task, run, engineReq)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 2. Engine computes submission → Plan.
	submitOutcome, err := eng.ComputeSubmission(
		taskID, submitterID, resultPath, req.CommitSHA,
		decision, voteChoice, req.Content, req.TokensUsed,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.ApplyPlan(submitOutcome.Plan); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update task: "+err.Error())
		return
	}

	// Record contribution events (append-only, never fails
	// the submit — best-effort logging). Inject model name
	// from the submit request — the engine doesn't have it
	// because it's a client-side config.
	for i := range submitOutcome.Events {
		evt := &submitOutcome.Events[i]
		if evt.ProjectID == 0 {
			evt.ProjectID = run.ProjectID
		}
		if req.Model != "" && evt.Metadata != "" {
			evt.Metadata = strings.TrimSuffix(evt.Metadata, "}") + fmt.Sprintf(`,"model":%q}`, req.Model)
		}
		if err := s.store.RecordContributionEvent(evt); err != nil {
			s.logger.Warn("recording contribution event", "error", err)
		}
	}

	// 3. Engine computes post-submit actions (artifacts,
	//    tally, resolution decisions).
	actions, err := eng.ComputePostSubmitActions(task, run, submitOutcome, engineReq, decision, voteChoice)
	if err != nil {
		s.logger.Error("post-submit actions failed", "task_id", taskID, "error", err)
	}

	// 4. Apply artifact mutations.
	if actions != nil && len(actions.ArtifactMutations) > 0 {
		if _, err := s.store.ApplyPlan(store.Plan{
			Version:   engine.EngineVersion,
			Mutations: actions.ArtifactMutations,
		}); err != nil {
			s.logger.Error("upserting artifact index", "error", err)
		}
	}

	// 5. Apply review/vote resolution + fire cascades.
	var rejectResult *invalidationResult
	var skipResult *skipCascadeResult
	if actions != nil {
		if actions.ReviewResolvePlan != nil {
			s.store.ApplyPlan(*actions.ReviewResolvePlan)
		}
		if actions.ShouldRejectTarget && actions.RejectTargetID != "" {
			res, err := s.performInvalidate(actions.RejectTargetID)
			if err != nil {
				s.logger.Error("review-request_changes cascade", "target", actions.RejectTargetID, "error", err)
			} else {
				rejectResult = res
			}
		}
		if actions.ShouldFailTarget && actions.RejectTargetID != "" {
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
					Task:            res.Task,
					Descendants:     res.SkippedDescendants,
					CrossRunReaders: res.CrossRunReaders,
					Dematerialized:  res.Dematerialized,
					Changed:         res.Changed,
					Rollbacks:       res.Rollbacks,
					AffectedRuns:    res.AffectedRuns,
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

	// 7. Ready-task sweep + run completion.
	readied, _ := s.store.UpdateReadyTasks(task.RunID)
	if actions != nil {
		for rid := range actions.CrossRunIDs {
			if n, err := s.store.UpdateReadyTasks(rid); err == nil {
				readied += n
			}
		}
	}
	completed, _ := s.store.CheckAndCompleteRun(task.RunID)

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
			"target":            task.ReviewsTarget,
			"descendants":       rejectResult.Descendants,
			"changed":           rejectResult.Changed,
			"rollbacks_count":   len(rejectResult.Rollbacks),
			"cross_run_readers": rejectResult.CrossRunReaders,
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
	writeJSON(w, http.StatusOK, resp)
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
	Task            *store.TaskRecord
	Descendants     []string
	CrossRunReaders []string
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
	AffectedRuns   map[int64]bool
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

	// 4. Cross-run readers → PENDING with claim clear.
	for _, readerID := range outcome.CrossRunReaders {
		mutations = append(mutations, store.SetTaskState{
			TaskID:     readerID,
			NewState:   store.TaskPending,
			ClearClaim: true,
		})
	}

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

	// Ready-task sweeps + run reactivation AFTER the plan
	// transaction commits (SQLite doesn't support nested
	// write transactions, so these run as separate calls).
	for runID := range outcome.AffectedRunIDs {
		_, _ = s.store.UpdateReadyTasks(runID)
		if r, _ := s.store.GetRun(runID); r != nil && r.State == store.RunCompleted {
			_ = s.store.UpdateRunState(runID, store.RunActive)
		}
	}

	changed := result.Changed + result.TasksDeleted

	return &invalidationResult{
		Task:            task,
		Descendants:     outcome.RegularDescendants,
		CrossRunReaders: outcome.CrossRunReaders,
		Dematerialized:  outcome.DematerializedIDs,
		Changed:         changed,
		Rollbacks:       rollbacks,
		AffectedRuns:    outcome.AffectedRunIDs,
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
	CrossRunReaders    []string
	Dematerialized     []string
	Changed            int
	Rollbacks          []rollbackOutcome
	AffectedRuns       map[int64]bool
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
	//    descendants — they lost their basis but should re-run
	//    with the rolled-back content, not be marked skipped.
	for _, readerID := range outcome.CrossRunReaders {
		mutations = append(mutations, store.SetTaskState{
			TaskID:     readerID,
			NewState:   store.TaskPending,
			ClearClaim: true,
		})
	}

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

	// Ready-task sweeps on affected runs — mainly for cross-run
	// readers that went PENDING; their runs might have other
	// tasks that became READY or blocked by the flip.
	for runID := range outcome.AffectedRunIDs {
		_, _ = s.store.UpdateReadyTasks(runID)
		if r, _ := s.store.GetRun(runID); r != nil && r.State == store.RunCompleted {
			_ = s.store.UpdateRunState(runID, store.RunActive)
		}
	}

	return &failCascadeResult{
		Task:               task,
		Reason:             reason,
		SkippedDescendants: skippedDescendants,
		CrossRunReaders:    outcome.CrossRunReaders,
		Dematerialized:     outcome.DematerializedIDs,
		Changed:            result.Changed + result.TasksDeleted,
		Rollbacks:          rollbacks,
		AffectedRuns:       outcome.AffectedRunIDs,
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
	winningSet := make(map[string]bool)
	losingSet := make(map[string]bool)
	for _, o := range declared {
		target := winningSet
		if o.ID != winningOptionID {
			target = losingSet
		}
		for _, shortID := range o.Activates {
			full := runPrefix + shortID
			target[full] = true
			for _, desc := range d.Descendants(shortID) {
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
		"cross_run_readers", len(result.CrossRunReaders),
		"changed", result.Changed,
		"artifacts_rolled_back", len(result.Rollbacks),
		"reason", req.Reason,
	)

	resp := map[string]interface{}{
		"status":      "invalidated",
		"task_id":     taskID,
		"descendants": result.Descendants,
		"changed":     result.Changed,
		"reason":      req.Reason,
	}
	if len(result.CrossRunReaders) > 0 {
		resp["artifact_readers"] = result.CrossRunReaders
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
	if run, _ := s.store.GetRun(t.RunID); run != nil {
		projectID = run.ProjectID
		runSeq = run.Seq
		if p, _ := s.store.GetProject(projectID); p != nil {
			remoteURL = p.RemoteURL
			projectName = p.Name
		}
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
		WritesArtifacts: unmarshalStringSlice(t.WritesArtifacts),
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
		FailReason:      t.FailReason,
		SkipReason:      t.SkipReason,
		ParkedFromState: t.ParkedFromState,
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
	// show who last wrote it. Quick index lookup per path.
	for _, path := range unmarshalStringSlice(t.ReadsArtifacts) {
		prov := artifactProvenance{Path: path}
		if art, err := s.store.GetArtifact(projectID, path); err == nil && art != nil {
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
