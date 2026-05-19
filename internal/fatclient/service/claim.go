package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/common/types"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// ClaimParams is the input shape for FatClient.ClaimTask. Mirrors
// the MCP tool's surface: the task to claim plus a flag for the
// optional inputs/feedback context bundle.
type ClaimParams struct {
	TaskID     string
	IncludeContext bool // false = skip inputs + review-feedback + previous-submission
}

// ReadTaskResult reads the rendered result.md for a task at
// its current CommitSHA. Used by surfaces that want to show
// "what was submitted" — the web UI's task detail page,
// future export-style views.
//
// Returns ("", false, nil) when the task hasn't reached a
// state with a recorded commit (state=ready, claimed without
// completion, etc.). Returns ("", true, error) on a real I/O
// failure (project clone unreachable, git read failed). The
// (string, found, error) shape lets callers distinguish
// "no submission yet" from "submission exists but unreadable."
//
// Reads via ReadFileAtCommit at the task's CommitSHA — the
// content lives in git, not on the coordinator. ResultDir
// (computed coord-side at task creation) is stitched with
// "result.md" to form the repo-relative path.
func (s *FatClient) ReadTaskResult(ctx context.Context, taskID string) (content string, found bool, err error) {
	meta, err := s.FetchTaskMeta(ctx, taskID)
	if err != nil {
		return "", false, err
	}
	if meta == nil || meta.CommitSHA == "" {
		return "", false, nil
	}
	if meta.ResultDir == "" {
		// Fallback for legacy rows without coord-computed
		// ResultDir: compose from runSeq/slug/taskDefID. Best-
		// effort; if it doesn't match the actual commit tree we
		// just return found=false.
		return "", false, nil
	}
	return s.ReadResultAtCommit(ctx, meta.ProjectID, meta.CommitSHA, meta.ResultDir)
}

// ReadResultAtCommit reads {resultDir}/result.md from the
// project's local clone at the given commit SHA. Generic over
// any (commit, dir) pair so callers can iterate a task's
// history (each iteration row carries its own commit + the
// task's stable ResultDir) rather than seeing only the latest.
//
// Returns (content, true, nil) on hit, ("", false, nil) when
// the file isn't in the tree at that commit, ("", true, error)
// on a real I/O failure (project clone unreachable, git read
// failed). The (string, found, error) shape lets callers
// distinguish "no submission for this iter" from "git read
// broke."
//
// The caller supplies resultDir directly because it lives on
// the task (TaskMeta.ResultDir), not on the iteration —
// iterations differ in commit_sha, not in result-dir layout.
func (s *FatClient) ReadResultAtCommit(ctx context.Context, projectID int64, commitSHA, resultDir string) (string, bool, error) {
	if commitSHA == "" || resultDir == "" {
		return "", false, nil
	}
	if s.enjugit == nil {
		return "", true, fmt.Errorf("no workspace configured")
	}
	// Read-only path: resolve the existing on-disk clone via
	// OpenView. Post-NDW.2 there is no lazy-clone fallback — when
	// the project isn't registered on this machine, or the clone
	// hasn't been materialized via enju_create_project yet, we
	// surface (false, nil) so the read-only surface (e.g. the
	// web UI) shows "no submission viewable here" instead of
	// crashing on a project the user hasn't adopted locally.
	view, verr := s.enjugit.OpenView(projectID)
	if verr != nil {
		if errors.Is(verr, enjugit.ErrCloneNotFound) ||
			errors.Is(verr, enjugit.ErrProjectNotRegistered) {
			return "", false, nil
		}
		return "", true, fmt.Errorf("open project clone: %w", verr)
	}
	repoPath := resultDir + "/result.md"
	data, ok, rerr := view.ReadFileAtCommit(commitSHA, repoPath)
	if rerr != nil {
		return "", true, fmt.Errorf("read at commit: %w", rerr)
	}
	if !ok {
		return "", false, nil
	}
	return string(data), true, nil
}

// ReleaseTask hands a claimed task back to the queue. Coord
// flips state CLAIMED → READY and clears the claim row. Used
// by the web UI's Release button and the MCP enju_release_task
// tool. Username is read from the coord client.
//
// Idempotent at the coord layer: releasing an already-released
// task is a no-op response, not an error. This is intentional
// — UI double-click ergonomics shouldn't surface as a 4xx.
func (s *FatClient) ReleaseTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("task_id is required")
	}
	data, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/release",
		map[string]string{"username": s.coord.Username()})
	if err != nil {
		return err
	}
	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if errMsg, ok := result["error"].(string); ok && errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
	}
	return nil
}

// FailTask drives a claimed task to FAILED with an operator-
// visible reason via POST /api/v1/tasks/{id}/fail (the same
// endpoint the compute executor and reconcile use). Unlike
// ReleaseTask (CLAIMED → READY, re-claimable), this is terminal:
// the task enters the fail cascade so the run surfaces a real
// failure instead of a bot silently re-claiming and looping on a
// deterministic error forever. Used by the daemon's
// bounded-retry policy.
func (s *FatClient) FailTask(ctx context.Context, taskID, reason string) error {
	if taskID == "" {
		return fmt.Errorf("task_id is required")
	}
	data, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/fail",
		map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if errMsg, ok := result["error"].(string); ok && errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
	}
	return nil
}

// CitizenVerifyFailResponse mirrors the coordinator's wire shape
// for POST /api/v1/tasks/{id}/citizen-verify-failed.
type CitizenVerifyFailResponse struct {
	Status    string `json:"status"` // "counted" | "escalated"
	TaskID    string `json:"task_id"`
	FailCount int    `json:"fail_count"`
	Cap       int    `json:"cap"`
	Reason    string `json:"reason,omitempty"`
}

// ReportCitizenVerifyFail posts a layer-① writes-verify failure to
// the coordinator. The coordinator owns the durable per-task
// counter and the failed_retryable escalation; the daemon only
// detects and reports (it never drives a citizen task to terminal
// FAILED via this path). Returns the coordinator's verdict so the
// daemon knows whether to clear its active claim (escalated) or
// release it for the next attempt (counted). The daemon never sets
// force — that is the operator escape hatch only.
func (s *FatClient) ReportCitizenVerifyFail(ctx context.Context, taskID, reason string) (*CitizenVerifyFailResponse, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}
	if reason == "" {
		return nil, fmt.Errorf("reason is required")
	}
	data, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/citizen-verify-failed",
		map[string]string{"reason": reason})
	if err != nil {
		return nil, err
	}
	var resp CitizenVerifyFailResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decoding citizen-verify-failed response: %w", err)
	}
	return &resp, nil
}

// ReleaseAllMyOpenClaimsResponse is the fat-client mirror of
// the coord's wire shape for POST /api/v1/me/release-claims.
type ReleaseAllMyOpenClaimsResponse struct {
	ReleasedTaskIDs []string `json:"released_task_ids"`
	Count           int      `json:"count"`
}

// ReleaseAllMyOpenClaims releases every open claim currently
// held by the calling citizen across all projects. Used by the
// bot daemon's startup recovery: a fresh daemon process has no
// in-memory record of claims its previous instance held, so
// without this call those claims sit until reaper deadline
// (~30min) and the daemon idles in the meantime (the orphan
// task appears CLAIMED-by-self in the ready scan and gets
// skipped).
//
// Returns the released task IDs + count. Empty list when there
// are no orphans is a normal first-run case.
func (s *FatClient) ReleaseAllMyOpenClaims(ctx context.Context) (*ReleaseAllMyOpenClaimsResponse, error) {
	data, err := s.coord.Post(ctx, "/api/v1/me/release-claims",
		map[string]string{"username": s.coord.Username()})
	if err != nil {
		return nil, err
	}
	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if errMsg, ok := result["error"].(string); ok && errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
	}
	var resp ReleaseAllMyOpenClaimsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode release-all response: %w", err)
	}
	return &resp, nil
}

// ClaimResult bundles every byte slice the format.ClaimResult
// renderer wants. Each field can be nil — the formatter omits
// the corresponding section. Lean-mode (IncludeContext=false)
// produces a result with only Data populated.
type ClaimResult struct {
	// Data is the raw JSON the coordinator returned from
	// POST /tasks/{id}/claim. Always present.
	Data []byte
	// Inputs is the resolved inputs descriptor (locally
	// resolved on the fat-client path, or coord-rendered on
	// the legacy path). Nil when IncludeContext=false.
	Inputs []byte
	// ReviewFeedback is the most recent reviewer prose for
	// request_changes/reject targeting this task, JSON-encoded.
	// Nil when no such feedback exists or IncludeContext=false.
	ReviewFeedback []byte
	// PreviousSubmission is the prior iteration's content,
	// JSON-encoded. Populated when reviewing a request_changes
	// loopback and the prior content is reachable. Nil
	// otherwise.
	PreviousSubmission []byte
}

// ClaimTask runs the full claim flow: pre-claim reconcile +
// untracked-presence check, POST the claim to the coordinator,
// then (optionally) gather the inputs descriptor + review
// feedback + previous submission for the agent to read.
//
// Best-effort wrapping policy: pre-claim helpers (reconcile,
// useFatClient check) tolerate missing workspaces silently —
// claim still works for legacy non-workspace clients. The hard
// gates (claim POST, untracked-presence) propagate errors.
func (s *FatClient) ClaimTask(ctx context.Context, params ClaimParams) (*ClaimResult, error) {
	if params.TaskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}

	// Pre-claim reconcile: an async upstream may have landed a
	// trailer commit since the caller's last tool invocation,
	// leaving coordinator state one step behind the git state.
	// If we POST the claim first, the coordinator sees the
	// downstream as still blocked (upstream still "claimed"
	// pre-reconcile) and rejects. Running pullBranchWithReconcile
	// here flips the upstream to accepted via the normal
	// reconcile path so the subsequent claim gate sees current
	// truth. Best-effort: a reconcile failure (no workspace,
	// network issue) just means we get the pre-fix behaviour
	// for this call, not an error — the claim POST below still
	// runs and returns its own error if the task isn't
	// claimable for other reasons.
	preMeta, preMetaErr := s.FetchTaskMeta(ctx, params.TaskID)
	if preMetaErr == nil && preMeta != nil && s.UseFatClient(preMeta) {
		if wf, _, _, _, perr := s.OpenWorkflow(ctx, preMeta.ProjectID); perr == nil && wf != nil {
			// Pre-claim reconcile must succeed — without it, the
			// worktree may sit on a stale branch (or the initial
			// commit on a brand-new bot clone), causing the
			// handler to run with no upstream context and produce
			// nothing useful. Failing here is preferable to the
			// silent-skip-then-empty-output footgun: the operator
			// sees a clear "couldn't refresh local clone" instead
			// of a successful-but-empty submission.
			if err := s.PullBranchWithReconcileWF(ctx, wf, preMeta.ProjectID, preMeta.Branch); err != nil {
				return nil, fmt.Errorf("pre-claim reconcile for task %s on branch %q: %w", preMeta.ID, preMeta.Branch, err)
			}
		}
	}

	// Pre-claim untracked-presence check. If this task reads
	// any artifact that's flagged untracked in the coordinator's
	// index but the file isn't on this citizen's workspace, the
	// claim can't succeed (the downstream can't read the
	// upstream's bytes from git — they were never committed).
	// Refuse the claim BEFORE flipping coordinator state so the
	// task stays available for a citizen who does have the file.
	if preMetaErr == nil && preMeta != nil && s.UseFatClient(preMeta) && len(preMeta.ReadsArtifacts) > 0 {
		if err := s.checkUntrackedReadsPresence(ctx, preMeta); err != nil {
			return nil, err
		}
	}

	data, err := s.coord.Post(ctx, "/api/v1/tasks/"+params.TaskID+"/claim", map[string]string{
		"username": s.Username(),
		"model":  s.modelName, // operator/model design — empty for unaided humans
	})
	if err != nil {
		return nil, err
	}

	// Coord returns 4xx error envelopes as `{"error": "..."}`
	// bodies; the coord client doesn't check HTTP status, so a
	// claim refusal (not a member, terminated run, role
	// mismatch, ...) lands here as `data` with no transport
	// error. Without this check we'd happily wrap the envelope
	// as a successful ClaimResult, set the bot's activeClaim
	// to a task it never actually got, and only notice the
	// failure on the next read — by which point the daemon has
	// burned a poll cycle and (worse) every subsequent iteration
	// would build a phantom orphaned claim coord-side.
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}

	result := &ClaimResult{Data: data}

	if !params.IncludeContext {
		return result, nil
	}

	// Decide inputs path based on whether the project has a
	// remote_url configured. Fat clients pull their own clone
	// and resolve templates locally; legacy clients get a
	// fully-resolved prompt from the coordinator.
	meta, metaErr := s.FetchTaskMeta(ctx, params.TaskID)
	if metaErr != nil {
		s.logger.Warn("fetchTaskMeta after claim failed", "task_id", params.TaskID, "error", metaErr)
	}
	if s.UseFatClient(meta) {
		inputs, ferr := s.FetchAndResolveLocally(ctx, meta)
		if ferr != nil {
			s.logger.Warn("fat-client resolve failed, falling back to legacy", "task_id", params.TaskID, "error", ferr)
			inputs, _ = s.coord.Get(ctx, "/api/v1/tasks/"+params.TaskID+"/inputs")
		}
		result.Inputs = inputs
	} else {
		result.Inputs, _ = s.coord.Get(ctx, "/api/v1/tasks/"+params.TaskID+"/inputs")
	}

	// If this task was bounced back via request_changes, find
	// the reviewer's feedback AND the author's previous
	// submission so they know what to fix and what they wrote
	// before.
	if meta != nil && meta.ProjectID > 0 {
		result.ReviewFeedback = s.fetchReviewFeedback(ctx, meta)
		s.TouchProject(meta.ProjectID)
	}

	// Read the previous submission if it still exists on disk.
	// After request_changes, the DB clears result_path/commit_sha
	// but the file stays in the working tree.
	//
	// Phase 6b.1: the prior content may live on a topic branch
	// the workspace isn't currently on (the pre-claim reconcile
	// checks out the run branch, dropping iter-1's topic-
	// branch-only content from the worktree). When the
	// coordinator surfaces a PreviousIterationCommit, read from
	// THAT commit's tree directly via ReadFileAtCommit; fall
	// back to the worktree ReadFile for legacy paths (no
	// iteration branch involved, prior commit unknown).
	if result.ReviewFeedback != nil && meta != nil && s.enjugit != nil {
		remoteURL, _, _ := s.FetchProjectMetaFull(ctx, meta.ProjectID)
		if wf, werr := s.enjugit.ForProject(meta.ProjectID, remoteURL); werr == nil {
			contentPath := filepath.Join(meta.ResultDir, "result.md")
			var content []byte
			if meta.PreviousIterationCommit != "" {
				if b, ok, _ := wf.ReadFileAtCommit(meta.PreviousIterationCommit, contentPath); ok && len(b) > 0 {
					content = b
				}
			}
			if len(content) == 0 {
				if b, rerr := wf.ReadFile(contentPath); rerr == nil && len(b) > 0 {
					content = b
				}
			}
			if len(content) > 0 {
				prev := map[string]string{"content": string(content)}
				result.PreviousSubmission, _ = json.Marshal(prev)
			}
		}
	}

	return result, nil
}

// fetchReviewFeedback looks up the most recent review task that
// targets this task and returned request_changes or reject.
// Returns the reviewer's content as JSON, or nil if no review
// feedback exists. Best-effort — failures return nil so the
// claim still succeeds with whatever we did manage to fetch.
func (s *FatClient) fetchReviewFeedback(ctx context.Context, meta *TaskMeta) []byte {
	tasksData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", meta.ProjectID, meta.RunSeq))
	if err != nil {
		return nil
	}
	var tasks []map[string]interface{}
	if json.Unmarshal(tasksData, &tasks) != nil {
		return nil
	}
	// After request_changes, the cascade clears the review
	// task's DB fields (review_decision, result_path,
	// claimed_by). But the review content persists in git —
	// both the result.md and metadata.json are still on disk.
	// We read metadata.json to recover the decision and
	// reviewer identity.
	if s.enjugit == nil {
		return nil
	}
	remoteURL, _, _ := s.FetchProjectMetaFull(ctx, meta.ProjectID)
	wf, werr := s.enjugit.ForProject(meta.ProjectID, remoteURL)
	if werr != nil {
		return nil
	}

	for _, t := range tasks {
		reviewsTarget, _ := t["reviews_target"].(string)
		// reviews_target carries one of two shapes:
		//  - bare def id ("expand") for singleton reviews.
		//  - "instanceKey:defID" ("alpha:expand") for per-
		//   instance reviews.
		// Empty instance key on both sides for singleton paths
		// means they still match.
		targetDef, targetInstance := parseReviewsTarget(reviewsTarget)
		if targetDef != meta.TaskDefID {
			continue
		}
		if targetInstance != meta.InstanceKey {
			continue
		}
		reviewID, _ := t["id"].(string)
		if reviewID == "" {
			continue
		}
		reviewResultDir, _ := t["result_dir"].(string)
		if reviewResultDir == "" {
			continue
		}
		metaPath := filepath.Join(reviewResultDir, "metadata.json")
		// Phase 6b foundational v1: a request_changes / reject
		// review's commit lives on the review's topic branch
		// (which DOES NOT merge to the run branch on negative
		// decisions). The workspace is on the run branch after
		// the pre-claim reconcile, so ReadFile of the workspace
		// path returns nothing. Prefer reading from the peer
		// task's latest_completed_commit_sha, falling back to
		// the worktree only when the API didn't surface a
		// commit (legacy / pre-foundational rows).
		latestCommit, _ := t["latest_completed_commit_sha"].(string)
		var metaBytes []byte
		var merr error
		if latestCommit != "" {
			if b, ok, _ := wf.ReadFileAtCommit(latestCommit, metaPath); ok && len(b) > 0 {
				metaBytes = b
			}
		}
		if len(metaBytes) == 0 {
			metaBytes, merr = wf.ReadFile(metaPath)
		}
		if merr != nil || len(metaBytes) == 0 {
			continue
		}
		var metaJSON map[string]interface{}
		if json.Unmarshal(metaBytes, &metaJSON) != nil {
			continue
		}
		decision, _ := metaJSON["decision"].(string)
		d := types.ReviewDecision(decision)
		if d != types.ReviewDecisionRequestChanges && d != types.ReviewDecisionReject {
			continue
		}
		contentPath := filepath.Join(reviewResultDir, "result.md")
		var content []byte
		if latestCommit != "" {
			if b, ok, _ := wf.ReadFileAtCommit(latestCommit, contentPath); ok && len(b) > 0 {
				content = b
			}
		}
		if len(content) == 0 {
			b, rerr := wf.ReadFile(contentPath)
			if rerr != nil {
				continue
			}
			content = b
		}
		reviewer, _ := metaJSON["username"].(string)
		feedback := map[string]interface{}{
			"reviewer": reviewer,
			"decision": decision,
			"content":  string(content),
			"review_id": reviewID,
		}
		data, _ := json.Marshal(feedback)
		return data
	}
	return nil
}

// checkUntrackedReadsPresence verifies that every upstream
// artifact this task reads is either tracked (in which case git
// resolution handles it) or, if untracked, actually exists on
// the local project. Returns a user-facing error when an
// untracked upstream is missing — the caller surfaces it as a
// tool-result-error so the task stays claimable by another
// citizen who does have the file.
func (s *FatClient) checkUntrackedReadsPresence(ctx context.Context, meta *TaskMeta) error {
	if len(meta.ReadsArtifacts) == 0 {
		return nil
	}
	// Fetch the project's artifact index for this branch. One
	// call lists every path the coordinator knows about —
	// cheaper than N per-path fetches.
	listPath := fmt.Sprintf("/api/v1/projects/%d/artifacts", meta.ProjectID)
	if meta.Branch != "" {
		listPath += "?branch=" + meta.Branch
	}
	data, err := s.coord.Get(ctx, listPath)
	if err != nil {
		// Best-effort: an index-listing failure shouldn't
		// block a claim (the fat-client resolver below still
		// reports a useful error if the file's truly
		// unreadable). Only block on a confirmed untracked-
		// and-missing signal.
		s.logger.Warn("listing artifacts for presence check", "project_id", meta.ProjectID, "error", err)
		return nil
	}
	var rows []map[string]interface{}
	if json.Unmarshal(data, &rows) != nil {
		return nil
	}
	trackedByPath := make(map[string]bool, len(rows))
	writerByPath := make(map[string]string, len(rows))
	for _, r := range rows {
		p, _ := r["path"].(string)
		if p == "" {
			continue
		}
		switch v := r["tracked"].(type) {
		case bool:
			trackedByPath[p] = v
		case nil:
			trackedByPath[p] = true
		}
		if t, _ := r["last_task_id"].(string); t != "" {
			writerByPath[p] = t
		}
	}
	remoteURL, _, _ := s.FetchProjectMetaFull(ctx, meta.ProjectID)
	wf, werr := s.enjugit.ForProject(meta.ProjectID, remoteURL)
	if werr != nil {
		s.logger.Warn("opening workflow for presence check", "project_id", meta.ProjectID, "error", werr)
		return nil
	}
	bigfilesDir := enjugit.ResolveBigfilesDir(wf.ProjectRoot(), meta.ProjectID, meta.ProjectName, meta.Branch)

	var missing []string
	for _, p := range meta.ReadsArtifacts {
		tracked, known := trackedByPath[p]
		if !known || tracked {
			continue
		}
		// Untracked artifacts live in bigfilesDir.
		full := filepath.Join(bigfilesDir, enjugit.ArtifactPath(p))
		if _, err := os.Stat(full); err != nil {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	var b strings.Builder
	b.WriteString("cannot claim — this task reads untracked artifact(s) that aren't in your workspace:\n")
	for _, p := range missing {
		fmt.Fprintf(&b, "  - %s", p)
		if writer := writerByPath[p]; writer != "" {
			fmt.Fprintf(&b, " (produced by %s)", writer)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nUntracked artifacts live outside git, in <project>/.enju/bigfiles/<branch>/. Options: ")
	b.WriteString("re-run the producer task locally so it materializes the file here, or set $ENJU_BIGFILES to a shared mount the producer writes to.")
	return fmt.Errorf("%s", b.String())
}

// FetchAndResolveLocally is the fat-client claim-time resolver:
// ask the coordinator for a dependency descriptor, open/pull
// the local clone, read upstream results and artifacts locally,
// render the resolved prompt via project. Returns a JSON blob
// that looks like the legacy /inputs response so formatters
// don't need to know which path produced it.
//
// Exported so the get_task_inputs handler (which also exercises
// the local-resolve path) can call it. Eventually
// handleGetTaskInputs ports to its own service method and the
// export can go away.
func (s *FatClient) FetchAndResolveLocally(ctx context.Context, meta *TaskMeta) ([]byte, error) {
	descData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/tasks/%s/inputs?client_mode=true", meta.ID))
	if err != nil {
		return nil, err
	}
	var desc struct {
		TaskID       string       `json:"task_id"`
		PromptTemplate   string       `json:"prompt_template"`
		UserPromptTemplate string       `json:"user_prompt_template"`
		ForEachParams   map[string]string  `json:"for_each_params"`
		Dependencies    []descDependencyRef `json:"dependencies"`
		ArtifactReads   []descArtifactRef  `json:"artifact_reads"`
		ProjectRemoteURL  string       `json:"project_remote_url"`
	}
	if err := json.Unmarshal(descData, &desc); err != nil {
		return nil, fmt.Errorf("parsing descriptor: %w", err)
	}
	if errMsg := extractErrorString(descData); errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}

	wf, _, _, _, err := s.OpenWorkflow(ctx, meta.ProjectID)
	if err != nil {
		return nil, err
	}
	// Pull + opportunistic reconcile via the WF-flavored helper.
	// Workflow's verbs handle locking internally — no caller-side
	// Lock/Unlock needed (matches other ported sites).
	if err := s.PullBranchWithReconcileWF(ctx, wf, meta.ProjectID, meta.Branch); err != nil {
		return nil, fmt.Errorf("refreshing local clone before resolving task %s: %w", meta.ID, err)
	}

	input := enjugit.ResolveInput{
		TaskID:             meta.ID,
		PromptTemplate:     desc.PromptTemplate,
		UserPromptTemplate: desc.UserPromptTemplate,
		ForEachParams:      desc.ForEachParams,
	}
	for _, d := range desc.Dependencies {
		ref := enjugit.DependencyRef{
			TaskDefID:      d.TaskDefID,
			InstanceKey:    d.InstanceKey,
			InstanceParams: d.InstanceParams,
			CommitSHA:      d.CommitSHA,
			ResultPath:     d.ResultPath,
			State:          d.State,
			VoteChoice:     d.VoteChoice,
		}
		for _, r := range d.Responses {
			pathUser := r.RealUsername
			if pathUser == "" {
				pathUser = r.Username
			}
			ref.Responses = append(ref.Responses, enjugit.CitizenResponseRef{
				Username:     r.Username,
				PathUsername: pathUser,
				Option:       r.Option,
				CommitSHA:    r.CommitSHA,
			})
		}
		input.Dependencies = append(input.Dependencies, ref)
	}
	for _, a := range desc.ArtifactReads {
		input.ArtifactReads = append(input.ArtifactReads, enjugit.ArtifactRef{
			Path:      a.Path,
			CommitSHA: a.CommitSHA,
		})
	}

	resolved, err := wf.Resolve(input)
	if err != nil {
		return nil, err
	}

	out := map[string]interface{}{
		"task_id":     meta.ID,
		"resolved_prompt": resolved.Prompt,
	}
	if resolved.UserPrompt != "" {
		out["resolved_user_prompt"] = resolved.UserPrompt
	}
	if len(resolved.ResolvedArtifacts) > 0 {
		out["artifacts"] = resolved.ResolvedArtifacts
	}
	if len(resolved.MissingArtifacts) > 0 {
		out["missing_artifacts"] = resolved.MissingArtifacts
	}

	// Review-task surfacing: when the caller is claiming an
	// action:review task, show the reviewed target's content
	// inline in the claim response. The reviewer shouldn't need
	// a separate enju_get_task round-trip just to see what
	// they're evaluating.
	if meta.Action == "review" && meta.ReviewsTarget != "" {
		targetDefID, targetInstanceKey := parseReviewsTarget(meta.ReviewsTarget)
		for _, d := range desc.Dependencies {
			if d.TaskDefID != targetDefID {
				continue
			}
			if d.InstanceKey != targetInstanceKey {
				continue
			}
			contentPath := filepath.Join(d.ResultPath, "result.md")
			data, ok, rerr := wf.ReadFileAtCommit(d.CommitSHA, contentPath)
			if rerr != nil || !ok {
				s.logger.Warn("reading reviewed target content",
					"review_task", meta.ID,
					"target", meta.ReviewsTarget,
					"path", contentPath,
					"commit", d.CommitSHA,
					"error", rerr,
				)
				break
			}
			reviewingBlock := map[string]interface{}{
				"target_def_id": targetDefID,
				"commit_sha":  d.CommitSHA,
				"result_path": d.ResultPath,
				"content":   string(data),
			}
			if targetInstanceKey != "" {
				reviewingBlock["instance_key"] = targetInstanceKey
			}
			runPrefix := fmt.Sprintf("%d:%d:", meta.ProjectID, meta.RunSeq)
			targetFullID := runPrefix + meta.ReviewsTarget
			if targetData, terr := s.coord.Get(ctx, "/api/v1/tasks/"+targetFullID); terr == nil {
				var targetRaw map[string]interface{}
				if json.Unmarshal(targetData, &targetRaw) == nil {
					if u, _ := targetRaw["claimed_by"].(string); u != "" {
						reviewingBlock["claimed_by"] = u
					}
				}
			}
			out["reviewing"] = reviewingBlock
			break
		}
	}
	return json.Marshal(out)
}

// --- Dependency-descriptor shapes (coordinator → client) ---

type descDependencyRef struct {
	TaskDefID   string      `json:"task_def_id"`
	InstanceKey  string      `json:"instance_key"`
	InstanceParams map[string]string `json:"instance_params"`
	CommitSHA   string      `json:"commit_sha"`
	ResultPath  string      `json:"result_path"`
	State     string      `json:"state,omitempty"`
	VoteChoice  string      `json:"vote_choice,omitempty"`
	Responses   []descResponseRef `json:"responses,omitempty"`
}

type descResponseRef struct {
	Username   string `json:"username"`
	RealUsername string `json:"real_username,omitempty"`
	Option    string `json:"option"`
	CommitSHA  string `json:"commit_sha,omitempty"`
}

type descArtifactRef struct {
	Path   string `json:"path"`
	CommitSHA string `json:"commit_sha"`
}

// --- Local helpers (small private copies of mcphandlers utils) ---

// parseReviewsTarget mirrors the mcphandlers helper. Splits a
// reviews_target value (either "defID" or "instanceKey:defID")
// into (defID, instanceKey).
func parseReviewsTarget(target string) (defID, instanceKey string) {
	if idx := strings.Index(target, ":"); idx > 0 {
		return target[idx+1:], target[:idx]
	}
	return target, ""
}

// extractErrorString pulls an `error` field out of a JSON
// response if present.
func extractErrorString(data []byte) string {
	var raw map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	if s, ok := raw["error"].(string); ok {
		return s
	}
	return ""
}
