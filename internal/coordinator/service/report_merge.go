package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReportMergeParams is the input shape for ReportMerge.
type ReportMergeParams struct {
	TopicBranch string
	RunBranch   string
	MergeSHA    string
	TaskID      string // optional — task whose ACCEPTED state drove this merge
}

// ReportMergeResponse is the wire shape returned to the caller
// after the branch_merged event has been recorded plus, post-
// Phase-8.3, after the SUBMITTED → ACCEPTED transition has
// landed. Mirrors the historical REST response with two new
// fields the fat-client / surface readers use.
type ReportMergeResponse struct {
	Status       string             `json:"status"`
	TopicBranch  string             `json:"topic_branch"`
	RunBranch    string             `json:"run_branch"`
	MergeSHA     string             `json:"merge_sha"`
	NewlyReady   []store.ReadiedTask `json:"newly_ready,omitempty"`
	RunCompleted bool               `json:"run_completed,omitempty"`
	// SyncMode/SyncRemote are the resolved run-completion sync
	// config, populated only when RunCompleted=true. For tasks that
	// land on a per-iteration topic branch, the run completes here
	// (at the merge-report), not at submit — so the fat-client
	// needs the sync mode on THIS response to drive the run-branch
	// → base merge/push. Mirrors the submit response's fields.
	SyncMode   string `json:"sync_mode,omitempty"`
	SyncRemote string `json:"sync_remote,omitempty"`
	// PublishPaths carries the run's declared output set (same as
	// the submit response), populated only when RunCompleted=true.
	// For a topic-branch task the run completes at the merge-report,
	// so the fat-client needs it on THIS response to drive the
	// artifacts-only base publish.
	PublishPaths []string `json:"publish_paths,omitempty"`
}

// ReportMerge handles a successful FF/merge-commit landing of a
// topic onto its run branch. Beyond stamping branch_merged for
// the audit timeline, it completes the task acceptance:
//
//  1. Validate run + project membership.
//  2. Emit branch_merged event inside the SUBMITTED state guard
//     (so a retry POST after the first one already landed
//     produces no duplicate audit row).
//  3. Look up the task. When it's in SUBMITTED, call acceptTask
//     to land the SUBMITTED → ACCEPTED transition + run-level
//     cascade. /merges is the gate to ACCEPTED for tasks whose
//     content needed integration; without this step, SUBMITTED
//     tasks would never become ACCEPTED and downstream would
//     stall.
//  4. Review-approve composition: when the merged task is a
//     review with an approve verdict, the merge carries the
//     upstream's content too (the review's topic was forked
//     from the upstream's). acceptTask runs a second time on
//     the upstream so its task state catches up to the
//     downstream-safe moment. The state-gate on artifact
//     visibility then makes upstream's artifacts live for
//     downstream readiness queries naturally.
//
// Returns the readied-task list + run-completed flag from the
// cascade so callers can render "next up" in the response.
func ReportMerge(c *Coordinator, caller *store.CitizenRecord, projectID int64, runSeq int, params ReportMergeParams) (*ReportMergeResponse, error) {
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
	if params.TopicBranch == "" || params.RunBranch == "" || params.MergeSHA == "" {
		return nil, fmt.Errorf("%w: topic_branch, run_branch, and merge_sha are required", ErrInvalidArgument)
	}
	citizenID := callerID(caller)

	// Diagnostic INFO log on every /merges receipt — gives the
	// "fat-client POST silently dropped" failure mode (load test:
	// 2/488 stuck SUBMITTED with merges in origin) a per-receipt
	// signal that survives even when the audit event below is
	// suppressed by the state guard. Cheap log per merge — 1 line
	// × N tasks at end-of-stage — well worth the future-incident
	// debuggability.
	if c.Logger != nil {
		c.Logger.Info("/merges: received",
			"task_id", params.TaskID,
			"topic_branch", params.TopicBranch,
			"run_branch", params.RunBranch,
			"merge_sha", params.MergeSHA,
			"project_id", projectID,
			"run_seq", runSeq)
	}

	resp := &ReportMergeResponse{
		Status:      "recorded",
		TopicBranch: params.TopicBranch,
		RunBranch:   params.RunBranch,
		MergeSHA:    params.MergeSHA,
	}

	// Idempotency contract for /merges: the fat-client retry loop
	// (see fatclient/service/submit.go reportMerge) means a
	// successful merge can produce N POSTs to this endpoint when
	// the first response is dropped on the way back. Both the
	// audit event AND the state flip must be exactly-once across
	// that retry cycle.
	//
	// branch_merged is therefore emitted at the merge-as-logical-
	// event point — co-fired with acceptTask inside the SUBMITTED
	// state guard — NOT per HTTP receipt. A retry POST that arrives
	// after the state has already flipped finds the task past
	// SUBMITTED and emits no audit row. The retry's diagnostic
	// signal is preserved by the INFO log line above, which fires
	// per receipt and is the right channel for "fat-client tried
	// again." If a per-POST audit signal becomes valuable in the
	// future, add a separate event type (e.g. merge_report_received)
	// rather than overloading branch_merged.
	//
	// The two no-task-found defensive branches below
	// (TaskID == "" and GetTask failure) intentionally drop the
	// audit row too — they're unreachable in normal flow (the fat-
	// client always sends task_id; coord state can't lose a row
	// between submit and merge in a healthy system) and a "we got
	// a /merges with no resolvable task" signal is better surfaced
	// by the INFO/Warn log lines than by an orphaned branch_merged
	// row that points at nothing.
	if params.TaskID == "" {
		return resp, nil
	}
	task, err := c.Store.GetTask(params.TaskID)
	if err != nil || task == nil {
		c.Logger.Warn("/merges: task not found", "task_id", params.TaskID, "error", err)
		return resp, nil
	}
	if store.TaskState(task.State) != store.TaskSubmitted {
		// Task already accepted (replay/retry) or in some other
		// state where the merge confirmation doesn't map onto a
		// state flip. Skip — both branch_merged and the state
		// flip have already been recorded by the original POST
		// that won the race; this is the second arrival.
		return resp, nil
	}

	// State is SUBMITTED — this is the FIRST /merges POST to land
	// for this merge. Stamp the audit row and run acceptTask in
	// the same logical step. If a retry beats us into ApplyPlan
	// for branch_merged, the state guard above will skip them on
	// arrival; only one of the racers reaches this point.
	c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{store.EmitEvent{Event: store.Event{
			CitizenID: citizenID,
			EventType: "branch_merged",
			TaskID:    params.TaskID,
			RunID:     run.ID,
			ProjectID: projectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"topic_branch": params.TopicBranch,
				"run_branch":   params.RunBranch,
				"merge_sha":    params.MergeSHA,
				"run_seq":      run.Seq,
			}),
			CreatedAt: time.Now(),
		}}},
	})

	acceptRes, err := c.acceptTask(task, params.MergeSHA)
	if err != nil {
		c.Logger.Error("/merges: acceptTask failed", "task_id", params.TaskID, "error", err)
		return resp, nil
	}
	resp.NewlyReady = acceptRes.ReadiedTasks
	resp.RunCompleted = acceptRes.RunCompleted

	// Step 3: review-approve composition. When the merged task
	// is a review with a target, the upstream's content rode in
	// on the same merge — flip its task too. The upstream's
	// artifact rows already exist in the index (inserted at
	// upstream's submit time); the state flip makes them
	// visible to downstream readiness queries via the writer-
	// state gate.
	//
	// We don't gate on task.ReviewDecision == approve because
	// (a) collectAcceptedMerges suppresses the review's topic
	// for reject/request_changes verdicts, so /merges only
	// fires here for approves in normal flow, and (b)
	// task.ReviewDecision is empty for multi-citizen reviews
	// (the per-citizen verdicts live on task_claims, the tally
	// winner doesn't backfill the task-level column). The
	// upstream's TaskSubmitted-state guard below catches replay
	// + stale-state retries; if upstream isn't in SUBMITTED, we
	// silently skip.
	if task.Action == "review" && task.ReviewsTarget != "" {
		upstreamID := composeUpstreamID(task.ID, task.ReviewsTarget)
		if upstreamID != "" {
			if upstream, uerr := c.Store.GetTask(upstreamID); uerr == nil && upstream != nil &&
				store.TaskState(upstream.State) == store.TaskSubmitted {
				upRes, upErr := c.acceptTask(upstream, params.MergeSHA)
				if upErr != nil {
					c.Logger.Error("/merges: upstream acceptTask failed",
						"review_id", task.ID, "upstream_id", upstreamID, "error", upErr)
				} else {
					// Append upstream's newly-ready tasks to the
					// response — its downstream consumers can
					// fan out alongside the review's. RunCompleted
					// is OR-aggregated; either acceptTask hitting
					// run-completion is enough to surface it.
					resp.NewlyReady = append(resp.NewlyReady, upRes.ReadiedTasks...)
					if upRes.RunCompleted {
						resp.RunCompleted = true
					}
				}
			}
		}
	}

	// When this merge completed the run, resolve the sync config so
	// the fat-client can run the run-completion sync off this
	// response (the submit response never carried run_completed for
	// a topic-branch task).
	if resp.RunCompleted {
		resp.SyncMode, resp.SyncRemote = parsePublishConfig(run)
		resp.PublishPaths = c.declaredArtifactPaths(run)
	}

	return resp, nil
}

// composeUpstreamID rebuilds the upstream task's full ID from a
// review task's ID + its reviews_target value. Mirrors
// engine.ComputePostSubmitActions's targetID assembly.
//
// reviewID has shape "<projectID>:<runSeq>:<reviewDefID>".
// reviewsTarget is either "<targetDefID>" or
// "<instanceKey>:<targetDefID>". Returns empty when reviewID
// can't be split into the expected three segments.
func composeUpstreamID(reviewID, reviewsTarget string) string {
	parts := strings.SplitN(reviewID, ":", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[0] + ":" + parts[1] + ":" + reviewsTarget
}

// callerID returns the citizen ID, or 0 when caller is nil.
// Pure helper for the membership-not-required-but-attribute-if-
// present pattern (report_merge: anonymous reports allowed for
// legacy clients, but stamp the citizen when one's present).
func callerID(caller *store.CitizenRecord) int64 {
	if caller == nil {
		return 0
	}
	return caller.ID
}
