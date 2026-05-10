package service

import (
	"encoding/json"
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// AcceptComputeTaskCoreResult carries the engine products both
// callers of AcceptComputeTaskCore need to continue their work.
// Sync-submit uses them to drive review/vote/reject cascades,
// materialization, and its HTTP response shape; async-reconcile
// ignores most of them and just does the ready sweep +
// run-complete check.
type AcceptComputeTaskCoreResult struct {
	Outcome   *engine.SubmissionOutcome
	Actions   *engine.PostSubmitActions
	ResultPath string
	Decision  string
	VoteChoice string
	SubmitterID int64
}

// AcceptComputeTaskCore runs the "task completed, apply the
// consequences" dance that both sync submit-result and async
// reconcile paths need:
//
//  1. ValidateSubmitRequest (artifacts, paths, citizen).
//  2. ComputeSubmission → state-transition plan
//     (single-citizen → SUBMITTED post-Phase-8.3, multi-citizen
//     → COLLECTING).
//  3. ApplyPlan(submit plan).
//  4. Record contribution events (best-effort, logged).
//  5. ComputePostSubmitActions (artifacts + tally + resolution).
//  6. ApplyPlan(artifact mutations).
//
// Phase 8.3 — the artifact-index rows still get inserted here
// at submit time (step 6). Their VISIBILITY to downstream
// consumers is the part that defers: every artifact-reading
// query (cascade readiness, list_artifacts, get_artifact,
// claim-time reads_artifacts validation) now joins the writer
// task and gates on state IN ('accepted', 'skipped'). A row
// inserted by a SUBMITTED writer is invisible until the
// SUBMITTED → ACCEPTED transition (inline acceptTask or via
// the /merges handler) lands. The cleaner alternative — defer
// the row insert itself — was discarded because it required
// stashing the actually-written list on the task row to
// support the review-approve-flips-upstream case; the state
// gate covers that case naturally (both rows go live the
// moment /merges flips both task states).
//
// Factoring the engine sequence here closes the "reconcile
// forgot step X" bug class that hit us once before
// (ready-sweep omission). New coordination steps added at this
// boundary affect both entry points automatically.
//
// Deliberately does NOT include:
//
//  - The ready-task sweep + run-complete check (callers run
//   those at different positions in their own flow — sync
//   after cascades, reconcile right after core — so leaving
//   them to the caller preserves current ordering byte-for-
//   byte).
//  - Review/vote/reject cascades, materialization, HTTP
//   response — those only apply to the sync path.
//
// Returns (&result, nil) on success. On engine validation or
// apply failure, returns an error with the submit state
// partially applied (the submit plan may have landed even if
// later steps fail — same semantics the legacy api had).
// Caller logs or surfaces the error as HTTP.
func (c *Coordinator) AcceptComputeTaskCore(
	task *store.TaskRecord,
	run *store.RunRecord,
	req *engine.SubmitRequest,
	model string,
) (*AcceptComputeTaskCoreResult, error) {
	eng := engine.New(c.Store, c.Logger)

	resultPath, decision, voteChoice, submitterID, err := eng.ValidateSubmitRequest(task, run, req)
	if err != nil {
		return nil, err
	}
	// Resolve optional model attribution. Empty model is fine
	// for human operators (unaided); bots without a model are
	// rejected at apply time. Unknown-but-valid names auto-
	// register — see ResolveModelByUsername for the rationale.
	modelID, err := ResolveModelByUsername(c.Store, model)
	if err != nil {
		return nil, err
	}
	submitOutcome, err := eng.ComputeSubmission(
		task.ID, submitterID, resultPath, req.CommitSHA,
		decision, voteChoice, req.Content, req.TokensUsed, modelID,
	)
	if err != nil {
		return nil, err
	}
	if _, err := c.Store.ApplyPlan(submitOutcome.Plan); err != nil {
		return nil, fmt.Errorf("applying submit plan: %w", err)
	}

	// Contribution events are append-only and best-effort: a
	// recording failure must not fail the submit (same contract
	// the legacy sync path promised). Model injection is a
	// per-submit client concern the engine doesn't know about,
	// so we stamp it here before writing.
	for i := range submitOutcome.Events {
		evt := &submitOutcome.Events[i]
		if evt.ProjectID == 0 {
			evt.ProjectID = run.ProjectID
		}
		if model != "" && evt.Metadata != "" {
			// parse-modify-marshal instead of a string-suffix
			// trick (which broke on `{}` metadata and on any
			// value containing a literal `}`). Roundtrip via
			// map[string]any survives both.
			var kv map[string]any
			if jerr := json.Unmarshal([]byte(evt.Metadata), &kv); jerr == nil {
				if kv == nil {
					kv = map[string]any{}
				}
				kv["model"] = model
				evt.Metadata = store.MarshalMetadata(kv)
			}
		}
		if rerr := c.Store.RecordContributionEvent(evt); rerr != nil {
			c.Logger.Warn("recording contribution event", "task_id", task.ID, "error", rerr)
		}
	}

	actions, err := eng.ComputePostSubmitActions(task, run, submitOutcome, req, decision, voteChoice)
	if err != nil {
		// Log but don't error — partial state is still
		// applied (events recorded, state transitioned), and a
		// missing post-submit-actions computation shouldn't
		// mask the accepted submission from the caller. Same
		// soft-fail pattern the pre-refactor sync path used.
		c.Logger.Error("post-submit actions failed", "task_id", task.ID, "error", err)
	}

	if actions != nil && len(actions.ArtifactMutations) > 0 {
		if _, err := c.Store.ApplyPlan(store.Plan{
			Version:  engine.EngineVersion,
			Mutations: actions.ArtifactMutations,
		}); err != nil {
			// Log-and-continue rather than hard-fail: the
			// submission has already landed at the task level,
			// and an artifact-index write failure is recoverable
			// (the next submit or invalidation rebuilds the row).
			// The state gate on artifact reads (Phase 8.3) keeps
			// these rows invisible until the writer task hits
			// ACCEPTED, so a missing row is fail-closed for
			// downstream consumers — they treat the artifact as
			// "not yet written" rather than firing prematurely.
			c.Logger.Error("upserting artifact index", "task_id", task.ID, "error", err)
		}
	}

	return &AcceptComputeTaskCoreResult{
		Outcome:   submitOutcome,
		Actions:   actions,
		ResultPath: resultPath,
		Decision:  decision,
		VoteChoice: voteChoice,
		SubmitterID: submitterID,
	}, nil
}
