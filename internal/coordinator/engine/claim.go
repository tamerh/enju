package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ComputeClaim validates whether a citizen can claim a task
// and returns a Plan with the appropriate mutations. Pure
// computation — reads state via ReadStore, never writes.
//
// Validates:
//   - Task exists
//   - Task is in a claimable state (READY for single-citizen;
//     READY or COLLECTING for multi-citizen)
//   - For multi-citizen: citizen doesn't already hold a slot
//     (one slot per citizen), cap not reached (active claims
//     < citizens count)
//
// Single-citizen tasks: SetClaim flips state to CLAIMED and
// sets claimed_by — exclusive ownership.
//
// Multi-citizen tasks: SetClaim records the claimer but does
// NOT change the task state (stays READY/COLLECTING). The
// task_claims table is the source of truth for who's
// working on it; claimed_by tracks the most recent claimer
// for display convenience only.
//
// Returns a Plan with a SetClaim mutation (which inserts the
// task_claims row and updates the task's claimed_by/state).
// CheckTaskAccess validates that a citizen is allowed to
// claim a task based on assign_to and require_role. Pure
// logic — no store reads.
func CheckTaskAccess(task *store.TaskRecord, caller *store.CitizenRecord) error {
	if task.AssignTo != "" {
		var assignees []string
		_ = json.Unmarshal([]byte(task.AssignTo), &assignees)
		if len(assignees) > 0 {
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
	}
	if task.RequireRole != "" {
		if caller.Role != task.RequireRole {
			return fmt.Errorf("task requires role %q — your role is %q", task.RequireRole, caller.Role)
		}
	}
	return nil
}

// ComputeClaim's model parameter is the normalized model-name
// label credited for this claim — a plain string, not a citizen
// reference (a model has no identity). Pass "" for a script or a
// human-without-LLM (hand-claim); the apply path enforces "an
// agent must name a model" so an empty model is always fine for a
// human/script and always rejected for an agent.
func (e *Engine) ComputeClaim(taskID string, citizenID int64, deadline time.Time, model string) (*store.Plan, error) {
	task, err := e.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	citizens := task.Citizens
	if citizens <= 0 {
		citizens = 1
	}

	if citizens == 1 {
		// Single-citizen: must be READY.
		if store.TaskState(task.State) != store.TaskReady {
			return nil, fmt.Errorf("task %q is not available for claiming (state: %s)", taskID, StateLabel(store.TaskState(task.State)))
		}
	} else {
		// Multi-citizen: READY or COLLECTING.
		if store.TaskState(task.State) != store.TaskReady && store.TaskState(task.State) != store.TaskCollecting {
			return nil, fmt.Errorf("task %q is not accepting claims (state: %s)", taskID, StateLabel(store.TaskState(task.State)))
		}
		// Check own-slot BEFORE cap so citizen gets a specific
		// "you already hold a slot" error, not "cap reached".
		hasClaim, err := e.store.HasActiveClaim(taskID, citizenID)
		if err != nil {
			return nil, err
		}
		if hasClaim {
			return nil, fmt.Errorf("you already have an active claim on task %q (one slot per citizen)", taskID)
		}
		active, err := e.store.CountActiveClaims(taskID)
		if err != nil {
			return nil, err
		}
		if active >= citizens {
			return nil, fmt.Errorf("task %q has reached its citizens cap (%d active claim(s))", taskID, active)
		}
	}

	return &store.Plan{
		Version: EngineVersion,
		Mutations: []store.Mutation{
			store.SetClaim{
				TaskID:    taskID,
				CitizenID: citizenID,
				Deadline:  deadline,
				Model:     model,
			},
		},
	}, nil
}
