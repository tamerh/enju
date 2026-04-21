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
	// Pre-validate action-specific invariants BEFORE touching the
	// local clone. The fat-client submit does commit+push before
	// the coordinator sees the report, so any server-side reject
	// after that point would leave a phantom commit stranded in
	// git history (append-only, nothing to roll back). Anything
	// the client can check up front belongs here.
	//
	// Task-state gate: a submission against an already-terminal
	// task (accepted / skipped / invalidated / rejected) has no
	// legitimate landing state. Reject it client-side with a
	// task-specific message — mirrors the server's existing
	// "task X cannot accept result (state: Y)" but saves a git
	// round-trip.
	if meta != nil && meta.State != "" {
		switch meta.State {
		case "accepted", "skipped", "failed", "invalid", "invalidated", "rejected":
			return mcp.NewToolResultError(fmt.Sprintf(
				"task %s is already in terminal state %q — re-open it with enju_invalidate_task first if you need to resubmit",
				taskID, meta.State,
			)), nil
		case "pending":
			return mcp.NewToolResultError(fmt.Sprintf(
				"task %s is blocked (waiting on upstream dependencies) — it's not ready for submission yet",
				taskID,
			)), nil
		case "ready":
			// Multi-citizen tasks stay in READY while claims
			// are being collected. Only reject for single-
			// citizen tasks where READY means "not yet claimed."
			if meta.Citizens <= 1 {
				return mcp.NewToolResultError(fmt.Sprintf(
					"task %s is available but not claimed — use enju_claim_task first",
					taskID,
				)), nil
			}
			// Multi-citizen: READY is valid — the engine
			// validates the citizen's active claim server-side.
		}
	}
	if meta != nil && meta.Action == "review" {
		if msg := validateReviewDecision(decision); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
	}
	if meta != nil && meta.Action == "vote" {
		if msg := validateVoteOption(option, meta.VoteOptionsJSON); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
	}

	proj, _, _, _, err := c.openProject(ctx, meta.ProjectID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
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
				return mcp.NewToolResultError("encoding outputs: " + err.Error()), nil
			}
			files = append(files, mcpgit.FileWrite{
				RepoRelPath: filepath.Join(resultDir, "result.json"),
				Content:     outputsBytes,
			})
		}
	}

	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("encoding metadata: " + err.Error()), nil
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
	// ProjectID + StateDir on the request enable auto-advance
	// of the fat-client's scan cursor past our own commit, so
	// the next fetch-path reconcile doesn't replay this
	// completion as a "new" trailer — logic lives inside
	// SubmitTaskResult now, shared with every other caller.
	proj.Lock()
	submitRes, err := proj.SubmitTaskResult(mcpgit.SubmitRequest{
		TaskID:        taskID,
		Username:      c.username,
		AuthorName:    authorName,
		AuthorEmail:   authorEmail,
		ModelName:     c.modelName,
		Files:         files,
		ArtifactPaths: artifactPaths,
		Branch:        meta.Branch,
		ProjectID:     meta.ProjectID,
		StateDir:      c.stateDir(),
	})
	proj.Unlock()
	if err != nil {
		return mcp.NewToolResultError("writing commit to local clone: " + err.Error()), nil
	}

	// Report the commit to the coordinator so it can update the
	// state machine, result_path, commit_sha, and artifact index.
	// For action:review tasks the decision field rides along in the
	// same report; the coordinator validates and (on reject) fires
	// the cascade on the reviewed target.
	reportBody := map[string]interface{}{
		"commit_sha":        submitRes.CommitSHA,
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
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/result", reportBody)
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
