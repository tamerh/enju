// Package api provides the REST API for the Cedar coordinator.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/dag"
	cedarGit "github.com/enju-ai/enju/internal/git"
	"github.com/enju-ai/enju/internal/store"
	cedarYaml "github.com/enju-ai/enju/internal/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// Server holds the coordinator state and dependencies.
type Server struct {
	store    *store.Store
	dags     map[string]*dag.DAG // problemID -> DAG (in-memory for fast queries)
	problems map[string]*cedarYaml.ParsedProblem
	git      *cedarGit.Writer
	logger   *slog.Logger
}

// NewServer creates a new API server.
func NewServer(st *store.Store, gitWriter *cedarGit.Writer, logger *slog.Logger) *Server {
	return &Server{
		store:    st,
		dags:     make(map[string]*dag.DAG),
		problems: make(map[string]*cedarYaml.ParsedProblem),
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
		// Problems
		r.Post("/problems", s.handleCreateProblem)
		r.Get("/problems", s.handleListProblems)
		r.Get("/problems/{problemID}", s.handleGetProblem)
		r.Get("/problems/{problemID}/tasks", s.handleListProblemTasks)

		// Tasks
		r.Get("/tasks/ready", s.handleListReadyTasks)
		r.Post("/tasks/{taskID}/claim", s.handleClaimTask)
		r.Get("/tasks/{taskID}", s.handleGetTask)
		r.Get("/tasks/{taskID}/inputs", s.handleGetTaskInputs)
		r.Post("/tasks/{taskID}/result", s.handleSubmitResult)
		r.Post("/tasks/{taskID}/release", s.handleReleaseTask)
		r.Post("/tasks/{taskID}/invalidate", s.handleInvalidateTask)

		// Participants
		r.Post("/participants/register", s.handleRegisterParticipant)
	})

	return r
}

// --- Health ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Problems ---

type createProblemRequest struct {
	YAML    string `json:"yaml"`
	RepoURL string `json:"repo_url,omitempty"`
}

type problemResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	TaskCount int    `json:"task_count"`
	CreatedAt string `json:"created_at"`
}

func (s *Server) handleCreateProblem(w http.ResponseWriter, r *http.Request) {
	var req createProblemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.YAML == "" {
		writeError(w, http.StatusBadRequest, "yaml is required")
		return
	}

	// Parse and validate the YAML
	parsed, err := cedarYaml.Parse([]byte(req.YAML))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid problem definition: "+err.Error())
		return
	}

	problemID := uuid.New().String()[:8]
	now := time.Now()

	// Store problem
	repoURL := req.RepoURL

	err = s.store.CreateProblem(&store.ProblemRecord{
		ID:        problemID,
		Name:      parsed.Problem.Name,
		YAMLData:  req.YAML,
		RepoURL:   repoURL,
		State:     store.ProblemActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		s.logger.Error("creating problem", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create problem")
		return
	}

	// Create task records from expanded DAG
	taskCount := 0
	for instanceKey, tasks := range parsed.ExpandedTasks {
		for _, ti := range tasks {
			mode := ti.Mode
			if mode == "" {
				mode = "autonomous"
			}
			resultType := ti.ResultType
			if resultType == "" {
				resultType = "text"
			}
			timeout := ti.Timeout
			if timeout == "" {
				timeout = parsed.Problem.Defaults.Timeout
			}

			// Build depends_on as comma-separated full IDs
			var deps []string
			for _, dep := range ti.DependsOn {
				deps = append(deps, cedarYaml.MakeFullID(instanceKey, dep))
			}

			// Determine initial state
			state := store.TaskPending
			if len(ti.DependsOn) == 0 {
				state = store.TaskReady
			}

			err := s.store.CreateTask(&store.TaskRecord{
				ID:          ti.FullID,
				ProblemID:   problemID,
				TaskDefID:   ti.ID,
				InstanceKey: instanceKey,
				Type:        ti.Type,
				Mode:        mode,
				Prompt:      ti.Prompt,
				UserPrompt:  ti.UserPrompt,
				Script:      ti.Script,
				ResultType:  resultType,
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

	// Cache DAG and parsed problem in memory
	s.dags[problemID] = parsed.DAG
	s.problems[problemID] = parsed

	s.logger.Info("problem created", "id", problemID, "name", parsed.Problem.Name, "tasks", taskCount)

	writeJSON(w, http.StatusCreated, problemResponse{
		ID:        problemID,
		Name:      parsed.Problem.Name,
		State:     string(store.ProblemActive),
		TaskCount: taskCount,
		CreatedAt: now.Format(time.RFC3339),
	})
}

func (s *Server) handleListProblems(w http.ResponseWriter, r *http.Request) {
	problems, err := s.store.ListProblems()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list problems")
		return
	}

	var resp []problemResponse
	for _, p := range problems {
		tasks, _ := s.store.ListTasksByProblem(p.ID)
		resp = append(resp, problemResponse{
			ID:        p.ID,
			Name:      p.Name,
			State:     string(p.State),
			TaskCount: len(tasks),
			CreatedAt: p.CreatedAt.Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetProblem(w http.ResponseWriter, r *http.Request) {
	problemID := chi.URLParam(r, "problemID")
	p, err := s.store.GetProblem(problemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "problem not found")
		return
	}

	tasks, _ := s.store.ListTasksByProblem(p.ID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         p.ID,
		"name":       p.Name,
		"state":      p.State,
		"repo_url":   p.RepoURL,
		"task_count": len(tasks),
		"created_at": p.CreatedAt.Format(time.RFC3339),
	})
}

func (s *Server) handleListProblemTasks(w http.ResponseWriter, r *http.Request) {
	problemID := chi.URLParam(r, "problemID")
	tasks, err := s.store.ListTasksByProblem(problemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponses(tasks))
}

// --- Tasks ---

type taskResponse struct {
	ID          string  `json:"id"`
	ProblemID   string  `json:"problem_id"`
	TaskDefID   string  `json:"task_def_id"`
	InstanceKey string  `json:"instance_key,omitempty"`
	Type        string  `json:"type"`
	Mode        string  `json:"mode"`
	Prompt      string  `json:"prompt,omitempty"`
	UserPrompt  string  `json:"user_prompt,omitempty"`
	Script      string  `json:"script,omitempty"`
	ResultType  string  `json:"result_type"`
	State       string  `json:"state"`
	ClaimedBy   string  `json:"claimed_by,omitempty"`
	ResultPath  string  `json:"result_path,omitempty"`
	DependsOn   string  `json:"depends_on,omitempty"`
}

func (s *Server) handleListReadyTasks(w http.ResponseWriter, r *http.Request) {
	problemID := r.URL.Query().Get("problem_id")
	tasks, err := s.store.ListReadyTasks(problemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponses(tasks))
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
	writeJSON(w, http.StatusOK, toTaskResponse(*task))
}

type claimRequest struct {
	ParticipantID string `json:"participant_id"`
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ParticipantID == "" {
		writeError(w, http.StatusBadRequest, "participant_id is required")
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

	if err := s.store.ClaimTask(taskID, req.ParticipantID, deadline); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.store.TouchParticipant(req.ParticipantID)

	// Return task with full details
	updatedTask, _ := s.store.GetTask(taskID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"task":     toTaskResponse(*updatedTask),
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
			data, err := s.git.ReadFile(depTask.ResultPath)
			if err != nil {
				s.logger.Warn("reading upstream result", "path", depTask.ResultPath, "error", err)
				inputs[depTask.TaskDefID] = map[string]interface{}{
					"status": "error",
					"error":  err.Error(),
				}
				continue
			}
			var content map[string]interface{}
			if err := json.Unmarshal(data, &content); err != nil {
				inputs[depTask.TaskDefID] = map[string]interface{}{
					"status": "error",
					"error":  "failed to parse result JSON",
				}
				continue
			}
			inputs[depTask.TaskDefID] = content
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"task_id": taskID,
		"inputs":  inputs,
	})
}

type submitResultRequest struct {
	Content    string `json:"content"`
	ResultType string `json:"result_type,omitempty"`
	TokensUsed int64  `json:"tokens_used,omitempty"`
	Model      string `json:"model,omitempty"`
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

	// Determine result file path
	resultPath := buildResultPath(task.ProblemID, task.InstanceKey, task.TaskDefID)

	// Write result to git repo
	resultData := map[string]interface{}{
		"task_id":     taskID,
		"result_type": req.ResultType,
		"content":     req.Content,
		"metadata": map[string]interface{}{
			"participant":  task.ClaimedBy,
			"model":        req.Model,
			"tokens_used":  req.TokensUsed,
			"timestamp":    time.Now().Format(time.RFC3339),
		},
	}

	jsonBytes, err := json.MarshalIndent(resultData, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal result")
		return
	}

	if err := s.git.WriteFile(resultPath, jsonBytes); err != nil {
		s.logger.Error("writing result file", "path", resultPath, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to write result")
		return
	}

	// Commit to git
	commitMsg := fmt.Sprintf("Result: %s (by %s)", taskID, task.ClaimedBy)
	if err := s.git.CommitAndPush(commitMsg); err != nil {
		s.logger.Warn("git commit/push failed", "error", err)
		// Non-fatal — result file is written, state will update
	}

	// Update task state
	if err := s.store.SubmitTaskResult(taskID, resultPath, req.TokensUsed); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update task: "+err.Error())
		return
	}

	// Update ready tasks — newly unblocked tasks become READY
	readied, _ := s.store.UpdateReadyTasks(task.ProblemID)

	s.logger.Info("result submitted", "task_id", taskID, "path", resultPath, "newly_ready", readied)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "accepted",
		"result_path":  resultPath,
		"newly_ready":  readied,
	})
}

func (s *Server) handleReleaseTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req struct {
		ParticipantID string `json:"participant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.store.ReleaseTask(taskID, req.ParticipantID); err != nil {
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
	d, ok := s.dags[task.ProblemID]
	if !ok {
		writeError(w, http.StatusInternalServerError, "DAG not loaded for this problem")
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

// --- Participants ---

type registerRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleRegisterParticipant(w http.ResponseWriter, r *http.Request) {
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

	err := s.store.CreateParticipant(&store.ParticipantRecord{
		ID:           id,
		Name:         req.Name,
		Token:        token,
		RegisteredAt: now,
		LastSeen:     now,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":    id,
		"name":  req.Name,
		"token": token,
	})
}

// --- Helpers ---

func toTaskResponse(t store.TaskRecord) taskResponse {
	return taskResponse{
		ID:          t.ID,
		ProblemID:   t.ProblemID,
		TaskDefID:   t.TaskDefID,
		InstanceKey: t.InstanceKey,
		Type:        t.Type,
		Mode:        t.Mode,
		Prompt:      t.Prompt,
		UserPrompt:  t.UserPrompt,
		Script:      t.Script,
		ResultType:  t.ResultType,
		State:       string(t.State),
		ClaimedBy:   t.ClaimedBy,
		ResultPath:  t.ResultPath,
		DependsOn:   t.DependsOn,
	}
}

func toTaskResponses(tasks []store.TaskRecord) []taskResponse {
	resp := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		resp = append(resp, toTaskResponse(t))
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
