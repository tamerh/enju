package service

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// IterationResponse is the wire shape for one iteration of a
// task (one row in task_claims). Used by REST + MCP. JSON tags
// are load-bearing — format.IterationList consumes them.
type IterationResponse struct {
	Seq            int    `json:"seq"`
	Citizen        string `json:"citizen"`
	Outcome        string `json:"outcome"`
	ClaimedAt      string `json:"claimed_at"`
	SubmittedAt    string `json:"submitted_at,omitempty"`
	CommitSHA      string `json:"commit_sha,omitempty"`
	Branch         string `json:"branch,omitempty"`
	ReviewDecision string `json:"review_decision,omitempty"`
	Option         string `json:"option,omitempty"`
	Model          string `json:"model,omitempty"`
}

// ListTaskIterations returns the iteration history for one
// task, gated through the task's parent project. Returns
// ErrNotFound for missing task or run; ErrNotMember for caller
// outside the project's membership.
func ListTaskIterations(s *store.Store, caller *store.CitizenRecord, taskID string) ([]IterationResponse, error) {
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
			ClaimedAt:      it.ClaimedAt.UTC().Format(time.RFC3339),
			CommitSHA:      it.CommitSHA,
			Branch:         it.Branch,
			ReviewDecision: it.ReviewDecision,
			Option:         it.Option,
		}
		if it.Outcome == "" {
			row.Outcome = "active"
		} else {
			row.Outcome = it.Outcome
		}
		if it.SubmittedAt != nil {
			row.SubmittedAt = it.SubmittedAt.UTC().Format(time.RFC3339)
		}
		if it.ModelID != nil {
			row.Model = CitizenUsername(s, *it.ModelID)
		}
		out = append(out, row)
	}
	return out, nil
}
