package service

import (
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// acceptTaskResult bundles what the caller needs after the
// SUBMITTED → ACCEPTED transition lands.
type acceptTaskResult struct {
	ReadiedTasks []store.ReadiedTask
	RunCompleted bool
}

// acceptTask is the single source of truth for the "task is now
// terminally accepted" sequence. Phase 8.3 split it from the
// inline submit path so the same closing work runs whether the
// caller is:
//
//   - service/submit.go's "no merge needed" branch (review-
//     reject, vote with no commit, etc.) — accepted_merges is
//     empty so there's nothing for the fat-client to confirm,
//     and the helper accepts the task immediately.
//   - report_merge.go's /merges handler, after recording
//     branch_merged for a topic the fat-client just landed on
//     the run branch. Review-approve case calls the helper
//     twice: once for the review's task, once for the upstream
//     target whose content rode in on the same merge.
//
// What the helper does, atomically inside one ApplyPlan:
//
//  1. SetTaskState → ACCEPTED. The apply layer's emission
//     branch fires task_completed because currentState is
//     SUBMITTED (not ACCEPTED), satisfying its non-self-loop
//     guard.
//  2. CompleteRun + AppendCascade. Re-evaluates run state and
//     fires the readiness sweep — newly-ready tasks get
//     task_ready emissions, downstream chains fan out.
//
// And after the apply:
//
//  3. Auto-triage hook (Phase 4c) when the run did NOT just
//     complete. Fires the on-idle remediation only when the
//     run lands on WAITING with no progress possible.
//  4. Auto-close-issue hook when the accepted task carries a
//     closes_issue_seq pointer. The issue moves to closed with
//     the task as the credit.
//
// mergeSHA is the post-merge run-branch tip. The /merges path
// passes it for audit (recorded as the accepted commit_sha);
// the inline path leaves it empty so RecordSubmission's prior
// commit_sha (the topic's tip) stays as the row's commit.
// Overwriting with empty would erase the original SHA and
// break ListTaskHistory + acceptedMergeForTask lookups.
//
// Returns the readied-task list and run-complete flag from the
// cascade so callers can build their response (newly-ready
// tasks for the submit response, run-completion for /merges).
func (c *Coordinator) acceptTask(
	task *store.TaskRecord,
	mergeSHA string,
) (*acceptTaskResult, error) {
	stateMut := store.SetTaskState{
		TaskID:   task.ID,
		NewState: store.TaskAccepted,
	}
	if mergeSHA != "" {
		stateMut.CommitSHA = mergeSHA
	}
	plan := store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			stateMut,
			store.CompleteRun{RunID: task.RunID},
		},
	}.AppendCascade(task.RunID)

	res, err := c.Store.ApplyPlan(plan)
	if err != nil {
		return nil, err
	}

	if !res.RunCompleted {
		c.MaybeAutoTriageIfIdle(task.RunID)
	}
	if task.ClosesIssueSeq > 0 {
		c.maybeAutoCloseIssue(task)
	}

	return &acceptTaskResult{
		ReadiedTasks: res.ReadiedTasks,
		RunCompleted: res.RunCompleted,
	}, nil
}
