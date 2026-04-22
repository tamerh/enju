package mcpserver

// Submit-path handlers + client-side validators. The fat
// client's submit flow is the most complex surface in the MCP
// layer: it pre-validates action-specific inputs (review
// decision, vote option) BEFORE any git write, computes the
// result-file layout (per-citizen subdir for multi-citizen
// tasks, per-output files for named-outputs schemas, review
// metadata for the immutable audit trail), commits and pushes
// to the project's remote, then reports the commit SHA back to
// the coordinator.
//
// The pre-validation is the invariant — any action-specific
// reject after a commit would strand a phantom commit in the
// append-only history. Every client-side check belongs before
// the workspace touch.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/mcpgit"
	"github.com/mark3labs/mcp-go/mcp"
)

// batchSubmission is one entry in the submit-results-batch
// input payload. The fields mirror single-submit (content,
// decision, option, outputs_json, artifacts_json) so a caller
// composing a batch rarely has to learn new shapes — it's the
// same per-task dict, just list-wrapped.
type batchSubmission struct {
	TaskID        string                 `json:"task_id"`
	Content       string                 `json:"content,omitempty"`
	Decision      string                 `json:"decision,omitempty"`
	Option        string                 `json:"option,omitempty"`
	OutputsJSON   string                 `json:"outputs_json,omitempty"`
	ArtifactsJSON string                 `json:"artifacts_json,omitempty"`
	// Raw retained for the coordinator's error reports — the
	// LLM sees "entry N: <error>" pointed at the entry that
	// failed validation, not a positional index it has to
	// cross-reference.
	raw map[string]interface{}
}

// batchEntryResult captures one entry's outcome for the
// aggregated batch response. IsError surfaces per-entry so the
// caller can inspect without parsing the Text.
type batchEntryResult struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"` // "accepted", "collecting", "error"
	Message string `json:"message,omitempty"`
}

// handleSubmitResultsBatch — one MCP tool call submitting N
// results under a single project lock, with N local commits
// coalesced into a single network push.
//
// Contract:
//
//   - Scope: one caller (this citizen), one project, one run.
//     Cross-run or cross-project batching is rejected upfront
//     rather than routed through — the submit path's workspace
//     + branch handling assumes a single repo+branch.
//   - Pre-validation: each entry's task must exist, be claimed
//     by this citizen, and carry the action-specific fields
//     (decision for review, option for vote, at least one of
//     content/outputs_json/artifacts_json otherwise). Intra-
//     batch dependency conflicts (one entry's task listed in
//     another's depends_on) are rejected so later entries
//     don't silently operate on post-cascade state. Any
//     validation failure rejects the whole batch before any
//     git or coordinator state mutates — no phantom commits.
//   - Execution: loops prepareFatSubmit → loops
//     Project.PrepareCommit under one lock → single
//     Project.PushPendingCommits → Project.CommitSHAsByTaskID
//     to remap post-rebase SHAs → per-entry coordinator
//     report. Legacy coordinator-writes projects (no
//     remote_url) fall back to per-entry submit calls in the
//     same loop — they have no local git step to coalesce.
//   - Failure semantics: best-effort within the batch. A
//     mid-loop PrepareCommit failure triggers a hard reset
//     of the branch back to the pre-batch HEAD so orphan
//     commits don't accumulate on retry (every retry
//     composes from a clean state). A push failure surfaces
//     for every entry — local commits stay in the reflog
//     for manual recovery. A coordinator-POST failure on a
//     single entry leaves that entry in error while
//     subsequent entries still attempt.
func (c *apiClient) handleSubmitResultsBatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	submissionsJSON, err := req.RequireString("submissions")
	if err != nil {
		return mcp.NewToolResultError("submissions is required (JSON array of submission objects)"), nil
	}
	var rawList []map[string]interface{}
	if err := json.Unmarshal([]byte(submissionsJSON), &rawList); err != nil {
		return mcp.NewToolResultError("submissions must be a JSON array: " + err.Error()), nil
	}
	if len(rawList) == 0 {
		return mcp.NewToolResultError("submissions is empty"), nil
	}

	// Step 1: parse + structural validation. Every
	// entry must carry a task_id; action-specific field
	// presence is checked per-entry after we fetch meta.
	entries := make([]batchSubmission, 0, len(rawList))
	for i, raw := range rawList {
		id, _ := raw["task_id"].(string)
		if id == "" {
			return mcp.NewToolResultError(fmt.Sprintf("submissions[%d]: task_id is required", i)), nil
		}
		entry := batchSubmission{TaskID: id, raw: raw}
		if v, ok := raw["content"].(string); ok {
			entry.Content = v
		}
		if v, ok := raw["decision"].(string); ok {
			entry.Decision = v
		}
		if v, ok := raw["option"].(string); ok {
			entry.Option = v
		}
		if v, ok := raw["outputs_json"].(string); ok {
			entry.OutputsJSON = v
		}
		if v, ok := raw["artifacts_json"].(string); ok {
			entry.ArtifactsJSON = v
		}
		entries = append(entries, entry)
	}

	// Step 2: snapshot fetch — all tasks exist and
	// their meta is coherent. Also pins project + run scope:
	// every entry must share one project + one run.
	type batchCtx struct {
		entry batchSubmission
		meta  *taskMeta
	}
	loaded := make([]batchCtx, 0, len(entries))
	var projectID int64
	var runSeq int
	for i, entry := range entries {
		meta, err := c.fetchTaskMeta(ctx, entry.TaskID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): task not found: %v", i, entry.TaskID, err)), nil
		}
		if i == 0 {
			projectID = meta.ProjectID
			runSeq = meta.RunSeq
		} else {
			if meta.ProjectID != projectID {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): cross-project batch not supported — all entries must share project %d", i, entry.TaskID, projectID)), nil
			}
			if meta.RunSeq != runSeq {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): cross-run batch not supported — all entries must share run %d", i, entry.TaskID, runSeq)), nil
			}
		}
		loaded = append(loaded, batchCtx{entry: entry, meta: meta})
	}

	// Step 3: action-specific field presence + primary
	// data present. Mirrors single-submit's validation: a
	// review needs `decision`, a vote needs `option`, anything
	// else needs at least one of content/outputs/artifacts.
	// Validating here (before any commit) preserves the no-
	// phantom-commit invariant for the whole batch.
	for i, bc := range loaded {
		e := bc.entry
		switch bc.meta.Action {
		case "review":
			if e.Decision == "" {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): review task requires 'decision'", i, e.TaskID)), nil
			}
			if errMsg := validateReviewDecision(e.Decision); errMsg != "" {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): %s", i, e.TaskID, errMsg)), nil
			}
		case "vote":
			if e.Option == "" {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): vote task requires 'option'", i, e.TaskID)), nil
			}
		default:
			if e.Content == "" && e.OutputsJSON == "" && e.ArtifactsJSON == "" {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): at least one of 'content', 'outputs_json', or 'artifacts_json' is required", i, e.TaskID)), nil
			}
		}
	}

	// Step 4: intra-batch dependency conflict check.
	// Catches the common case where an earlier entry's cascade
	// (review reject / vote activates) would flip a later
	// entry's task to SKIPPED/FAILED/READY before it can
	// submit. Conservative heuristic: reject if any entry's
	// task appears in another entry's direct depends_on. Does
	// NOT simulate full cascades (transitive review-target
	// descendants, vote losing-set walks) — those remain the
	// best-effort runtime failure mode. Direct edges cover
	// the shape the real workflows exercise (bulk approvals
	// on sibling tasks, labeling over independent items);
	// deeper simulation can land later if a concrete
	// workflow needs it.
	idSet := make(map[string]int, len(loaded))
	for i, bc := range loaded {
		idSet[bc.entry.TaskID] = i
	}
	for i, bc := range loaded {
		deps := strings.Split(bc.meta.DependsOn, ",")
		for _, dep := range deps {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if j, ok := idSet[dep]; ok && j != i {
				return mcp.NewToolResultError(fmt.Sprintf(
					"submissions[%d] (%s) directly depends on submissions[%d] (%s); batch must not mix a task with its upstream — submit the upstream first, then batch the rest",
					i, bc.entry.TaskID, j, loaded[j].entry.TaskID)), nil
			}
		}
	}

	// Execute: push coalescing.
	//
	// prepareFatSubmit (pure computation, no git) → loop
	// PrepareCommit (overlay + local commit, no push) →
	// single PushPendingCommits → CommitSHAsByTaskID to
	// recover post-rebase SHAs → per-entry coordinator
	// report. N commits, 1 push, 1 rebase (if needed).
	//
	// Lock scope: acquired once around all prepares + the
	// single push. A rebase inside PushPendingCommits
	// rewrites every un-pushed local commit; if another
	// goroutine could prepare between commits, its commit
	// would be tangled in the rebase in an undefined way.
	//
	// Fallback: any entry whose prepare fails (terminal
	// state, validation, etc.) is skipped. Its report
	// becomes an error entry and subsequent entries still
	// attempt. We don't rewind earlier commits — a rebase
	// on top handles the "keep what landed, drop what
	// didn't" part implicitly.
	results := make([]batchEntryResult, len(loaded))
	var prepared []*preparedFatSubmit
	preparedIdx := make([]int, 0, len(loaded))
	for i, bc := range loaded {
		e := bc.entry
		// Legacy coordinator-writes path (no fat client) has
		// no local git step to coalesce — fall back to the
		// per-entry submit call. The remaining fat-client
		// entries still batch together.
		if !c.useFatClient(bc.meta) {
			results[i] = c.submitOneForBatch(ctx, e, bc.meta)
			continue
		}
		outputs, outputLists, artifacts, parseErr := parseBatchEntryOutputs(e)
		if parseErr != "" {
			results[i] = batchEntryResult{TaskID: e.TaskID, Status: "error", Message: parseErr}
			continue
		}
		prep, errRes := c.prepareFatSubmit(ctx, e.TaskID, bc.meta, e.Content, outputs, outputLists, artifacts, e.Decision, e.Option)
		if errRes != nil {
			results[i] = batchEntryResult{TaskID: e.TaskID, Status: "error", Message: toolResultPlainText(errRes)}
			continue
		}
		// output_lists rides on the report body for
		// coordinator-side dynamic for_each materialization.
		// Set here so the post-push loop doesn't have to
		// re-parse the entry.
		if len(outputLists) > 0 {
			prep.ReportBody["output_lists"] = outputLists
		}
		prepared = append(prepared, prep)
		preparedIdx = append(preparedIdx, i)
	}

	if len(prepared) > 0 {
		// Every prepared entry shares the same project (scope
		// check earlier) and branch (same run). Grab from the
		// first.
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
			// reset path below is a no-op in that case —
			// there's nothing to roll back to.
			preBatchHead = ""
		}
		commitErr := ""
		taskIDs := make([]string, 0, len(prepared))
		for _, prep := range prepared {
			if _, err := proj.PrepareCommit(mcpgit.SubmitRequest{
				TaskID:        prep.TaskID,
				Username:      c.username,
				AuthorName:    prep.AuthorName,
				AuthorEmail:   prep.AuthorEmail,
				ModelName:     c.modelName,
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
					c.logger.Warn("batch rollback: hard-reset after prepare failure",
						"error", rerr, "head", preBatchHead, "failed_at", commitErr)
				}
			}
			proj.Unlock()
			for _, idx := range preparedIdx {
				if results[idx].Status == "" {
					results[idx] = batchEntryResult{TaskID: loaded[idx].entry.TaskID, Status: "error", Message: commitErr}
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
					results[preparedIdx[k]] = batchEntryResult{
						TaskID:  prep.TaskID,
						Status:  "error",
						Message: "pushing coalesced batch commits: " + pushErr.Error(),
					}
				}
			} else {
				// Advance the scan cursor once to the final
				// HEAD — covers all N commits we just pushed.
				c.advanceScanCursor(prepared[0].Meta.ProjectID, branch, finalHeadSHA)
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
					data, err := c.post(ctx, "/api/v1/tasks/"+prep.TaskID+"/result", prep.ReportBody)
					if err != nil {
						results[preparedIdx[k]] = batchEntryResult{TaskID: prep.TaskID, Status: "error", Message: "reporting commit: " + err.Error()}
						continue
					}
					if errMsg := extractErrorString(data); errMsg != "" {
						results[preparedIdx[k]] = batchEntryResult{TaskID: prep.TaskID, Status: "error", Message: decorateCoordinatorRejection(errMsg)}
						continue
					}
					results[preparedIdx[k]] = batchEntryResult{TaskID: prep.TaskID, Status: "accepted", Message: formatSubmitResult(data, prep.TaskID)}
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
	return mcp.NewToolResultText(formatBatchResults(results, anySuccess)), nil
}

// parseBatchEntryOutputs duplicates the single-submit
// outputs_json + artifacts_json parsing for the batch path.
// Returns either a populated (outputs, outputLists, artifacts)
// triple or an error string describing the malformed entry.
// Kept close to handleSubmitResult's parsing so the shapes
// stay identical — the batch entry's input is exactly the
// same JSON as a single-submit body.
func parseBatchEntryOutputs(e batchSubmission) (map[string]string, map[string][]string, map[string]string, string) {
	var outputs map[string]string
	var outputLists map[string][]string
	if e.OutputsJSON != "" {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(e.OutputsJSON), &raw); err != nil {
			return nil, nil, nil, "outputs_json must be valid JSON: " + err.Error()
		}
		for name, v := range raw {
			switch val := v.(type) {
			case string:
				if outputs == nil {
					outputs = make(map[string]string)
				}
				outputs[name] = val
			case []interface{}:
				list := make([]string, 0, len(val))
				for _, item := range val {
					s, ok := item.(string)
					if !ok {
						return nil, nil, nil, fmt.Sprintf("outputs_json[%q]: list items must be strings", name)
					}
					list = append(list, s)
				}
				if outputLists == nil {
					outputLists = make(map[string][]string)
				}
				outputLists[name] = list
			default:
				return nil, nil, nil, fmt.Sprintf("outputs_json[%q]: value must be a string or list of strings", name)
			}
		}
	}
	var artifacts map[string]string
	if e.ArtifactsJSON != "" {
		if err := json.Unmarshal([]byte(e.ArtifactsJSON), &artifacts); err != nil {
			return nil, nil, nil, "artifacts_json must be valid JSON: " + err.Error()
		}
	}
	return outputs, outputLists, artifacts, ""
}

// advanceScanCursor advances the fat-client's scan cursor
// past a pushed commit. Encapsulates the (project id, state
// dir, branch, sha) wiring so batch and single-submit share
// the same call shape.
func (c *apiClient) advanceScanCursor(projectID int64, branch, sha string) {
	if sha == "" {
		return
	}
	// mcpgit's advanceCursorIfConfigured lives inside the
	// package; since we're in mcpserver we go through the
	// public workspace API — but the only current caller
	// that needs this is post-push, where HEAD resolution
	// happens before Unlock. Here we do the no-op when
	// configuration is missing, same as the internal helper.
	mcpgit.AdvanceScanCursor(projectID, c.stateDir(), branch, sha)
}

// submitOneForBatch executes a single batch entry through the
// per-task submit path and converts the CallToolResult into a
// structured batchEntryResult. Used only for the legacy
// coordinator-writes fallback (projects without a remote_url)
// — fat-client entries go through the coalesced prepare +
// single-push path in handleSubmitResultsBatch.
//
// Shares parseBatchEntryOutputs with that path so the
// outputs_json + artifacts_json shape stays identical
// between the two. A single-submit body IS a batch-entry
// body minus the list wrapper — no second parser to drift
// against the first.
func (c *apiClient) submitOneForBatch(ctx context.Context, e batchSubmission, meta *taskMeta) batchEntryResult {
	outputs, outputLists, artifacts, parseErr := parseBatchEntryOutputs(e)
	if parseErr != "" {
		return batchEntryResult{TaskID: e.TaskID, Status: "error", Message: parseErr}
	}

	var res *mcp.CallToolResult
	if c.useFatClient(meta) {
		res, _ = c.submitResultFatClient(ctx, e.TaskID, meta, e.Content, outputs, outputLists, artifacts, e.Decision, e.Option)
	} else {
		// Legacy coordinator-writes path — same shape the
		// single-submit handler builds.
		body := map[string]interface{}{"model": c.modelName}
		if outputs != nil {
			body["outputs"] = outputs
		}
		if e.Content != "" {
			body["content"] = e.Content
		}
		if artifacts != nil {
			body["artifacts"] = artifacts
		}
		if e.Decision != "" {
			body["decision"] = e.Decision
		}
		if e.Option != "" {
			body["option"] = e.Option
		}
		data, err := c.post(ctx, "/api/v1/tasks/"+e.TaskID+"/result", body)
		if err != nil {
			return batchEntryResult{TaskID: e.TaskID, Status: "error", Message: err.Error()}
		}
		res = mcp.NewToolResultText(formatSubmitResult(data, e.TaskID))
	}
	if res == nil {
		return batchEntryResult{TaskID: e.TaskID, Status: "error", Message: "submit returned nil result"}
	}
	if res.IsError {
		return batchEntryResult{TaskID: e.TaskID, Status: "error", Message: toolResultPlainText(res)}
	}
	// Default path reports "accepted" — the single-submit
	// formatter already distinguishes collecting / accepted in
	// its text, we surface that back via Message for UI.
	return batchEntryResult{TaskID: e.TaskID, Status: "accepted", Message: toolResultPlainText(res)}
}

// toolResultPlainText flattens a CallToolResult's text content
// into a single string for the batch-entry Message field.
// Multi-part content concatenates with newlines; non-text
// content is dropped (batch responses never emit image/resource
// content from submit anyway).
func toolResultPlainText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	data, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	var shape struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(data, &shape) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range shape.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// formatBatchResults renders a batch outcome. Header line
// summarizes N/M successful; each entry gets a one-line
// status with ✓/✗ prefix and the per-entry message appended
// for errors. Structured enough that a scripted citizen can
// grep for "✗" but readable for humans.
func formatBatchResults(results []batchEntryResult, anySuccess bool) string {
	var b strings.Builder
	var ok, fail int
	for _, r := range results {
		if r.Status == "error" {
			fail++
		} else {
			ok++
		}
	}
	if fail == 0 {
		b.WriteString(fmt.Sprintf("✓ Batch submit: %d/%d accepted\n\n", ok, len(results)))
	} else if ok == 0 {
		b.WriteString(fmt.Sprintf("✗ Batch submit: 0/%d accepted (all failed)\n\n", len(results)))
	} else {
		b.WriteString(fmt.Sprintf("⚠ Batch submit: %d/%d accepted, %d failed\n\n", ok, len(results), fail))
	}
	for _, r := range results {
		prefix := "✓"
		if r.Status == "error" {
			prefix = "✗"
		}
		b.WriteString(fmt.Sprintf("%s %s\n", prefix, r.TaskID))
		if r.Status == "error" && r.Message != "" {
			// Indent the error message so it nests cleanly
			// under the task id — easier for humans to skim
			// a failure list.
			for _, line := range strings.Split(r.Message, "\n") {
				if line != "" {
					b.WriteString("    " + line + "\n")
				}
			}
		}
	}
	if !anySuccess {
		b.WriteString("\nNothing landed — check the errors above, fix, and resubmit.\n")
	}
	return b.String()
}

// parseReviewsTarget splits a stored reviews_target into its
// (task_def_id, instance_key) components. For non-for_each
// reviews the target is just the def id ("draft" → "draft", "").
// For per-instance reviews it's the instance-matched short form
// "instanceKey:defID" ("alpha:expand" → "expand", "alpha").
//
// Keeps server.go's review-block resolver independent of which
// shape materialize.go / create_run.go actually wrote — both
// cases produce a valid (defID, instanceKey) pair the Dependency
// list can be matched against.
func parseReviewsTarget(target string) (defID, instanceKey string) {
	if idx := strings.Index(target, ":"); idx >= 0 {
		return target[idx+1:], target[:idx]
	}
	return target, ""
}
func (c *apiClient) handleSubmitResult(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	content := req.GetString("content", "")
	outputsJSON := req.GetString("outputs_json", "")
	artifactsJSON := req.GetString("artifacts_json", "")
	decision := req.GetString("decision", "")
	option := req.GetString("option", "")

	// Primary-field presence check. A vote task can submit with
	// just `option`, a review task with just `decision` — those
	// actions treat the action-specific field as the primary
	// signal and prose content is optional commentary. Without
	// the decision/option here the check emits a misleading
	// "content is required" error on an option-only vote.
	if content == "" && outputsJSON == "" && artifactsJSON == "" && decision == "" && option == "" {
		return mcp.NewToolResultError("at least one of 'content', 'outputs_json', 'artifacts_json', 'decision' (review), or 'option' (vote) is required"), nil
	}
	// Any non-empty decision must be valid, regardless of action.
	// The "required for review" check happens in the fat-client
	// pre-validation and on the coordinator.
	if errMsg := validateReviewDecision(decision); decision != "" && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}

	// Parse outputs_json into two buckets: plain string
	// values (the existing named-outputs path, one file per
	// output) and list<string> values (Phase J.1 — routed to
	// the coordinator so dynamic for_each can materialize
	// downstream instances from the resolved list). Accepting
	// interface{} here keeps the tool's input JSON shape
	// natural — a list param is a JSON array, a string param
	// is a JSON string.
	var outputs map[string]string
	var outputLists map[string][]string
	if outputsJSON != "" {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(outputsJSON), &raw); err != nil {
			return mcp.NewToolResultError("outputs_json must be valid JSON object: " + err.Error()), nil
		}
		for name, v := range raw {
			switch val := v.(type) {
			case string:
				if outputs == nil {
					outputs = make(map[string]string)
				}
				outputs[name] = val
			case []interface{}:
				list := make([]string, 0, len(val))
				for i, item := range val {
					s, ok := item.(string)
					if !ok {
						return mcp.NewToolResultError(fmt.Sprintf("outputs_json[%q][%d]: list items must be strings", name, i)), nil
					}
					list = append(list, s)
				}
				if outputLists == nil {
					outputLists = make(map[string][]string)
				}
				outputLists[name] = list
			default:
				return mcp.NewToolResultError(fmt.Sprintf("outputs_json[%q]: value must be a string or a list of strings", name)), nil
			}
		}
	}
	var artifacts map[string]string
	if artifactsJSON != "" {
		if err := json.Unmarshal([]byte(artifactsJSON), &artifacts); err != nil {
			return mcp.NewToolResultError("artifacts_json must be valid JSON object: " + err.Error()), nil
		}
	}

	// Task-existence check up front. fetchTaskMeta returns an
	// error if the coordinator can't find the task (typo, wiped
	// DB, wrong ID). Surface that as a clean "task not found"
	// instead of letting the legacy fallback path POST into a
	// void and surface the server's internal "commit_sha is
	// required" contract error as if it were the real problem.
	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("task %q not found: %v", taskID, metaErr)), nil
	}
	if c.useFatClient(meta) {
		return c.submitResultFatClient(ctx, taskID, meta, content, outputs, outputLists, artifacts, decision, option)
	}

	// Legacy coordinator-writes path.
	body := map[string]interface{}{
		"model": c.modelName,
	}
	if outputs != nil {
		body["outputs"] = outputs
	}
	if content != "" {
		body["content"] = content
	}
	if artifacts != nil {
		body["artifacts"] = artifacts
	}
	if decision != "" {
		body["decision"] = decision
	}
	if option != "" {
		body["option"] = option
	}
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatSubmitResult(data, taskID)), nil
}
// submitResultFatClient is the iteration A.2 submit path: write the
// result and any artifacts into the project's local clone, commit,
// push (with retry on non-fast-forward), and report the resulting
// commit SHA back to the coordinator.
// preparedFatSubmit carries everything the commit + push +
// coordinator-report steps need, computed once during
// pre-validation + file composition. Splitting this out lets
// the batch submit path loop prepare → single push → loop
// report, coalescing N pushes into one over the network.
//
// Single-submit callers don't see this type directly — they
// go through submitResultFatClient which composes prepare +
// push + report into one call.
type preparedFatSubmit struct {
	TaskID        string
	Meta          *taskMeta
	Project       *mcpgit.Project
	Files         []mcpgit.FileWrite
	ArtifactPaths []string
	ResultDir     string
	AuthorName    string
	AuthorEmail   string
	// ReportBody is the POST payload for /tasks/{id}/result.
	// commit_sha + resolved SHA are filled in by the caller
	// after the push completes (the prep step doesn't know
	// the final SHA yet — a rebase during push may rewrite
	// it).
	ReportBody map[string]interface{}
}

// prepareFatSubmit runs every pre-commit step the fat-client
// submit path needs: action-specific validation, workspace
// open, multi-citizen result-dir resolution, named-outputs
// file composition, metadata.json assembly, artifact file
// staging, and report-body shaping. Does NOT touch the
// workspace (no file writes yet) and does NOT acquire the
// project lock — the caller orchestrates both. Returns
// either a prepared bundle or an MCP tool-error result.
//
// Invariants:
//   - Terminal-state rejection before any git write (no
//     phantom commits).
//   - Review decision / vote option validation client-side.
//   - Per-citizen result subdir for multi-citizen tasks.
//   - Named outputs honoured (per-output file when schema
//     declares one, else result.json blob).
//   - Artifact paths sorted for deterministic commit-message
//     ordering.
func (c *apiClient) prepareFatSubmit(
	ctx context.Context,
	taskID string,
	meta *taskMeta,
	content string,
	outputs map[string]string,
	outputLists map[string][]string,
	artifacts map[string]string,
	decision string,
	option string,
) (*preparedFatSubmit, *mcp.CallToolResult) {
	// Task-state gate: a submission against an already-terminal
	// task (accepted / skipped / invalidated / rejected) has no
	// legitimate landing state. Reject it client-side with a
	// task-specific message — mirrors the server's existing
	// "task X cannot accept result (state: Y)" but saves a git
	// round-trip.
	if meta != nil && meta.State != "" {
		switch meta.State {
		case "accepted", "skipped", "failed", "invalid", "invalidated", "rejected":
			return nil, mcp.NewToolResultError(fmt.Sprintf(
				"task %s is already in terminal state %q — re-open it with enju_invalidate_task first if you need to resubmit",
				taskID, meta.State,
			))
		case "pending":
			return nil, mcp.NewToolResultError(fmt.Sprintf(
				"task %s is blocked (waiting on upstream dependencies) — it's not ready for submission yet",
				taskID,
			))
		case "ready":
			// Multi-citizen tasks stay in READY while claims
			// are being collected. Only reject for single-
			// citizen tasks where READY means "not yet claimed."
			if meta.Citizens <= 1 {
				return nil, mcp.NewToolResultError(fmt.Sprintf(
					"task %s is available but not claimed — use enju_claim_task first",
					taskID,
				))
			}
			// Multi-citizen: READY is valid — the engine
			// validates the citizen's active claim server-side.
		}
	}
	if meta != nil && meta.Action == "review" {
		if msg := validateReviewDecision(decision); msg != "" {
			return nil, mcp.NewToolResultError(msg)
		}
	}
	if meta != nil && meta.Action == "vote" {
		if msg := validateVoteOption(option, meta.VoteOptionsJSON); msg != "" {
			return nil, mcp.NewToolResultError(msg)
		}
	}

	proj, _, _, _, err := c.openProject(ctx, meta.ProjectID)
	if err != nil {
		return nil, mcp.NewToolResultError(err.Error())
	}

	// Multi-citizen tasks route each citizen's submission into
	// its own `citizen-<username>/` subdirectory so parallel
	// submitters don't race on the same result.md. Single-
	// citizen tasks keep the flat `runs/{seq}/{task}/` layout.
	// The task's declared citizens count is stored on the DB
	// row and surfaced via taskMeta.Citizens.
	// ResultDir arrives pre-computed on taskMeta (server-side
	// schema; see engine.ComputeResultDir). Multi-citizen tasks
	// still nest per-citizen subdirs under it for submission
	// isolation — that's a sync-layer concern, not a layout
	// schema concern.
	baseResultDir := meta.ResultDir
	resultDir := baseResultDir
	if meta.Citizens > 1 {
		resultDir = filepath.Join(baseResultDir, "citizen-"+c.username)
	}

	// Build the metadata.json that accompanies every submit.
	// Result type defaults to text; it gets flipped to json
	// below when the caller supplies named outputs.
	resultType := "text"
	if outputs != nil {
		resultType = "json"
	}
	metadata := map[string]interface{}{
		"task_id":     taskID,
		"username":    c.username,
		"model":       c.modelName,
		"result_type": resultType,
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	// Review-action metadata: persist decision + target into
	// metadata.json so git-log archaeology can reconstruct the
	// verdict without the coordinator DB. The coordinator also
	// records the decision in tasks.review_decision, but that's
	// mutable (invalidation clears it) — the git commit is the
	// immutable audit record.
	if meta != nil && meta.Action == "review" {
		metadata["action"] = "review"
		metadata["decision"] = decision
		if meta.ReviewsTarget != "" {
			metadata["reviews_target"] = meta.ReviewsTarget
		}
	}
	// Vote-action metadata: mirror the review audit shape so
	// git-log archaeology on vote tasks reveals the winning
	// option plus the declared options list (so an auditor can
	// see what the choices were, not just which one won).
	if meta != nil && meta.Action == "vote" {
		metadata["action"] = "vote"
		metadata["option"] = option
		if meta.VoteOptionsJSON != "" {
			// Embed the parsed options as a structured field so
			// the commit's metadata.json is self-describing —
			// no need to reference the coordinator DB or the
			// original run YAML.
			var parsed interface{}
			if json.Unmarshal([]byte(meta.VoteOptionsJSON), &parsed) == nil {
				metadata["options"] = parsed
			}
		}
	}

	files := []mcpgit.FileWrite{}

	// Single-file result path: `content` is a string blob.
	if content != "" {
		files = append(files, mcpgit.FileWrite{
			RepoRelPath: filepath.Join(resultDir, "result.md"),
			Content:     []byte(content),
		})
	}

	// Phase J.1 — list<string> named outputs are stringified
	// to newline-joined text for on-disk storage so the
	// existing file-per-output path and downstream
	// `{{task.field}}` template resolution keep working
	// unchanged. The structured list value is separately
	// carried to the coordinator via reportBody.output_lists
	// so dynamic for_each materialization doesn't need to
	// re-parse the git file.
	if len(outputLists) > 0 {
		if outputs == nil {
			outputs = make(map[string]string, len(outputLists))
		}
		for name, list := range outputLists {
			outputs[name] = strings.Join(list, "\n")
		}
	}

	// Named outputs path: if the task declares an outputs schema
	// with per-output `file:` specs, each output lands in its own
	// file per the schema and metadata.json carries an
	// output_files index. Otherwise the outputs map is serialized
	// as a single result.json blob (legacy-compatible default).
	if outputs != nil {
		metadata["named_outputs"] = true
		schema := mcpgit.ParseNamedOutputSchema(meta.OutputsSchemaJSON)
		hasFileSpec := false
		for _, s := range schema {
			if s.File != "" {
				hasFileSpec = true
				break
			}
		}
		if hasFileSpec {
			outFiles, fileIndex := mcpgit.BuildNamedOutputFiles(resultDir, schema, outputs)
			files = append(files, outFiles...)
			metadata["output_files"] = fileIndex
		} else {
			outputsBytes, err := json.MarshalIndent(outputs, "", "  ")
			if err != nil {
				return nil, mcp.NewToolResultError("encoding outputs: " + err.Error())
			}
			files = append(files, mcpgit.FileWrite{
				RepoRelPath: filepath.Join(resultDir, "result.json"),
				Content:     outputsBytes,
			})
		}
	}

	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, mcp.NewToolResultError("encoding metadata: " + err.Error())
	}
	files = append(files, mcpgit.FileWrite{
		RepoRelPath: filepath.Join(resultDir, "metadata.json"),
		Content:     metaBytes,
	})

	// Artifact writes. Kept in sorted-key order for deterministic
	// commit-message body ordering.
	var artifactPaths []string
	if len(artifacts) > 0 {
		artifactPaths = make([]string, 0, len(artifacts))
		for p := range artifacts {
			artifactPaths = append(artifactPaths, p)
		}
		sortStringsStable(artifactPaths)
		for _, p := range artifactPaths {
			files = append(files, mcpgit.FileWrite{
				RepoRelPath: mcpgit.ArtifactPath(p),
				Content:     []byte(artifacts[p]),
			})
		}
	}

	authorName, authorEmail := c.commitAuthor(ctx)

	// Report body is shaped here but commit_sha is filled in
	// by the caller AFTER the push completes (a rebase during
	// push may rewrite the SHA, and the batch path especially
	// needs to defer this assignment until after the single
	// coalesced push + CommitSHAsByTaskID remap).
	reportBody := map[string]interface{}{
		"commit_sha":        "", // filled in post-push
		"result_path":       resultDir,
		"artifacts_written": artifactPaths,
		"tokens_used":       0,
		"model":             c.modelName,
		// Username identifies the submitting citizen for
		// multi-citizen task bookkeeping (so the coordinator
		// credits the right task_claims row). Single-citizen
		// tasks tolerate it but use tasks.claimed_by as the
		// implicit claimer.
		"username": c.username,
		// Content rides along so the coordinator can persist
		// the citizen's prose on task_claims.content for
		// multi-citizen vote/review tasks. The fat-client
		// already wrote this prose to the per-citizen
		// result.md, but the DB column is the authoritative
		// source for {{task.responses}} rendering.
		"content": content,
	}
	if len(outputLists) > 0 {
		// Phase J.1 — carry list<string> named output
		// values through to the coordinator so it can
		// materialize dynamic for_each downstreams from
		// the resolved lists.
		reportBody["output_lists"] = outputLists
	}
	if decision != "" {
		reportBody["decision"] = decision
	}
	if option != "" {
		reportBody["option"] = option
	}
	return &preparedFatSubmit{
		TaskID:        taskID,
		Meta:          meta,
		Project:       proj,
		Files:         files,
		ArtifactPaths: artifactPaths,
		ResultDir:     resultDir,
		AuthorName:    authorName,
		AuthorEmail:   authorEmail,
		ReportBody:    reportBody,
	}, nil
}

// submitResultFatClient is the single-submit composition over
// prepareFatSubmit + SubmitTaskResult (commit + push) +
// coordinator report. Preserves the pre-refactor behaviour
// exactly: one lock acquisition per submission, commit and
// push together, post to the coordinator with the resolved
// SHA.
//
// Batch callers use prepareFatSubmit directly, then
// PrepareCommit in a loop, then PushPendingCommits once,
// then CommitSHAsByTaskID to remap post-rebase SHAs, then
// the coordinator posts — coalescing the network push.
func (c *apiClient) submitResultFatClient(
	ctx context.Context,
	taskID string,
	meta *taskMeta,
	content string,
	outputs map[string]string,
	outputLists map[string][]string,
	artifacts map[string]string,
	decision string,
	option string,
) (*mcp.CallToolResult, error) {
	prep, errRes := c.prepareFatSubmit(ctx, taskID, meta, content, outputs, outputLists, artifacts, decision, option)
	if errRes != nil {
		return errRes, nil
	}

	prep.Project.Lock()
	submitRes, err := prep.Project.SubmitTaskResult(mcpgit.SubmitRequest{
		TaskID:        prep.TaskID,
		Username:      c.username,
		AuthorName:    prep.AuthorName,
		AuthorEmail:   prep.AuthorEmail,
		ModelName:     c.modelName,
		Files:         prep.Files,
		ArtifactPaths: prep.ArtifactPaths,
		Branch:        prep.Meta.Branch,
		ProjectID:     prep.Meta.ProjectID,
		StateDir:      c.stateDir(),
	})
	prep.Project.Unlock()
	if err != nil {
		return mcp.NewToolResultError("writing commit to local clone: " + err.Error()), nil
	}

	prep.ReportBody["commit_sha"] = submitRes.CommitSHA
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", prep.ReportBody)
	if err != nil {
		return mcp.NewToolResultError("reporting commit: " + err.Error()), nil
	}
	if errMsg := extractErrorString(data); errMsg != "" {
		return mcp.NewToolResultError(decorateCoordinatorRejection(errMsg)), nil
	}
	return mcp.NewToolResultText(formatSubmitResult(data, taskID)), nil
}

// validateReviewDecision returns an empty string when the decision
// is acceptable for a review-action task ("approve" or "reject"),
// or a single-sentence error message otherwise. Centralized so the
// missing/invalid variants share identical phrasing — the bug
// tripped on three different messages being emitted from three
// different places.
func validateReviewDecision(decision string) string {
	switch decision {
	case "approve", "reject", "request_changes", "comment":
		return ""
	case "":
		return "decision is required on action:review tasks (must be \"approve\", \"request_changes\", \"reject\", or \"comment\")"
	default:
		return invalidDecisionMessage(decision)
	}
}
// invalidDecisionMessage renders the shared phrasing for an
// unrecognized decision value — same copy everywhere so users
// don't see three slightly-different wordings from three
// different validation points.
func invalidDecisionMessage(decision string) string {
	return fmt.Sprintf("decision %q is invalid (must be \"approve\", \"request_changes\", \"reject\", or \"comment\")", decision)
}
// validateVoteOption is the client-side pre-validation guard for
// action:vote submissions. Returns an empty string when the
// option is acceptable, or a single-sentence error message
// otherwise. Runs BEFORE any git write in submitResultFatClient
// so a bad option id can't strand a phantom commit in the
// append-only history.
//
// optionsJSON is the serialized options list from the task's
// vote_options column. An empty JSON (e.g. a storage row that
// somehow lost its declared options) falls through as a
// coordinator-side consistency error rather than a vote-option
// UX error — we don't try to second-guess the DB.
func validateVoteOption(option, optionsJSON string) string {
	if optionsJSON == "" {
		// Don't block the submit client-side if we can't see
		// the declared options; let the coordinator respond
		// with its own consistency error. Better to surface
		// one error from one place than two slightly different
		// ones.
		return ""
	}
	var declared []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(optionsJSON), &declared); err != nil || len(declared) == 0 {
		return ""
	}
	known := make([]string, 0, len(declared))
	for _, o := range declared {
		known = append(known, o.ID)
	}
	if option == "" {
		return fmt.Sprintf(`option is required on action:vote tasks (must be one of: %s)`, strings.Join(known, ", "))
	}
	for _, id := range known {
		if id == option {
			return ""
		}
	}
	return fmt.Sprintf(`option %q is invalid (must be one of: %s)`,
		option, strings.Join(known, ", "))
}
// decorateCoordinatorRejection wraps a raw coordinator error string
// with an actionable hint when the rejection looks like a
// stale-state issue (commit SHA mismatch, unknown commit, state
// transition conflict, etc.). For unrelated rejections it returns
// the original message unchanged.
func decorateCoordinatorRejection(errMsg string) string {
	lower := strings.ToLower(errMsg)
	staleSignals := []string{
		"stale",
		"unknown commit",
		"commit not found",
		"invalid state transition",
		"not in state",
		"already accepted",
		"superseded",
	}
	for _, sig := range staleSignals {
		if strings.Contains(lower, sig) {
			return "coordinator rejected report: " + errMsg +
				" (hint: your local clone may be out of sync — try enju_project_sync and re-claim the task)"
		}
	}
	return "coordinator rejected report: " + errMsg
}
