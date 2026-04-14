// Package api provides the REST API for the Enju coordinator.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/dag"
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

// Router returns the chi router with all endpoints registered.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/health", s.handleHealth)

	r.Route("/api/v1", func(r chi.Router) {
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

		// Legacy flat listing — still useful for dashboards
		r.Get("/runs", s.handleListRuns)

		// Tasks
		r.Get("/tasks/ready", s.handleListReadyTasks)
		r.Post("/tasks/{taskID}/claim", s.handleClaimTask)
		r.Get("/tasks/{taskID}", s.handleGetTask)
		r.Get("/tasks/{taskID}/inputs", s.handleGetTaskInputs)
		r.Post("/tasks/{taskID}/result", s.handleSubmitResult)
		r.Post("/tasks/{taskID}/release", s.handleReleaseTask)
		r.Post("/tasks/{taskID}/invalidate", s.handleInvalidateTask)

		// Citizens
		r.Post("/citizens/register", s.handleRegisterCitizen)
		r.Get("/citizens/by-username/{username}/dashboard", s.handleCitizenDashboard)
		r.Put("/citizens/by-username/{username}/profile", s.handleUpdateProfile)
		r.Get("/citizens/by-username/{username}", s.handleGetCitizenByUsername)
	})

	return r
}

// --- Health ---

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
			ID:        run.ID,
			ProjectID: run.ProjectID,
			Seq:       run.Seq,
			Name:      run.Name,
			State:     string(run.State),
			TaskCount: len(tasks),
			CreatedAt: run.CreatedAt.Format(time.RFC3339),
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
	YAML    string `json:"yaml"`
	RepoURL string `json:"repo_url,omitempty"`
}

type runResponse struct {
	ID        int64  `json:"id"`                   // global DB ID
	ProjectID int64  `json:"project_id,omitempty"` // parent project
	Seq       int    `json:"seq"`                  // sequence within project (this is the user-facing run #)
	Name      string `json:"name"`
	State     string `json:"state"`
	TaskCount int    `json:"task_count"`
	CreatedAt string `json:"created_at"`
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

	// Parse and validate the YAML
	parsed, err := enjuYaml.Parse([]byte(req.YAML))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run definition: "+err.Error())
		return
	}

	// === Pre-flight validation ===
	//
	// Walk every expanded task and validate declared paths, usernames,
	// etc. BEFORE touching the database. This keeps run creation atomic
	// from the caller's perspective: a failed submission never leaves a
	// ghost run with partial tasks behind, and the per-project run
	// sequence counter doesn't advance on rejected submissions.
	//
	// Anything that could plausibly reject the submission belongs in
	// this loop, not in the second (writing) loop below.
	for _, tasks := range parsed.ExpandedTasks {
		for _, ti := range tasks {
			for _, p := range ti.ReadsArtifacts {
				if err := validateArtifactPath(p); err != nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q: invalid reads_artifacts path %q: %v", ti.ID, p, err))
					return
				}
			}
			for _, p := range ti.WritesArtifacts {
				if err := validateArtifactPath(p); err != nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q: invalid writes_artifacts path %q: %v", ti.ID, p, err))
					return
				}
			}
			for _, uname := range ti.AssignTo {
				if err := store.ValidateUsername(uname); err != nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q: invalid assign_to username %q: %v", ti.ID, uname, err))
					return
				}
				c, _ := s.store.GetCitizenByUsername(uname)
				if c == nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q: assign_to citizen %q is not registered", ti.ID, uname))
					return
				}
			}
		}
	}

	// === Creation ===
	//
	// All validations passed. Now create the run and its tasks. Both
	// steps touch the database, but by this point we've done everything
	// we can to ensure success without a transaction. CreateTask
	// failures at this point are genuine DB errors and get logged.
	now := time.Now()
	repoURL := req.RepoURL

	runID, runSeq, err := s.store.CreateRun(&store.RunRecord{
		ProjectID: projectID,
		Name:      parsed.Run.Name,
		Ref:       parsed.Run.Ref,
		YAMLData:  req.YAML,
		RepoURL:   repoURL,
		State:     store.RunActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		s.logger.Error("creating run", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create run: "+err.Error())
		return
	}

	// Create task records from expanded DAG.
	// Task IDs are prefixed with {project_id}:{run_seq}: to make them globally unique
	// and addressable within the per-project hierarchy.
	runPrefix := fmt.Sprintf("%d:%d:", projectID, runSeq)
	taskCount := 0
	taskSeq := 0
	// Iterate ExpandedTasks in sorted-key order so the seq column (and
	// therefore downstream display order in run_status) is
	// deterministic across processes — Go map iteration is not.
	instanceKeys := make([]string, 0, len(parsed.ExpandedTasks))
	for k := range parsed.ExpandedTasks {
		instanceKeys = append(instanceKeys, k)
	}
	sort.Strings(instanceKeys)
	for _, instanceKey := range instanceKeys {
		tasks := parsed.ExpandedTasks[instanceKey]
		for _, ti := range tasks {
			taskSeq++
			resultType := ti.ResultType
			if resultType == "" {
				resultType = "text"
			}
			timeout := ti.Timeout
			if timeout == "" {
				timeout = parsed.Run.Defaults.Timeout
			}

			// build() populates ti.DependsOn with short IDs already
			// resolved against the current expansion mode (singletons,
			// per-iteration binding, or fan-in). We just prepend the
			// run prefix to get fully-qualified store IDs.
			var deps []string
			for _, dep := range ti.DependsOn {
				deps = append(deps, runPrefix+dep)
			}

			// Determine initial state
			state := store.TaskPending
			if len(ti.DependsOn) == 0 {
				state = store.TaskReady
			}

			paramsJSON := ""
			if len(ti.Params) > 0 {
				if b, err := json.Marshal(ti.Params); err == nil {
					paramsJSON = string(b)
				}
			}
			err := s.store.CreateTask(&store.TaskRecord{
				ID:             runPrefix + ti.FullID,
				RunID:          runID,
				Seq:            taskSeq,
				TaskDefID:      ti.ID,
				InstanceKey:    instanceKey,
				InstanceParams: paramsJSON,
				Ref:         ti.Ref,
				Action:      ti.Action,
				Prompt:      ti.Prompt,
				UserPrompt:  ti.UserPrompt,
				Script:       ti.Script,
				Outputs:      marshalOutputs(ti.Outputs),
				Requirements: marshalRequirements(ti.Requirements),
				ResultType:   resultType,
				Timeout:     timeout,
				State:       state,
				DependsOn:   strings.Join(deps, ","),
				ReadsArtifacts:  marshalStringSlice(ti.ReadsArtifacts),
				WritesArtifacts: marshalStringSlice(ti.WritesArtifacts),
				AssignTo:        marshalStringSlice([]string(ti.AssignTo)),
				RequireRole:     ti.RequireRole,
				CreatedAt:   now,
			})
			if err != nil {
				s.logger.Error("creating task", "task_id", ti.FullID, "error", err)
				writeError(w, http.StatusInternalServerError, "failed to create tasks")
				return
			}
			taskCount++
		}
	}

	// Cache DAG and parsed run in memory
	s.dags[runID] = parsed.DAG
	s.runs[runID] = parsed

	s.logger.Info("run created", "id", runID, "project_id", projectID, "seq", runSeq, "name", parsed.Run.Name, "tasks", taskCount)

	writeJSON(w, http.StatusCreated, runResponse{
		ID:        runID,
		ProjectID: projectID,
		Seq:       runSeq,
		Name:      parsed.Run.Name,
		State:     string(store.RunActive),
		TaskCount: taskCount,
		CreatedAt: now.Format(time.RFC3339),
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
			ID:        p.ID,
			ProjectID: p.ProjectID,
			Seq:       p.Seq,
			Name:      p.Name,
			State:     string(p.State),
			TaskCount: len(tasks),
			CreatedAt: p.CreatedAt.Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, resp)
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         p.ID,
		"project_id": p.ProjectID,
		"seq":        p.Seq,
		"name":       p.Name,
		"state":      p.State,
		"repo_url":   p.RepoURL,
		"task_count": len(tasks),
		"created_at": p.CreatedAt.Format(time.RFC3339),
	})
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
	writeJSON(w, http.StatusOK, s.toTaskResponse(*task))
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

	// Access control: assign_to and require_role are optional. When
	// unset the task is open to any registered citizen (default).
	// When set they narrow who can claim.
	if err := s.checkTaskAccess(task, caller); err != nil {
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

	if err := s.store.ClaimTask(taskID, caller.ID, deadline); err != nil {
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
}

// handleSubmitResult is the metadata-only submit path. The client
// has already done the git work; the coordinator just validates the
// report, updates the state machine, updates the artifact index,
// runs the scheduler, and checks run completion.
func (s *Server) handleSubmitResult(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req submitResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CommitSHA == "" {
		writeError(w, http.StatusBadRequest, "commit_sha is required — the coordinator no longer writes result files, clients must write + push + report")
		return
	}

	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
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
func (s *Server) handleGetTaskInputsDescriptor(w http.ResponseWriter, r *http.Request, task *store.TaskRecord, run *store.RunRecord) {
	deps := []map[string]interface{}{}
	if task.DependsOn != "" {
		for _, depID := range strings.Split(task.DependsOn, ",") {
			depID = strings.TrimSpace(depID)
			depTask, err := s.store.GetTask(depID)
			if err != nil || depTask == nil {
				continue
			}
			var params map[string]string
			if depTask.InstanceParams != "" {
				_ = json.Unmarshal([]byte(depTask.InstanceParams), &params)
			}
			deps = append(deps, map[string]interface{}{
				"task_def_id":     depTask.TaskDefID,
				"instance_key":    depTask.InstanceKey,
				"instance_params": params,
				"commit_sha":      depTask.CommitSHA,
				"result_path":     depTask.ResultPath,
			})
		}
	}

	artifactReads := []map[string]interface{}{}
	for _, p := range unmarshalStringSlice(task.ReadsArtifacts) {
		art, err := s.store.GetArtifact(run.ProjectID, p)
		if err != nil || art == nil {
			// Missing artifact — the client's resolver will
			// surface this via MissingArtifacts in its
			// ResolvedPrompt. We still send the path so the
			// client knows it was declared.
			artifactReads = append(artifactReads, map[string]interface{}{
				"path":       p,
				"commit_sha": "",
			})
			continue
		}
		artifactReads = append(artifactReads, map[string]interface{}{
			"path":       p,
			"commit_sha": art.CommitSHA,
		})
	}

	var forEachParams map[string]string
	if task.InstanceParams != "" {
		_ = json.Unmarshal([]byte(task.InstanceParams), &forEachParams)
	}

	// Include the project's remote URL so the client knows where to
	// pull/push. This is the same URL the client already has from
	// the task/claim response; duplicating it here keeps the
	// descriptor self-contained.
	var remoteURL string
	if p, _ := s.store.GetProject(run.ProjectID); p != nil {
		remoteURL = p.RemoteURL
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"task_id":              task.ID,
		"prompt_template":      task.Prompt,
		"user_prompt_template": task.UserPrompt,
		"for_each_params":      forEachParams,
		"dependencies":         deps,
		"artifact_reads":       artifactReads,
		"project_id":           run.ProjectID,
		"project_remote_url":   remoteURL,
	})
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

	// Validate declared artifacts against the task's writes_artifacts
	// allowlist — same contract as the legacy path, just without
	// writing any files.
	if len(req.ArtifactsWritten) > 0 {
		declared := unmarshalStringSlice(task.WritesArtifacts)
		allowed := make(map[string]bool, len(declared))
		for _, p := range declared {
			allowed[p] = true
		}
		for _, path := range req.ArtifactsWritten {
			if err := validateArtifactPath(path); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid artifact path %q: %v", path, err))
				return
			}
			if !allowed[path] {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("artifact %q not in writes_artifacts for this task", path))
				return
			}
		}
	}

	// The client computed the result_path itself. Verify it's the
	// expected form for this task so we don't accept arbitrary
	// paths.
	// Expected result layout since iteration A.5 — projects are
	// namespaced by project ID so two projects sharing a remote
	// don't collide on runs/1/foo/... etc.
	expectedResultPath := fmt.Sprintf("projects/%d/runs/%d", run.ProjectID, run.Seq)
	if task.InstanceKey != "" {
		expectedResultPath += "/" + task.InstanceKey
	}
	expectedResultPath += "/" + task.TaskDefID
	if req.ResultPath != "" && req.ResultPath != expectedResultPath {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("result_path %q does not match expected %q for this task", req.ResultPath, expectedResultPath))
		return
	}
	resultPath := expectedResultPath

	// Update state machine — this also updates task_claims and
	// citizen score counters, same as the legacy path.
	if err := s.store.SubmitTaskResult(taskID, resultPath, req.CommitSHA, req.TokensUsed); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update task: "+err.Error())
		return
	}

	// Update artifacts index. All reported paths share the same
	// commit SHA because the client committed them atomically.
	if len(req.ArtifactsWritten) > 0 {
		now := time.Now()
		for _, path := range req.ArtifactsWritten {
			if err := s.store.UpsertArtifact(&store.ArtifactRecord{
				ProjectID:  run.ProjectID,
				Path:       path,
				LastWriter: task.ClaimedBy,
				LastTaskID: taskID,
				LastRunID:  task.RunID,
				CommitSHA:  req.CommitSHA,
				CreatedAt:  now,
				UpdatedAt:  now,
			}); err != nil {
				s.logger.Error("upserting artifact index", "path", path, "error", err)
				// Don't fail the request — git is the source of
				// truth, the index is just a cache.
			}
		}
	}

	// Update ready tasks — newly unblocked tasks become READY.
	readied, _ := s.store.UpdateReadyTasks(task.RunID)

	// Cross-run artifact propagation, same as the legacy path.
	if len(req.ArtifactsWritten) > 0 {
		otherRuns := map[int64]bool{}
		for _, path := range req.ArtifactsWritten {
			readers, err := s.store.ListTasksReadingArtifact(run.ProjectID, path, false)
			if err != nil {
				s.logger.Warn("listing cross-run readers", "path", path, "error", err)
				continue
			}
			for _, rd := range readers {
				if rd.RunID != task.RunID {
					otherRuns[rd.RunID] = true
				}
			}
		}
		for rid := range otherRuns {
			if n, err := s.store.UpdateReadyTasks(rid); err == nil {
				readied += n
			}
		}
	}

	// Mark run completed if all tasks accepted.
	completed, _ := s.store.CheckAndCompleteRun(task.RunID)
	if completed {
		s.logger.Info("run completed", "run_id", task.RunID)
	}

	s.logger.Info("result reported",
		"task_id", taskID,
		"path", resultPath,
		"commit", req.CommitSHA,
		"newly_ready", readied,
	)

	resp := map[string]interface{}{
		"status":      "accepted",
		"result_path": resultPath,
		"commit_sha":  req.CommitSHA,
		"newly_ready": readied,
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
	return parsed.DAG, nil
}

func (s *Server) handleInvalidateTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req invalidateRequest
	json.NewDecoder(r.Body).Decode(&req)

	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	// Load the DAG lazily. After a coordinator restart the in-memory
	// cache is empty — we rehydrate from the run's stored YAML on
	// first access so features like this stay restart-safe.
	d, err := s.getOrLoadDAG(task.RunID)
	if err != nil {
		s.logger.Error("loading DAG for invalidation", "run_id", task.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load DAG")
		return
	}

	// The DAG indexes nodes by {instanceKey:taskDefID} short form, not
	// the fully-qualified store form ({projectID:runSeq:...}). Convert
	// on the way in and out so the cascade walk matches the right
	// nodes, then returns store-form IDs the client can actually use.
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}
	runPrefix := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq)
	dagNodeID := enjuYaml.MakeFullID(task.InstanceKey, task.TaskDefID)

	shortDescendants := d.Descendants(dagNodeID)
	descendants := make([]string, 0, len(shortDescendants))
	for _, short := range shortDescendants {
		descendants = append(descendants, runPrefix+short)
	}

	// --- Build the invalidation set ---
	//
	// Two sources of cascading:
	//  1. Intra-run DAG descendants (target's downstream via
	//     {{task.content}} references or explicit depends_on).
	//  2. Cross-run artifact readers — tasks in any run whose
	//     reads_artifacts contains a path written by a task in the
	//     invalidation set, where that task is currently the
	//     most recent accepted writer of the path.
	//
	// We build the full set of affected task IDs before any state
	// transitions so the rollback walker and the store transition
	// get a consistent view.
	invalidatedSet := make(map[string]bool, 1+len(descendants))
	invalidatedSet[taskID] = true
	for _, d := range descendants {
		invalidatedSet[d] = true
	}

	// Collect unique artifact paths written by any task in the
	// initial invalidation set. These are the candidate rollback
	// targets.
	writtenPaths := make([]string, 0)
	seenPath := make(map[string]bool)
	collectWrites := func(t *store.TaskRecord) {
		for _, p := range unmarshalStringSlice(t.WritesArtifacts) {
			if !seenPath[p] {
				seenPath[p] = true
				writtenPaths = append(writtenPaths, p)
			}
		}
	}
	collectWrites(task)
	for _, descID := range descendants {
		dt, err := s.store.GetTask(descID)
		if err != nil || dt == nil {
			continue
		}
		collectWrites(dt)
	}

	// --- Cross-run cascade via artifact edges ---
	//
	// For each path in writtenPaths, check whether the artifact's
	// current last_task_id is still in the invalidated set. If so,
	// every ACCEPTED task in the project that reads that path will
	// see the artifact rolled back and must be cascaded.
	//
	// Note: this is non-recursive — we cascade direct readers, not
	// their intra-run descendants. That's a known limitation; see
	// the buildout plan.
	var crossRunReaders []string
	affectedRunIDs := map[int64]bool{task.RunID: true}
	for _, p := range writtenPaths {
		art, _ := s.store.GetArtifact(run.ProjectID, p)
		if art == nil {
			continue
		}
		if !invalidatedSet[art.LastTaskID] {
			continue
		}
		readers, err := s.store.ListTasksReadingArtifact(run.ProjectID, p, true)
		if err != nil {
			s.logger.Warn("listing artifact readers", "path", p, "error", err)
			continue
		}
		for _, r := range readers {
			if invalidatedSet[r.ID] {
				continue
			}
			invalidatedSet[r.ID] = true
			crossRunReaders = append(crossRunReaders, r.ID)
			affectedRunIDs[r.RunID] = true
		}
	}

	// --- DB-only artifact rollback (iteration A orchestrator model) ---
	//
	// Phase 1's rollback walked git history to rewrite files in a
	// coordinator-owned working tree. The orchestrator model never
	// rewrites git history; instead, git is an immutable append log
	// and the "current" version of any artifact is a DB pointer
	// (artifacts.last_task_id / commit_sha). Invalidation finds a
	// new pointer for each affected path: the most recent task in
	// the same project whose state is ACCEPTED and which is not in
	// the invalidated set, and which declared the path in its
	// writes_artifacts. If no such task exists, the index row is
	// deleted — the artifact's content may still live in git
	// history but the DB no longer knows where.
	type rollbackOutcome struct {
		Path              string
		Deleted           bool
		RestoredFromTask  string
		RestoredCommitSHA string
	}
	var rollbacks []rollbackOutcome
	for _, p := range writtenPaths {
		art, _ := s.store.GetArtifact(run.ProjectID, p)
		if art == nil || !invalidatedSet[art.LastTaskID] {
			// Current pointer isn't one of the invalidated tasks;
			// leave the index alone.
			continue
		}
		// Find the next-most-recent prior writer (still ACCEPTED,
		// not in invalidated set). The store doesn't have a
		// dedicated "prior writers of this path" query; we iterate
		// all tasks in the project that declare this path in
		// writes_artifacts, filter to ACCEPTED + not-invalidated,
		// and pick the most recent by submitted_at.
		priorTasks, err := s.store.ListTasksWritingArtifact(run.ProjectID, p, true)
		if err != nil {
			s.logger.Warn("listing prior writers", "path", p, "error", err)
			continue
		}
		var pick *store.TaskRecord
		for i := range priorTasks {
			t := &priorTasks[i]
			if invalidatedSet[t.ID] {
				continue
			}
			if t.CommitSHA == "" {
				// Legacy rows without a commit SHA can't be used
				// as a rollback target in the new model.
				continue
			}
			if pick == nil || (t.SubmittedAt != nil && pick.SubmittedAt != nil && t.SubmittedAt.After(*pick.SubmittedAt)) {
				pick = t
			}
		}
		if pick == nil {
			// No prior writer — drop the index row.
			if err := s.store.DeleteArtifact(run.ProjectID, p); err != nil {
				s.logger.Warn("deleting artifact index row", "path", p, "error", err)
			}
			rollbacks = append(rollbacks, rollbackOutcome{Path: p, Deleted: true})
			continue
		}
		// Point the index at the prior writer's commit.
		now := time.Now()
		if err := s.store.UpsertArtifact(&store.ArtifactRecord{
			ProjectID:  run.ProjectID,
			Path:       p,
			LastWriter: pick.ClaimedBy,
			LastTaskID: pick.ID,
			LastRunID:  pick.RunID,
			CommitSHA:  pick.CommitSHA,
			CreatedAt:  now, // upsert preserves created_at via ON CONFLICT
			UpdatedAt:  now,
		}); err != nil {
			s.logger.Warn("updating artifact index after rollback", "path", p, "error", err)
		}
		rollbacks = append(rollbacks, rollbackOutcome{
			Path:              p,
			RestoredFromTask:  pick.ID,
			RestoredCommitSHA: pick.CommitSHA,
		})
	}

	// Combine DAG descendants + cross-run artifact readers into a
	// single cascade list for the store transition. The store treats
	// them uniformly (all → PENDING, claim fields cleared).
	cascadeIDs := make([]string, 0, len(descendants)+len(crossRunReaders))
	cascadeIDs = append(cascadeIDs, descendants...)
	cascadeIDs = append(cascadeIDs, crossRunReaders...)

	changed, err := s.store.InvalidateTask(taskID, cascadeIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Every affected run needs UpdateReadyTasks to reflect the new
	// state, and any run that was 'completed' should flip back to
	// 'active'. affectedRunIDs started with the target's run and was
	// augmented with each cross-run reader's run above.
	for runID := range affectedRunIDs {
		_, _ = s.store.UpdateReadyTasks(runID)
		if r, _ := s.store.GetRun(runID); r != nil && r.State == store.RunCompleted {
			_ = s.store.UpdateRunState(runID, store.RunActive)
		}
	}

	s.logger.Info("task invalidated",
		"task_id", taskID,
		"descendants", len(descendants),
		"cross_run_readers", len(crossRunReaders),
		"changed", changed,
		"artifacts_rolled_back", len(rollbacks),
		"reason", req.Reason,
	)

	resp := map[string]interface{}{
		"status":      "invalidated",
		"task_id":     taskID,
		"descendants": descendants,
		"changed":     changed,
		"reason":      req.Reason,
	}
	if len(crossRunReaders) > 0 {
		resp["artifact_readers"] = crossRunReaders
	}
	if len(rollbacks) > 0 {
		rbView := make([]map[string]interface{}, 0, len(rollbacks))
		for _, rb := range rollbacks {
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
func (s *Server) checkTaskAccess(task *store.TaskRecord, caller *store.CitizenRecord) error {
	// AssignTo narrows to a specific set of citizen usernames.
	if assignees := unmarshalStringSlice(task.AssignTo); len(assignees) > 0 {
		allowed := false
		for _, u := range assignees {
			if u == caller.Username {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("task is assigned to %s — you are not in the list", strings.Join(assignees, ", "))
		}
	}

	// RequireRole checks the caller's global citizens.role. Per-project
	// roles are a Phase 2 feature that depends on project membership.
	if task.RequireRole != "" {
		if caller.Role != task.RequireRole {
			return fmt.Errorf("task requires role %q — your role is %q", task.RequireRole, caller.Role)
		}
	}

	return nil
}

func (s *Server) toTaskResponse(t store.TaskRecord) taskResponse {
	// Look up the run to get project_id and run_seq, and the
	// project to get the remote URL so fat clients know whether
	// this task's content should be written locally (client-side)
	// or via the legacy coordinator-writes path.
	var projectID int64
	var runSeq int
	var remoteURL string
	if run, _ := s.store.GetRun(t.RunID); run != nil {
		projectID = run.ProjectID
		runSeq = run.Seq
		if p, _ := s.store.GetProject(projectID); p != nil {
			remoteURL = p.RemoteURL
		}
	}
	iterationLabel := ""
	if t.InstanceKey != "" {
		iterationLabel = formatIterationLabel(t.InstanceParams, t.InstanceKey)
	}
	return taskResponse{
		ID:               t.ID,
		RunID:            t.RunID,
		RunSeq:           runSeq,
		ProjectID:        projectID,
		ProjectRemoteURL: remoteURL,
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
	}
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
