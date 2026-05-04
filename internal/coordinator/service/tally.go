package service

import (
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TallyTaskResponse is the wire shape for enju_tally_task.
// Mirrors the legacy REST shape — Status varies by outcome
// ("already_resolved" / "resolved" / "collecting") so the
// formatter can render the right branch.
type TallyTaskResponse struct {
	Status   string     `json:"status"`
	TaskID   string     `json:"task_id"`
	State    string     `json:"state,omitempty"`
	Message   string     `json:"message,omitempty"`
	Tally    *TallyView   `json:"tally,omitempty"`
	WinningOption string     `json:"winning_option,omitempty"`
	Verdict   string     `json:"verdict,omitempty"`
	NewlyReady  any      `json:"newly_ready,omitempty"`
	Skipped   []string    `json:"skipped,omitempty"`
}

// TallyView is the inline tally-result block embedded in the
// response. Vote and review fields differ — only the relevant
// ones are populated for each action type.
type TallyView struct {
	Resolved    bool      `json:"resolved"`
	Reason     string     `json:"reason,omitempty"`
	// Vote fields.
	TotalVotes int      `json:"total_votes,omitempty"`
	Counts    map[string]int `json:"counts,omitempty"`
	// Review fields.
	Verdict    string     `json:"verdict,omitempty"`
	Approves    int      `json:"approves,omitempty"`
	Rejects    int      `json:"rejects,omitempty"`
	TotalReviews int      `json:"total_reviews,omitempty"`
}

// TallyTask forces a tally evaluation on a collecting vote or
// review task. Re-runs the same engine logic a submission would,
// resolves if threshold + quorum permit, and reports the
// outcome. Used when a vote is stuck past its deadline or the
// scheduler hasn't re-evaluated it lately.
//
// Membership-gated. Returns "already_resolved" without writing
// when the task is already accepted; "collecting" when more
// submissions are needed; "resolved" when this call moved the
// task to ACCEPTED.
func (c *Coordinator) TallyTask(caller *store.CitizenRecord, taskID string) (*TallyTaskResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("%w: task %q not found", ErrNotFound, taskID)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("%w: run for task %q not found", ErrNotFound, taskID)
	}
	if !CanReadProject(c.Store, run.ProjectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrForbidden)
	}

	if task.Action != "vote" && task.Action != "review" {
		return nil, fmt.Errorf("%w: tally is only valid on action:vote or action:review tasks (got %q)", ErrInvalidArgument, task.Action)
	}
	if store.TaskState(task.State) == store.TaskAccepted {
		return &TallyTaskResponse{
			Status: "already_resolved",
			TaskID: taskID,
			State:  string(task.State),
			Message: "task is already accepted — nothing to tally",
		}, nil
	}
	if store.TaskState(task.State) != store.TaskCollecting {
		return nil, fmt.Errorf("%w: task %q is not collecting submissions yet (state: %s) — nothing to tally",
			ErrInvalidArgument, taskID, engine.StateLabel(store.TaskState(task.State)))
	}

	resp := &TallyTaskResponse{TaskID: taskID, State: string(task.State)}
	eng := engine.New(c.Store, c.Logger)

	if task.Action == "vote" {
		outcome, err := eng.EvaluateVoteTally(task)
		if err != nil {
			return nil, fmt.Errorf("tally failed: %w", err)
		}
		resp.Tally = &TallyView{
			Resolved:  outcome.Resolved,
			TotalVotes: outcome.TotalVotes,
			Counts:   outcome.Counts,
			Reason:   outcome.Reason,
		}
		if !outcome.Resolved {
			resp.Status = "collecting"
			return resp, nil
		}
		plan := store.Plan{
			Version: engine.EngineVersion,
			Mutations: []store.Mutation{
				store.SetTaskState{
					TaskID:     taskID,
					NewState:   store.TaskAccepted,
					VoteChoice: outcome.WinningOption,
					CommitSHA:  task.CommitSHA,
				},
				store.CompleteRun{RunID: task.RunID},
			},
		}.AppendCascade(task.RunID)
		result, err := c.Store.ApplyPlan(plan)
		if err != nil {
			return nil, fmt.Errorf("resolve failed: %w", err)
		}
		// Skip cascade routes losing-branch descendants to
		// SKIPPED. Best-effort — failure to cascade leaves
		// them in their pre-vote state; the next ready-sweep
		// won't promote them since their predecessors are
		// still unresolved-from-their-own-perspective.
		updated, _ := c.Store.GetTask(taskID)
		if updated != nil {
			if skipRes, scErr := c.PerformSkipCascade(updated, outcome.WinningOption); scErr == nil && skipRes != nil {
				resp.Skipped = skipRes.Skipped
			}
		}
		resp.Status = "resolved"
		resp.WinningOption = outcome.WinningOption
		resp.NewlyReady = result.TasksReadied
		return resp, nil
	}

	// review
	outcome, err := eng.EvaluateReviewTally(task)
	if err != nil {
		return nil, fmt.Errorf("tally failed: %w", err)
	}
	resp.Tally = &TallyView{
		Resolved:   outcome.Resolved,
		Verdict:    string(outcome.Verdict),
		Approves:    outcome.Approves,
		Rejects:    outcome.Rejects,
		TotalReviews: outcome.TotalReviews,
		Reason:    outcome.Reason,
	}
	if !outcome.Resolved {
		resp.Status = "collecting"
		return resp, nil
	}
	plan := store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetTaskState{
				TaskID:   taskID,
				NewState: store.TaskAccepted,
			},
			store.CompleteRun{RunID: task.RunID},
		},
	}.AppendCascade(task.RunID)
	result, err := c.Store.ApplyPlan(plan)
	if err != nil {
		return nil, fmt.Errorf("resolve failed: %w", err)
	}
	if outcome.Verdict == store.ReviewDecisionReject && task.ReviewsTarget != "" {
		targetFullID := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq) + task.ReviewsTarget
		if _, err := c.PerformInvalidate(targetFullID, "review_reject"); err != nil {
			c.Logger.Warn("tally review-reject cascade failed",
				"review_task", taskID, "target", targetFullID, "error", err)
		}
	}
	// Cascade fired inside ApplyPlan above (UpdateReadyTasks
	// mutation in the plan). The pre-refactor extra
	// s.UpdateReadyTasks call here was redundant (idempotent
	// re-run finds 0 pending tasks because the first call
	// already promoted them).
	resp.Status = "resolved"
	resp.Verdict = string(outcome.Verdict)
	resp.NewlyReady = result.TasksReadied
	return resp, nil
}
