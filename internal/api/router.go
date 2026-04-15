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
				ReviewsTarget:   ti.Reviews,
				VoteOptions:     marshalVoteOptions(ti.Options),
				Citizens:        ti.Citizens,
				MinQuorum:       ti.MinQuorum,
				VoteThreshold:   ti.Threshold,
				VoteDeadline:    ti.Deadline,
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
	ReviewsTarget   string   `json:"reviews_target,omitempty"`   // Phase E: target task id this review evaluates
	ReviewDecision  string   `json:"review_decision,omitempty"`  // Phase E: approve/reject once submitted
	VoteOptions     string   `json:"vote_options,omitempty"`     // Phase E.2: declared options JSON
	VoteChoice      string   `json:"vote_choice,omitempty"`      // Phase E.2: winning option id
	Citizens        int      `json:"citizens,omitempty"`         // Phase E.2: invited voter count
	MinQuorum       int      `json:"min_quorum,omitempty"`       // Phase E.2: required submitted count
	VoteThreshold   string   `json:"vote_threshold,omitempty"`   // Phase E.2: agreement rule
	VoteDeadline    string   `json:"vote_deadline,omitempty"`    // Phase E.2: voting-closes duration
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
			depEntry := map[string]interface{}{
				"task_def_id":     depTask.TaskDefID,
				"instance_key":    depTask.InstanceKey,
				"instance_params": params,
				"commit_sha":      depTask.CommitSHA,
				"result_path":     depTask.ResultPath,
				// Phase E.2: upstream vote tasks carry their
				// winning option id so downstream prompts can
				// reference it via {{task.winning_option}}.
				// Non-vote upstreams send an empty string.
				"vote_choice":     depTask.VoteChoice,
			}
			// Phase E.2 session 2b — multi-citizen upstreams
			// (vote or review with citizens > 1) also carry
			// the per-citizen submission list so downstream
			// prompts can render {{task.responses}}. Only
			// populated when the upstream has citizens > 1
			// and has resolved, otherwise there's nothing to
			// render.
			if depTask.Citizens > 1 {
				if submissions, err := s.store.ListVoteSubmissions(depTask.ID); err == nil && len(submissions) > 0 {
					perCitizen := make([]map[string]interface{}, 0, len(submissions))
					for _, sub := range submissions {
						perCitizen = append(perCitizen, map[string]interface{}{
							"username": s.citizenUsername(sub.CitizenID),
							"option":   sub.Option,
							"content":  sub.Content,
						})
					}
					depEntry["responses"] = perCitizen
				}
			}
			deps = append(deps, depEntry)
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
	// Multi-citizen tasks (Phase E.2 session 2a) write each
	// submission into its own `citizen-<username>/` subdirectory
	// under the task's base result path. The submitted result_path
	// is expected to be either the base (session 1 single-citizen
	// shape) or base + citizen subdir (multi-citizen shape).
	if req.ResultPath != "" && req.ResultPath != expectedResultPath {
		allowedCitizenSubdir := false
		if task.Citizens > 1 && strings.HasPrefix(req.ResultPath, expectedResultPath+"/citizen-") {
			allowedCitizenSubdir = true
		}
		if !allowedCitizenSubdir {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("result_path %q does not match expected %q for this task", req.ResultPath, expectedResultPath))
			return
		}
	}
	// Store the task-level result path (the common parent) on
	// the tasks row so downstream template resolution can find
	// the base dir. Per-citizen subdirs live underneath it.
	resultPath := expectedResultPath

	// Review tasks must carry an explicit decision; vote tasks
	// must carry an option id. Non-matching tasks that happen to
	// send either field are tolerated — the columns stay empty
	// for them because we only persist per-action.
	decision := ""
	voteChoice := ""
	if task.Action == "review" {
		switch req.Decision {
		case "approve", "reject":
			decision = req.Decision
		case "":
			writeError(w, http.StatusBadRequest, `decision is required on action:review tasks (must be "approve" or "reject")`)
			return
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf(`decision %q is invalid (must be "approve" or "reject")`, req.Decision))
			return
		}
	}
	if task.Action == "vote" {
		// Validate the option id against the declared list on the
		// task row. VoteOptions stores the JSON-encoded options
		// array from YAML; we re-decode here instead of piping
		// through yaml.TaskDef so the router has one source of
		// truth.
		var declared []struct {
			ID        string   `json:"id"`
			Label     string   `json:"label,omitempty"`
			Activates []string `json:"activates,omitempty"`
		}
		if err := json.Unmarshal([]byte(task.VoteOptions), &declared); err != nil || len(declared) == 0 {
			writeError(w, http.StatusInternalServerError, "vote task has no declared options — this is a storage inconsistency")
			return
		}
		known := make([]string, len(declared))
		for i, o := range declared {
			known[i] = o.ID
		}
		if req.Option == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(`option is required on action:vote tasks (must be one of: %s)`, strings.Join(known, ", ")))
			return
		}
		ok := false
		for _, id := range known {
			if id == req.Option {
				ok = true
				break
			}
		}
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(`option %q is invalid (must be one of: %s)`, req.Option, strings.Join(known, ", ")))
			return
		}
		voteChoice = req.Option
	}

	// Resolve the submitting citizen. For single-citizen tasks
	// this is optional — tasks.claimed_by tells us who has the
	// exclusive claim. For multi-citizen tasks the caller MUST
	// identify themselves so the right task_claims slot gets
	// credited.
	var submitterID int64
	if task.Citizens > 1 {
		if req.Username == "" {
			writeError(w, http.StatusBadRequest, "username is required on multi-citizen task submissions")
			return
		}
		citizen, err := s.store.GetCitizenByUsername(req.Username)
		if err != nil || citizen == nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown citizen %q", req.Username))
			return
		}
		submitterID = citizen.ID
	} else {
		submitterID = task.ClaimedBy
	}

	// Update state machine. For single-citizen tasks this also
	// transitions to ACCEPTED in one shot; for multi-citizen
	// tasks it records the citizen's vote and transitions to
	// COLLECTING (tally runs below).
	submitRes, err := s.store.SubmitTaskResult(taskID, submitterID, resultPath, req.CommitSHA, decision, voteChoice, req.Content, req.TokensUsed)
	if err != nil {
		// Terminal-state rejections (late submits) are a
		// client-visible 400, not a server-side 500 — the
		// caller raced with the tally and lost. Everything
		// else is still a 500.
		if strings.Contains(err.Error(), "already resolved") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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

	// Cross-run artifact propagation, same as the legacy path.
	// We collect affected runs now but defer the readiness sweep
	// until AFTER any action-specific cascades (review reject,
	// vote skip) have run. Otherwise the initial readiness pass
	// would promote losing-branch tasks to READY and the skip
	// cascade would immediately flip them back to SKIPPED — the
	// user-facing `readied` count would double-count those as
	// "newly unlocked" even though they never actually entered
	// the work queue.
	otherRuns := map[int64]bool{}
	if len(req.ArtifactsWritten) > 0 {
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
	}

	// Review resolution — three paths:
	//
	//   1. Single-reviewer review (citizens = 1): SubmitTaskResult
	//      already flipped the task to ACCEPTED. If the decision
	//      was "reject", fire the invalidation cascade on the
	//      target task directly. (Session E.1 behavior.)
	//
	//   2. Multi-reviewer review (citizens > 1): SubmitTaskResult
	//      recorded the reviewer's verdict on their task_claims
	//      row and moved the task to COLLECTING. Run the review
	//      tally: any-reject-kills means the first reject
	//      resolves the task as rejected immediately; all-approve
	//      with quorum met resolves as accepted.
	//
	// The existing review-reject cascade machinery is reused for
	// both paths — the difference is just who triggers it (one
	// reviewer vs. the tally on the Nth vote).
	var rejectResult *invalidationResult
	var reviewTally *reviewTallyOutcome
	if task.Action == "review" && task.ReviewsTarget != "" {
		shouldReject := false
		shouldAccept := false

		if submitRes != nil && submitRes.Resolved {
			// Single-reviewer — decision already committed to
			// tasks.review_decision. Fire the cascade on reject.
			shouldReject = decision == "reject"
			shouldAccept = decision == "approve"
		} else if submitRes != nil && submitRes.Collecting {
			// Multi-reviewer — run the tally.
			outcome, err := s.evaluateReviewTally(task)
			if err != nil {
				s.logger.Error("review tally failed", "review_task", taskID, "error", err)
			} else {
				reviewTally = outcome
				if outcome != nil && outcome.Resolved {
					if outcome.Verdict == "reject" {
						shouldReject = true
						// Roll up the tally verdict into the
						// task record so the formatter can see
						// "✗ rejected by majority/any-reject".
						_ = s.store.ResolveMultiCitizenReview(taskID, "reject", req.CommitSHA)
					} else {
						shouldAccept = true
						_ = s.store.ResolveMultiCitizenReview(taskID, "approve", req.CommitSHA)
					}
				}
			}
		}

		if shouldReject {
			targetFullID := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq) + task.ReviewsTarget
			res, err := s.performInvalidate(targetFullID)
			if err != nil {
				s.logger.Error("review-reject cascade failed",
					"review_task", taskID, "target", targetFullID, "error", err)
			} else {
				rejectResult = res
				s.logger.Info("review rejected target task",
					"review_task", taskID,
					"target", targetFullID,
					"descendants", len(res.Descendants),
					"changed", res.Changed,
				)
			}
		}
		_ = shouldAccept // reserved for future "approve side-effect" hook
	}

	// Vote resolution. Two paths:
	//
	//   1. Single-voter vote (citizens = 1): SubmitTaskResult
	//      already flipped the task to ACCEPTED with voteChoice
	//      as the winning option. We fire the skip cascade here.
	//
	//   2. Multi-voter vote (citizens > 1): SubmitTaskResult
	//      recorded the citizen's vote and moved the task to
	//      COLLECTING. We run the tally now — if it resolves,
	//      we flip to ACCEPTED + fire the skip cascade. If not,
	//      we leave the task in COLLECTING for more submissions.
	var skipResult *skipCascadeResult
	var tallyOutcome *voteTallyOutcome
	if task.Action == "vote" {
		if submitRes != nil && submitRes.Resolved && voteChoice != "" {
			// Single-voter fast path — SubmitTaskResult already
			// transitioned to ACCEPTED. Fire the cascade.
			res, err := s.performSkipCascade(task, voteChoice)
			if err != nil {
				s.logger.Error("vote skip cascade failed",
					"vote_task", taskID, "choice", voteChoice, "error", err)
			} else {
				skipResult = res
			}
		} else if submitRes != nil && submitRes.Collecting {
			// Multi-voter collecting path — run the tally.
			outcome, err := s.evaluateVoteTally(task)
			if err != nil {
				s.logger.Error("vote tally failed",
					"vote_task", taskID, "error", err)
			} else {
				tallyOutcome = outcome
				if outcome != nil && outcome.Resolved {
					// Winning option found — transition to
					// ACCEPTED and fire the skip cascade.
					if err := s.store.ResolveMultiCitizenVote(taskID, outcome.WinningOption, req.CommitSHA); err != nil {
						s.logger.Error("resolving multi-citizen vote",
							"vote_task", taskID, "error", err)
					} else {
						// Re-load the task so performSkipCascade
						// sees the updated row with the winning
						// option set.
						updated, _ := s.store.GetTask(taskID)
						if updated != nil {
							res, err := s.performSkipCascade(updated, outcome.WinningOption)
							if err != nil {
								s.logger.Error("vote skip cascade failed",
									"vote_task", taskID, "error", err)
							} else {
								skipResult = res
							}
						}
					}
				}
			}
		}
	}

	// Now the action-specific cascades have settled, run the
	// readiness sweep. Losing-branch tasks are already SKIPPED so
	// they won't enter the "newly ready" count; only tasks that
	// genuinely transitioned from PENDING to READY get reported.
	readied, _ := s.store.UpdateReadyTasks(task.RunID)
	for rid := range otherRuns {
		if n, err := s.store.UpdateReadyTasks(rid); err == nil {
			readied += n
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

	status := "accepted"
	voteStillCollecting := tallyOutcome != nil && !tallyOutcome.Resolved
	reviewStillCollecting := reviewTally != nil && !reviewTally.Resolved
	if submitRes != nil && submitRes.Collecting && (voteStillCollecting || reviewStillCollecting || (tallyOutcome == nil && reviewTally == nil)) {
		status = "collecting"
	}
	resp := map[string]interface{}{
		"status":      status,
		"result_path": resultPath,
		"commit_sha":  req.CommitSHA,
		"newly_ready": readied,
	}
	if decision != "" {
		resp["decision"] = decision
	}
	if rejectResult != nil {
		resp["review_cascade"] = map[string]interface{}{
			"target":      task.ReviewsTarget,
			"descendants": rejectResult.Descendants,
			"changed":     rejectResult.Changed,
		}
	}
	if reviewTally != nil {
		resp["review_tally"] = map[string]interface{}{
			"resolved":      reviewTally.Resolved,
			"verdict":       reviewTally.Verdict,
			"approves":      reviewTally.Approves,
			"rejects":       reviewTally.Rejects,
			"total_reviews": reviewTally.TotalReviews,
			"reason":        reviewTally.Reason,
		}
	}
	if task.Action == "vote" {
		voteResp := map[string]interface{}{}
		if tallyOutcome != nil && tallyOutcome.Resolved {
			voteResp["winning_option"] = tallyOutcome.WinningOption
			voteResp["votes_tallied"] = tallyOutcome.TotalVotes
			voteResp["counts"] = tallyOutcome.Counts
		} else if submitRes != nil && submitRes.Resolved && voteChoice != "" {
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

// invalidationResult summarizes what performInvalidate actually
// changed on a single invocation. Used by the HTTP handler to
// render the response body and by the review-reject path in
// handleSubmitResultReport to log what happened.
type invalidationResult struct {
	Task            *store.TaskRecord
	Descendants     []string
	CrossRunReaders []string
	Changed         int
	Rollbacks       []rollbackOutcome
	AffectedRuns    map[int64]bool
}

type rollbackOutcome struct {
	Path              string
	Deleted           bool
	RestoredFromTask  string
	RestoredCommitSHA string
}

// performInvalidate is the shared cascade-invalidation implementation
// used by handleInvalidateTask (external API) and the review-reject
// path inside handleSubmitResultReport. Walks the DAG for
// intra-run descendants, rolls back the artifact index for
// cross-run readers, and calls Store.InvalidateTask to flip states
// atomically. Returns nil + error only on hard failures; soft
// failures (artifact index misses, etc.) are logged and the cascade
// continues.
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
	runPrefix := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq)
	dagNodeID := enjuYaml.MakeFullID(task.InstanceKey, task.TaskDefID)

	shortDescendants := d.Descendants(dagNodeID)
	descendants := make([]string, 0, len(shortDescendants))
	for _, short := range shortDescendants {
		descendants = append(descendants, runPrefix+short)
	}

	invalidatedSet := make(map[string]bool, 1+len(descendants))
	invalidatedSet[taskID] = true
	for _, dd := range descendants {
		invalidatedSet[dd] = true
	}

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

	var rollbacks []rollbackOutcome
	for _, p := range writtenPaths {
		art, _ := s.store.GetArtifact(run.ProjectID, p)
		if art == nil || !invalidatedSet[art.LastTaskID] {
			continue
		}
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
				continue
			}
			if pick == nil || (t.SubmittedAt != nil && pick.SubmittedAt != nil && t.SubmittedAt.After(*pick.SubmittedAt)) {
				pick = t
			}
		}
		if pick == nil {
			if err := s.store.DeleteArtifact(run.ProjectID, p); err != nil {
				s.logger.Warn("deleting artifact index row", "path", p, "error", err)
			}
			rollbacks = append(rollbacks, rollbackOutcome{Path: p, Deleted: true})
			continue
		}
		now := time.Now()
		if err := s.store.UpsertArtifact(&store.ArtifactRecord{
			ProjectID:  run.ProjectID,
			Path:       p,
			LastWriter: pick.ClaimedBy,
			LastTaskID: pick.ID,
			LastRunID:  pick.RunID,
			CommitSHA:  pick.CommitSHA,
			CreatedAt:  now,
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

	cascadeIDs := make([]string, 0, len(descendants)+len(crossRunReaders))
	cascadeIDs = append(cascadeIDs, descendants...)
	cascadeIDs = append(cascadeIDs, crossRunReaders...)

	changed, err := s.store.InvalidateTask(taskID, cascadeIDs)
	if err != nil {
		return nil, err
	}
	for runID := range affectedRunIDs {
		_, _ = s.store.UpdateReadyTasks(runID)
		if r, _ := s.store.GetRun(runID); r != nil && r.State == store.RunCompleted {
			_ = s.store.UpdateRunState(runID, store.RunActive)
		}
	}
	return &invalidationResult{
		Task:            task,
		Descendants:     descendants,
		CrossRunReaders: crossRunReaders,
		Changed:         changed,
		Rollbacks:       rollbacks,
		AffectedRuns:    affectedRunIDs,
	}, nil
}

// reviewTallyOutcome describes the result of evaluating a
// multi-reviewer review task's submissions against the default
// any-reject-kills policy: any "reject" vote immediately
// resolves the task as rejected; all "approve" votes (with
// quorum met) resolve as accepted; otherwise the task stays in
// COLLECTING waiting for more reviewers.
type reviewTallyOutcome struct {
	Resolved bool
	// Verdict is "approve" or "reject" when Resolved is true;
	// empty otherwise.
	Verdict      string
	Approves     int
	Rejects      int
	TotalReviews int
	Reason       string
}

// evaluateReviewTally walks the per-citizen review submissions
// and applies the any-reject-kills rule. If any reviewer rejected,
// the task resolves immediately as "reject" (the first dissenter
// wins under the default policy). Otherwise we check whether
// quorum has been met and every submission is "approve"; if so
// the task resolves as "approve." Missing quorum or a mixed
// incomplete set leaves the task in COLLECTING.
func (s *Server) evaluateReviewTally(task *store.TaskRecord) (*reviewTallyOutcome, error) {
	submissions, err := s.store.ListVoteSubmissions(task.ID)
	if err != nil {
		return nil, fmt.Errorf("listing review submissions: %w", err)
	}
	out := &reviewTallyOutcome{TotalReviews: len(submissions)}
	for _, sub := range submissions {
		switch sub.Option {
		case "approve":
			out.Approves++
		case "reject":
			out.Rejects++
		}
	}

	// Any reject immediately wins under the default dissent
	// policy. This matches real-world code review: one "you
	// can't ship this" kills the submission.
	if out.Rejects > 0 {
		out.Resolved = true
		out.Verdict = "reject"
		return out, nil
	}

	// No rejects so far. We need all reviewers to weigh in
	// before we can declare "approve." Quorum defaults to the
	// task's citizens count.
	needed := task.MinQuorum
	if needed <= 0 {
		needed = task.Citizens
		if needed <= 0 {
			needed = 1
		}
	}
	if out.Approves < needed {
		out.Reason = fmt.Sprintf("approvals not yet at quorum (%d of %d)", out.Approves, needed)
		return out, nil
	}
	out.Resolved = true
	out.Verdict = "approve"
	return out, nil
}

// voteTallyOutcome describes the result of evaluating the current
// set of submitted votes against a multi-citizen vote task's
// threshold + min_quorum + deadline rules. Resolved is true when
// the task should transition to ACCEPTED; WinningOption is set in
// that case. When Resolved is false the task stays in COLLECTING
// and waits for more submissions (or a deadline trigger).
type voteTallyOutcome struct {
	Resolved      bool
	WinningOption string
	// Counts is the per-option vote count at evaluation time,
	// useful for logging and formatter output.
	Counts map[string]int
	// TotalVotes is the number of submissions that contributed.
	TotalVotes int
	// Reason explains why a non-resolved tally didn't resolve
	// (e.g. "quorum not met", "threshold not met"). Empty on
	// resolved outcomes.
	Reason string
}

// evaluateVoteTally applies the task's threshold + quorum rules
// to the current set of submitted votes. It never mutates state
// — it only reports whether the submissions so far justify a
// resolution. The caller (handleSubmitResultReport) decides
// what to do with the result.
func (s *Server) evaluateVoteTally(task *store.TaskRecord) (*voteTallyOutcome, error) {
	submissions, err := s.store.ListVoteSubmissions(task.ID)
	if err != nil {
		return nil, fmt.Errorf("listing submissions: %w", err)
	}
	counts := make(map[string]int, len(submissions))
	for _, sub := range submissions {
		if sub.Option == "" {
			continue
		}
		counts[sub.Option]++
	}
	total := len(submissions)

	// Decode declared options in stable order for tie-breaking.
	var declared []struct {
		ID        string   `json:"id"`
		Label     string   `json:"label,omitempty"`
		Activates []string `json:"activates,omitempty"`
	}
	if task.VoteOptions != "" {
		_ = json.Unmarshal([]byte(task.VoteOptions), &declared)
	}

	// Quorum check. Explicit min_quorum takes precedence;
	// otherwise default to citizens count for multi-voter tasks
	// (wait for everyone) and 1 for single-voter tasks. This
	// matches the intuition "3 citizens can vote" → all 3
	// should vote before resolution unless the author opts
	// into a smaller quorum explicitly.
	minQuorum := task.MinQuorum
	if minQuorum <= 0 {
		if task.Citizens > 1 {
			minQuorum = task.Citizens
		} else {
			minQuorum = 1
		}
	}
	if total < minQuorum {
		return &voteTallyOutcome{
			Counts:     counts,
			TotalVotes: total,
			Reason:     fmt.Sprintf("quorum not met (%d of %d)", total, minQuorum),
		}, nil
	}

	// Pick a winner based on the threshold rule.
	threshold := task.VoteThreshold
	if threshold == "" {
		threshold = "plurality"
	}
	winner, reason := pickWinner(declared, counts, total, threshold)
	if winner == "" {
		return &voteTallyOutcome{
			Counts:     counts,
			TotalVotes: total,
			Reason:     reason,
		}, nil
	}
	return &voteTallyOutcome{
		Resolved:      true,
		WinningOption: winner,
		Counts:        counts,
		TotalVotes:    total,
	}, nil
}

// pickWinner returns the winning option id given the current
// counts and the threshold rule. Returns an empty string + a
// reason when no winner can be declared (tally stays open).
// Ties under plurality/majority are broken by the order in which
// options were declared in YAML — the first-declared option wins
// a tie.
func pickWinner(declared []struct {
	ID        string   `json:"id"`
	Label     string   `json:"label,omitempty"`
	Activates []string `json:"activates,omitempty"`
}, counts map[string]int, total int, threshold string) (string, string) {
	if total == 0 {
		return "", "no votes cast"
	}

	// Find max count.
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	if maxCount == 0 {
		return "", "no votes cast"
	}

	// Leaders in declaration order — first one is our tie-break
	// winner for plurality/majority.
	var leaders []string
	for _, opt := range declared {
		if counts[opt.ID] == maxCount {
			leaders = append(leaders, opt.ID)
		}
	}
	if len(leaders) == 0 {
		return "", "no declared option matched the top count"
	}
	winner := leaders[0]

	lower := strings.ToLower(threshold)
	switch {
	case lower == "plurality":
		return winner, ""
	case lower == "majority":
		if maxCount*2 > total {
			return winner, ""
		}
		return "", fmt.Sprintf("majority not met (%d of %d)", maxCount, total)
	case lower == "unanimous":
		if maxCount == total && len(leaders) == 1 {
			return winner, ""
		}
		return "", fmt.Sprintf("unanimous not met (%d of %d agree)", maxCount, total)
	case strings.HasPrefix(lower, "percent:"):
		pctStr := strings.TrimPrefix(lower, "percent:")
		pct, err := strconv.Atoi(pctStr)
		if err != nil || pct < 1 || pct > 100 {
			return "", fmt.Sprintf("invalid percent threshold %q", threshold)
		}
		// Need winner's share ≥ pct% of total.
		if maxCount*100 >= pct*total {
			return winner, ""
		}
		return "", fmt.Sprintf("percent:%d not met (%d of %d = %d%%)", pct, maxCount, total, (maxCount*100)/total)
	}
	return "", fmt.Sprintf("unknown threshold %q", threshold)
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

	// Flip them to SKIPPED in one go.
	if _, err := s.store.MarkTasksSkipped(skipIDs); err != nil {
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
	resp := taskResponse{
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
		ReviewsTarget:   t.ReviewsTarget,
		ReviewDecision:  t.ReviewDecision,
		VoteOptions:     t.VoteOptions,
		VoteChoice:      t.VoteChoice,
		Citizens:        t.Citizens,
		MinQuorum:       t.MinQuorum,
		VoteThreshold:   t.VoteThreshold,
		VoteDeadline:    t.VoteDeadline,
	}
	// Phase E.2 session 2a/2b — surface per-citizen claim and
	// submission state for multi-citizen vote AND review tasks
	// so MCP formatters can render the Voting / Review block
	// without an extra round trip.
	if (t.Action == "vote" || t.Action == "review") && t.Citizens > 1 {
		if submissions, err := s.store.ListVoteSubmissions(t.ID); err == nil {
			for _, sub := range submissions {
				uname := s.citizenUsername(sub.CitizenID)
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
			for _, c := range claims {
				uname := s.citizenUsername(c.CitizenID)
				if uname != "" {
					resp.ActiveClaimants = append(resp.ActiveClaimants, uname)
				}
			}
		}
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
