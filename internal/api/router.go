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
	runs map[int64]*enjuYaml.ParsedRun
	git      *enjuGit.Writer
	logger   *slog.Logger
}

// NewServer creates a new API server.
func NewServer(st *store.Store, gitWriter *enjuGit.Writer, logger *slog.Logger) *Server {
	return &Server{
		store:    st,
		dags:     make(map[int64]*dag.DAG),
		runs: make(map[int64]*enjuYaml.ParsedRun),
		git:      gitWriter,
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
		r.Get("/citizens/{citizenID}/dashboard", s.handleCitizenDashboard)
		r.Put("/citizens/{citizenID}/profile", s.handleUpdateProfile)
		r.Get("/citizens/{citizenID}", s.handleGetCitizen)
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
	ID          string `json:"id"`
	RunID       int64  `json:"run_id"`     // global run ID
	RunSeq      int    `json:"run_seq"`    // per-project run sequence
	ProjectID   int64  `json:"project_id"` // parent project
	Seq         int    `json:"seq"`        // task sequence within run
	TaskDefID   string `json:"task_def_id"`
	InstanceKey string  `json:"instance_key,omitempty"`
	Ref         string  `json:"ref,omitempty"`
	Action      string  `json:"action"`
	Prompt       string `json:"prompt,omitempty"`
	UserPrompt   string `json:"user_prompt,omitempty"`
	Script       string `json:"script,omitempty"`
	Outputs      string `json:"outputs,omitempty"`
	Requirements string `json:"requirements,omitempty"`
	ResultType   string `json:"result_type"`
	State       string  `json:"state"`
	ClaimedBy   string  `json:"claimed_by,omitempty"`
	ResultPath  string  `json:"result_path,omitempty"`
	DependsOn   string  `json:"depends_on,omitempty"`
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

type claimRequest struct {
	CitizenID string `json:"citizen_id"`
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CitizenID == "" {
		writeError(w, http.StatusBadRequest, "citizen_id is required")
		return
	}

	// Get task to determine timeout
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "task not found")
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

	if err := s.store.ClaimTask(taskID, req.CitizenID, deadline); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.store.TouchCitizen(req.CitizenID)

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

	if task.DependsOn == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"inputs": map[string]interface{}{},
		})
		return
	}

	// Collect upstream results
	inputs := make(map[string]interface{})
	deps := strings.Split(task.DependsOn, ",")
	for _, depID := range deps {
		depID = strings.TrimSpace(depID)
		depTask, err := s.store.GetTask(depID)
		if err != nil || depTask == nil {
			continue
		}

		// Read result content from git repo
		if depTask.ResultPath != "" {
			result, err := readResultForTemplate(s.git, depTask.ResultPath, depTask.TaskDefID)
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

	// Resolve the prompt template with upstream results
	resolvedPrompt := template.ResolveUpstream(task.Prompt, inputs)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"task_id":         taskID,
		"resolved_prompt": resolvedPrompt,
		"inputs":          inputs,
	})
}

type submitResultRequest struct {
	Content    string            `json:"content"`              // text content (for simple results)
	Outputs    map[string]string `json:"outputs,omitempty"`    // named outputs — values are content strings
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

	var resultPath string
	// Result directory path: projects/{project_id}/runs/{run_seq}/tasks/{task_def_id}
	// We need to look up the run's project_id and seq
	run, runErr := s.store.GetRun(task.RunID)
	if runErr != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run not found for task")
		return
	}
	runIDStr := fmt.Sprintf("%d/%d", run.ProjectID, run.Seq)

	if len(req.Outputs) > 0 {
		// Multi-file or JSON outputs
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
			// Write each output to its declared file
			metadata["named_outputs"] = true
			resultPath, err = writeMultiFileResult(s.git, runIDStr, task.InstanceKey, task.TaskDefID, schema, req.Outputs, metadata)
		} else {
			// Legacy: everything in one JSON blob
			outputsJSON, _ := json.MarshalIndent(req.Outputs, "", "  ")
			metadata["named_outputs"] = true
			resultPath, err = writeResult(s.git, runIDStr, task.InstanceKey, task.TaskDefID, string(outputsJSON), "json", metadata)
		}
	} else {
		// Simple text result
		resultPath, err = writeResult(s.git, runIDStr, task.InstanceKey, task.TaskDefID, req.Content, resultType, metadata)
	}
	if err != nil {
		s.logger.Error("writing result", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to write result")
		return
	}

	// Commit to git
	commitMsg := fmt.Sprintf("Result: %s (by %s)", taskID, task.ClaimedBy)
	if err := s.git.CommitAndPush(commitMsg); err != nil {
		s.logger.Warn("git commit/push failed", "error", err)
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
	if completed {
		resp["run_completed"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReleaseTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req struct {
		CitizenID string `json:"citizen_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.store.ReleaseTask(taskID, req.CitizenID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to release task")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

type invalidateRequest struct {
	Reason string `json:"reason"`
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

	// Find descendants using the in-memory DAG
	d, ok := s.dags[task.RunID]
	if !ok {
		writeError(w, http.StatusInternalServerError, "DAG not loaded for this run")
		return
	}

	descendants := d.Descendants(taskID)

	if err := s.store.InvalidateTask(taskID, descendants); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to invalidate")
		return
	}

	s.logger.Info("task invalidated", "task_id", taskID, "descendants", len(descendants), "reason", req.Reason)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "invalidated",
		"descendants": descendants,
		"reason":      req.Reason,
	})
}

// --- Citizens ---

type registerRequest struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
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

	id := uuid.New().String()[:8]
	token := uuid.New().String()
	now := time.Now()

	err := s.store.CreateCitizen(&store.CitizenRecord{
		ID:           id,
		Name:         req.Name,
		Email:        req.Email,
		Role:         "citizen",
		Token:        token,
		RegisteredAt: now,
		LastSeen:     now,
	})
	if err != nil {
		if strings.Contains(err.Error(), "email already exists") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to register")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":    id,
		"name":  req.Name,
		"email": req.Email,
		"role":  "citizen",
		"token": token,
	})
}

func (s *Server) handleGetCitizen(w http.ResponseWriter, r *http.Request) {
	citizenID := chi.URLParam(r, "citizenID")

	citizen, err := s.store.GetCitizen(citizenID)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":                citizen.ID,
		"name":              citizen.Name,
		"email":             citizen.Email,
		"role":              citizen.Role,
		"score":             citizen.Score,
		"tasks_completed":   citizen.TasksCompleted,
		"tasks_timed_out":   citizen.TasksTimedOut,
		"registered_at":     citizen.RegisteredAt.Format(time.RFC3339),
	})
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	citizenID := chi.URLParam(r, "citizenID")

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

	if err := s.store.UpdateCitizenProfile(citizenID, req.Name, req.Email); err != nil {
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
	citizenID := chi.URLParam(r, "citizenID")

	citizen, err := s.store.GetCitizen(citizenID)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}

	active, _ := s.store.ListCitizenActiveTasks(citizenID)
	recent, _ := s.store.ListCitizenCompletedTasks(citizenID, 5)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"citizen": map[string]interface{}{
			"id":                citizen.ID,
			"name":              citizen.Name,
			"email":             citizen.Email,
			"role":              citizen.Role,
			"score":             citizen.Score,
			"tasks_completed":   citizen.TasksCompleted,
			"tasks_timed_out":   citizen.TasksTimedOut,
			"tasks_released":    citizen.TasksReleased,
			"tokens_contributed": citizen.TokensContrib,
			"registered_at":     citizen.RegisteredAt.Format(time.RFC3339),
		},
		"active_tasks":  s.toTaskResponses(active),
		"recent_tasks":  s.toTaskResponses(recent),
	})
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

func (s *Server) toTaskResponse(t store.TaskRecord) taskResponse {
	// Look up the run to get project_id and run_seq
	var projectID int64
	var runSeq int
	if run, _ := s.store.GetRun(t.RunID); run != nil {
		projectID = run.ProjectID
		runSeq = run.Seq
	}
	return taskResponse{
		ID:           t.ID,
		RunID:        t.RunID,
		RunSeq:       runSeq,
		ProjectID:    projectID,
		Seq:          t.Seq,
		TaskDefID:    t.TaskDefID,
		InstanceKey:  t.InstanceKey,
		Ref:          t.Ref,
		Action:       t.Action,
		Prompt:       t.Prompt,
		UserPrompt:   t.UserPrompt,
		Script:       t.Script,
		Outputs:      t.Outputs,
		Requirements: t.Requirements,
		ResultType:   t.ResultType,
		State:        string(t.State),
		ClaimedBy:    t.ClaimedBy,
		ResultPath:   t.ResultPath,
		DependsOn:    t.DependsOn,
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
