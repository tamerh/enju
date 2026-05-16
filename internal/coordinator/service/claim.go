package service

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ClaimTaskParams is the input shape for ClaimTask.
type ClaimTaskParams struct {
	// Username is the claimant. Required. Body-supplied today
	// (legacy quirk — the auth context citizen could differ
	// from this; tightening to "must match auth caller" is a
	// follow-up flagged in the operator/model attribution
	// notes). Multi-bot processes claim on behalf of distinct
	// citizens, so the lookup is by username, not by token.
	Username string

	// Model is the optional LLM citizen username producing the
	// words for this claim (operator/model split). Empty for
	// unaided humans; bots that omit it are rejected by the
	// apply layer with a clear "bots must declare a model"
	// message. Unknown-but-valid names auto-register as new
	// kind='model' citizens — see ResolveModelByUsername for
	// the rationale.
	Model string
}

// ClaimTaskResponse is the wire shape for enju_claim_task.
// Mirrors the historical REST envelope: task body + deadline.
type ClaimTaskResponse struct {
	Task   *TaskResponse `json:"task"`
	Deadline string    `json:"deadline"`
}

// ClaimTask is the operator-facing claim entry point. It runs
// the access gate, computes the claim plan via engine, applies
// it, touches the citizen, and returns the updated task view.
//
// The store-level write happens through ApplyPlan (engine
// computes, store applies) — same path as every other production
// write. The legacy Store.ClaimTask method is test-fixture-only.
//
// Errors:
//   - ErrInvalidArgument: missing username, bad model name
//   - ErrNotFound: citizen / task / run not found
//   - ErrNotMember: caller can't read this project
//   - ErrForbidden: assign_to / require_role gate refuses
//   - ErrConflict: claim collision (slot full, race lost) —
//   wraps the engine.ComputeClaim error verbatim
// defaultClaimTimeout is the lease length when a task declares no
// explicit `timeout:`. It bounds "claimed/running but the worker
// went silent" recovery — see taskClaimTimeout.
const defaultClaimTimeout = 30 * time.Minute

// taskClaimTimeout is the single source for a task's lease length,
// shared by ClaimTask (anchors at claim) and MarkTaskRunning
// (re-anchors at RUNNING so the budget covers actual execution,
// not the claim-time guess). Keeping one function means the two
// anchor points can never drift to different durations.
func taskClaimTimeout(task *store.TaskRecord) time.Duration {
	if task != nil && task.Timeout != "" {
		if d, perr := time.ParseDuration(task.Timeout); perr == nil {
			return d
		}
	}
	return defaultClaimTimeout
}

func ClaimTask(s store.CoordinatorStore, logger *slog.Logger, taskID string, params ClaimTaskParams) (*ClaimTaskResponse, error) {
	if params.Username == "" {
		return nil, fmt.Errorf("%w: username is required", ErrInvalidArgument)
	}
	caller, err := s.GetCitizenByUsername(params.Username)
	if err != nil {
		return nil, err
	}
	if caller == nil {
		return nil, fmt.Errorf("%w: citizen %q not found", ErrNotFound, params.Username)
	}

	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("%w: task not found", ErrNotFound)
	}
	run, err := s.GetRun(task.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: run for task %q not found", ErrNotFound, taskID)
	}
	if !CanReadProject(s, run.ProjectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrNotMember)
	}

	if err := engine.CheckTaskAccess(task, caller); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrForbidden, err.Error())
	}

	deadline := time.Now().Add(taskClaimTimeout(task))

	modelID, err := ResolveModelByUsername(s, params.Model)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}

	eng := engine.New(s, logger)
	plan, err := eng.ComputeClaim(taskID, caller.ID, deadline, modelID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrConflict, err.Error())
	}
	if _, err := s.ApplyPlan(*plan); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrConflict, err.Error())
	}

	updated, _ := s.GetTask(taskID)
	resp := &ClaimTaskResponse{
		Deadline: deadline.Format(time.RFC3339),
	}
	if updated != nil {
		tr := ToTaskResponse(s, *updated)
		resp.Task = &tr
	}
	return resp, nil
}

// ResolveModelByUsername mirrors the legacy api helper. Empty
// input returns (nil, nil) for the unaided-human case. Unknown
// names auto-register a kind='model' citizen — local-mode
// philosophy: don't make Ollama users pre-register before they
// can submit. Reject if the username resolves to a non-model
// citizen so callers can't attribute their submit to a teammate's
// account. Hosted-mode policy gating (require pre-registration)
// is deferred — see the operator/model design doc.
func ResolveModelByUsername(s store.CoordinatorStore, modelName string) (*int64, error) {
	if modelName == "" {
		return nil, nil
	}
	c, err := s.GetCitizenByUsername(modelName)
	if err != nil {
		return nil, fmt.Errorf("look up model %q: %w", modelName, err)
	}
	if c != nil {
		if c.Kind != store.CitizenKindModel {
			return nil, fmt.Errorf("citizen %q has kind %q, not %q — operators cannot be self-attributed as their own model", modelName, c.Kind, "model")
		}
		return &c.ID, nil
	}
	now := time.Now()
	res, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateCitizen{Citizen: store.CitizenRecord{
				Username:     modelName,
				Name:         modelName,
				Role:         "citizen",
				Token:        "model:" + modelName,
				RegisteredAt: now,
				LastSeen:     now,
				Kind:         store.CitizenKindModel,
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("auto-register model %q: %w", modelName, err)
	}
	id := res.CitizenID
	return &id, nil
}

// IsClaimContention reports whether the error is the engine's
// "task already claimed / slots full / cap reached" family.
// The legacy api mapped any ComputeClaim/ApplyPlan failure to
// 409 Conflict; preserved here so the REST handler can do the
// same. Used as a hint, not authoritative — anything else
// surfaced by the engine still falls through to 409 since the
// legacy behavior was "all conflict" for this code path.
func IsClaimContention(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return errors.Is(err, ErrConflict) ||
		strings.Contains(msg, "already claimed") ||
		strings.Contains(msg, "slots") ||
		strings.Contains(msg, "cap")
}
