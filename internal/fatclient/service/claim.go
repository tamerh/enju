package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/common/types"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// ClaimParams is the input shape for FatClient.ClaimTask. Mirrors
// the MCP tool's surface: the task to claim plus a flag for the
// optional inputs/feedback context bundle.
type ClaimParams struct {
	TaskID     string
	IncludeContext bool // false = skip inputs + review-feedback + previous-submission
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
		if proj, _, _, _, perr := s.OpenProject(ctx, preMeta.ProjectID); perr == nil && proj != nil {
			_ = s.PullBranchWithReconcile(ctx, proj, preMeta.ProjectID, preMeta.Branch)
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
	if result.ReviewFeedback != nil && meta != nil && s.workspace != nil {
		remoteURL, projName, _ := s.FetchProjectMetaFull(ctx, meta.ProjectID)
		if proj, perr := s.workspace.ForProject(meta.ProjectID, remoteURL, projName); perr == nil {
			contentPath := filepath.Join(meta.ResultDir, "result.md")
			var content []byte
			if meta.PreviousIterationCommit != "" {
				if b, ok, _ := proj.ReadFileAtCommit(meta.PreviousIterationCommit, contentPath); ok && len(b) > 0 {
					content = b
				}
			}
			if len(content) == 0 {
				if b, rerr := proj.ReadFile(contentPath); rerr == nil && len(b) > 0 {
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
	if s.workspace == nil {
		return nil
	}
	remoteURL, projName, _ := s.FetchProjectMetaFull(ctx, meta.ProjectID)
	proj, perr := s.workspace.ForProject(meta.ProjectID, remoteURL, projName)
	if perr != nil {
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
			if b, ok, _ := proj.ReadFileAtCommit(latestCommit, metaPath); ok && len(b) > 0 {
				metaBytes = b
			}
		}
		if len(metaBytes) == 0 {
			metaBytes, merr = proj.ReadFile(metaPath)
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
			if b, ok, _ := proj.ReadFileAtCommit(latestCommit, contentPath); ok && len(b) > 0 {
				content = b
			}
		}
		if len(content) == 0 {
			b, rerr := proj.ReadFile(contentPath)
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
// the local workspace. Returns a user-facing error when an
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
	remoteURL, projName, _ := s.FetchProjectMetaFull(ctx, meta.ProjectID)
	proj, perr := s.workspace.ForProject(meta.ProjectID, remoteURL, projName)
	if perr != nil {
		s.logger.Warn("opening project for presence check", "project_id", meta.ProjectID, "error", perr)
		return nil
	}
	workDir := proj.WorkDir()

	var missing []string
	for _, p := range meta.ReadsArtifacts {
		tracked, known := trackedByPath[p]
		if !known || tracked {
			continue
		}
		// Best-effort shared-root hookup: if ENJU_SHARED_ROOT
		// is configured AND the producer wrote through it,
		// the downstream workspace won't have a file yet —
		// the symlink needs to be materialized before the
		// stat succeeds. EnsureSharedSymlink is a noop when
		// the env var is unset, so this call is free in
		// local-only setups.
		if err := workspace.EnsureSharedSymlink(workspace.ArtifactPath(p), workDir,
			meta.ProjectID, meta.ProjectName, meta.Branch, p); err != nil {
			s.logger.Warn("shared-root symlink setup", "path", p, "error", err)
		}
		full := filepath.Join(workDir, workspace.ArtifactPath(p))
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
	b.WriteString("\nUntracked artifacts live outside git — only the producer's workspace (or a shared mount via $ENJU_SHARED_ROOT) has the bytes. Options: ")
	b.WriteString("re-run the producer task locally so it materializes the file here, or configure $ENJU_SHARED_ROOT to point at a mount the producer writes to.")
	return fmt.Errorf("%s", b.String())
}

// FetchAndResolveLocally is the fat-client claim-time resolver:
// ask the coordinator for a dependency descriptor, open/pull
// the local clone, read upstream results and artifacts locally,
// render the resolved prompt via workspace. Returns a JSON blob
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

	proj, _, _, _, err := s.OpenProject(ctx, meta.ProjectID)
	if err != nil {
		return nil, err
	}
	// Pull + opportunistic reconcile: picks up async
	// completions (scanner) + reaps failed detached wrappers
	// (reaper). Lock-managed internally so we can re-acquire
	// below for the resolver's git reads.
	if err := s.PullBranchWithReconcile(ctx, proj, meta.ProjectID, meta.Branch); err != nil {
		return nil, fmt.Errorf("refreshing local clone before resolving task %s: %w", meta.ID, err)
	}
	proj.Lock()
	defer proj.Unlock()

	input := workspace.ResolveInput{
		TaskID:       meta.ID,
		PromptTemplate:   desc.PromptTemplate,
		UserPromptTemplate: desc.UserPromptTemplate,
		ForEachParams:   desc.ForEachParams,
	}
	for _, d := range desc.Dependencies {
		ref := workspace.DependencyRef{
			TaskDefID:   d.TaskDefID,
			InstanceKey:  d.InstanceKey,
			InstanceParams: d.InstanceParams,
			CommitSHA:   d.CommitSHA,
			ResultPath:  d.ResultPath,
			State:     d.State,
			VoteChoice:  d.VoteChoice,
		}
		for _, r := range d.Responses {
			pathUser := r.RealUsername
			if pathUser == "" {
				pathUser = r.Username
			}
			ref.Responses = append(ref.Responses, workspace.CitizenResponseRef{
				Username:   r.Username,
				PathUsername: pathUser,
				Option:    r.Option,
				CommitSHA:  r.CommitSHA,
			})
		}
		input.Dependencies = append(input.Dependencies, ref)
	}
	for _, a := range desc.ArtifactReads {
		input.ArtifactReads = append(input.ArtifactReads, workspace.ArtifactRef{
			Path:   a.Path,
			CommitSHA: a.CommitSHA,
		})
	}

	resolved, err := proj.Resolve(input)
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
			data, ok, rerr := proj.ReadFileAtCommit(d.CommitSHA, contentPath)
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
