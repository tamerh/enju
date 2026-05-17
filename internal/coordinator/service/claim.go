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

	// Model is the normalized model-name label that produced the
	// words for this claim — a plain string, not a citizen (a
	// model has no identity). Empty for scripts and unaided
	// humans; agents that omit it are rejected by the apply layer
	// with a clear "an agent must name a model" message. Stamped
	// automatically by the submitter from its runtime identity,
	// never declared in workflow YAML.
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

	// Resolve the caller within the project's tenant — username is
	// a tenant-scoped handle (assign_to: dev-bot), never a global
	// identity, and never prefixed in YAML. Two owners may each
	// have a "dev-bot"; the right one is the one in this project's
	// tenant. Single-operator self-host has exactly one tenant, so
	// this is identical to a global lookup — zero behavior change.
	// When the project's tenant can't be determined (no owner row,
	// e.g. minimal test scaffolding) fall back to the global
	// lookup so existing single-tenant flows are unaffected.
	var caller *store.CitizenRecord
	if projTenant, ok := tenantOfProject(s, run.ProjectID); ok {
		caller, err = s.GetCitizenByUsernameInTenant(params.Username, projTenant)
	} else {
		caller, err = s.GetCitizenByUsername(params.Username)
	}
	if err != nil {
		return nil, err
	}
	if caller == nil {
		return nil, fmt.Errorf("%w: citizen %q not found", ErrNotFound, params.Username)
	}

	if !CanReadProject(s, run.ProjectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrNotMember)
	}

	if err := engine.CheckTaskAccess(task, caller); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrForbidden, err.Error())
	}

	deadline := time.Now().Add(taskClaimTimeout(task))

	eng := engine.New(s, logger)
	plan, err := eng.ComputeClaim(taskID, caller.ID, deadline, params.Model)
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

// tenantOfProject returns the tenant of a project — the tenant of
// its owning citizen (the root of the parent_id chain). The tenant
// is the seam the assign_to handle resolves within: "dev-bot" in a
// project means the dev-bot owned by that project's tenant. Returns
// (0, false) when the project has no resolvable owner (minimal test
// scaffolding) so the caller can fall back to a global lookup and
// single-tenant flows stay unaffected.
func tenantOfProject(s store.CoordinatorStore, projectID int64) (int64, bool) {
	members, err := s.ListProjectMembers(projectID)
	if err != nil {
		return 0, false
	}
	for _, m := range members {
		if m.Role != store.ProjectRoleOwner {
			continue
		}
		owner, err := s.GetCitizen(m.CitizenID)
		if err != nil || owner == nil || owner.TenantID == nil {
			return 0, false
		}
		return *owner.TenantID, true
	}
	return 0, false
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
