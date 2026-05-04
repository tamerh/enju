package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

// --- Tasks ---

// taskResponse aliases service.TaskResponse so this package's
// literal-struct call sites (handleCreateRun, etc.) keep
// compiling while the canonical shape lives in service.
type taskResponse = service.TaskResponse

// taskHistoryEntry / artifactProvenance / voteSubmissionRef
// alias their service-layer counterparts for the same reason.
type taskHistoryEntry = service.TaskHistoryEntry
type artifactProvenance = service.ArtifactProvenance
type voteSubmissionRef = service.VoteSubmissionRef

// taskHistoryEntry, artifactProvenance, voteSubmissionRef now
// live in the service layer (see service/tasks.go) and are
// aliased near the top of this file. Their original inline
// definitions were removed when the toTaskResponse builder
// moved to service.

func (s *Server) handleListReadyTasks(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	runSeq, _ := strconv.Atoi(r.URL.Query().Get("run_id"))
	resp, err := service.ListReadyTasks(s.store, caller, service.ReadyTasksParams{
		ProjectID: projectID,
		RunSeq:    runSeq,
	})
	if err != nil {
		switch err {
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusInternalServerError, "failed to list tasks")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Lazy deadline-vote resolution stays HTTP-side: it's a
	// side effect we want to fire on view (any caller hitting
	// the endpoint nudges the tally), but service.GetTask is
	// pure-read by contract. Resolve first, then read.
	if task, _ := s.store.GetTask(taskID); task != nil {
		s.maybeResolveDeadlineVote(task)
	}
	resp, err := service.GetTask(s.store, caller, taskID)
	switch err {
	case nil:
	case service.ErrNotFound:
		writeError(w, http.StatusNotFound, "task not found")
		return
	case service.ErrNotMember:
		writeError(w, http.StatusForbidden, "not a member of this project")
		return
	default:
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
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
				TaskID:   task.ID,
				NewState:  store.TaskAccepted,
				VoteChoice: outcome.WinningOption,
				CommitSHA: task.CommitSHA,
			},
		},
	}.AppendCascade(task.RunID)); err != nil {
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
	// Cascade above already ran via AppendCascade.
	s.logger.Info("vote resolved via deadline sweep",
		"task_id", task.ID, "winning_option", outcome.WinningOption)
}

func (s *Server) handleListIterations(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	caller := citizenFromRequest(r)
	out, err := service.ListTaskIterations(s.store, caller, taskID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, "task not found")
		case errors.Is(err, service.ErrNotMember):
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusInternalServerError, "listing iterations: "+err.Error())
		}
		return
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

func (s *Server) handleGetTaskInputsDescriptor(w http.ResponseWriter, r *http.Request, task *store.TaskRecord, run *store.RunRecord) {
	desc, err := s.engine().BuildInputsDescriptor(task, run)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "building descriptor: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, desc)
}

// toTaskResponse delegates to service.ToTaskResponse. The api
// package keeps its method-style wrapper so existing callers
// (`s.toTaskResponse(t)`, `s.toTaskResponses(tasks)`) read the
// same way they always did.
func (s *Server) toTaskResponse(t store.TaskRecord) taskResponse {
	return service.ToTaskResponse(s.store, t)
}

func (s *Server) toTaskResponses(tasks []store.TaskRecord) []taskResponse {
	return service.ToTaskResponses(s.store, tasks)
}
