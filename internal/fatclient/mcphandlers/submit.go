package mcphandlers

// Submit-path MCP handlers. The fat-client orchestration —
// pre-validation, workspace open, file composition, commit +
// push, coordinator report, accepted-merges FF — moved to
// internal/fatclient/service/submit*.go. Each handler here is
// args parse → action-specific input validation → call into
// service.Session → render the structured result via the
// formatters at the bottom of this file.
//
// What stays in mcphandlers:
//
//   - Per-tool input parsing (outputs_json / artifacts_json).
//   - Action-specific input validators (validateReviewDecision,
//     validateVoteOption) — these run on raw user input before
//     the service-layer call. The service has its own copies
//     (service.ValidateReviewDecision / ValidateVoteOption) for
//     the same checks at submit time, but the handler-side ones
//     give friendlier per-tool error messages.
//   - Response formatters (formatBatchResults).
//   - parseReviewsTarget — kept for the existing test that
//     pins the engine-side ↔ fat-client parsing contract.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

// handleSubmitResultsBatch — one MCP tool call submitting N
// results under a single project lock, with N local commits
// coalesced into a single network push.
//
// Validation is layered:
//   - Structural (each entry has task_id, valid JSON shapes)
//     happens here before any service call.
//   - Scope coherence (one project, one run, no intra-batch
//     dependency conflicts) and action-specific field presence
//     also happens here so the whole batch can be rejected
//     before a single git or coordinator-state mutation.
//   - Service-layer pre-flight (terminal-state, decision /
//     option re-check) runs inside SubmitResultsBatch.
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

	// Step 1: parse + structural validation. Every entry must
	// carry a task_id; outputs/artifacts JSON shapes are
	// parsed up front so a malformed entry can't silently
	// drop bytes once the batch starts running.
	entries := make([]service.SubmitBatchEntry, 0, len(rawList))
	for i, raw := range rawList {
		id, _ := raw["task_id"].(string)
		if id == "" {
			return mcp.NewToolResultError(fmt.Sprintf("submissions[%d]: task_id is required", i)), nil
		}
		entry := service.SubmitBatchEntry{TaskID: id}
		if v, ok := raw["content"].(string); ok {
			entry.Content = v
		}
		if v, ok := raw["decision"].(string); ok {
			entry.Decision = v
		}
		if v, ok := raw["option"].(string); ok {
			entry.Option = v
		}
		outputsJSON, _ := raw["outputs_json"].(string)
		artifactsJSON, _ := raw["artifacts_json"].(string)
		outputs, outputLists, artifacts, parseErr := parseEntryOutputs(outputsJSON, artifactsJSON)
		if parseErr != "" {
			return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): %s", i, id, parseErr)), nil
		}
		entry.Outputs = outputs
		entry.OutputLists = outputLists
		entry.Artifacts = artifacts
		if v, ok := raw["model"].(string); ok {
			entry.Model = v
		}
		entries = append(entries, entry)
	}

	// Step 2: snapshot fetch — all tasks exist and their
	// meta is coherent. Also pins project + run scope.
	metas := make([]*service.TaskMeta, len(entries))
	for i, entry := range entries {
		meta, err := c.session.FetchTaskMeta(ctx, entry.TaskID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): task not found: %v", i, entry.TaskID, err)), nil
		}
		metas[i] = meta
	}
	projectID, runSeq, badIndex, ok := service.CoherentBatchScope(metas)
	if !ok {
		bad := metas[badIndex]
		if bad.ProjectID != projectID {
			return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): cross-project batch not supported — all entries must share project %d", badIndex, bad.ID, projectID)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): cross-run batch not supported — all entries must share run %d", badIndex, bad.ID, runSeq)), nil
	}

	// Step 3: action-specific field presence + primary data
	// present. Mirrors single-submit's validation: a review
	// needs `decision`, a vote needs `option`, anything else
	// needs at least one of content/outputs/artifacts.
	// Validating here (before any commit) preserves the no-
	// phantom-commit invariant for the whole batch.
	for i, entry := range entries {
		meta := metas[i]
		switch meta.Action {
		case "review":
			if entry.Decision == "" {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): review task requires 'decision'", i, entry.TaskID)), nil
			}
			if errMsg := validateReviewDecision(entry.Decision); errMsg != "" {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): %s", i, entry.TaskID, errMsg)), nil
			}
		case "vote":
			if entry.Option == "" {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): vote task requires 'option'", i, entry.TaskID)), nil
			}
		default:
			if entry.Content == "" && len(entry.Outputs) == 0 && len(entry.OutputLists) == 0 && len(entry.Artifacts) == 0 {
				return mcp.NewToolResultError(fmt.Sprintf("submissions[%d] (%s): at least one of 'content', 'outputs_json', or 'artifacts_json' is required", i, entry.TaskID)), nil
			}
		}
	}

	// Step 4: intra-batch dependency conflict check. Catches
	// the common case where an earlier entry's cascade
	// (review reject / vote activates) would flip a later
	// entry's task to SKIPPED/FAILED/READY before it can
	// submit.
	if i, j := service.IntraBatchDependencyConflict(metas); i >= 0 {
		return mcp.NewToolResultError(fmt.Sprintf(
			"submissions[%d] (%s) directly depends on submissions[%d] (%s); batch must not mix a task with its upstream — submit the upstream first, then batch the rest",
			i, entries[i].TaskID, j, entries[j].TaskID)), nil
	}

	authorName, authorEmail := c.commitAuthor(ctx)
	result, err := c.session.SubmitResultsBatch(ctx, service.SubmitBatchParams{
		Entries:     entries,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatBatchResults(result.Entries, result.AnySuccess)), nil
}

// handleSubmitResult is the single-submit MCP handler. Parse
// args + validate the per-tool surface, then delegate to
// service.Session.SubmitTaskResult.
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
	modelOverride := req.GetString("model", "")

	// Primary-field presence check. A vote task can submit
	// with just `option`, a review task with just `decision`
	// — those actions treat the action-specific field as the
	// primary signal and prose content is optional commentary.
	// Without the decision/option here the check emits a
	// misleading "content is required" error on an option-only
	// vote.
	if content == "" && outputsJSON == "" && artifactsJSON == "" && decision == "" && option == "" {
		return mcp.NewToolResultError("at least one of 'content', 'outputs_json', 'artifacts_json', 'decision' (review), or 'option' (vote) is required"), nil
	}
	// Any non-empty decision must be valid, regardless of
	// action. The "required for review" check happens in the
	// service-layer pre-validation and on the coordinator.
	if errMsg := validateReviewDecision(decision); decision != "" && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}

	outputs, outputLists, artifacts, parseErr := parseEntryOutputs(outputsJSON, artifactsJSON)
	if parseErr != "" {
		return mcp.NewToolResultError(parseErr), nil
	}

	// Task-existence check up front. fetchTaskMeta returns an
	// error if the coordinator can't find the task (typo, wiped
	// DB, wrong ID). Surface that as a clean "task not found"
	// instead of letting the legacy fallback path POST into a
	// void.
	meta, metaErr := c.session.FetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("task %q not found: %v", taskID, metaErr)), nil
	}
	if !c.session.UseFatClient(meta) {
		// Git is a hard prerequisite for write operations: the
		// coordinator no longer accepts content directly (per
		// ARCHITECTURE.md #3, content lives only in git). When
		// workspace is unset there's no local clone to commit
		// to, so submit cannot proceed. Surface the friendly
		// "use enju mcp" message here so the user knows what
		// to fix; service.SubmitTaskResult would otherwise
		// surface a more generic "no workspace configured"
		// from the OpenProject step.
		return mcp.NewToolResultError("enju_submit_result requires a local workspace; run via `enju mcp` so the result is committed to git before the coordinator records the submission"), nil
	}
	return c.submitResultFatClient(ctx, taskID, meta, content, outputs, outputLists, artifacts, decision, option, modelOverride)
}

// submitResultFatClient is a thin forwarder over
// Session.SubmitTaskResult. Kept on apiClient because:
//   - enju_review (review.go) calls it for its own slim
//     surface that ends in the same submit dance.
//   - server_test.go pins the pre-validation behaviour by
//     calling it directly with crafted task meta.
//
// Will go away when both call sites move to call
// c.session.SubmitTaskResult(...) themselves.
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
	modelOverride string,
) (*mcp.CallToolResult, error) {
	authorName, authorEmail := c.commitAuthor(ctx)
	res := c.session.SubmitTaskResult(ctx, service.SubmitParams{
		TaskID:        taskID,
		Meta:          meta,
		Content:       content,
		Outputs:       outputs,
		OutputLists:   outputLists,
		Artifacts:     artifacts,
		Decision:      decision,
		Option:        option,
		ModelOverride: modelOverride,
		AuthorName:    authorName,
		AuthorEmail:   authorEmail,
	})
	if res.ErrorMessage != "" {
		return mcp.NewToolResultError(res.ErrorMessage), nil
	}
	return mcp.NewToolResultText(format.SubmitResult(res.ResponseBody, taskID)), nil
}

// parseEntryOutputs parses outputs_json + artifacts_json into
// the typed shapes the service layer wants. Returns either a
// populated (outputs, outputLists, artifacts) triple or an
// error string describing the malformed input. Shared between
// single-submit and batch entries so the JSON shape contract
// is the same on both paths.
func parseEntryOutputs(outputsJSON, artifactsJSON string) (map[string]string, map[string][]string, map[string]string, string) {
	var outputs map[string]string
	var outputLists map[string][]string
	if outputsJSON != "" {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(outputsJSON), &raw); err != nil {
			return nil, nil, nil, "outputs_json must be valid JSON object: " + err.Error()
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
						return nil, nil, nil, fmt.Sprintf("outputs_json[%q][%d]: list items must be strings", name, i)
					}
					list = append(list, s)
				}
				if outputLists == nil {
					outputLists = make(map[string][]string)
				}
				outputLists[name] = list
			default:
				return nil, nil, nil, fmt.Sprintf("outputs_json[%q]: value must be a string or a list of strings", name)
			}
		}
	}
	var artifacts map[string]string
	if artifactsJSON != "" {
		if err := json.Unmarshal([]byte(artifactsJSON), &artifacts); err != nil {
			return nil, nil, nil, "artifacts_json must be valid JSON object: " + err.Error()
		}
	}
	return outputs, outputLists, artifacts, ""
}

// parseReviewsTarget splits a stored reviews_target into its
// (task_def_id, instance_key) components. For non-for_each
// reviews the target is just the def id ("draft" → "draft", "").
// For per-instance reviews it's the instance-matched short form
// "instanceKey:defID" ("alpha:expand" → "expand", "alpha").
//
// `idx > 0` (not `>= 0`) skips pathological ":foo" shapes that
// would otherwise parse as (defID="foo", instanceKey=""). The
// materializer never writes such values, but matching the
// engine-side parseReviewsTargetForMerge boundary keeps the
// two implementations in lockstep — they MUST agree byte-for-
// byte or the merge-target collector and the review-feedback
// resolver will diverge on edge inputs.
//
// Kept on the mcphandlers side because TestParseReviewsTarget
// pins this contract; the service layer has its own private
// copy used by claim.go's review-feedback resolver.
func parseReviewsTarget(target string) (defID, instanceKey string) {
	if idx := strings.Index(target, ":"); idx > 0 {
		return target[idx+1:], target[:idx]
	}
	return target, ""
}

// validateReviewDecision is the per-tool input validator for
// review-action submissions. Returns an empty string when the
// decision is acceptable for a review-action task ("approve",
// "reject", "request_changes", "comment"), or a single-sentence
// error message otherwise. Centralized so the missing/invalid
// variants share identical phrasing — the bug tripped on three
// different messages being emitted from three different places.
//
// service.ValidateReviewDecision applies the same rule at
// submit time (after the args parse); this handler-side copy
// gives a friendlier per-tool error before the service call.
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

// validateVoteOption is the handler-side per-tool guard for
// action:vote submissions. Returns an empty string when the
// option is acceptable, or a single-sentence error message
// otherwise. Mirrors service.ValidateVoteOption so failing
// inputs surface a friendly error before any service call.
func validateVoteOption(option, optionsJSON string) string {
	if optionsJSON == "" {
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
		return fmt.Sprintf("option is required on action:vote tasks (must be one of: %s)", strings.Join(known, ", "))
	}
	for _, id := range known {
		if id == option {
			return ""
		}
	}
	return fmt.Sprintf("option %q is invalid (must be one of: %s)",
		option, strings.Join(known, ", "))
}

// decorateCoordinatorRejection wraps a raw coordinator error
// string with an actionable hint when the rejection looks like
// a stale-state issue. Handler-side copy mirrors
// service.DecorateCoordinatorRejection — the test pins the
// phrasing here.
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

// formatBatchResults renders a batch outcome. Header line
// summarizes N/M successful; each entry gets a one-line
// status with ✓/✗ prefix and the per-entry message appended
// for errors. Structured enough that a scripted citizen can
// grep for "✗" but readable for humans.
func formatBatchResults(results []service.SubmitBatchEntryResult, anySuccess bool) string {
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
		if r.Status != "error" && r.Message != "" {
			// Successful entries: the service stashed the
			// raw coordinator response body in Message.
			// Render it via format.SubmitResult so the per-
			// entry block matches single-submit's output.
			rendered := format.SubmitResult([]byte(r.Message), r.TaskID)
			for _, line := range strings.Split(rendered, "\n") {
				if line != "" {
					b.WriteString("  " + line + "\n")
				}
			}
			continue
		}
		if r.Status == "error" && r.Message != "" {
			// Indent the error message so it nests cleanly
			// under the task id — easier for humans to skim
			// a failure list.
			for _, line := range strings.Split(r.Message, "\n") {
				if line != "" {
					b.WriteString("  " + line + "\n")
				}
			}
		}
	}
	if !anySuccess {
		b.WriteString("\nNothing landed — check the errors above, fix, and resubmit.\n")
	}
	return b.String()
}
