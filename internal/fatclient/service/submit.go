package service

// Submit-path orchestration. The fat-client submit flow is the
// most complex surface in the MCP layer: action-specific
// validation, multi-citizen result-dir resolution, named-outputs
// file composition, immutable-audit metadata.json assembly,
// artifact staging, commit + push (with topic-branch flow),
// coordinator report, and post-accept FF merges.
//
// The handlers in mcphandlers/submit.go now do args parse +
// validation + format only; this file holds the orchestration
// they call into via FatClient.SubmitTaskResult.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/types"
	"github.com/enju-ai/enju/internal/fatclient/project"
)

// SubmitParams is the input shape for FatClient.SubmitTaskResult.
// Every field a submit needs travels through here so the
// service layer doesn't reach back into mcphandlers state.
//
// AuthorName + AuthorEmail are resolved by the handler
// (apiClient caches the citizen profile via sync.Once) and
// passed in pre-resolved — the service layer doesn't carry the
// profile cache.
type SubmitParams struct {
	TaskID      string
	Meta        *TaskMeta
	Content     string
	Outputs     map[string]string
	OutputLists map[string][]string
	// Artifacts holds path → content for tracked artifact writes.
	// Entries land in the commit at their declared repo-relative
	// path. Bot/citizen submitters populate this from the working
	// tree per the task's writes_artifacts(track:true) declaration.
	Artifacts map[string]string
	// UntrackedArtifacts lists the path-only set of artifact writes
	// declared with track:false. These files are NOT committed (the
	// .gitignore managed block keeps them out) but ARE reported to
	// the coordinator so it records a tracked=false index row that
	// downstream tasks can verify by stat. Caller is responsible for
	// having stat'd that the files exist on disk before passing
	// them in — prepareFatSubmit re-validates and fails loudly on
	// any missing entry.
	UntrackedArtifacts []string
	Decision           string
	Option             string
	ModelOverride      string
	AuthorName         string
	AuthorEmail        string
}

// SubmitResult bundles the data the formatter and downstream
// callers need from a single submit. ResponseBody carries the
// raw coordinator response (still wanted by the format.SubmitResult
// renderer); IsValidationError flags client-side rejections so
// the handler renders them as IsError tool results without
// having to second-guess the message contents.
type SubmitResult struct {
	// ResponseBody is the raw /tasks/{id}/result response. nil
	// when the call short-circuited before the coordinator POST
	// (validation reject, prepare failure, commit failure).
	ResponseBody []byte
	// ErrorMessage is non-empty when the submit didn't reach
	// terminal accept. Set on validation failures, commit/push
	// failures, coordinator rejections, and merge failures.
	ErrorMessage string
	// IsValidationError distinguishes pre-flight rejections
	// (terminal-state task, invalid decision/option) from
	// transport / git failures. The handler maps this to the
	// MCP IsError flag.
	IsValidationError bool
}

// SubmitTaskResult is the single-submit composition: validate
// → prepareFatSubmit → SubmitTaskResult workspace primitive
// (commit + push) → coordinator report → applyAcceptedMerges.
// Preserves the pre-port behaviour exactly: one lock acquisition
// per submission, commit and push together, post to the
// coordinator with the resolved SHA.
//
// The legacy "no fat-client" branch (workspace unset) was
// folded into this method too — it returns a validation error
// because git is a hard prerequisite for write operations
// post-Option-B.
func (s *FatClient) SubmitTaskResult(ctx context.Context, params SubmitParams) *SubmitResult {
	if params.Meta == nil {
		return &SubmitResult{ErrorMessage: "task metadata is required", IsValidationError: true}
	}

	// Action-specific pre-validation runs FIRST, before any
	// workspace check. This preserves the iteration-E.1
	// invariant ("always check action-specific inputs before
	// any side effect") AND lets a no-workspace caller still
	// get a meaningful "decision is required" error rather
	// than a generic "no workspace" one.
	prep, prepErr := s.prepareFatSubmit(ctx, params)
	if prepErr != nil {
		return &SubmitResult{ErrorMessage: prepErr.Error(), IsValidationError: true}
	}

	// Living-workflow phase 6b foundational v1: topic-branch
	// flow. Commits land on the per-iteration topic branch the
	// coordinator handed back at claim time, forked from the
	// run branch tip. After the coordinator transitions the
	// task to ACCEPTED (immediately for unreviewed answer/
	// compute, on review-approve for reviewed paths), it
	// returns `accepted_merges` in the submit response; this
	// handler then FF-pushes each topic SHA onto the run
	// branch, refusing loudly on non-FF (linear progression
	// guarantees this never fires in normal operation).
	//
	// Empty IterationBranch — vote/review actions, or pre-
	// phase-5 rows — keeps the legacy "commit directly to the
	// run branch" behavior so tasks without a topic continue
	// to advance the run branch directly.
	commitBranch := prep.Meta.Branch
	baseBranch := ""
	if prep.Meta.IterationBranch != "" {
		commitBranch = prep.Meta.IterationBranch
		baseBranch = prep.Meta.Branch
		// Action:review forks its topic from the upstream's
		// topic (when present) so the review's commit lands
		// on top of the upstream's content. On approve the
		// resulting topic is a fast-forward of the run branch
		// AND carries the upstream's commit too — one FF push
		// advances main with both the upstream content and
		// the reviewer's verdict, no concurrent-merge
		// gymnastics. When the upstream has no topic (legacy
		// run-branch submission) we fall back to forking from
		// the run branch.
		if prep.Meta.Action == "review" && prep.Meta.UpstreamIterationBranch != "" {
			baseBranch = prep.Meta.UpstreamIterationBranch
		}
	}
	// populate verdict + iter_seq trailers so
	// `git log` over a project's history can reconstruct
	// review verdicts and iteration counters without the
	// events.db. Verdict comes from the submission decision
	// (review) or vote choice (vote); IterSeq comes from the
	// active claim that the coordinator surfaced alongside
	// the iteration branch.
	verdict := prep.Decision
	if verdict == "" {
		verdict = prep.Option
	}
	prep.Project.Lock()
	submitRes, err := prep.Project.SubmitTaskResult(project.SubmitRequest{
		TaskID:        prep.TaskID,
		Username:      s.Username(),
		AuthorName:    prep.AuthorName,
		AuthorEmail:   prep.AuthorEmail,
		ModelName:     prep.EffectiveModel,
		Files:         prep.Files,
		ArtifactPaths: prep.ArtifactPaths,
		Branch:        commitBranch,
		BaseBranch:    baseBranch,
		ProjectID:     prep.Meta.ProjectID,
		StateDir:      s.StateDir(),
		Trailers: project.EnjuTrailers{
			TaskID:             prep.TaskID,
			Verdict:            verdict,
			IterSeq:            prep.Meta.IterSeq,
			UntrackedArtifacts: prep.UntrackedArtifactPaths,
		},
	})
	prep.Project.Unlock()
	if err != nil {
		return &SubmitResult{ErrorMessage: "writing commit to local clone: " + err.Error()}
	}

	prep.ReportBody["commit_sha"] = submitRes.CommitSHA
	data, err := s.coord.Post(ctx, "/api/v1/tasks/"+prep.TaskID+"/result", prep.ReportBody)
	if err != nil {
		return &SubmitResult{ErrorMessage: "reporting commit: " + err.Error()}
	}
	if errMsg := extractErrorString(data); errMsg != "" {
		return &SubmitResult{ErrorMessage: DecorateCoordinatorRejection(errMsg)}
	}
	if err := s.applyAcceptedMerges(ctx, prep.Project, data); err != nil {
		return &SubmitResult{ErrorMessage: "auto-merging accepted topic branch: " + err.Error()}
	}
	if prep.Meta != nil && prep.Meta.ProjectID > 0 {
		s.TouchProject(prep.Meta.ProjectID)
	}
	return &SubmitResult{ResponseBody: data}
}

// preparedFatSubmit carries everything the commit + push +
// coordinator-report steps need, computed once during
// pre-validation + file composition. Splitting this out lets
// the batch submit path loop prepare → single push → loop
// report, coalescing N pushes into one over the network.
//
// Single-submit callers don't see this type directly — they
// go through SubmitTaskResult which composes prepare +
// push + report into one call.
type preparedFatSubmit struct {
	TaskID    string
	Meta      *TaskMeta
	Project   *project.Clone
	Files     []project.FileWrite
	// ArtifactPaths is the tracked-artifact subset of Files (paths
	// only) — feeds the commit message body and the Enju-Artifacts
	// trailer. Untracked paths are NOT here; they ride
	// UntrackedArtifactPaths into the Enju-Untracked-Artifacts
	// trailer so the async reconcile path can still see them.
	ArtifactPaths          []string
	UntrackedArtifactPaths []string
	ResultDir              string
	AuthorName             string
	AuthorEmail            string
	// ReportBody is the POST payload for /tasks/{id}/result.
	// commit_sha + resolved SHA are filled in by the caller
	// after the push completes (the prep step doesn't know
	// the final SHA yet — a rebase during push may rewrite
	// it).
	ReportBody map[string]interface{}
	// EffectiveModel is the resolved model identifier for this
	// submission (per-call override if the caller passed one,
	// else the session default). Stashed here so the caller can
	// pass it consistently into both the report body (already
	// done, ReportBody["model"]) AND the git commit's
	// AI-Model trailer (SubmitRequest.ModelName) — without this
	// field the trailer would silently use s.modelName even when
	// the caller overrode for attribution.
	EffectiveModel string
	// Decision is the review verdict ("approve"/"reject"/
	// "request_changes"/"comment") when the task is action:review.
	// Empty for non-review submissions. Surfaced on the prep
	// struct so the SubmitTaskResult call site can stamp it
	// onto the Enju-Verdict trailer alongside the
	// other trailer values.
	Decision string
	// Option is the chosen vote option id when the task is
	// action:vote. Empty for non-vote submissions. Same
	// rationale as Decision — feeds into Enju-Verdict.
	Option string
}

// prepareFatSubmit runs every pre-commit step the fat-client
// submit path needs: action-specific validation, workspace
// open, multi-citizen result-dir resolution, named-outputs
// file composition, metadata.json assembly, artifact file
// staging, and report-body shaping. Does NOT touch the
// workspace (no file writes yet) and does NOT acquire the
// project lock — the caller orchestrates both. Returns
// either a prepared bundle or an error.
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
func (s *FatClient) prepareFatSubmit(ctx context.Context, params SubmitParams) (*preparedFatSubmit, error) {
	taskID := params.TaskID
	meta := params.Meta
	content := params.Content
	outputs := params.Outputs
	outputLists := params.OutputLists
	artifacts := params.Artifacts
	decision := params.Decision
	option := params.Option

	// Resolve the effective model once and reuse across the
	// three sites that need it: metadata.json, the report body,
	// and the EffectiveModel field on the returned prep struct
	// (so SubmitTaskResult/PrepareCommit get the same value via
	// prep.EffectiveModel). One call, one source of truth — a
	// future override-precedence change touches one line, not
	// three.
	effectiveModel := s.EffectiveModel(params.ModelOverride)

	// Task-state gate: a submission against an already-terminal
	// task (accepted / skipped / invalidated / rejected) has no
	// legitimate landing state. Reject it client-side with a
	// task-specific message — mirrors the server's existing
	// "task X cannot accept result (state: Y)" but saves a git
	// round-trip.
	if meta != nil && meta.State != "" {
		switch meta.State {
		case "accepted", "skipped", "failed", "invalid", "invalidated", "rejected":
			return nil, fmt.Errorf(
				"task %s is already in terminal state %q — re-open it with enju_invalidate_task first if you need to resubmit",
				taskID, meta.State,
			)
		case "pending":
			return nil, fmt.Errorf(
				"task %s is blocked (waiting on upstream dependencies) — it's not ready for submission yet",
				taskID,
			)
		case "ready":
			// Multi-citizen tasks stay in READY while claims
			// are being collected. Only reject for single-
			// citizen tasks where READY means "not yet claimed."
			if meta.Citizens <= 1 {
				return nil, fmt.Errorf(
					"task %s is available but not claimed — use enju_claim_task first",
					taskID,
				)
			}
			// Multi-citizen: READY is valid — the engine
			// validates the citizen's active claim server-side.
		}
	}
	if meta != nil && meta.Action == "review" {
		if msg := ValidateReviewDecision(decision); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
	}
	if meta != nil && meta.Action == "vote" {
		if msg := ValidateVoteOption(option, meta.VoteOptionsJSON); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
	}

	proj, _, _, _, err := s.OpenProject(ctx, meta.ProjectID)
	if err != nil {
		return nil, err
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
		resultDir = filepath.Join(baseResultDir, "citizen-"+s.Username())
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
		"username":    s.Username(),
		"model":       effectiveModel,
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

	files := []project.FileWrite{}

	// Single-file result path: `content` is a string blob.
	if content != "" {
		files = append(files, project.FileWrite{
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
		schema := project.ParseNamedOutputSchema(meta.OutputsSchemaJSON)
		hasFileSpec := false
		for _, sp := range schema {
			if sp.File != "" {
				hasFileSpec = true
				break
			}
		}
		if hasFileSpec {
			outFiles, fileIndex := project.BuildNamedOutputFiles(resultDir, schema, outputs)
			files = append(files, outFiles...)
			metadata["output_files"] = fileIndex
		} else {
			outputsBytes, err := json.MarshalIndent(outputs, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("encoding outputs: %w", err)
			}
			files = append(files, project.FileWrite{
				RepoRelPath: filepath.Join(resultDir, "result.json"),
				Content:     outputsBytes,
			})
		}
	}

	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding metadata: %w", err)
	}
	files = append(files, project.FileWrite{
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
		sort.Strings(artifactPaths)
		for _, p := range artifactPaths {
			files = append(files, project.FileWrite{
				RepoRelPath: project.ArtifactPath(p),
				Content:     []byte(artifacts[p]),
			})
		}
	}

	// Untracked artifact paths: stat-only verification — we never
	// commit them (the .gitignore managed block keeps them out)
	// but we DO require they exist on disk before claiming the
	// task succeeded. Silent acceptance of "I declared X but
	// didn't write it" was the data-loss bug fixed here. Sorted
	// for deterministic ordering across reportBody, the trailer,
	// and any retry that re-prepares the same submit.
	untrackedPaths := append([]string(nil), params.UntrackedArtifacts...)
	sort.Strings(untrackedPaths)
	if len(untrackedPaths) > 0 {
		var missing []string
		for _, p := range untrackedPaths {
			if _, statErr := os.Stat(filepath.Join(proj.WorkDir(), project.ArtifactPath(p))); statErr != nil {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("declared untracked artifact(s) missing on disk: %s", strings.Join(missing, ", "))
		}
	}

	// Report body is shaped here but commit_sha is filled in
	// by the caller AFTER the push completes (a rebase during
	// push may rewrite the SHA, and the batch path especially
	// needs to defer this assignment until after the single
	// coalesced push + CommitSHAsByTaskID remap).
	// artifacts_written carries BOTH tracked and untracked paths.
	// The coordinator looks up each entry against the task's
	// declared writes_artifacts to recover the track flag (see
	// engine/submit_orchestrate.go). Tracked paths are also in
	// the commit; untracked ones are not — but the coord still
	// records a tracked=false index row so downstream tasks can
	// verify their presence by stat at claim time.
	allArtifactPaths := append([]string(nil), artifactPaths...)
	allArtifactPaths = append(allArtifactPaths, untrackedPaths...)
	sort.Strings(allArtifactPaths)

	reportBody := map[string]interface{}{
		"commit_sha":        "", // filled in post-push
		"result_path":       resultDir,
		"artifacts_written": allArtifactPaths,
		"tokens_used":       0,
		"model":             effectiveModel,
		// Username identifies the submitting citizen for
		// multi-citizen task bookkeeping (so the coordinator
		// credits the right task_claims row). Single-citizen
		// tasks tolerate it but use tasks.claimed_by as the
		// implicit claimer.
		"username": s.Username(),
		// content is sent transiently for the narrow
		// {{review.feedback}} substitution path inside
		// maybeSpawnRemediation. Coord no longer persists
		// submission prose anywhere (per ARCHITECTURE.md #3 —
		// no content column on task_claims or task_submissions).
		// The substituted remediation prompt does land on the
		// spawned task's tasks.prompt row, which is task
		// definition and part of the metadata-on-coord surface
		// tracked under "metadata privacy gap" in TODO.
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
		TaskID:                 taskID,
		Meta:                   meta,
		Project:                proj,
		Files:                  files,
		ArtifactPaths:          artifactPaths,
		UntrackedArtifactPaths: untrackedPaths,
		ResultDir:              resultDir,
		AuthorName:             params.AuthorName,
		AuthorEmail:            params.AuthorEmail,
		ReportBody:             reportBody,
		EffectiveModel:         effectiveModel,
		Decision:               decision,
		Option:                 option,
	}, nil
}

// reportMerge POSTs a branch_merged report to the coordinator.
// fires after each successful FF push from
// applyAcceptedMerges. Best-effort: on transport / coordinator
// error we log and move on. The merge has already landed in
// git; the audit gap is the only consequence and it's already
// part of the "events are a strict consumer" contract.
func (s *FatClient) reportMerge(ctx context.Context, projectID, runSeq int64, taskID, topicBranch, runBranch, mergeSHA string) {
	body := map[string]interface{}{
		"topic_branch": topicBranch,
		"run_branch":   runBranch,
		"merge_sha":    mergeSHA,
	}
	if taskID != "" {
		body["task_id"] = taskID
	}
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d/merges", projectID, runSeq)
	if _, err := s.coord.Post(ctx, path, body); err != nil {
		// Soft-log; never bubble up.
		_ = err
	}
}

// applyAcceptedMerges drives the post-submit FF-merge of any
// topic branches the coordinator marked ACCEPTED. The submit
// response carries an `accepted_merges` array; each entry is
// (task_id, topic_branch, run_branch, commit_sha) and the fat-
// client FF-pushes the topic SHA onto the run branch's ref
// (locally + on origin). Hard-fail on non-FF — under linear
// progression that should never fire in normal use.
//
// Empty / missing array is a no-op (older coordinators, vote/
// review submits without an accepted target). The merge step
// is idempotent: a re-submit that resurfaces the same merge
// targets just performs a same-SHA push, which workspace treats
// as already-up-to-date.
func (s *FatClient) applyAcceptedMerges(ctx context.Context, proj *project.Clone, responseBody []byte) error {
	if proj == nil || len(responseBody) == 0 {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(responseBody, &raw); err != nil {
		return nil
	}
	merges, ok := raw["accepted_merges"].([]interface{})
	if !ok || len(merges) == 0 {
		return nil
	}
	// Capture project + run identifiers for the post-merge
	// branch_merged report. Best-effort: a missing
	// project_id / run_id (coordinator that doesn't surface
	// either, or older payload shapes) means we skip the
	// report; the merge itself still happens.
	var reportProjectID, reportRunSeq int64
	if v, ok := raw["project_id"].(float64); ok {
		reportProjectID = int64(v)
	}
	if v, ok := raw["run_seq"].(float64); ok {
		reportRunSeq = int64(v)
	}
	proj.Lock()
	defer proj.Unlock()
	type mergeReport struct {
		taskID, topicBranch, runBranch, mergeSHA string
	}
	var reports []mergeReport
	var lastRunBranch string
	for _, m := range merges {
		entry, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		runBranch, _ := entry["run_branch"].(string)
		commitSHA, _ := entry["commit_sha"].(string)
		topicBranch, _ := entry["topic_branch"].(string)
		taskID, _ := entry["task_id"].(string)
		if runBranch == "" || commitSHA == "" {
			continue
		}
		if err := proj.FastForwardBranchToCommit(runBranch, commitSHA); err != nil {
			return fmt.Errorf(
				"task %s: ff-merging topic %q onto run branch %q at %s: %w",
				taskID, topicBranch, runBranch, commitSHA, err)
		}
		reports = append(reports, mergeReport{
			taskID: taskID, topicBranch: topicBranch,
			runBranch: runBranch, mergeSHA: commitSHA,
		})
		lastRunBranch = runBranch
	}
	// Report each successful merge to the coordinator so the
	// audit timeline gets a branch_merged event. Fire after
	// the FF push to avoid emitting a phantom event for a
	// merge that didn't actually land. Best-effort: a network
	// blip drops the report but the merge stands.
	if reportProjectID > 0 && reportRunSeq > 0 {
		for _, rep := range reports {
			s.reportMerge(ctx, reportProjectID, reportRunSeq, rep.taskID,
				rep.topicBranch, rep.runBranch, rep.mergeSHA)
		}
	}
	// Switch the workspace HEAD back to the run branch after a
	// successful merge so the next operation in this session
	// (a manual git commit, a sibling submit, the user
	// inspecting the workspace) sees the authoritative merged
	// state rather than the now-stale topic branch we were
	// on coming out of SubmitTaskResult. Without this the
	// workspace stays on the topic and any later manual commit
	// lands on a branch the next claim won't fork from.
	if lastRunBranch != "" {
		if err := proj.CheckoutBranch(lastRunBranch); err != nil {
			return fmt.Errorf("returning workspace to run branch %q after merge: %w", lastRunBranch, err)
		}
	}
	return nil
}

// ValidateReviewDecision returns an empty string when the
// decision is acceptable for a review-action task ("approve",
// "reject", "request_changes", "comment"), or a single-sentence
// error message otherwise. Exported (vs the mcphandlers-side
// validateReviewDecision) so prepareFatSubmit can call it
// without circular imports — the handler keeps its own copy
// for the per-tool args parse path.
func ValidateReviewDecision(decision string) string {
	switch {
	case types.IsValidReviewDecision(decision):
		return ""
	case decision == "":
		return "decision is required on action:review tasks (must be \"approve\", \"request_changes\", \"reject\", or \"comment\")"
	default:
		return fmt.Sprintf("decision %q is invalid (must be \"approve\", \"request_changes\", \"reject\", or \"comment\")", decision)
	}
}

// ValidateVoteOption is the client-side pre-validation guard
// for action:vote submissions. Returns an empty string when the
// option is acceptable, or a single-sentence error message
// otherwise. Runs BEFORE any git write in SubmitTaskResult so a
// bad option id can't strand a phantom commit in the
// append-only history.
//
// optionsJSON is the serialized options list from the task's
// vote_options column. An empty JSON falls through as a
// coordinator-side consistency error rather than a vote-option
// UX error — we don't try to second-guess the DB.
func ValidateVoteOption(option, optionsJSON string) string {
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

// DecorateCoordinatorRejection wraps a raw coordinator error
// string with an actionable hint when the rejection looks like
// a stale-state issue (commit SHA mismatch, unknown commit,
// state transition conflict, etc.). For unrelated rejections
// it returns the original message unchanged.
func DecorateCoordinatorRejection(errMsg string) string {
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
