// Package api provides the REST API for the Enju coordinator.
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
	enjuGit "github.com/enju-ai/enju/internal/git"
	"github.com/enju-ai/enju/internal/store"
	"github.com/enju-ai/enju/internal/template"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// Server holds the coordinator state and dependencies.
type Server struct {
	store    *store.Store
	dags     map[int64]*dag.DAG // runID -> DAG (in-memory for fast queries)
	runs     map[int64]*enjuYaml.ParsedRun
	registry *enjuGit.Registry
	logger   *slog.Logger
}

// NewServer creates a new API server.
func NewServer(st *store.Store, registry *enjuGit.Registry, logger *slog.Logger) *Server {
	return &Server{
		store:    st,
		dags:     make(map[int64]*dag.DAG),
		runs:     make(map[int64]*enjuYaml.ParsedRun),
		registry: registry,
		logger:   logger,
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
}

type projectResponse struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
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

	now := time.Now()
	id, err := s.store.CreateProject(&store.ProjectRecord{
		Name:        req.Name,
		Description: req.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create project: "+err.Error())
		return
	}

	// Initialize the per-project git repo. On failure, roll back the DB
	// row so the system doesn't end up with a project that has no repo.
	if _, err := s.registry.CreateProjectRepo(id, req.Name); err != nil {
		s.logger.Error("initializing project repo", "project_id", id, "error", err)
		if delErr := s.store.DeleteProject(id); delErr != nil {
			s.logger.Error("rolling back project after repo init failure",
				"project_id", id, "error", delErr)
		}
		writeError(w, http.StatusInternalServerError, "failed to initialize project repo: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, projectResponse{
		ID:        id,
		Name:      req.Name,
		CreatedAt: now.Format(time.RFC3339),
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
		resp = append(resp, projectResponse{
			ID:          p.ID,
			Name:        p.Name,
			Description: p.Description,
			RunCount:    len(runs),
			CreatedAt:   p.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	p, err := s.store.GetProject(projectID)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	runs, _ := s.store.ListRunsByProject(p.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":          p.ID,
		"name":        p.Name,
		"description": p.Description,
		"run_count":   len(runs),
		"created_at":  p.CreatedAt.Format(time.RFC3339),
	})
}

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

	gw, err := s.registry.For(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to access project repo")
		return
	}

	content, ok, err := readArtifact(gw, path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read artifact")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}

	meta, _ := s.store.GetArtifact(projectID, path)
	resp := map[string]interface{}{
		"path":    path,
		"content": content,
	}
	if meta != nil {
		resp["last_writer"] = s.citizenUsername(meta.LastWriter)
		resp["last_task_id"] = meta.LastTaskID
		resp["last_run_id"] = meta.LastRunID
		resp["updated_at"] = meta.UpdatedAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
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
	for instanceKey, tasks := range parsed.ExpandedTasks {
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

			// Build depends_on as comma-separated full IDs with run prefix
			var deps []string
			for _, dep := range ti.DependsOn {
				deps = append(deps, runPrefix+enjuYaml.MakeFullID(instanceKey, dep))
			}

			// Determine initial state
			state := store.TaskPending
			if len(ti.DependsOn) == 0 {
				state = store.TaskReady
			}

			err := s.store.CreateTask(&store.TaskRecord{
				ID:          runPrefix + ti.FullID,
				RunID:       runID,
				Seq:         taskSeq,
				TaskDefID:   ti.ID,
				InstanceKey: instanceKey,
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
	ID              string   `json:"id"`
	RunID           int64    `json:"run_id"`     // global run ID
	RunSeq          int      `json:"run_seq"`    // per-project run sequence
	ProjectID       int64    `json:"project_id"` // parent project
	Seq             int      `json:"seq"`        // task sequence within run
	TaskDefID       string   `json:"task_def_id"`
	InstanceKey     string   `json:"instance_key,omitempty"`
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

func (s *Server) handleGetTaskInputs(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	// Look up the run's project so we know which repo to read from.
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}
	gw, err := s.registry.For(run.ProjectID)
	if err != nil {
		s.logger.Error("getting project writer", "project_id", run.ProjectID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to access project repo")
		return
	}

	// Collect upstream task results (if any deps).
	inputs := make(map[string]interface{})
	if task.DependsOn != "" {
		deps := strings.Split(task.DependsOn, ",")
		for _, depID := range deps {
			depID = strings.TrimSpace(depID)
			depTask, err := s.store.GetTask(depID)
			if err != nil || depTask == nil {
				continue
			}

			// depTask.ResultPath is repo-relative (e.g. "runs/2/foundation").
			if depTask.ResultPath != "" {
				result, err := readResultForTemplate(gw, depTask.ResultPath, depTask.TaskDefID)
				if err != nil {
					s.logger.Warn("reading upstream result", "path", depTask.ResultPath, "error", err)
					inputs[depTask.TaskDefID] = map[string]interface{}{
						"status": "error",
						"error":  err.Error(),
					}
					continue
				}
				inputs[depTask.TaskDefID] = result
			}
		}
	}

	// Collect declared artifact reads (snapshot at claim time — last
	// write wins, see Phase C plan). Missing paths are tracked
	// separately so the claim response can warn about declared
	// inputs that don't exist on disk — a state that can happen
	// legitimately after a cascade rollback that deleted the
	// artifact, or when a YAML author declared a read to a path
	// that was never written.
	artifacts := make(map[string]string)
	var missingArtifacts []string
	for _, p := range unmarshalStringSlice(task.ReadsArtifacts) {
		content, ok, err := readArtifact(gw, p)
		if err != nil {
			s.logger.Warn("reading artifact", "path", p, "error", err)
			missingArtifacts = append(missingArtifacts, p)
			continue
		}
		if ok {
			artifacts[p] = content
		} else {
			missingArtifacts = append(missingArtifacts, p)
		}
	}

	// Resolve {{task.field}} first, then {{artifact:path}}.
	// Unresolved {{artifact:...}} references stay literal in the
	// resolved prompt as a visible secondary signal that the input
	// was missing.
	resolvedPrompt := template.ResolveUpstream(task.Prompt, inputs)
	resolvedPrompt = template.ResolveArtifacts(resolvedPrompt, artifacts)

	resp := map[string]interface{}{
		"task_id":         taskID,
		"resolved_prompt": resolvedPrompt,
		"inputs":          inputs,
	}
	if len(artifacts) > 0 {
		resp["artifacts"] = artifacts
	}
	if len(missingArtifacts) > 0 {
		resp["missing_artifacts"] = missingArtifacts
	}
	writeJSON(w, http.StatusOK, resp)
}

type submitResultRequest struct {
	Content    string            `json:"content"`              // text content (for simple results)
	Outputs    map[string]string `json:"outputs,omitempty"`    // named outputs — values are content strings
	Artifacts  map[string]string `json:"artifacts,omitempty"`  // user-facing path -> new content
	ResultType string            `json:"result_type,omitempty"`
	TokensUsed int64             `json:"tokens_used,omitempty"`
	Model      string            `json:"model,omitempty"`
}

func (s *Server) handleSubmitResult(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req submitResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	// Write result as separate files (content + metadata)
	resultType := req.ResultType
	if resultType == "" {
		resultType = "text"
	}

	metadata := map[string]interface{}{
		"task_id":     taskID,
		"citizen":     task.ClaimedBy,
		"model":       req.Model,
		"tokens_used": req.TokensUsed,
		"result_type": resultType,
		"timestamp":   time.Now().Format(time.RFC3339),
	}

	// Look up the run to find which project's repo to write into.
	run, runErr := s.store.GetRun(task.RunID)
	if runErr != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}

	gw, err := s.registry.For(run.ProjectID)
	if err != nil {
		s.logger.Error("getting project writer", "project_id", run.ProjectID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to access project repo")
		return
	}

	// Hold the per-project writer lock across all writes + commit + push
	// so the submission lands as one atomic git commit.
	gw.Lock()
	defer gw.Unlock()

	var resultPath string

	if len(req.Outputs) > 0 {
		// Parse task's output schema to know file layouts
		schema := map[string]outputFileSpec{}
		if task.Outputs != "" {
			var rawSchema map[string]interface{}
			if err := json.Unmarshal([]byte(task.Outputs), &rawSchema); err == nil {
				for name, v := range rawSchema {
					switch val := v.(type) {
					case string:
						schema[name] = outputFileSpec{Description: val}
					case map[string]interface{}:
						spec := outputFileSpec{}
						if d, ok := val["Description"].(string); ok {
							spec.Description = d
						}
						if f, ok := val["File"].(string); ok {
							spec.File = f
						}
						if fmt, ok := val["Format"].(string); ok {
							spec.Format = fmt
						}
						schema[name] = spec
					}
				}
			}
		}

		// Check if any output has a file declared
		hasFileOutputs := false
		for _, s := range schema {
			if s.File != "" {
				hasFileOutputs = true
				break
			}
		}

		if hasFileOutputs {
			metadata["named_outputs"] = true
			resultPath, err = writeMultiFileResult(gw, run.Seq, task.InstanceKey, task.TaskDefID, schema, req.Outputs, metadata)
		} else {
			outputsJSON, _ := json.MarshalIndent(req.Outputs, "", "  ")
			metadata["named_outputs"] = true
			resultPath, err = writeResult(gw, run.Seq, task.InstanceKey, task.TaskDefID, string(outputsJSON), "json", metadata)
		}
	} else {
		resultPath, err = writeResult(gw, run.Seq, task.InstanceKey, task.TaskDefID, req.Content, resultType, metadata)
	}
	if err != nil {
		s.logger.Error("writing result", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to write result")
		return
	}

	// --- Artifacts ---
	// writes_artifacts is an upper bound: the citizen MAY write any
	// subset of declared paths, but never anything outside the list.
	writtenArtifacts := []string{}
	if len(req.Artifacts) > 0 {
		declared := unmarshalStringSlice(task.WritesArtifacts)
		allowed := make(map[string]bool, len(declared))
		for _, p := range declared {
			allowed[p] = true
		}
		// Validate every submitted artifact: must be in the allow-list,
		// must pass path validation. Validate all up front so a bad
		// submission fails before any writes happen.
		for path := range req.Artifacts {
			if err := validateArtifactPath(path); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid artifact path %q: %v", path, err))
				return
			}
			if !allowed[path] {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("artifact %q not in writes_artifacts for this task", path))
				return
			}
		}
		// All good — write each one to the project repo.
		for path, content := range req.Artifacts {
			if err := writeArtifact(gw, path, []byte(content)); err != nil {
				s.logger.Error("writing artifact", "path", path, "error", err)
				writeError(w, http.StatusInternalServerError, "failed to write artifact "+path)
				return
			}
			writtenArtifacts = append(writtenArtifacts, path)
		}
	}

	claimerUsername := s.citizenUsername(task.ClaimedBy)
	commitMsg := fmt.Sprintf("Task %s by @%s: result", taskID, claimerUsername)
	if len(writtenArtifacts) > 0 {
		commitMsg = fmt.Sprintf("Task %s by @%s: result + %d artifact(s)\n\nArtifacts: %s",
			taskID, claimerUsername, len(writtenArtifacts), strings.Join(writtenArtifacts, ", "))
	}
	if err := gw.CommitAndPush(commitMsg); err != nil {
		s.logger.Warn("git commit/push failed", "error", err)
	}

	// Upsert artifact index rows AFTER the commit succeeds, so the DB
	// only reflects state that actually made it into git.
	if len(writtenArtifacts) > 0 {
		now := time.Now()
		for _, path := range writtenArtifacts {
			if err := s.store.UpsertArtifact(&store.ArtifactRecord{
				ProjectID:  run.ProjectID,
				Path:       path,
				LastWriter: task.ClaimedBy,
				LastTaskID: taskID,
				LastRunID:  task.RunID,
				CreatedAt:  now,
				UpdatedAt:  now,
			}); err != nil {
				s.logger.Error("upserting artifact index", "path", path, "error", err)
				// Don't fail the request — git is the source of truth.
			}
		}
	}

	// Update task state
	if err := s.store.SubmitTaskResult(taskID, resultPath, req.TokensUsed); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update task: "+err.Error())
		return
	}

	// Update ready tasks — newly unblocked tasks become READY
	readied, _ := s.store.UpdateReadyTasks(task.RunID)

	// Check if all tasks are done — mark run as completed
	completed, _ := s.store.CheckAndCompleteRun(task.RunID)
	if completed {
		s.logger.Info("run completed", "run_id", task.RunID)
	}

	s.logger.Info("result submitted", "task_id", taskID, "path", resultPath, "newly_ready", readied)

	resp := map[string]interface{}{
		"status":       "accepted",
		"result_path":  resultPath,
		"newly_ready":  readied,
	}
	if len(writtenArtifacts) > 0 {
		resp["artifacts_written"] = writtenArtifacts
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

	// --- Artifact rollback ---
	//
	// Roll back declared writes to a valid prior version (a commit
	// whose author task is currently ACCEPTED and not in the
	// invalidated set). The walker's accepted-state check catches
	// previously-invalidated tasks — a bug from iteration 3.1 where
	// the walker would resurrect ghost revisions.
	var rollbacks []artifactRollback
	if len(writtenPaths) > 0 {
		gw, err := s.registry.For(run.ProjectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to access project repo")
			return
		}
		gw.Lock()
		defer gw.Unlock()

		rollbacks, err = rollbackArtifactsForInvalidation(gw, s.store, invalidatedSet, writtenPaths)
		if err != nil {
			s.logger.Error("rolling back artifacts", "task_id", taskID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to roll back artifacts: "+err.Error())
			return
		}

		// Commit the rollback as one atomic git commit so the history
		// clearly shows where the artifact state came from.
		commitMsg := fmt.Sprintf("Rollback: invalidated %s", taskID)
		if req.Reason != "" {
			commitMsg += " — " + req.Reason
		}
		if len(rollbacks) > 0 {
			parts := make([]string, 0, len(rollbacks))
			for _, rb := range rollbacks {
				if rb.Deleted {
					parts = append(parts, rb.Path+" (deleted)")
				} else {
					parts = append(parts, rb.Path+" ← "+rb.RestoredTask)
				}
			}
			commitMsg += "\n\nArtifacts: " + strings.Join(parts, ", ")
		}
		if err := gw.Commit(commitMsg); err != nil {
			s.logger.Error("committing rollback", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to commit rollback")
			return
		}
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

	// Update the artifacts index rows to match the rolled-back state.
	// For rolled-back files we point `last_writer` at the task we
	// restored from; for deleted files we drop the index row entirely.
	// If the index update fails we log and move on — git is the source
	// of truth, the index is just a cache.
	now := time.Now()
	for _, rb := range rollbacks {
		if rb.Deleted {
			if err := s.store.DeleteArtifact(run.ProjectID, rb.Path); err != nil {
				s.logger.Warn("deleting artifact index row", "path", rb.Path, "error", err)
			}
			continue
		}
		// Find the citizen id for the restored owner username so the
		// artifacts.last_writer int fk stays correct.
		var writerID int64
		if rb.RestoredOwner != "" {
			if c, _ := s.store.GetCitizenByUsername(rb.RestoredOwner); c != nil {
				writerID = c.ID
			}
		}
		if err := s.store.UpsertArtifact(&store.ArtifactRecord{
			ProjectID:  run.ProjectID,
			Path:       rb.Path,
			LastWriter: writerID,
			LastTaskID: rb.RestoredTask,
			LastRunID:  task.RunID, // best effort — we don't re-lookup the original run
			CreatedAt:  now,        // upsert preserves created_at via ON CONFLICT
			UpdatedAt:  now,
		}); err != nil {
			s.logger.Warn("updating artifact index after rollback", "path", rb.Path, "error", err)
		}
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
				item["restored_from_task"] = rb.RestoredTask
				item["restored_from_commit"] = rb.RestoredHash
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

	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
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
	// Look up the run to get project_id and run_seq
	var projectID int64
	var runSeq int
	if run, _ := s.store.GetRun(t.RunID); run != nil {
		projectID = run.ProjectID
		runSeq = run.Seq
	}
	return taskResponse{
		ID:              t.ID,
		RunID:           t.RunID,
		RunSeq:          runSeq,
		ProjectID:       projectID,
		Seq:             t.Seq,
		TaskDefID:       t.TaskDefID,
		InstanceKey:     t.InstanceKey,
		Ref:             t.Ref,
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
