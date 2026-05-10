package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReportMergeFailedParams is the input shape for
// ReportMergeFailed. The fat-client populates these from a
// non-conflict failure of MergeAcceptedTopic — push rejected,
// "object not found" on a freshly-added remote, network
// timeout post-fetch, etc. Conflict failures (real content
// overlap) take a different path: ReportMergeConflict spawns a
// merge_resolve task and leaves the underlying task in
// SUBMITTED for human resolution.
type ReportMergeFailedParams struct {
	TopicBranch string
	RunBranch   string
	Error       string // verbatim from the underlying git error
	TaskID      string
}

// ReportMergeFailedResponse is the wire shape returned to the
// caller after a merge_failed audit event has been recorded
// AND the underlying task has been driven to FAILED with its
// fail-cascade fired. Callers (fat-client's enju_execute_run
// path) surface the fail-reason + skipped-descendant list
// directly without a follow-up query.
type ReportMergeFailedResponse struct {
	Status             string                 `json:"status"`
	TaskID             string                 `json:"task_id"`
	Reason             string                 `json:"reason"`
	SkippedDescendants []string               `json:"skipped_descendants,omitempty"`
	Rollbacks          []ArtifactRollbackView `json:"rollbacks,omitempty"`
}

// ReportMergeFailed handles the post-submit "merge couldn't
// land" terminal-failure path. Phase 8.4 closes the silent-
// stall class of bugs where a fat-client's auto-merge of an
// ACCEPTED topic onto the run branch hit a non-conflict error
// (push rejected, transport timeout, ref not found) and the
// fat-client logged a Warn + swallowed the error: the task
// stayed ACCEPTED on the coord, downstream tasks fanned out
// against an artifact-index entry whose underlying commit
// never landed, and the cascade stalled one task removed from
// the actual failure.
//
// Post-Phase-8.3 the underlying task is in SUBMITTED at the
// time of the merge attempt (the deferred-accept model). This
// handler:
//
//  1. Validates the task is still in SUBMITTED (replay-safe;
//     a re-issued report on an already-failed task no-ops).
//  2. Invokes the standard fail-cascade: target → FAILED with
//     reason "merge_failed: <error>", regular descendants →
//     SKIPPED with skip_reason "upstream failed: <id>",
//     dynamic descendants deleted, artifact rollbacks
//     computed and applied. Exact same machinery
//     PerformFailCascade uses for engine.fail / review-reject.
//  3. Records a task_failed contribution event attributed to
//     the original claimant so their citizen profile reflects
//     the failure (mirrors FailTask's attribution).
//  4. Returns the skipped-descendant + rollback set so the
//     caller can render them in enju_execute_run's failure
//     line.
//
// Per the design discussion: merge_failed is TERMINAL. The
// claim isn't reopened for retry — git-level non-conflict
// failures are operator misconfig / Enju bugs / infrastructure
// problems, not "the citizen will fix it on retry." Operator
// must invalidate to retry.
func ReportMergeFailed(c *Coordinator, caller *store.CitizenRecord, projectID int64, runSeq int, params ReportMergeFailedParams) (*ReportMergeFailedResponse, error) {
	run, err := c.Store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: run not found", ErrNotFound)
	}
	if !CanReadProject(c.Store, projectID, callerID(caller)) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrNotMember)
	}
	if params.TaskID == "" {
		return nil, fmt.Errorf("%w: task_id is required", ErrInvalidArgument)
	}
	if params.TopicBranch == "" || params.RunBranch == "" {
		return nil, fmt.Errorf("%w: topic_branch and run_branch are required", ErrInvalidArgument)
	}
	if strings.TrimSpace(params.Error) == "" {
		return nil, fmt.Errorf("%w: error is required (the underlying git error verbatim)", ErrInvalidArgument)
	}

	task, err := c.Store.GetTask(params.TaskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("%w: task %q not found", ErrNotFound, params.TaskID)
	}
	if store.TaskState(task.State) != store.TaskSubmitted {
		// Replay / out-of-order report. The task already moved
		// past SUBMITTED — either someone accepted it on a
		// retry, or a prior /merges/failed already drove it to
		// FAILED. Either way, no state change to make. Surface
		// the current state so the caller can decide how to
		// render rather than silently no-op.
		return &ReportMergeFailedResponse{
			Status: string(task.State),
			TaskID: params.TaskID,
			Reason: fmt.Sprintf("noop: task already in %s", task.State),
		}, nil
	}

	reason := fmt.Sprintf("merge_failed: %s", strings.TrimSpace(params.Error))

	// Audit-emit BEFORE the fail-cascade so the timeline
	// records the merge-failure trigger ahead of its
	// cascade_fired consequence event. Same chokepoint pattern
	// the conflict handler uses.
	citizenID := callerID(caller)
	if _, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{store.EmitEvent{Event: store.Event{
			CitizenID: citizenID,
			EventType: "merge_failed",
			TaskID:    params.TaskID,
			RunID:     run.ID,
			ProjectID: projectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"topic_branch": params.TopicBranch,
				"run_branch":   params.RunBranch,
				"run_seq":      run.Seq,
				"error":        params.Error,
			}),
			CreatedAt: time.Now(),
		}}},
	}); err != nil {
		return nil, fmt.Errorf("recording merge_failed event: %w", err)
	}

	// Engine pre-validates the fail-ability (state guard); the
	// SUBMITTED case was added to ComputeFailTask's allowed
	// from-state list in Phase 8.3 specifically for this path.
	if _, err := engine.New(c.Store, c.Logger).ComputeFailTask(params.TaskID, reason); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}
	res, err := c.PerformFailCascade(params.TaskID, reason)
	if err != nil {
		return nil, fmt.Errorf("merge_failed cascade: %w", err)
	}

	// Citizen attribution on the task_failed contribution
	// event: the original claimant, not the fat-client posting
	// the report. Mirrors FailTask's pattern — failure is
	// attributed to the citizen whose work didn't land, so the
	// profile counter reflects "tasks that fell through" from
	// THEIR perspective. Best-effort: an event-record failure
	// here doesn't roll back the cascade.
	if updated, _ := c.Store.GetTask(params.TaskID); updated != nil && updated.ClaimedBy > 0 {
		c.Store.RecordContributionEvent(&store.ContributionEvent{
			CitizenID: updated.ClaimedBy,
			EventType: "task_failed",
			TaskID:    params.TaskID,
			RunID:     updated.RunID,
			ProjectID: run.ProjectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"reason":     reason,
				"merge_fail": true,
			}),
			CreatedAt: time.Now(),
		})
	}

	rb := make([]ArtifactRollbackView, 0, len(res.Rollbacks))
	for _, r := range res.Rollbacks {
		v := ArtifactRollbackView{Path: r.Path}
		if r.Deleted {
			v.Deleted = true
		} else {
			v.RestoredFromTask = r.RestoredFromTask
			v.RestoredFromCommit = r.RestoredFromCommit
		}
		rb = append(rb, v)
	}
	return &ReportMergeFailedResponse{
		Status:             "failed",
		TaskID:             params.TaskID,
		Reason:             reason,
		SkippedDescendants: res.SkippedDescendants,
		Rollbacks:          rb,
	}, nil
}
