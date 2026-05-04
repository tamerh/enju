package service

import (
	"errors"
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// SetCycleBudgetResponse is the wire shape for
// enju_set_cycle_budget. Returns the post-update used/max so
// the caller can confirm.
type SetCycleBudgetResponse struct {
	Status      string         `json:"status"` // "updated"
	RunID       string         `json:"run_id"`
	CycleBudget map[string]int `json:"cycle_budget"`
}

// ErrInvalidArgument is the sentinel for caller-supplied
// argument errors (e.g. non-positive max). Transports map this
// to 400-style responses; service callers can errors.Is-check
// to distinguish from internal store failures.
var ErrInvalidArgument = errors.New("invalid argument")

// SetCycleBudget bumps the per-run spawn cap. Membership-gated.
// Returns ErrInvalidArgument when max <= 0.
func SetCycleBudget(s *store.Store, caller *store.CitizenRecord, projectID int64, runSeq int, max int) (*SetCycleBudgetResponse, error) {
	if max <= 0 {
		return nil, fmt.Errorf("%w: max must be positive", ErrInvalidArgument)
	}
	run, err := s.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, ErrNotMember
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetCycleBudgetMax{RunID: run.ID, CitizenID: caller.ID, NewMax: max},
		},
	}); err != nil {
		return nil, err
	}
	used, newMax, _ := s.GetCycleBudget(run.ID)
	return &SetCycleBudgetResponse{
		Status:      "updated",
		RunID:       fmt.Sprintf("%d:%d", projectID, runSeq),
		CycleBudget: map[string]int{"used": used, "max": newMax},
	}, nil
}
