package engine

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/store"
)

// SubmissionOutcome is the engine's computation result for
// a task submission. The Plan carries the mutations; the
// flags tell the router what happened so it can build the
// right response and trigger post-submit cascades. Events
// are contribution-log entries to record after the Plan
// succeeds.
type SubmissionOutcome struct {
	Plan       store.Plan
	Resolved   bool // single-citizen → true; multi-citizen → false
	Collecting bool // multi-citizen → true
	Events     []store.ContributionEvent
}

// ComputeSubmission validates a task submission and returns
// a Plan with a RecordSubmission mutation. Pure computation
// — reads state via ReadStore, never writes.
//
// Validates:
//   - Task exists
//   - Task is in a submittable state
//   - For single-citizen: state is CLAIMED or RUNNING. One
//     submit → ACCEPTED (Resolved=true).
//   - For multi-citizen: state is READY or COLLECTING, and
//     the citizen has an active claim (uses HasActiveClaim,
//     NOT a state check — fixes the bug where multi-citizen
//     tasks in READY state were rejected by the old
//     state==claimed check). Each submit records the
//     citizen's vote and transitions to COLLECTING
//     (Collecting=true). Final resolution is a separate
//     tally step.
//
// The RecordSubmission mutation handles both paths:
//   - Single-citizen: task → ACCEPTED, claim → completed,
//     citizen score updated.
//   - Multi-citizen: task → COLLECTING, claim → completed
//     with choice/content, citizen tokens updated (score
//     waits for tally resolution).
//
// The router calls this instead of store.SubmitTaskResult.
func (e *Engine) ComputeSubmission(
	taskID string,
	citizenID int64,
	resultPath, commitSHA, decision, voteChoice, content string,
	tokensUsed int64,
) (*SubmissionOutcome, error) {
	task, err := e.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	citizens := task.Citizens
	if citizens <= 0 {
		citizens = 1
	}

	outcome := &SubmissionOutcome{}

	if citizens == 1 {
		// Single-citizen: must be CLAIMED or RUNNING.
		if store.TaskState(task.State) != store.TaskClaimed &&
			store.TaskState(task.State) != store.TaskRunning {
			return nil, fmt.Errorf("task %q cannot accept result (state: %s)", taskID, task.State)
		}
		outcome.Resolved = true
	} else {
		// Multi-citizen: READY or COLLECTING.
		switch store.TaskState(task.State) {
		case store.TaskReady, store.TaskCollecting:
			// OK.
		case store.TaskAccepted, store.TaskSkipped:
			return nil, fmt.Errorf("task %q already resolved (state: %s) — your submission arrived after the tally closed", taskID, task.State)
		default:
			return nil, fmt.Errorf("task %q cannot accept result (state: %s)", taskID, task.State)
		}
		// Check active claim — the correct semantic for
		// multi-citizen tasks. Fixes the bug where the
		// fat-client checked task.state==claimed instead.
		hasClaim, err := e.store.HasActiveClaim(taskID, citizenID)
		if err != nil {
			return nil, err
		}
		if !hasClaim {
			return nil, fmt.Errorf("task %q has no open claim for this citizen — claim it first", taskID)
		}
		outcome.Collecting = true
	}

	outcome.Plan = store.Plan{
		Version: EngineVersion,
		Mutations: []store.Mutation{
			store.RecordSubmission{
				TaskID:     taskID,
				CitizenID:  citizenID,
				ResultPath: resultPath,
				CommitSHA:  commitSHA,
				Decision:   decision,
				VoteChoice: voteChoice,
				Content:    content,
				TokensUsed: tokensUsed,
			},
		},
	}

	// Emit contribution event. Single-citizen → task_completed.
	// Multi-citizen → review_given or vote_cast (the task isn't
	// "completed" until the tally resolves; the individual
	// submission is a review/vote contribution).
	//
	// Estimated token count: (prompt + content) / 4. This is
	// a consistent cross-model estimate, not an API invoice
	// number. Good enough for comparative analysis ("atomic
	// tasks vs monolithic sessions") in the preprint.
	now := time.Now()
	promptChars := len(task.Prompt)
	contentChars := len(content)
	estimatedTokens := (promptChars + contentChars) / 4
	metadata := fmt.Sprintf(
		`{"tokens":%d,"prompt_chars":%d,"content_chars":%d,"estimated_tokens":%d,"action":%q}`,
		tokensUsed, promptChars, contentChars, estimatedTokens, task.Action,
	)
	if outcome.Resolved {
		eventType := "task_completed"
		subtype := task.Action
		if task.Action == "review" {
			eventType = "review_given"
			subtype = decision
		} else if task.Action == "vote" {
			eventType = "vote_cast"
			subtype = voteChoice
		}
		outcome.Events = append(outcome.Events, store.ContributionEvent{
			CitizenID:    citizenID,
			EventType:    eventType,
			EventSubtype: subtype,
			TaskID:       taskID,
			RunID:        task.RunID,
			Metadata:     metadata,
			CreatedAt:    now,
		})
	} else if outcome.Collecting {
		eventType := "vote_cast"
		subtype := voteChoice
		if task.Action == "review" {
			eventType = "review_given"
			subtype = decision
		}
		outcome.Events = append(outcome.Events, store.ContributionEvent{
			CitizenID:    citizenID,
			EventType:    eventType,
			EventSubtype: subtype,
			TaskID:       taskID,
			RunID:        task.RunID,
			Metadata:     metadata,
			CreatedAt:    now,
		})
	}

	return outcome, nil
}
