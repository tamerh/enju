package service

import (
	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// IterationResponse is an alias for wire.Iteration — the shared
// JSON shape. Existing call sites stay readable; rename to
// wire.Iteration on touch.
type IterationResponse = wire.Iteration

// ListTaskIterations returns the iteration history for one
// task, gated through the task's parent project. Returns
// ErrNotFound for missing task or run; ErrNotMember for caller
// outside the project's membership.
func ListTaskIterations(s store.CoordinatorStore, caller *store.CitizenRecord, taskID string) ([]IterationResponse, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrNotFound
	}
	run, err := s.GetRun(task.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	if !CanReadProject(s, run.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	iters, err := s.ListTaskIterations(taskID)
	if err != nil {
		return nil, err
	}
	out := make([]IterationResponse, 0, len(iters))
	for _, it := range iters {
		row := IterationResponse{
			Seq:            it.Seq,
			Citizen:        it.Username,
			ClaimedAt:      it.ClaimedAt.UTC(),
			CommitSHA:      it.CommitSHA,
			Branch:         it.Branch,
			ReviewDecision: string(it.ReviewDecision),
			Option:         it.Option,
		}
		if it.Outcome == "" {
			row.Outcome = "active"
		} else {
			row.Outcome = string(it.Outcome)
		}
		if it.SubmittedAt != nil {
			t := it.SubmittedAt.UTC()
			row.SubmittedAt = &t
		}
		if it.ModelID != nil {
			row.Model = CitizenUsername(s, *it.ModelID)
		}
		out = append(out, row)
	}
	return out, nil
}
