package service

// Batch-submit orchestration. The single-call shape that submits
// N results under one project lock with a single coalesced
// network push. Designed for paper-scale evaluation workflows
// (bulk review, labeling cohorts) where N individual submits
// inflate conversation context and tool-call budgets without
// buying wall-clock time.
//
// Lives next to submit.go because every helper here either
// composes prepareFatSubmit or mirrors SubmitTaskResult's
// post-prepare flow with the push step coalesced. The actual
// git work is delegated to enjugit.Workflow.SubmitBatch which
// owns the lock / loop-commit / coalesced-push / trailer-scan
// sequence under one structured-diagnostics trace.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// SubmitBatchEntry is one entry in the batch submission input.
// The fields mirror single-submit (content, decision, option,
// outputs_json, artifacts_json) so a caller composing a batch
// rarely has to learn new shapes — it's the same per-task dict,
// just list-wrapped.
type SubmitBatchEntry struct {
	TaskID             string
	Content            string
	Decision           string
	Option             string
	Outputs            map[string]string
	OutputLists        map[string][]string
	Artifacts          map[string]string
	UntrackedArtifacts []string
	Model              string // per-entry model override
}

// SubmitBatchParams is the input shape for
// FatClient.SubmitResultsBatch.
//
// AuthorName + AuthorEmail are resolved by the handler (the
// apiClient profile cache is handler-side); the service layer
// uses the same pair for every commit in the batch.
type SubmitBatchParams struct {
	Entries     []SubmitBatchEntry
	AuthorName  string
	AuthorEmail string
}

// SubmitBatchEntryResult captures one entry's outcome for the
// aggregated batch response. Status surfaces per-entry so the
// caller can inspect without parsing the Message text.
type SubmitBatchEntryResult struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"` // "accepted", "collecting", "error"
	Message string `json:"message,omitempty"`
}

// SubmitBatchResult bundles the per-entry outcomes and an
// aggregate flag so the formatter can render the final
// "anything landed?" line without re-walking the entries.
type SubmitBatchResult struct {
	Entries    []SubmitBatchEntryResult
	AnySuccess bool
}

// SubmitResultsBatch is the bulk-submit composition. Per entry:
// validate + prepareFatSubmit. Then one Workflow.SubmitBatch
// covers loop-commit + per-branch push (with verify) + per-
// branch trailer scan under one lock. Then a per-entry
// coordinator POST loop.
//
// Behavioural parity with single-submit (SubmitTaskResult):
//   - PushWithVerify catches the silent "commit reported but
//     never landed in bare" failure (TP53 Bug 1).
//   - ErrSubmitVerify is unpacked and reported as a
//     push_verify_failed audit event so it shows in run_status
//     and the event log.
//   - TouchProject ticks the project's last-write timestamp
//     after any successful entry so projectreg cache
//     invalidation matches single-submit.
//
// Failure semantics: best-effort within the batch.
//   - prepareFatSubmit failure on one entry leaves it in error
//     and continues with the rest.
//   - Mid-loop commit failure inside SubmitBatch hard-resets
//     the branch back to pre-batch HEAD; the failed entry
//     surfaces a *WorkflowOpError with step trace; later
//     entries surface a generic "rolled back" message.
//   - Push failure inside SubmitBatch flags every committed
//     entry with the same push error; commits stay local.
//   - Coordinator-POST failure on a single entry leaves that
//     entry in error while subsequent entries still attempt.
//
// All structural validation (cross-project, cross-run, intra-
// batch dependency conflicts, action-specific field presence)
// happens in the handler before this is called — service-side
// failures here are git or coordinator-transport issues.
func (s *FatClient) SubmitResultsBatch(ctx context.Context, params SubmitBatchParams) (*SubmitBatchResult, error) {
	if len(params.Entries) == 0 {
		return nil, fmt.Errorf("entries is empty")
	}

	// Snapshot fetch — all tasks exist and their meta is
	// coherent. The handler already validated cross-project /
	// cross-run scope before getting here.
	type batchCtx struct {
		entry SubmitBatchEntry
		meta  *TaskMeta
	}
	loaded := make([]batchCtx, 0, len(params.Entries))
	for i, entry := range params.Entries {
		meta, err := s.FetchTaskMeta(ctx, entry.TaskID)
		if err != nil {
			return nil, fmt.Errorf("entries[%d] (%s): task not found: %w", i, entry.TaskID, err)
		}
		loaded = append(loaded, batchCtx{entry: entry, meta: meta})
	}

	results := make([]SubmitBatchEntryResult, len(loaded))
	var prepared []*preparedFatSubmit
	preparedIdx := make([]int, 0, len(loaded))
	var submitReqs []enjugit.SubmitRequest
	for i, bc := range loaded {
		e := bc.entry
		// Legacy coordinator-writes path (no fat client) has
		// no local git step to coalesce — fall back to the
		// per-entry submit call. The remaining fat-client
		// entries still batch together.
		if !s.UseFatClient(bc.meta) {
			results[i] = s.submitOneForBatch(ctx, e, bc.meta, params.AuthorName, params.AuthorEmail)
			continue
		}
		prep, prepErr := s.prepareFatSubmit(ctx, SubmitParams{
			TaskID:             e.TaskID,
			Meta:               bc.meta,
			Content:            e.Content,
			Outputs:            e.Outputs,
			OutputLists:        e.OutputLists,
			Artifacts:          e.Artifacts,
			UntrackedArtifacts: e.UntrackedArtifacts,
			Decision:           e.Decision,
			Option:             e.Option,
			ModelOverride:      e.Model,
			AuthorName:         params.AuthorName,
			AuthorEmail:        params.AuthorEmail,
		})
		if prepErr != nil {
			results[i] = SubmitBatchEntryResult{TaskID: e.TaskID, Status: "error", Message: prepErr.Error()}
			continue
		}
		// output_lists rides on the report body for
		// coordinator-side dynamic for_each materialization.
		// Set here so the post-push loop doesn't have to
		// re-parse the entry.
		if len(e.OutputLists) > 0 {
			prep.ReportBody["output_lists"] = e.OutputLists
		}
		prepared = append(prepared, prep)
		preparedIdx = append(preparedIdx, i)
		submitReqs = append(submitReqs, buildBatchSubmitRequest(prep, e))
	}

	if len(prepared) > 0 {
		// All prepared entries share one project (handler
		// enforces single-project scope), but may target
		// different branches — answer/develop tasks each have
		// their own per-iteration topic, vote/review tasks
		// land on the run branch directly. enjugit.SubmitBatch
		// groups by effective branch internally and processes
		// one group at a time inside the lock.
		wf := prepared[0].Workflow
		batchRes, batchErr := wf.SubmitBatch(submitReqs)

		// Build a quick lookup branch → final HEAD SHA so the
		// per-entry coord-report fallback (when trailer scan
		// missed an entry) and the scan-cursor advance can use
		// it without re-walking the Branches slice.
		branchHeads := map[string]string{}
		if batchRes != nil {
			for _, b := range batchRes.Branches {
				branchHeads[b.Name] = b.FinalHeadSHA
			}
		}

		switch {
		case batchErr != nil && batchRes == nil:
			// Hard pre-flight failure (empty reqs, validation,
			// etc.). Mark every prepared entry with the error.
			for _, idx := range preparedIdx {
				results[idx] = SubmitBatchEntryResult{
					TaskID:  loaded[idx].entry.TaskID,
					Status:  "error",
					Message: "batch failed: " + batchErr.Error(),
				}
			}
		case batchErr != nil:
			// Mid-loop commit failure (rollback fired) OR push
			// failure. Per-entry Err is set on every Attempted
			// entry that was rolled back, on the failed entry
			// itself, and on the failed branch's entries on
			// push fail. Entries on already-pushed branches
			// (multi-branch batch + later push fail) keep Err
			// nil and are still successes — render those as
			// "accepted" with the post-push report.
			for k, prep := range prepared {
				idx := preparedIdx[k]
				e := batchRes.Entries[k]
				// Push-verify failure on this entry's branch:
				// post the same audit event single-submit does
				// so the run_status / event log can see it.
				// Match single-submit's nil-checks: project_id +
				// run_seq must be present to route the event.
				var verifyErr *enjugit.ErrSubmitVerify
				if e.Err != nil && errors.As(e.Err, &verifyErr) && prep.Meta != nil && prep.Meta.ProjectID > 0 && prep.Meta.RunSeq > 0 {
					s.reportPushVerifyFailed(ctx, prep.Meta.ProjectID, int64(prep.Meta.RunSeq),
						prep.TaskID, verifyErr.Branch, verifyErr.LocalSHA, verifyErr.RemoteSHA, "")
				}
				switch {
				case e.Err != nil:
					results[idx] = SubmitBatchEntryResult{
						TaskID: prep.TaskID, Status: "error",
						Message: e.Err.Error(),
					}
				case !e.Attempted:
					results[idx] = SubmitBatchEntryResult{
						TaskID: prep.TaskID, Status: "error",
						Message: "rolled back due to earlier batch failure",
					}
				default:
					// Committed AND its branch pushed (a sibling
					// branch failed later). Report this entry to
					// coord as a success.
					results[idx] = s.reportBatchEntryToCoord(ctx, prep, e, branchHeads)
				}
			}
		default:
			// Success path: every branch landed. Advance scan
			// cursor per branch, then per-entry coord report
			// (which also drives accept-cascade auto-merges).
			for _, b := range batchRes.Branches {
				if b.FinalHeadSHA != "" {
					enjugit.AdvanceScanCursor(prepared[0].Meta.ProjectID, s.StateDir(), b.Name, b.FinalHeadSHA)
				}
			}
			for k, prep := range prepared {
				idx := preparedIdx[k]
				results[idx] = s.reportBatchEntryToCoord(ctx, prep, batchRes.Entries[k], branchHeads)
			}
		}
	}

	anySuccess := false
	for _, r := range results {
		if r.Status != "error" {
			anySuccess = true
			break
		}
	}
	// TouchProject ticks the projectreg's last-write timestamp
	// for cache invalidation. Mirrors the single-submit path's
	// post-success TouchProject call. Handler contract
	// guarantees single-project scope, so one tick covers the
	// whole batch — read it from the first prepared entry's
	// meta (the legacy fallback path operates on the same
	// project too).
	if anySuccess && len(prepared) > 0 && prepared[0].Meta != nil && prepared[0].Meta.ProjectID > 0 {
		s.TouchProject(prepared[0].Meta.ProjectID)
	}
	return &SubmitBatchResult{Entries: results, AnySuccess: anySuccess}, nil
}

// reportBatchEntryToCoord posts one batch entry's result to the
// coordinator and drives any accept-cascade auto-merges the
// coordinator returns. Factored out so both the success path
// and the partial-success path (multi-branch batch where one
// branch pushed while another failed) share the exact same
// coord transport + auto-merge + error-rendering shape — a
// future change to the coord-report contract touches one site,
// not two.
//
// The auto-merge step matters for review-task batches: when a
// review ACCEPTs, the coordinator's response carries an
// `accepted_merges` array naming the topic branches to FF-merge
// into the run branch. Without this call, the topic stays
// stranded and the review's commit never reaches the run
// branch — which the integration tests catch by walking the
// bare remote's HEAD for trailer-bearing commits.
func (s *FatClient) reportBatchEntryToCoord(ctx context.Context, prep *preparedFatSubmit, e enjugit.BatchEntryResult, branchHeads map[string]string) SubmitBatchEntryResult {
	sha := e.CommitSHA
	if sha == "" {
		sha = branchHeads[e.BranchName]
	}
	prep.ReportBody["commit_sha"] = sha
	data, err := s.coord.Post(ctx, "/api/v1/tasks/"+prep.TaskID+"/result", prep.ReportBody)
	if err != nil {
		return SubmitBatchEntryResult{TaskID: prep.TaskID, Status: "error", Message: "reporting commit: " + err.Error()}
	}
	if errMsg := extractErrorString(data); errMsg != "" {
		return SubmitBatchEntryResult{TaskID: prep.TaskID, Status: "error", Message: DecorateCoordinatorRejection(errMsg)}
	}
	if mergeErr := s.applyAcceptedMerges(ctx, prep.Workflow, data); mergeErr != nil {
		return SubmitBatchEntryResult{TaskID: prep.TaskID, Status: "error", Message: "auto-merging accepted topic branch: " + mergeErr.Error()}
	}
	return SubmitBatchEntryResult{TaskID: prep.TaskID, Status: "accepted", Message: string(data)}
}

// buildBatchSubmitRequest composes the per-entry enjugit
// SubmitRequest from a preparedFatSubmit + its raw entry.
// Mirrors the request shape used by the single-submit path
// (see submit.go's SubmitTaskResult call site) so batch and
// single-submit produce structurally identical commits AND
// land on the same branch (per-iteration topic branch when
// IterationBranch is set, else the run branch). enjugit's
// SubmitBatch supports multi-branch batches by grouping reqs
// by effective branch internally — the service just hands it
// the heterogeneous list.
func buildBatchSubmitRequest(prep *preparedFatSubmit, e SubmitBatchEntry) enjugit.SubmitRequest {
	commitBranch := prep.Meta.Branch
	baseBranch := ""
	if prep.Meta.IterationBranch != "" {
		commitBranch = prep.Meta.IterationBranch
		baseBranch = prep.Meta.Branch
		if prep.Meta.Action == "review" && prep.Meta.UpstreamIterationBranch != "" {
			baseBranch = prep.Meta.UpstreamIterationBranch
		}
	}
	verdict := prep.Decision
	if verdict == "" {
		verdict = prep.Option
	}
	customTrailers := map[string]string{}
	if len(prep.UntrackedArtifactPaths) > 0 {
		customTrailers["Enju-Untracked-Artifacts"] = strings.Join(prep.UntrackedArtifactPaths, ", ")
	}
	return enjugit.SubmitRequest{
		TaskID:         prep.TaskID,
		IterSeq:        prep.Meta.IterSeq,
		RunSeq:         prep.Meta.RunSeq,
		RunSlug:        prep.Meta.RunSlug,
		TaskDef:        prep.Meta.TaskDefID,
		InstanceKey:    prep.Meta.InstanceKey,
		RunBranch:      baseBranch,
		BranchOverride: commitBranch,
		Files:          prep.Files,
		ArtifactPaths:  prep.ArtifactPaths,
		Citizen:        enjugit.Identity{Name: prep.AuthorName, Email: prep.AuthorEmail},
		ModelName:      prep.EffectiveModel,
		Verdict:        verdict,
		CustomTrailers: customTrailers,
	}
}

// shortBatchSHA truncates a SHA for human-readable error
// messages.
func shortBatchSHA(sha string) string {
	if len(sha) >= 8 {
		return sha[:8]
	}
	return sha
}

// submitOneForBatch executes a single batch entry through the
// per-task submit path and converts the result into a structured
// SubmitBatchEntryResult. Used only for the legacy
// coordinator-writes fallback (projects without a remote_url) —
// fat-client entries go through the coalesced prepare +
// single-push path in SubmitResultsBatch.
func (s *FatClient) submitOneForBatch(ctx context.Context, e SubmitBatchEntry, meta *TaskMeta, authorName, authorEmail string) SubmitBatchEntryResult {
	if !s.UseFatClient(meta) {
		// Same git-required contract as SubmitTaskResult.
		return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "error", Message: "submit requires a local workspace; run via `enju mcp`"}
	}
	res := s.SubmitTaskResult(ctx, SubmitParams{
		TaskID:             e.TaskID,
		Meta:               meta,
		Content:            e.Content,
		Outputs:            e.Outputs,
		OutputLists:        e.OutputLists,
		Artifacts:          e.Artifacts,
		UntrackedArtifacts: e.UntrackedArtifacts,
		Decision:           e.Decision,
		Option:             e.Option,
		ModelOverride:      e.Model,
		AuthorName:         authorName,
		AuthorEmail:        authorEmail,
	})
	if res == nil {
		return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "error", Message: "submit returned nil result"}
	}
	if res.ErrorMessage != "" {
		return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "error", Message: res.ErrorMessage}
	}
	return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "accepted", Message: string(res.ResponseBody)}
}

// IntraBatchDependencyConflict reports whether any entry's task
// id appears in another entry's direct DependsOn. The handler
// uses this to reject a batch where an earlier entry's cascade
// (review reject / vote activates) would flip a later entry's
// task to SKIPPED/FAILED/READY before it can submit.
//
// Returns the (i, j) pair of conflicting entries on hit
// (entries[i].DependsOn references entries[j].TaskID), or (-1,
// -1) when clean.
//
// Conservative heuristic: only checks DIRECT depends_on edges,
// not transitive review-target descendants or vote losing-set
// walks. Direct edges cover the shape the real workflows
// exercise (bulk approvals on sibling tasks, labeling over
// independent items). Lives on the service side because the
// shape of "what is a depends_on edge" is a service-layer fact
// (it's what the TaskMeta DependsOn field encodes).
func IntraBatchDependencyConflict(metas []*TaskMeta) (int, int) {
	if len(metas) < 2 {
		return -1, -1
	}
	idSet := make(map[string]int, len(metas))
	for i, m := range metas {
		if m == nil {
			continue
		}
		idSet[m.ID] = i
	}
	for i, m := range metas {
		if m == nil {
			continue
		}
		for _, dep := range strings.Split(m.DependsOn, ",") {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if j, ok := idSet[dep]; ok && j != i {
				return i, j
			}
		}
	}
	return -1, -1
}

// CoherentBatchScope verifies every entry shares one project
// and one run. Returns (projectID, runSeq, badIndex, ok).
// On mismatch, badIndex points at the first entry whose
// project or run differs from entry 0; ok=false. Handler
// surfaces the user-facing error.
//
// Lives here so the same scope policy applies regardless of
// who's calling — keep the rule in one place.
func CoherentBatchScope(metas []*TaskMeta) (projectID int64, runSeq int, badIndex int, ok bool) {
	if len(metas) == 0 {
		return 0, 0, 0, true
	}
	if metas[0] == nil {
		return 0, 0, 0, true
	}
	projectID = metas[0].ProjectID
	runSeq = metas[0].RunSeq
	for i := 1; i < len(metas); i++ {
		m := metas[i]
		if m == nil {
			continue
		}
		if m.ProjectID != projectID || m.RunSeq != runSeq {
			return projectID, runSeq, i, false
		}
	}
	return projectID, runSeq, -1, true
}
