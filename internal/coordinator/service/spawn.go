package service

import (
	"errors"
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// SpawnTaskParams is the input shape for SpawnTask. Mirrors the
// REST handler's spawnTaskRequest so the api translator is a
// straight field copy.
type SpawnTaskParams struct {
	ParentTaskID string
	TaskDefID  string
	Action    string
	Prompt    string
	UserPrompt  string
	Citizens   int
	DependsOn  []string
	AssignTo   []string
	RequireRole string
	ResultType  string
	Trigger   string
}

// SpawnTaskResponse is the wire shape for spawn. Mirrors the
// historical REST shape exactly.
type SpawnTaskResponse struct {
	Status     string     `json:"status"`
	TaskID     string     `json:"task_id"`
	TaskDefID    string     `json:"task_def_id"`
	ParentTaskID  string     `json:"parent_task_id,omitempty"`
	Trigger    string     `json:"trigger,omitempty"`
	CycleBudget   CycleBudgetInfo `json:"cycle_budget"`
}

// CycleBudgetInfo is the inline cycle-budget block in the spawn
// response — gives the caller a chance to see they're approaching
// the cap without an extra round-trip.
type CycleBudgetInfo struct {
	Used int `json:"used"`
	Max int `json:"max"`
}

// ErrCycleBudgetExhausted is returned by SpawnTask when the run's
// cycle budget is full. Distinct sentinel so the REST handler can
// translate it to 409 Conflict (matches the historical "request
// can't be fulfilled in current state" convention) and the MCP
// caller can render a distinct "extend budget then resume" hint.
var ErrCycleBudgetExhausted = errors.New("cycle budget exhausted")

// SpawnTask creates a new task in an existing run at runtime.
// Member-gated; the spawning citizen is the authenticated caller.
// Returns ErrCycleBudgetExhausted when the run's cycle cap is
// hit — the underlying store-side spawn auto-pauses the run as
// part of that error.
func SpawnTask(s *store.Store, caller *store.CitizenRecord, projectID int64, runSeq int, params SpawnTaskParams) (*SpawnTaskResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	if params.TaskDefID == "" {
		return nil, fmt.Errorf("%w: task_def_id is required", ErrInvalidArgument)
	}
	if params.Action == "" {
		return nil, fmt.Errorf("%w: action is required", ErrInvalidArgument)
	}

	run, err := s.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: run not found", ErrNotFound)
	}
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrNotMember)
	}

	res, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SpawnTask{Spec: store.SpawnSpec{
				RunID:        run.ID,
				ParentTaskID: params.ParentTaskID,
				TaskDefID:    params.TaskDefID,
				Action:       params.Action,
				Prompt:       params.Prompt,
				UserPrompt:   params.UserPrompt,
				Citizens:     params.Citizens,
				DependsOn:    params.DependsOn,
				AssignTo:     params.AssignTo,
				RequireRole:  params.RequireRole,
				ResultType:   params.ResultType,
				Trigger:      params.Trigger,
				SpawnedBy:    caller.ID,
			}},
		},
	})
	if err != nil {
		// The underlying store error is a string-typed message
		// (validation/not-found cases). Cycle exhaustion is now
		// signaled via ApplyResult.BudgetExhausted, not via the
		// error path — see below.
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}
	if res.BudgetExhausted {
		// The handler committed the pause + emitted
		// cycle_budget_exhausted; convert the result flag to a
		// typed sentinel so transports can render a distinct UX
		// without scraping strings.
		used, maxBudget, _ := s.GetCycleBudget(run.ID)
		return nil, fmt.Errorf("%w: cycle budget exhausted for run %d (%d/%d) — run paused; extend budget and resume to allow further spawns",
			ErrCycleBudgetExhausted, run.ID, used, maxBudget)
	}
	taskID := res.SpawnedTaskID

	used, maxBudget, _ := s.GetCycleBudget(run.ID)
	return &SpawnTaskResponse{
		Status:    "spawned",
		TaskID:    taskID,
		TaskDefID:   params.TaskDefID,
		ParentTaskID: params.ParentTaskID,
		Trigger:    params.Trigger,
		CycleBudget:  CycleBudgetInfo{Used: used, Max: maxBudget},
	}, nil
}
