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
// post-prepare flow with the push step coalesced.

import (
	"context"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// SubmitBatchEntry is one entry in the batch submission input.
// The fields mirror single-submit (content, decision, option,
// outputs_json, artifacts_json) so a caller composing a batch
// rarely has to learn new shapes — it's the same per-task dict,
// just list-wrapped.
type SubmitBatchEntry struct {
	TaskID        string
	Content       string
	Decision      string
	Option        string
	Outputs       map[string]string
	OutputLists   map[string][]string
	Artifacts     map[string]string
	Model         string // per-entry model override
}

// SubmitBatchParams is the input shape for
// Session.SubmitResultsBatch.
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

// SubmitResultsBatch is the bulk-submit composition. Validates
// each entry, then loops prepareFatSubmit → loops PrepareCommit
// under one lock → single PushPendingCommits → CommitSHAsByTaskID
// to remap post-rebase SHAs → per-entry coordinator report.
// Legacy coordinator-writes projects (no remote_url) fall back
// to per-entry submit calls in the same loop — they have no
// local git step to coalesce.
//
// Failure semantics: best-effort within the batch. A mid-loop
// PrepareCommit failure triggers a hard reset of the branch
// back to the pre-batch HEAD so orphan commits don't accumulate
// on retry. A push failure surfaces for every entry. A
// coordinator-POST failure on a single entry leaves that entry
// in error while subsequent entries still attempt.
//
// All structural validation (cross-project, cross-run, intra-
// batch dependency conflicts, action-specific field presence)
// happens in the handler before this is called — service-side
// failures here are git or coordinator-transport issues.
func (s *Session) SubmitResultsBatch(ctx context.Context, params SubmitBatchParams) (*SubmitBatchResult, error) {
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
			TaskID:        e.TaskID,
			Meta:          bc.meta,
			Content:       e.Content,
			Outputs:       e.Outputs,
			OutputLists:   e.OutputLists,
			Artifacts:     e.Artifacts,
			Decision:      e.Decision,
			Option:        e.Option,
			ModelOverride: e.Model,
			AuthorName:    params.AuthorName,
			AuthorEmail:   params.AuthorEmail,
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
	}

	if len(prepared) > 0 {
		// Every prepared entry shares the same project (scope
		// check enforced by the handler) and branch (same
		// run). Grab from the first.
		proj := prepared[0].Project
		branch := prepared[0].Meta.Branch

		proj.Lock()
		// Snapshot the pre-batch HEAD so a mid-loop
		// PrepareCommit failure can roll back the K already-
		// committed entries. Without this rollback the local
		// branch carries K unpushed commits; a retry appends
		// another K' with the same Enju-Task-Complete
		// trailers, leaving the history with duplicate
		// task-id trailers that CommitSHAsByTaskID then
		// papers over (newest wins). The orphan commits stay
		// on the branch and show up in `git log`, which is
		// confusing even though it's not corrupting.
		preBatchHead, headErr := proj.HeadHash()
		if headErr != nil {
			// Workspace has no HEAD yet (fresh clone, before
			// any commit). Leave preBatchHead empty; the
			// reset path below is a no-op in that case.
			preBatchHead = ""
		}
		commitErr := ""
		taskIDs := make([]string, 0, len(prepared))
		for _, prep := range prepared {
			if _, err := proj.PrepareCommit(workspace.SubmitRequest{
				TaskID:        prep.TaskID,
				Username:      s.Username(),
				AuthorName:    prep.AuthorName,
				AuthorEmail:   prep.AuthorEmail,
				ModelName:     prep.EffectiveModel,
				Files:         prep.Files,
				ArtifactPaths: prep.ArtifactPaths,
				Branch:        branch,
			}); err != nil {
				commitErr = fmt.Sprintf("writing commit for %s to local clone: %v", prep.TaskID, err)
				break
			}
			taskIDs = append(taskIDs, prep.TaskID)
		}

		// If a prepare failed mid-loop, roll the branch back
		// to the pre-batch HEAD so the partial commit chain
		// doesn't persist as orphan commits. A subsequent
		// retry re-prepares from a clean state; no duplicate
		// Enju-Task-Complete trailers in `git log`.
		if commitErr != "" {
			if preBatchHead != "" && len(taskIDs) > 0 {
				if rerr := proj.ResetBranchToHash(preBatchHead); rerr != nil {
					s.logger.Warn("batch rollback: hard-reset after prepare failure",
						"error", rerr, "head", preBatchHead, "failed_at", commitErr)
				}
			}
			proj.Unlock()
			for _, idx := range preparedIdx {
				if results[idx].Status == "" {
					results[idx] = SubmitBatchEntryResult{TaskID: loaded[idx].entry.TaskID, Status: "error", Message: commitErr}
				}
			}
		} else {
			_, finalHeadSHA, pushErr := proj.PushPendingCommits(branch, 3)
			var shaByTask map[string]string
			if pushErr == nil {
				shaByTask, _ = proj.CommitSHAsByTaskID(taskIDs, len(taskIDs)*2+16)
			}
			proj.Unlock()

			// If the push failed, every entry in the batch
			// reports that same error — none reached the
			// coordinator. Local commits stay in the reflog
			// for a manual retry.
			if pushErr != nil {
				for k, prep := range prepared {
					results[preparedIdx[k]] = SubmitBatchEntryResult{
						TaskID:  prep.TaskID,
						Status:  "error",
						Message: "pushing coalesced batch commits: " + pushErr.Error(),
					}
				}
			} else {
				// Advance the scan cursor once to the final
				// HEAD — covers all N commits we just pushed.
				s.advanceScanCursor(prepared[0].Meta.ProjectID, branch, finalHeadSHA)
				// Report each entry to the coordinator with
				// its post-rebase SHA (same as local if no
				// rebase happened).
				for k, prep := range prepared {
					sha := shaByTask[prep.TaskID]
					if sha == "" {
						// Fallback — shouldn't happen because
						// every commit carries the Enju-Task-
						// Complete trailer, but be defensive:
						// report HEAD so the coordinator gets
						// *some* valid SHA rather than an
						// empty one it rejects.
						sha = finalHeadSHA
					}
					prep.ReportBody["commit_sha"] = sha
					data, err := s.coord.Post(ctx, "/api/v1/tasks/"+prep.TaskID+"/result", prep.ReportBody)
					if err != nil {
						results[preparedIdx[k]] = SubmitBatchEntryResult{TaskID: prep.TaskID, Status: "error", Message: "reporting commit: " + err.Error()}
						continue
					}
					if errMsg := extractErrorString(data); errMsg != "" {
						results[preparedIdx[k]] = SubmitBatchEntryResult{TaskID: prep.TaskID, Status: "error", Message: DecorateCoordinatorRejection(errMsg)}
						continue
					}
					// Stash the response body in Message so
					// the formatter can render the per-entry
					// detail; the per-entry formatter uses
					// format.SubmitResult on this string.
					results[preparedIdx[k]] = SubmitBatchEntryResult{TaskID: prep.TaskID, Status: "accepted", Message: string(data)}
				}
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
	return &SubmitBatchResult{Entries: results, AnySuccess: anySuccess}, nil
}

// submitOneForBatch executes a single batch entry through the
// per-task submit path and converts the result into a structured
// SubmitBatchEntryResult. Used only for the legacy
// coordinator-writes fallback (projects without a remote_url) —
// fat-client entries go through the coalesced prepare +
// single-push path in SubmitResultsBatch.
func (s *Session) submitOneForBatch(ctx context.Context, e SubmitBatchEntry, meta *TaskMeta, authorName, authorEmail string) SubmitBatchEntryResult {
	if !s.UseFatClient(meta) {
		// Same git-required contract as SubmitTaskResult.
		return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "error", Message: "submit requires a local workspace; run via `enju mcp`"}
	}
	res := s.SubmitTaskResult(ctx, SubmitParams{
		TaskID:        e.TaskID,
		Meta:          meta,
		Content:       e.Content,
		Outputs:       e.Outputs,
		OutputLists:   e.OutputLists,
		Artifacts:     e.Artifacts,
		Decision:      e.Decision,
		Option:        e.Option,
		ModelOverride: e.Model,
		AuthorName:    authorName,
		AuthorEmail:   authorEmail,
	})
	if res == nil {
		return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "error", Message: "submit returned nil result"}
	}
	if res.ErrorMessage != "" {
		return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "error", Message: res.ErrorMessage}
	}
	return SubmitBatchEntryResult{TaskID: e.TaskID, Status: "accepted", Message: string(res.ResponseBody)}
}

// advanceScanCursor advances the fat-client's scan cursor past
// a pushed commit. Encapsulates the (project id, state dir,
// branch, sha) wiring so single + batch submit share the same
// call shape.
func (s *Session) advanceScanCursor(projectID int64, branch, sha string) {
	if sha == "" {
		return
	}
	workspace.AdvanceScanCursor(projectID, s.StateDir(), branch, sha)
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

