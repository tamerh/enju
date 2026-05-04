package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// SubmitResultParams is the input shape for SubmitTaskResult.
// Mirrors the legacy submitResultRequest body shape — flat
// because clients post it that way and the engine's
// SubmitRequest takes the same fields.
type SubmitResultParams struct {
	CommitSHA    string
	ResultPath    string
	ArtifactsWritten []string
	TokensUsed    int64
	Model      string
	Username     string
	Decision     string // review verdict
	Option      string // vote choice
	Content     string // reviewer prose for {{review.feedback}}
	OutputLists   map[string][]string
}

// SubmitResultResponse is the wire shape for the submit result.
// Many fields are conditional on action type — vote vs review
// vs answer all populate different blocks. Mirrors the legacy
// REST shape exactly so existing fat-client / CLI parsing stays
// stable.
type SubmitResultResponse struct {
	Status        string         `json:"status"`
	ResultPath      string         `json:"result_path"`
	CommitSHA       string         `json:"commit_sha"`
	NewlyReady      []store.ReadiedTask `json:"newly_ready,omitempty"`
	ContributionNumber int          `json:"contribution_number,omitempty"`
	ProjectsThisMonth  int          `json:"projects_this_month,omitempty"`
	Decision       string         `json:"decision,omitempty"`
	ReviewCascade    *ReviewCascadeView   `json:"review_cascade,omitempty"`
	ReviewTally     *ReviewTallyView    `json:"review_tally,omitempty"`
	VoteResolution    *VoteResolutionView   `json:"vote_resolution,omitempty"`
	ArtifactsWritten   []string        `json:"artifacts_written,omitempty"`
	RunCompleted     bool          `json:"run_completed,omitempty"`
	AcceptedMerges    []AcceptedMergeView   `json:"accepted_merges,omitempty"`
	ProjectID       int64          `json:"project_id,omitempty"`
	RunSeq        int           `json:"run_seq,omitempty"`
}

// ReviewCascadeView is the inline review-cascade summary the
// reviewer sees on a request_changes / reject submit.
type ReviewCascadeView struct {
	Target     string  `json:"target"`
	Descendants  []string `json:"descendants"`
	Changed    int    `json:"changed"`
	RollbacksCount int    `json:"rollbacks_count"`
}

// ReviewTallyView mirrors the legacy review_tally inline block.
type ReviewTallyView struct {
	Resolved   bool  `json:"resolved"`
	Verdict   string `json:"verdict"`
	Approves   int  `json:"approves"`
	Rejects   int  `json:"rejects"`
	TotalReviews int  `json:"total_reviews"`
	Reason    string `json:"reason"`
}

// VoteResolutionView covers the vote-action response branches:
// resolved (winning_option + counts), still-collecting (counts +
// reason), and bare auto-resolution (winning_option only).
type VoteResolutionView struct {
	WinningOption string     `json:"winning_option,omitempty"`
	VotesTallied int      `json:"votes_tallied,omitempty"`
	Counts    map[string]int `json:"counts,omitempty"`
	Collecting  bool      `json:"collecting,omitempty"`
	VotesSoFar  int      `json:"votes_so_far,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	Skipped    []string    `json:"skipped,omitempty"`
	SkippedCount int      `json:"skipped_count,omitempty"`
}

// AcceptedMergeView is one (topic-branch, run-branch, commit-sha)
// tuple the fat-client must FF-push to advance the run branch
// onto the accepted iteration's tip. Phase 6b.1 + auto-merge.
type AcceptedMergeView struct {
	TaskID   string `json:"task_id"`
	TopicBranch string `json:"topic_branch"`
	RunBranch  string `json:"run_branch"`
	CommitSHA  string `json:"commit_sha"`
}

// SubmitTaskResult is the orchestration body of the submit
// path: it runs AcceptComputeTaskCore, then walks every post-
// acceptance branch (review-resolve plan, request_changes /
// reject cascades with optional spawn_remediation, vote-resolve
// plan, review-approve close, skip cascade, dynamic
// materialization), then the ready-task sweep + run completion
// + auto-triage hook + auto-close issue, then assembles the
// response.
//
// The caller is expected to have:
//   - resolved the task + project membership
//   - decided whether commit_sha is actually present on the
//   project's remote (the fat-client side does that today)
//
// Errors here are submit-engine errors (validation, plan apply
// failure). Cascade-hop failures are logged but don't fail the
// submission — the legacy api had the same contract.
func (c *Coordinator) SubmitTaskResult(task *store.TaskRecord, params SubmitResultParams) (*SubmitResultResponse, error) {
	taskID := task.ID
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("run not found for task")
	}

	// Steps 1–6 (validate → submit plan → events → post-actions
	// → artifact mutations) are the shared sync/async core.
	engineReq := &engine.SubmitRequest{
		TaskID:      taskID,
		ResultPath:    params.ResultPath,
		CommitSHA:    params.CommitSHA,
		Decision:     params.Decision,
		Option:      params.Option,
		Username:     params.Username,
		Content:     params.Content,
		TokensUsed:    params.TokensUsed,
		ArtifactsWritten: params.ArtifactsWritten,
		OutputLists:   params.OutputLists,
	}
	core, err := c.AcceptComputeTaskCore(task, run, engineReq, params.Model)
	if err != nil {
		// Caller decides 400 vs 500 based on the message
		// prefix — preserved for compatibility with the legacy
		// REST handler.
		return nil, err
	}
	submitOutcome := core.Outcome
	actions := core.Actions
	resultPath := core.ResultPath
	decision := core.Decision
	voteChoice := core.VoteChoice
	submitterID := core.SubmitterID

	// Step 5. Apply review/vote resolution + fire cascades.
	var rejectResult *RemediationOrInvalidation
	var skipResult *SkipCascadeResult
	if actions != nil {
		if actions.ReviewResolvePlan != nil {
			c.Store.ApplyPlan(*actions.ReviewResolvePlan)
		}
		if actions.ShouldRejectTarget && actions.RejectTargetID != "" {
			// emit task_request_changes BEFORE the
			// remediation/invalidate branches so audit
			// consumers see the verdict regardless of which
			// downstream flow runs.
			targetIter, _ := c.Store.GetOpenClaimIterSeq(actions.RejectTargetID)
			// Route through ApplyPlan so the chokepoint contract
			// holds. Single-mutation plan, post-commit drain
			// fires the event to EventStore as before.
			c.Store.ApplyPlan(store.Plan{
				Version: engine.EngineVersion,
				Mutations: []store.Mutation{store.EmitEvent{Event: store.Event{
					CitizenID: submitterID,
					EventType: "task_request_changes",
					TaskID:    actions.RejectTargetID,
					RunID:     task.RunID,
					ProjectID: run.ProjectID,
					Metadata: store.MarshalMetadata(map[string]any{
						"reviewer_id":    submitterID,
						"review_task_id": taskID,
						"iter_seq":       targetIter,
						"decision":       params.Decision,
					}),
					CreatedAt: time.Now(),
				}}},
			})
			// Phase 4b — if the target opted into
			// spawn_remediation on request_changes, skip the
			// invalidate cascade and spawn the remediation
			// instead.
			if spawned, ok := c.MaybeSpawnRemediation(taskID, actions.RejectTargetID, store.ReviewDecisionRequestChanges, store.ReviewDecision(params.Decision), params.Content, submitterID); ok {
				rejectResult = &RemediationOrInvalidation{Task: spawned.Task}
			} else {
				res, cerr := c.PerformInvalidate(actions.RejectTargetID, "request_changes")
				if cerr != nil {
					c.Logger.Error("review-request_changes cascade", "target", actions.RejectTargetID, "error", cerr)
				} else {
					rejectResult = &RemediationOrInvalidation{
						Task:    res.Task,
						Descendants: res.Descendants,
						Changed:   res.Changed,
						Rollbacks:  res.Rollbacks,
					}
				}
				// Phase 6c — request_changes does NOT close
				// the claim; it stays open (outcome=NULL) so
				// the next claim by the same citizen reuses it
				// (revision-within-iteration semantics).
			}
		}
		if actions.ShouldFailTarget && actions.RejectTargetID != "" {
			if spawned, ok := c.MaybeSpawnRemediation(taskID, actions.RejectTargetID, store.ReviewDecisionReject, store.ReviewDecision(params.Decision), params.Content, submitterID); ok {
				rejectResult = &RemediationOrInvalidation{Task: spawned.Task}
			} else {
				// Validate fail-ability (engine precondition)
				// then run the full cascade.
				if _, cerr := engine.New(c.Store, c.Logger).ComputeFailTask(actions.RejectTargetID, "rejected by reviewer"); cerr != nil {
					c.Logger.Error("review-reject fail: compute", "target", actions.RejectTargetID, "error", cerr)
				} else if res, cerr := c.PerformFailCascade(actions.RejectTargetID, "rejected by reviewer"); cerr != nil {
					c.Logger.Error("review-reject fail: cascade", "target", actions.RejectTargetID, "error", cerr)
				} else {
					rejectResult = &RemediationOrInvalidation{
						Task:    res.Task,
						Descendants: res.SkippedDescendants,
						Changed:   res.Changed,
						Rollbacks:  res.Rollbacks,
					}
				}
				// Phase 6c — reject is terminal; close the
				// claim with outcome=rejected. Routed through
				// ApplyPlan so the iteration_completed event
				// rides the chokepoint.
				if _, cerr := c.Store.ApplyPlan(store.Plan{
					Version: engine.EngineVersion,
					Mutations: []store.Mutation{
						store.MarkLatestClaimOutcome{TaskID: actions.RejectTargetID, Outcome: "rejected"},
					},
				}); cerr != nil {
					c.Logger.Warn("close claim on review reject",
						"target", actions.RejectTargetID, "error", cerr)
				}
			}
		}
		if actions.VoteResolvePlan != nil {
			c.Store.ApplyPlan(*actions.VoteResolvePlan)
		}
		// Phase 6c — review approve closes the upstream's
		// claim with outcome=completed. Detected as "this is
		// a review submit AND neither rejection flag is set."
		if task.Action == "review" && task.ReviewsTarget != "" &&
			!actions.ShouldRejectTarget && !actions.ShouldFailTarget {
			targetDef, targetInstance := parseReviewsTargetForMerge(task.ReviewsTarget)
			runTasks, _ := c.Store.ListTasksByRun(task.RunID)
			for _, rt := range runTasks {
				if rt.TaskDefID != targetDef || rt.InstanceKey != targetInstance {
					continue
				}
				if _, cerr := c.Store.ApplyPlan(store.Plan{
					Version: engine.EngineVersion,
					Mutations: []store.Mutation{
						store.MarkLatestClaimOutcome{TaskID: rt.ID, Outcome: store.ClaimOutcomeCompleted},
					},
				}); cerr != nil {
					c.Logger.Warn("close upstream claim on review approve",
						"target", rt.ID, "error", cerr)
				}
				break
			}
		}
		if actions.ShouldSkipCascade {
			updated, _ := c.Store.GetTask(taskID)
			if updated != nil {
				res, cerr := c.PerformSkipCascade(updated, actions.WinningOption)
				if cerr != nil {
					c.Logger.Error("skip cascade", "error", cerr)
				} else {
					skipResult = res
				}
			}
		}
	}

	// Step 6. Dynamic materialization.
	if submitOutcome.Resolved && len(params.OutputLists) > 0 {
		if mErr := c.MaterializeDeferredTasks(task, run, params.OutputLists); mErr != nil {
			c.Logger.Error("materializing deferred tasks", "task_id", taskID, "error", mErr)
		}
	}

	// Step 7. Ready-task sweep + run completion.
	// Fire the cascade as a dedicated plan through ApplyPlan so
	// the single emit site (applyUpdateReadyTasks) handles it —
	// step 5's review/vote-resolve plans + step 6's
	// materialization may all have left tasks newly-ready, but
	// none of them appended the cascade individually because we
	// want ONE cascade after the whole sequence, not one per
	// intermediate plan (avoids duplicate task_ready events for
	// tasks that briefly looked ready then resolved further).
	// Cascade + run-state evaluation in one plan: the readiness
	// pass fires task_ready for any newly-promoted tasks; the
	// CompleteRun re-evaluates the run's state from the
	// resulting task counts (active / idle / completed). One tx,
	// one drain.
	cascadeResult, _ := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CompleteRun{RunID: task.RunID},
		},
	}.AppendCascade(task.RunID))
	readied := cascadeResult.ReadiedTasks
	completed := cascadeResult.RunCompleted

	// Step 7b. Living-workflow phase 4c — auto-triage hook on
	// idle. CheckAndCompleteRun returns true only on the
	// active|idle → completed edge; we still need to fire the
	// idle hook on every transition that lands on idle.
	if !completed {
		c.MaybeAutoTriageIfIdle(task.RunID)
	}

	// Step 7c. Auto-close on accept (phase 4c). If this task
	// was spawned by auto-triage to fix an issue, transition
	// that issue to "closed" now that the fix landed.
	if task.ClosesIssueSeq > 0 && submitOutcome.Resolved {
		c.maybeAutoCloseIssue(task)
	}

	// Step 8. Build response.
	c.Logger.Info("result reported", "task_id", taskID, "path", resultPath, "commit", params.CommitSHA, "newly_ready", len(readied))

	status := "accepted"
	reviewTally := actions.ReviewTally
	tallyOutcome := actions.VoteTally
	voteStillCollecting := tallyOutcome != nil && !tallyOutcome.Resolved
	reviewStillCollecting := reviewTally != nil && !reviewTally.Resolved
	if submitOutcome.Collecting && (voteStillCollecting || reviewStillCollecting || (tallyOutcome == nil && reviewTally == nil)) {
		status = "collecting"
	}

	resp := &SubmitResultResponse{
		Status:    status,
		ResultPath:  resultPath,
		CommitSHA:   params.CommitSHA,
		NewlyReady:  readied,
	}
	if submitterID > 0 {
		contribCount, _ := c.Store.CountContributionEvents(submitterID)
		projectsThisMonth, _ := c.Store.CountProjectsThisMonth(submitterID)
		resp.ContributionNumber = contribCount
		resp.ProjectsThisMonth = projectsThisMonth
	}
	if decision != "" {
		resp.Decision = decision
	}
	if rejectResult != nil {
		resp.ReviewCascade = &ReviewCascadeView{
			Target:     task.ReviewsTarget,
			Descendants:  rejectResult.Descendants,
			Changed:    rejectResult.Changed,
			RollbacksCount: len(rejectResult.Rollbacks),
		}
	}
	if reviewTally != nil {
		resp.ReviewTally = &ReviewTallyView{
			Resolved:   reviewTally.Resolved,
			Verdict:   string(reviewTally.Verdict),
			Approves:   reviewTally.Approves,
			Rejects:   reviewTally.Rejects,
			TotalReviews: reviewTally.TotalReviews,
			Reason:    reviewTally.Reason,
		}
	}
	if task.Action == "vote" {
		var v VoteResolutionView
		populated := false
		if tallyOutcome != nil && tallyOutcome.Resolved {
			v.WinningOption = tallyOutcome.WinningOption
			v.VotesTallied = tallyOutcome.TotalVotes
			v.Counts = tallyOutcome.Counts
			populated = true
		} else if submitOutcome.Resolved && voteChoice != "" {
			v.WinningOption = voteChoice
			populated = true
		} else if tallyOutcome != nil {
			v.Collecting = true
			v.VotesSoFar = tallyOutcome.TotalVotes
			v.Counts = tallyOutcome.Counts
			v.Reason = tallyOutcome.Reason
			populated = true
		}
		if skipResult != nil {
			v.Skipped = skipResult.Skipped
			v.SkippedCount = len(skipResult.Skipped)
			populated = true
		}
		if populated {
			resp.VoteResolution = &v
		}
	}
	if len(params.ArtifactsWritten) > 0 {
		resp.ArtifactsWritten = params.ArtifactsWritten
	}
	if completed {
		resp.RunCompleted = true
	}

	if merges := c.collectAcceptedMerges(taskID, task, actions, run.Branch); len(merges) > 0 {
		resp.AcceptedMerges = merges
		resp.ProjectID = run.ProjectID
		resp.RunSeq = run.Seq
	}
	return resp, nil
}

// RemediationOrInvalidation collapses the two shapes the
// post-acceptance reject/request_changes path can produce: a
// remediation spawn (just a task) OR an invalidation/fail
// cascade (task + descendants + rollbacks). Used internally to
// keep the response-rendering switch flat.
type RemediationOrInvalidation struct {
	Task    *store.TaskRecord
	Descendants []string
	Changed   int
	Rollbacks  []RollbackOutcome
}

// maybeAutoCloseIssue closes the issue linked to a submitted
// fix task. Triggered when a task with closes_issue_seq > 0
// reaches accepted. Best-effort.
func (c *Coordinator) maybeAutoCloseIssue(task *store.TaskRecord) {
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return
	}
	issue, err := c.Store.GetIssueBySeq(run.ProjectID, task.ClosesIssueSeq)
	if err != nil || issue == nil {
		return
	}
	// citizen_id = 0 (system close on auto-triage). The audit
	// event records closed_by_task_id pointing at the fix
	// task — better attribution.
	if _, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CloseIssue{
				IssueID:        issue.ID,
				CitizenID:      0,
				Status:         store.IssueStatusClosed,
				ClosedByTaskID: task.ID,
			},
		},
	}); err != nil {
		c.Logger.Warn("auto-close issue failed", "issue", issue.ID, "task", task.ID, "error", err)
	}
}

// collectAcceptedMerges builds the post-submit list of tasks
// whose accepted topic branch needs FF-merging onto the run
// branch. Caller surfaces these in the submit response so the
// fat-client can drive the merges (trust-the-client — the
// coordinator never touches git).
func (c *Coordinator) collectAcceptedMerges(
	submittedTaskID string,
	submittedTask *store.TaskRecord,
	actions *engine.PostSubmitActions,
	runBranch string,
) []AcceptedMergeView {
	if runBranch == "" {
		return nil
	}
	var out []AcceptedMergeView

	// Case 1: submitter's own task accepted on this submit.
	// Three suppression rules, all variants of "this commit
	// doesn't yet belong on main":
	//
	//  (a) Review that did NOT approve — reject/request_changes
	//   keeps the topic as audit, never merged.
	//  (b) Task with a downstream review (phase 6b.2 fix).
	//   Engine auto-accepts answer/compute on submit even with
	//   pending reviewer; without this guard those topics
	//   merged to main BEFORE the reviewer saw them.
	//  (c) Future state-machine cases — keep this gate the
	//   only place merge eligibility is decided.
	skipMergeOfSelf := false
	if submittedTask != nil && submittedTask.Action == "review" &&
		actions != nil && (actions.ShouldRejectTarget || actions.ShouldFailTarget) {
		skipMergeOfSelf = true
	}
	if !skipMergeOfSelf && submittedTask != nil && c.taskHasDownstreamReview(submittedTask) {
		skipMergeOfSelf = true
	}
	if !skipMergeOfSelf {
		if cur, err := c.Store.GetTask(submittedTaskID); err == nil && cur != nil &&
			store.TaskState(cur.State) == store.TaskAccepted {
			if t := c.acceptedMergeForTask(cur.ID, runBranch); t != nil {
				out = append(out, *t)
			}
		}
	}

	// Case 2: review approved upstream. Review's own topic was
	// forked from upstream's topic at claim time, so the case-1
	// merge above (which emits the review's own topic) is
	// sufficient when the review HAS its own topic. The
	// fallback below covers legacy/multi-citizen rows where the
	// reviewer's IterationBranch is empty.
	if submittedTask != nil && submittedTask.Action == "review" && submittedTask.ReviewsTarget != "" &&
		actions != nil && !actions.ShouldRejectTarget && !actions.ShouldFailTarget {
		reviewHadTopic := false
		for _, m := range out {
			if m.TaskID == submittedTaskID {
				reviewHadTopic = true
				break
			}
		}
		if !reviewHadTopic {
			targetDef, targetInstance := parseReviewsTargetForMerge(submittedTask.ReviewsTarget)
			runTasks, _ := c.Store.ListTasksByRun(submittedTask.RunID)
			for _, rt := range runTasks {
				if rt.TaskDefID != targetDef {
					continue
				}
				if rt.InstanceKey != targetInstance {
					continue
				}
				if store.TaskState(rt.State) != store.TaskAccepted {
					continue
				}
				if t := c.acceptedMergeForTask(rt.ID, runBranch); t != nil {
					out = append(out, *t)
				}
				break
			}
		}
	}

	return out
}

// taskHasDownstreamReview reports whether any task in the same
// run is an action:review that targets `t` (matched on
// (TaskDefID, InstanceKey)). O(log N) via the
// idx_tasks_reviews_target partial index.
func (c *Coordinator) taskHasDownstreamReview(t *store.TaskRecord) bool {
	if t == nil {
		return false
	}
	has, err := c.Store.HasReviewerOfTarget(t.RunID, t.TaskDefID, t.InstanceKey)
	if err != nil {
		return false
	}
	return has
}

// acceptedMergeForTask renders one merge target for a task
// currently in state=accepted. Returns nil when the task has
// no topic branch (vote/review action) or no commit_sha on its
// latest claim (untracked-only outputs).
func (c *Coordinator) acceptedMergeForTask(taskID, runBranch string) *AcceptedMergeView {
	hist, err := c.Store.ListTaskHistory(taskID)
	if err != nil {
		return nil
	}
	for i := len(hist) - 1; i >= 0; i-- {
		h := hist[i]
		if h.CommitSHA == "" {
			continue
		}
		if h.Branch == "" {
			return nil
		}
		return &AcceptedMergeView{
			TaskID:   taskID,
			TopicBranch: h.Branch,
			RunBranch:  runBranch,
			CommitSHA:  h.CommitSHA,
		}
	}
	return nil
}

// parseReviewsTargetForMerge mirrors mcpserver.parseReviewsTarget
// for the merge-targets path. Split a reviews_target value
// (either "defID" or "instanceKey:defID") into (defID, instanceKey).
func parseReviewsTargetForMerge(target string) (string, string) {
	if idx := strings.Index(target, ":"); idx > 0 {
		return target[idx+1:], target[:idx]
	}
	return target, ""
}
