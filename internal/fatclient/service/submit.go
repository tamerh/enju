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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/types"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
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
	//.gitignore managed block keeps them out) but ARE reported to
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

	// UsePlumbing routes the commit through the plumbing
	// variant (SubmitComputeTaskResult — direct object/ref
	// writes, no checkout, no worktree mutation) instead of
	// the worktree-based porcelain path.
	//
	// Set true by bot daemons: the LLM already wrote its
	// outputs to an ephemeral per-claim CWD; the daemon
	// doesn't need the worktree updated post-submit.
	// Plumbing-mode also unblocks future concurrent claims
	// within one bot daemon (worktree contention disappears).
	//
	// Left false by MCP-side submits: an operator using
	// claude-code wrote files to their working tree and
	// expects the commit + worktree state to stay in sync.
	// Inline-YAML flows that bootstrap scripts via an
	// answer-task also rely on the worktree-update side
	// effect so the next task's exec finds the script.
	//
	// Two callers diverging on a bool is workable today (bot
	// daemon: true; MCP: false). If a third call site needs
	// a third behavior (a webui submit, a future enju-cli
	// path, etc.), promote this to an enum or a strategy
	// interface — the bool can't carry three states without
	// implicit coupling.
	UsePlumbing bool

	// ScratchDir, when non-empty, is the per-claim ephemeral
	// CWD the LLM handler wrote into (P4c — typically
	// <project>/.enju/scratch/<bot>/<task-iter>/). Phase 4d.2
	// uses it as the stat target for untracked-artifact
	// verification: declared track:false artifacts must
	// EXIST somewhere the submit can see, and post-P4d the
	// persistent worktree doesn't have them — the daemon
	// stops copying. Empty value falls back to the
	// workspace's worktree (pre-P4d behavior, MCP-side
	// submits where the operator wrote into the worktree).
	ScratchDir string
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
	// Untracked-artifact paths flow into commits via the
	// Enju-Untracked-Artifacts trailer (the scanner reads it to
	// reconcile artifacts that were declared track:false and
	// thus not committed in the tree).
	customTrailers := map[string]string{}
	if len(prep.UntrackedArtifactPaths) > 0 {
		customTrailers["Enju-Untracked-Artifacts"] = strings.Join(prep.UntrackedArtifactPaths, ", ")
	}
	// Branch resolution: enjugit's Conventions.BranchName composes
	// the topic branch from RunSeq/RunSlug/TaskDef/InstanceKey/
	// IterSeq, so we pass those fields and the workflow re-derives.
	// commitBranch is informational only here.
	_ = commitBranch
	// commitBranch is the branch the commit lands on. For
	// answer/develop topic-branch flow it's the per-iteration
	// topic; for vote/review/legacy paths it's the run branch
	// directly. Pass it as BranchOverride so Workflow uses it
	// verbatim instead of re-deriving via Conventions.
	//
	// P4d — choice of submit variant:
	//   • Worktree (SubmitTaskResult, porcelain): MCP-driven
	//     submits where the OPERATOR wrote files to their
	//     working tree first. Keeps worktree state in sync
	//     with HEAD. Inline-YAML flows that bootstrap scripts
	//     via an answer-task rely on this side effect.
	//   • Plumbing (SubmitComputeTaskResult): bot-daemon
	//     submits where the LLM wrote files to an ephemeral
	//     CWD (P4c). No worktree mutation, no checkout — sets
	//     up future concurrent claims within one bot daemon.
	//
	// params.UsePlumbing picks between the two. Bot daemons
	// set it true; MCP-side submits leave it false.
	//
	// SubmitComputeTaskResult REQUIRES RunBranch (it's the
	// resolve-base fallback). For vote/review actions where
	// baseBranch was previously empty (commit lands on the
	// run branch directly), thread prep.Meta.Branch as the
	// run-branch reference so resolve-base has something to
	// anchor on. The porcelain path tolerates empty RunBranch
	// (its prepareBranchForCommit has a wider fallback set);
	// we only set the fallback for the plumbing call.
	runBranchForPlumbing := baseBranch
	if runBranchForPlumbing == "" {
		runBranchForPlumbing = prep.Meta.Branch
	}
	submitReq := enjugit.SubmitRequest{
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
	var submitRes *enjugit.SubmitResult
	var err error
	if params.UsePlumbing {
		submitReq.RunBranch = runBranchForPlumbing
		submitRes, err = prep.Workflow.SubmitComputeTaskResult(submitReq)
	} else {
		submitRes, err = prep.Workflow.SubmitTaskResult(submitReq)
	}
	if err != nil {
		// Verify-after-push catches the production "commit
		// reported but never landed in bare" failure mode.
		// Post a push_verify_failed audit event so the failure
		// is visible in run_status / event log instead of buried
		// in a daemon-only stderr. errors.As against the typed
		// *enjugit.ErrSubmitVerify gives Branch/LocalSHA/RemoteSHA.
		var verifyErr *enjugit.ErrSubmitVerify
		if errors.As(err, &verifyErr) {
			runSeq := int64(0)
			if prep.Meta != nil {
				runSeq = int64(prep.Meta.RunSeq)
			}
			if prep.Meta != nil && prep.Meta.ProjectID > 0 && runSeq > 0 {
				s.reportPushVerifyFailed(ctx, prep.Meta.ProjectID, runSeq, prep.TaskID,
					verifyErr.Branch, verifyErr.LocalSHA, verifyErr.RemoteSHA, "")
			} else {
				s.logger.Warn("push verify failed but no project_id/run_seq context to report",
					"task", prep.TaskID, "branch", verifyErr.Branch,
					"local_sha", verifyErr.LocalSHA, "remote_sha", verifyErr.RemoteSHA)
			}
		}
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
	if err := s.applyAcceptedMerges(ctx, prep.Workflow, data); err != nil {
		return &SubmitResult{ErrorMessage: "auto-merging accepted topic branch: " + err.Error()}
	}
	s.applyRunCompletion(ctx, mergeWorkflowOrNil(prep.Workflow), prep.Meta, data)
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
	TaskID   string
	Meta     *TaskMeta
	Workflow *enjugit.Workflow
	Files    []enjugit.FileWrite
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
// - Terminal-state rejection before any git write (no
// phantom commits).
// - Review decision / vote option validation client-side.
// - Per-citizen result subdir for multi-citizen tasks.
// - Named outputs honoured (per-output file when schema
// declares one, else result.json blob).
// - Artifact paths sorted for deterministic commit-message
// ordering.
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

	// Plumbing-mode invariant: the underlying SubmitComputeTaskResult
	// requires RunBranch to be non-empty (it's the resolve-base
	// fallback when the topic branch has no local ref yet). The
	// porcelain path tolerated empty RunBranch via a wider fallback
	// set; plumbing is stricter. Validate at submit-entry so a
	// caller who set UsePlumbing=true without a Branch on Meta
	// fails loud here rather than mid-commit. The downstream call
	// site (after this prep returns) uses prep.Meta.Branch as the
	// runBranchForPlumbing fallback when baseBranch is empty.
	if params.UsePlumbing && (meta == nil || meta.Branch == "") {
		return nil, fmt.Errorf("UsePlumbing=true requires meta.Branch to be non-empty (the plumbing commit's resolve-base anchor)")
	}

	wf, _, _, _, err := s.OpenWorkflow(ctx, meta.ProjectID)
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

	files := []enjugit.FileWrite{}

	// Single-file result path: `content` is a string blob.
	if content != "" {
		files = append(files, enjugit.FileWrite{
			RepoRelPath: filepath.Join(resultDir, "result.md"),
			Content:     []byte(content),
		})
	}

	// list<string> named outputs are stringified to newline-
	// joined text for on-disk storage so the existing file-
	// per-output path and downstream
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
		schema := ParseNamedOutputSchema(meta.OutputsSchemaJSON)
		hasFileSpec := false
		for _, sp := range schema {
			if sp.File != "" {
				hasFileSpec = true
				break
			}
		}
		if hasFileSpec {
			// Convert at the boundary so the rest of the submit
			// flow stays on enjugit.FileWrite.
			outFiles, fileIndex := BuildNamedOutputFiles(resultDir, schema, outputs)
			for _, f := range outFiles {
				files = append(files, enjugit.FileWrite{
					RepoRelPath: f.RepoRelPath,
					Content:     f.Content,
					Mode:        f.Mode,
				})
			}
			metadata["output_files"] = fileIndex
		} else {
			outputsBytes, err := json.MarshalIndent(outputs, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("encoding outputs: %w", err)
			}
			files = append(files, enjugit.FileWrite{
				RepoRelPath: filepath.Join(resultDir, "result.json"),
				Content:     outputsBytes,
			})
		}
	}

	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding metadata: %w", err)
	}
	files = append(files, enjugit.FileWrite{
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
			files = append(files, enjugit.FileWrite{
				RepoRelPath: enjugit.ArtifactPath(p),
				Content:     []byte(artifacts[p]),
			})
		}
	}

	// Untracked artifact paths: stat-only verification — we never
	// commit them (the.gitignore managed block keeps them out)
	// but we DO require they exist on disk before claiming the
	// task succeeded. Silent acceptance of "I declared X but
	// didn't write it" was the data-loss bug fixed here. Sorted
	// for deterministic ordering across reportBody, the trailer,
	// and any retry that re-prepares the same submit.
	//
	// Stat target — Phase 4d.2:
	//   • params.ScratchDir set (bot daemon, post-P4c) →
	//     verify against the ephemeral CWD where the LLM
	//     handler wrote the file. No worktree copy needed.
	//   • Empty (MCP-side, operator's flow) → fall back to
	//     the workspace's worktree.
	statRoot := wf.WorkDir()
	if params.ScratchDir != "" {
		statRoot = params.ScratchDir
	}
	untrackedPaths := append([]string(nil), params.UntrackedArtifacts...)
	sort.Strings(untrackedPaths)
	if len(untrackedPaths) > 0 {
		var missing []string
		for _, p := range untrackedPaths {
			if _, statErr := os.Stat(filepath.Join(statRoot, enjugit.ArtifactPath(p))); statErr != nil {
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
		// iter_seq the daemon claimed under (surfaced by the
		// coordinator at claim time). The coordinator uses it as a
		// single-citizen superseded-claim guard: if a citizen
		// verify-fail escalation closed this looping claim and a
		// retry advanced iter_seq, a late submit from this stale
		// attempt carries the OLD value and is refused — no
		// double-accept onto the new claimant's iteration.
		"iter_seq": meta.IterSeq,
	}

	if len(outputLists) > 0 {
		// Carry list<string> named output values through to
		// the coordinator so it can materialize dynamic
		// for_each downstreams from
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
		Workflow:               wf,
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

// reportPushVerifyFailed POSTs a push_verify_failed audit
// event so the silent-push class of bugs (push reported
// success but commit didn't land in bare) surfaces in
// run_status / event log instead of being buried in a daemon-
// only log. Best-effort: a transport failure on this report
// is logged but doesn't override the underlying submit error
// the caller already sees.
func (s *FatClient) reportPushVerifyFailed(ctx context.Context, projectID, runSeq int64, taskID, branch, localSHA, remoteSHA, remoteURL string) {
	body := map[string]interface{}{
		"branch":     branch,
		"local_sha":  localSHA,
		"remote_sha": remoteSHA,
		"remote_url": remoteURL,
	}
	if taskID != "" {
		body["task_id"] = taskID
	}
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d/push-verify-failed", projectID, runSeq)
	if _, err := s.coord.Post(ctx, path, body); err != nil {
		s.logger.Warn("posting push_verify_failed event failed",
			"task", taskID, "branch", branch, "error", err)
	}
}

// reportMergeConflict POSTs a merge_conflict_detected report
// to the coordinator. Best-effort: a network blip drops the
// report but the underlying accept stood — the run branch is
// just left at its pre-merge tip until the report eventually
// lands (or an operator manually resolves and reports the
// merge).
func (s *FatClient) reportMergeConflict(ctx context.Context, projectID, runSeq int64, taskID, topicBranch, runBranch, topicCommit, runTipCommit string, conflictFiles []string) {
	body := map[string]interface{}{
		"topic_branch":   topicBranch,
		"run_branch":     runBranch,
		"topic_commit":   topicCommit,
		"run_tip_commit": runTipCommit,
		"conflict_files": conflictFiles,
	}
	if taskID != "" {
		body["task_id"] = taskID
	}
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d/merges/conflicts", projectID, runSeq)
	if _, err := s.coord.Post(ctx, path, body); err != nil {
		// Soft-log; never bubble up. Mirrors reportMerge.
		_ = err
	}
}

// reportMergeFailed POSTs a merge_failed report to the
// coordinator for non-conflict failures of the post-submit
// auto-merge. the brief routes these terminal-merge
// failures (push rejected, transport timeout, ref not found)
// through to a coord-side fail-cascade so the silent-stall
// class of bugs can't reappear. Caller is expected to also
// return the underlying error so enju_execute_run surfaces the
// failure to the operator — this fn just records the audit +
// drives the state flip.
//
// Best-effort POST: a transport failure here means the audit
// is missing but the local merge state is unchanged; operator
// can manually invalidate the SUBMITTED task to recover. Logs
// at Warn (not silent) because the report failure compounds
// the original problem.
func (s *FatClient) reportMergeFailed(ctx context.Context, projectID, runSeq int64, taskID, topicBranch, runBranch, errMsg string) {
	body := map[string]interface{}{
		"topic_branch": topicBranch,
		"run_branch":   runBranch,
		"error":        errMsg,
	}
	if taskID != "" {
		body["task_id"] = taskID
	}
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d/merges/failed", projectID, runSeq)
	if _, err := s.coord.Post(ctx, path, body); err != nil {
		s.logger.Warn("reportMergeFailed post: audit missing for merge failure",
			"task_id", taskID, "topic_branch", topicBranch, "post_error", err, "merge_error", errMsg)
	}
}

// reportMergeRetryBackoffs is the per-attempt sleep schedule
// for reportMerge's bounded retry loop. Five attempts, ~7.75s
// max wall-clock before giving up. Sized for the "transient
// SQLITE_BUSY / coord overloaded under peak parallel-merge"
// class of failure — long enough to ride out a multi-second
// contention spike, short enough that a truly down coord
// surfaces fast through ExecuteOutcome.
//
// Idempotency: the /merges handler emits branch_merged inside
// the SUBMITTED state guard and only flips SUBMITTED→ACCEPTED,
// so a duplicate POST after the first call already landed is a
// no-op on coord side for both the audit event and the state
// flip (see coordinator/service/report_merge.go).
var reportMergeRetryBackoffs = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// reportMerge POSTs a branch_merged report to the coordinator
// after each successful FF/merge-commit push from
// applyAcceptedMerges. Retries on transport / coord error with
// bounded exponential backoff (see reportMergeRetryBackoffs)
// because a dropped POST leaves the task wedged at SUBMITTED
// forever — the merge has already landed in git, but coord's
// SUBMITTED→ACCEPTED gate fires only on /merges receipt, and
// downstream's reads_artifacts dual-tier dep gate requires
// ACCEPTED. Load testing (500-task fan-out) caught a small
// number of tasks stuck this way under peak concurrency.
//
// Returns the final error when every retry fails so the caller
// can surface the failure through ExecuteOutcome — silent stall
// is the failure mode this guards against. ctx cancellation
// short-circuits the backoff sleep cleanly.
// Returns the coordinator's /merges response body on success. For
// a task that landed on a per-iteration topic branch, the run
// completes HERE (the SUBMITTED→ACCEPTED gate fires on /merges
// receipt), so this body — not the submit response — is the one
// that carries run_completed + the resolved sync mode.
func (s *FatClient) reportMerge(ctx context.Context, projectID, runSeq int64, taskID, topicBranch, runBranch, mergeSHA string) ([]byte, error) {
	body := map[string]interface{}{
		"topic_branch": topicBranch,
		"run_branch":   runBranch,
		"merge_sha":    mergeSHA,
	}
	if taskID != "" {
		body["task_id"] = taskID
	}
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d/merges", projectID, runSeq)
	var lastErr error
	for attempt := 0; attempt <= len(reportMergeRetryBackoffs); attempt++ {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("reportMerge cancelled (task=%s topic=%s): %w", taskID, topicBranch, ctx.Err())
		}
		data, err := s.coord.Post(ctx, path, body)
		// The coord HTTP client doesn't surface non-2xx as a Go
		// error — it returns the body as `data` with err==nil
		// (see coord/client.go's doWithAutoReregister). So a 503
		// or 500 from /merges has to be detected via the
		// `{"error": "..."}` envelope coord writes on every
		// failure path. Treat both transport err and an
		// envelope-error body as retry-worthy.
		if err == nil {
			if envMsg := coord.ExtractError(data); envMsg != "" {
				err = fmt.Errorf("coord /merges error: %s", envMsg)
			}
		}
		if err == nil {
			if attempt > 0 {
				s.logger.Info("reportMerge: succeeded after retry",
					"task_id", taskID, "topic_branch", topicBranch,
					"merge_sha", mergeSHA, "attempts", attempt+1)
			}
			return data, nil
		}
		lastErr = err
		if attempt == len(reportMergeRetryBackoffs) {
			break
		}
		backoff := reportMergeRetryBackoffs[attempt]
		s.logger.Warn("reportMerge: post failed, retrying",
			"task_id", taskID, "topic_branch", topicBranch,
			"merge_sha", mergeSHA, "attempt", attempt+1,
			"backoff", backoff, "error", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("reportMerge cancelled mid-backoff (task=%s topic=%s): %w", taskID, topicBranch, ctx.Err())
		case <-time.After(backoff):
		}
	}
	return nil, fmt.Errorf("reportMerge exhausted retries (task=%s topic=%s merge=%s): %w",
		taskID, topicBranch, mergeSHA, lastErr)
}

// mergeWorkflow is the Workflow surface applyRunCompletion needs.
// The narrow interface makes the call-chain testable without a real
// git repo and avoids the nil-interface gotcha when prep.Workflow is
// a typed nil (see mergeWorkflowOrNil).
type mergeWorkflow interface {
	DefaultBranch() string
	RemoteURL() string
	MergeRunIntoBase(runBranch, baseBranch string, author enjugit.MergeAuthor) (*enjugit.MergeResult, error)
	PushBranch(branch string) error
}

// mergeWorkflowOrNil converts a concrete *enjugit.Workflow to the
// mergeWorkflow interface, returning nil when wf is nil. A direct
// conversion (*enjugit.Workflow)(nil) → mergeWorkflow produces a
// non-nil interface that holds a nil pointer, breaking == nil checks
// inside applyRunCompletion.
func mergeWorkflowOrNil(wf *enjugit.Workflow) mergeWorkflow {
	if wf == nil {
		return nil
	}
	return wf
}

// applyRunCompletion handles the run-completion sync step. Called
// after applyAcceptedMerges when RunCompleted=true comes back in
// the coordinator's submit response. Non-fatal: failures are logged
// and surfaced as warnings, not returned as submit errors (the local
// commit already landed; the merge/push is a best-effort step).
//
// Sync modes (from the workflow's sync: block, defaulting to "merge"):
//
//	none  — no-op.
//	merge — FF-merge run-branch into base_branch locally.
//	push  — same as merge, then push base_branch to remote.
//
// Conflicts are surfaced via fat-client logs only by design — the run
// branch is preserved for manual git merge; there is intentionally no
// coord-side signal (enju_run_status shows the run completed normally).
func (s *FatClient) applyRunCompletion(_ context.Context, wf mergeWorkflow, meta *TaskMeta, responseBody []byte) {
	if wf == nil || meta == nil || len(responseBody) == 0 {
		return
	}
	var resp struct {
		RunCompleted bool   `json:"run_completed"`
		SyncMode     string `json:"sync_mode"`
		SyncRemote   string `json:"sync_remote"`
	}
	if err := json.Unmarshal(responseBody, &resp); err != nil || !resp.RunCompleted {
		return
	}
	mode := resp.SyncMode
	if mode == "" {
		mode = "merge"
	}
	if mode == "none" {
		return
	}
	runBranch := meta.Branch
	baseBranch := wf.DefaultBranch()
	if runBranch == "" || baseBranch == "" {
		s.logger.Warn("applyRunCompletion: missing branch names",
			"run_branch", runBranch, "base_branch", baseBranch)
		return
	}
	author := enjugit.MergeAuthor{AutoOrManual: "auto"}
	_, err := wf.MergeRunIntoBase(runBranch, baseBranch, author)
	if err != nil {
		var conflict *enjugit.ErrConflict
		if errors.As(err, &conflict) {
			// Run completed; the conflict is an operational git concern.
			// The run branch is intact and base_branch is unchanged.
			// No coord-side signal is emitted — by design for v1 (see
			// docstring). A future contributor should not add a coord
			// POST here without a dedicated endpoint + run state.
			s.logger.Error("applyRunCompletion: run-branch merge conflict — resolve manually",
				"run_branch", runBranch, "base_branch", baseBranch,
				"conflict_files", conflict.Paths,
				"hint", "git checkout "+baseBranch+" && git merge "+runBranch,
				"project_id", meta.ProjectID, "run_seq", meta.RunSeq)
			return
		}
		s.logger.Warn("applyRunCompletion: merge failed",
			"run_branch", runBranch, "base_branch", baseBranch, "err", err)
		return
	}

	if mode != "push" {
		s.logger.Info("applyRunCompletion: run merged into base branch",
			"run_branch", runBranch, "base_branch", baseBranch)
		return
	}
	remote := resp.SyncRemote
	if remote == "" {
		remote = "origin"
	}
	if wf.RemoteURL() == "" {
		s.logger.Warn("applyRunCompletion: push requested but no remote configured",
			"base_branch", baseBranch)
		return
	}
	// Push base_branch to the configured remote. Non-fatal on failure —
	// local merge already landed; operator retries with `git push`.
	if err := wf.PushBranch(baseBranch); err != nil {
		s.logger.Warn("applyRunCompletion: push failed; run merged locally but not shared",
			"base_branch", baseBranch, "remote", remote, "err", err)
		return
	}
	s.logger.Info("applyRunCompletion: run merged and pushed",
		"run_branch", runBranch, "base_branch", baseBranch, "remote", remote)
}

// applyAcceptedMerges drives the post-submit auto-merge of any
// topic branches the coordinator marked ACCEPTED. The submit
// response carries an `accepted_merges` array; each entry is
// (task_id, topic_branch, run_branch, commit_sha) and the fat-
// client merges the topic SHA onto the run branch (locally +
// on origin). FF when possible, real merge commit when a sibling
// already advanced the run branch past the topic's fork point.
//
// Empty / missing array is a no-op (older coordinators, vote/
// review submits without an accepted target). The merge step
// is idempotent: a re-submit that resurfaces the same merge
// targets just performs a same-SHA push, which workspace treats
// as already-up-to-date.
//
// On *enjugit.ErrConflict the merge is left aborted (run branch
// unchanged) and we POST a merge_conflict_detected report to
// the coordinator instead of failing the whole submit. The
// accept stood; the audit timeline carries the signal. // of the parallel-merge work hooks that coord-side report into
// a merge_resolve task spawn so downstream is unblocked once a
// human (or future merge-resolver bot) finishes the merge by
// hand.
func (s *FatClient) applyAcceptedMerges(ctx context.Context, wf *enjugit.Workflow, responseBody []byte) error {
	if wf == nil || len(responseBody) == 0 {
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
	type mergeReport struct {
		taskID, topicBranch, runBranch, mergeSHA string
	}
	var reports []mergeReport
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
		mergeAuthor := enjugit.MergeAuthor{
			Citizen:      enjugit.Identity{Name: s.coord.CitizenName(), Email: s.coord.CitizenEmail()},
			TaskID:       taskID,
			AutoOrManual: "auto",
		}
		// MergeAcceptedTopic checks out the target branch
		// internally so HEAD ends up on the run branch after
		// the merge — no explicit checkout-back needed in
		// service. See enjugit/producing.go's checkout-target
		// step (added to fix the lost-commit regression).
		_, mergeErr := wf.MergeAcceptedTopic(topicBranch, runBranch, mergeAuthor)
		if mergeErr != nil {
			// Conflict: the accept stood, but the post-accept
			// merge can't reconcile two parallel siblings.
			// Report to coord (so the audit timeline has the
			// signal) and keep going on remaining merges
			// rather than failing the whole submit.
			var conflict *enjugit.ErrConflict
			if errors.As(mergeErr, &conflict) {
				if reportProjectID > 0 && reportRunSeq > 0 {
					s.reportMergeConflict(ctx, reportProjectID, reportRunSeq,
						taskID, conflict.TopicBranch, conflict.Branch,
						conflict.TopicCommit, conflict.RunTipCommit,
						conflict.Paths)
				} else {
					s.logger.Warn("dropped merge_conflict report: project_id/run_seq missing from submit response",
						"task", taskID,
						"topic_branch", conflict.TopicBranch,
						"run_branch", conflict.Branch,
						"conflict_files", conflict.Paths)
				}
				continue
			}
			// — non-conflict merge failure (push
			// rejected, transport timeout, "object not
			// found" on a freshly-added remote, etc). Fire a
			// /merges/failed report BEFORE returning so the
			// underlying task is driven to FAILED + cascaded
			// on coord. The accept stood at submit time but
			// the merge can't land — leaving the task in
			// SUBMITTED is the silent-stall class of bugs
			// closes. After the report, the caller
			// gets the underlying error and surfaces it via
			// the ExecuteOutcome chain.
			if reportProjectID > 0 && reportRunSeq > 0 {
				s.reportMergeFailed(ctx, reportProjectID, reportRunSeq, taskID,
					topicBranch, runBranch, mergeErr.Error())
			} else {
				s.logger.Error("dropped merge_failed report: project_id/run_seq missing from submit response",
					"task", taskID,
					"topic_branch", topicBranch,
					"run_branch", runBranch,
					"merge_error", mergeErr)
			}
			return fmt.Errorf(
				"task %s: merging topic %q onto run branch %q at %s: %w",
				taskID, topicBranch, runBranch, commitSHA, mergeErr)
		}
		reports = append(reports, mergeReport{
			taskID: taskID, topicBranch: topicBranch,
			runBranch: runBranch, mergeSHA: commitSHA,
		})
	}
	// Report each successful merge to the coordinator so the
	// audit timeline gets a branch_merged event AND coord
	// flips the task SUBMITTED→ACCEPTED. /merges is the gate;
	// a dropped POST leaves the task stalled with the merge
	// already integrated upstream — load testing caught this
	// exact silent-stall (a small number of tasks wedged
	// permanently). reportMerge retries with bounded backoff
	// and bubbles the final error up so the operator sees a
	// loud failure via ExecuteOutcome instead.
	if reportProjectID > 0 && reportRunSeq > 0 {
		for _, rep := range reports {
			mergeResp, err := s.reportMerge(ctx, reportProjectID, reportRunSeq, rep.taskID,
				rep.topicBranch, rep.runBranch, rep.mergeSHA)
			if err != nil {
				return fmt.Errorf("post-merge report to coord: %w", err)
			}
			// For a topic-branch task the run completes HERE, not at
			// submit — so the run-completion sync (run branch → base
			// merge/push from the workflow's sync: setting) must be
			// driven off the merge-report response. applyRunCompletion
			// no-ops unless this body carries run_completed, so it's
			// safe to call for every reported merge; it fires once,
			// on the merge that finalized the run.
			s.applyRunCompletion(ctx, mergeWorkflowOrNil(wf),
				&TaskMeta{Branch: rep.runBranch, ProjectID: reportProjectID, RunSeq: int(reportRunSeq)},
				mergeResp)
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
