package mcphandlers

// Claim-path handlers. handleClaimTask does the coordinator
// state transition (READY → CLAIMED), then the fat-client
// resolver assembles the response: inline review-target
// content for action:review tasks, reviewer feedback +
// previous submission if this task was bounced via
// request_changes. handleGetTaskInputs is the standalone
// descriptor fetcher.
//
// fetchReviewFeedback walks the run's review tasks looking for
// one that targets this task and stored a request_changes /
// reject verdict; when found, it reads the reviewer's
// metadata.json + result.md out of git to surface the prose
// inline. Per-instance reviews ride on this path too —
// parseReviewsTarget (in submit.go) splits the stored shape
// so def id + instance key are matched together.

import (
	"github.com/enju-ai/enju/internal/common/format"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/mcpgit"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	// include_context defaults to true — the existing
	// context-rich response. The lean form skips the inputs
	// fetch + review-feedback + previous-submission reads for
	// scripted citizens that don't need the inlined data.
	// Claim state still flips identically; only the response
	// shape changes.
	includeContext := req.GetBool("include_context", true)

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
	// for this call, not an error — the claim POST below
	// still runs and returns its own error if the task isn't
	// claimable for other reasons.
	preMeta, preMetaErr := c.fetchTaskMeta(ctx, taskID)
	if preMetaErr == nil && preMeta != nil && c.useFatClient(preMeta) {
		if proj, _, _, _, perr := c.openProject(ctx, preMeta.ProjectID); perr == nil && proj != nil {
			_ = c.pullBranchWithReconcile(ctx, proj, preMeta.ProjectID, preMeta.Branch)
		}
	}

	// Pre-claim untracked-presence check. If this task reads any
	// artifact that's flagged untracked in the coordinator's index
	// but the file isn't on this citizen's workspace, the claim
	// can't succeed (the downstream can't read the upstream's
	// bytes from git — they were never committed). Refuse the
	// claim BEFORE flipping coordinator state so the task stays
	// available for a citizen who does have the file.
	if preMetaErr == nil && preMeta != nil && c.useFatClient(preMeta) && len(preMeta.ReadsArtifacts) > 0 {
		if err := c.checkUntrackedReadsPresence(ctx, preMeta); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
		"username": c.username,
		"model":    c.modelName, // operator/model design — empty for unaided humans
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Decide which inputs path to take based on whether the
	// project has a remote_url configured. Fat clients pull their
	// own clone and resolve templates locally; legacy clients get
	// a fully-resolved prompt from the coordinator.
	//
	// Lean mode (include_context=false) skips this entire
	// block, plus the review-feedback + previous-submission
	// reads below. The formatter already handles nil
	// inputsData / nil extra by omitting those sections, so
	// the minimal response is a natural subset — no new
	// rendering path.
	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		c.logger.Warn("fetchTaskMeta after claim failed", "task_id", taskID, "error", metaErr)
	}
	var inputs []byte
	var reviewFeedback []byte
	var previousSubmission []byte
	if includeContext {
		if c.useFatClient(meta) {
			inputs, err = c.fetchAndResolveLocally(ctx, meta)
			if err != nil {
				c.logger.Warn("fat-client resolve failed, falling back to legacy", "task_id", taskID, "error", err)
				inputs, _ = c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
			}
		} else {
			inputs, _ = c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
		}

		// If this task was bounced back via request_changes, find the
		// reviewer's feedback AND the author's previous submission
		// so they know what to fix and what they wrote before.
		if meta != nil && meta.ProjectID > 0 {
			reviewFeedback = c.fetchReviewFeedback(ctx, meta)
		}

		// Read the previous submission if it still exists on disk.
		// After request_changes, the DB clears result_path/commit_sha
		// but the file stays in the working tree.
		//
		// Phase 6b.1: the prior content may live on a topic
		// branch the workspace isn't currently on (the pre-claim
		// reconcile checks out the run branch, dropping iter-1's
		// topic-branch-only content from the worktree). When the
		// coordinator surfaces a PreviousIterationCommit, read
		// from THAT commit's tree directly via ReadFileAtCommit;
		// fall back to the worktree ReadFile for legacy paths
		// (no iteration branch involved, prior commit unknown).
		if reviewFeedback != nil && meta != nil && c.workspace != nil {
			remoteURL, projName, _ := c.fetchProjectMetaFull(ctx, meta.ProjectID)
			if proj, perr := c.workspace.ForProject(meta.ProjectID, remoteURL, projName); perr == nil {
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
					previousSubmission, _ = json.Marshal(prev)
				}
			}
		}
	}

	return mcp.NewToolResultText(format.ClaimResult(data, inputs, c.username, reviewFeedback, previousSubmission)), nil
}
// fetchReviewFeedback looks up the most recent review task that
// targets this task and returned request_changes or reject. Returns
// the reviewer's content as JSON, or nil if no review feedback exists.
func (c *apiClient) fetchReviewFeedback(ctx context.Context, meta *taskMeta) []byte {
	// List all tasks in this run to find the reviewer.
	tasksData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", meta.ProjectID, meta.RunSeq))
	if err != nil {
		return nil
	}
	var tasks []map[string]interface{}
	if json.Unmarshal(tasksData, &tasks) != nil {
		return nil
	}
	// Find review tasks targeting this task's def ID.
	// After request_changes, the cascade clears the review task's
	// DB fields (review_decision, result_path, claimed_by). But
	// the review content persists in git — both the result.md and
	// metadata.json are still on disk. We read metadata.json to
	// recover the decision and reviewer identity.
	if c.workspace == nil {
		return nil
	}
	remoteURL, projName, _ := c.fetchProjectMetaFull(ctx, meta.ProjectID)
	proj, perr := c.workspace.ForProject(meta.ProjectID, remoteURL, projName)
	if perr != nil {
		return nil
	}

	for _, t := range tasks {
		reviewsTarget, _ := t["reviews_target"].(string)
		// reviews_target carries one of two shapes:
		//   - bare def id ("expand") for singleton reviews.
		//   - "instanceKey:defID" ("alpha:expand") for per-
		//     instance reviews (static or dynamic for_each).
		// Split and match both components against this task's
		// (TaskDefID, InstanceKey) — empty instance key on both
		// sides for singleton paths means they still match.
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
		// Read the review's result_dir directly from the
		// peer task's response — the server pre-computes the
		// path via engine.ComputeResultDir, so callers never
		// need to reassemble it from (runSeq, instanceKey,
		// taskDefID).
		reviewResultDir, _ := t["result_dir"].(string)
		if reviewResultDir == "" {
			continue
		}
		metaPath := filepath.Join(reviewResultDir, "metadata.json")
		// Phase 6b foundational v1: a request_changes /
		// reject review's commit lives on the review's topic
		// branch (which DOES NOT merge to the run branch on
		// negative decisions). The workspace is on the run
		// branch after the pre-claim reconcile, so ReadFile
		// of the workspace path returns nothing. Prefer
		// reading the metadata.json + result.md from the
		// peer task's latest_completed_commit_sha, falling
		// back to the worktree only when the API didn't
		// surface a commit (legacy / pre-foundational rows).
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
		if decision != "request_changes" && decision != "reject" {
			continue
		}
		// Read the review content via the same commit-aware
		// path — same rationale: negative-verdict reviews
		// don't merge to the run branch.
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
			"reviewer":  reviewer,
			"decision":  decision,
			"content":   string(content),
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
//
// Cheap path (no API calls) when the task doesn't read anything:
// the caller guards on len(preMeta.ReadsArtifacts) > 0.
func (c *apiClient) checkUntrackedReadsPresence(ctx context.Context, meta *taskMeta) error {
	if len(meta.ReadsArtifacts) == 0 {
		return nil
	}
	// Fetch the project's artifact index for this branch. One
	// call lists every path the coordinator knows about —
	// cheaper than N per-path fetches, and the list doesn't
	// race against coordinator state because the pre-claim
	// reconcile above already promoted any just-landed writes.
	listPath := fmt.Sprintf("/api/v1/projects/%d/artifacts", meta.ProjectID)
	if meta.Branch != "" {
		listPath += "?branch=" + meta.Branch
	}
	data, err := c.get(ctx, listPath)
	if err != nil {
		// Best-effort: an index-listing failure shouldn't
		// block a claim (the fat-client resolver below still
		// reports a useful error if the file's truly
		// unreadable). Only block on a confirmed untracked-
		// and-missing signal.
		c.logger.Warn("listing artifacts for presence check", "project_id", meta.ProjectID, "error", err)
		return nil
	}
	var rows []map[string]interface{}
	if json.Unmarshal(data, &rows) != nil {
		return nil
	}
	// Build path → tracked map. Absent path = not in index
	// yet (producer task hasn't accepted); treat as tracked
	// here since the git resolver handles that failure mode
	// with its own clear message.
	trackedByPath := make(map[string]bool, len(rows))
	writerByPath := make(map[string]string, len(rows))
	for _, r := range rows {
		p, _ := r["path"].(string)
		if p == "" {
			continue
		}
		// The tracked field is either absent (legacy row,
		// implicit true), a *bool rendering, or a bare bool.
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
	// Open the project clone so we can stat files in the
	// working tree. The pre-claim reconcile earlier already
	// pulled the branch, so .gitignore + any tracked files
	// are current.
	remoteURL, projName, _ := c.fetchProjectMetaFull(ctx, meta.ProjectID)
	proj, perr := c.workspace.ForProject(meta.ProjectID, remoteURL, projName)
	if perr != nil {
		c.logger.Warn("opening project for presence check", "project_id", meta.ProjectID, "error", perr)
		return nil
	}
	workDir := proj.WorkDir()

	var missing []string
	for _, p := range meta.ReadsArtifacts {
		tracked, known := trackedByPath[p]
		if !known || tracked {
			continue // tracked or not-yet-in-index; skip
		}
		// Best-effort shared-root hookup: if ENJU_SHARED_ROOT
		// is configured AND the producer wrote through it, the
		// downstream workspace won't have a file yet — the
		// symlink needs to be materialized before the stat
		// succeeds. EnsureSharedSymlink is a noop when the env
		// var is unset, so this call is free in local-only
		// setups.
		if err := mcpgit.EnsureSharedSymlink(mcpgit.ArtifactPath(p), workDir,
			meta.ProjectID, meta.ProjectName, meta.Branch, p); err != nil {
			c.logger.Warn("shared-root symlink setup", "path", p, "error", err)
		}
		full := filepath.Join(workDir, mcpgit.ArtifactPath(p))
		if _, err := os.Stat(full); err != nil {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	// Render a concrete, actionable error. Name the missing
	// paths + their producer task so the citizen knows who
	// to ask (or which task to re-run themselves).
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

func (c *apiClient) handleGetTaskInputs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		return mcp.NewToolResultError(metaErr.Error()), nil
	}

	if c.useFatClient(meta) {
		data, err := c.fetchAndResolveLocally(ctx, meta)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(formatJSON(data)), nil
	}

	data, err := c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatJSON(data)), nil
}
// fetchAndResolveLocally is the fat-client claim-time resolver: ask
// the coordinator for a dependency descriptor, open/pull the local
// clone, read upstream results and artifacts locally, render the
// resolved prompt via mcpgit. Returns a JSON blob that looks like the
// legacy /inputs response so formatters don't need to know which
// path produced it.
func (c *apiClient) fetchAndResolveLocally(ctx context.Context, meta *taskMeta) ([]byte, error) {
	descData, err := c.get(ctx, fmt.Sprintf("/api/v1/tasks/%s/inputs?client_mode=true", meta.ID))
	if err != nil {
		return nil, err
	}
	var desc struct {
		TaskID             string              `json:"task_id"`
		PromptTemplate     string              `json:"prompt_template"`
		UserPromptTemplate string              `json:"user_prompt_template"`
		ForEachParams      map[string]string   `json:"for_each_params"`
		Dependencies       []descDependencyRef `json:"dependencies"`
		ArtifactReads      []descArtifactRef   `json:"artifact_reads"`
		ProjectRemoteURL   string              `json:"project_remote_url"`
	}
	if err := json.Unmarshal(descData, &desc); err != nil {
		return nil, fmt.Errorf("parsing descriptor: %w", err)
	}
	if errMsg := extractErrorString(descData); errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}

	proj, _, _, _, err := c.openProject(ctx, meta.ProjectID)
	if err != nil {
		return nil, err
	}
	// Pull + opportunistic reconcile: picks up any async
	// completions (scanner) + reaps failed detached wrappers
	// (reaper) that may have finished since the last tool call.
	// Lock-managed internally so we can re-acquire below for the
	// resolver's git reads.
	if err := c.pullBranchWithReconcile(ctx, proj, meta.ProjectID, meta.Branch); err != nil {
		return nil, fmt.Errorf("refreshing local clone before resolving task %s: %w", meta.ID, err)
	}
	proj.Lock()
	defer proj.Unlock()

	input := mcpgit.ResolveInput{
		TaskID:             meta.ID,
		PromptTemplate:     desc.PromptTemplate,
		UserPromptTemplate: desc.UserPromptTemplate,
		ForEachParams:      desc.ForEachParams,
	}
	for _, d := range desc.Dependencies {
		ref := mcpgit.DependencyRef{
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
				// Pre-migration descriptors only sent
				// `username`; treat it as the path component.
				pathUser = r.Username
			}
			ref.Responses = append(ref.Responses, mcpgit.CitizenResponseRef{
				Username:     r.Username,
				PathUsername: pathUser,
				Option:       r.Option,
				CommitSHA:    r.CommitSHA,
			})
		}
		input.Dependencies = append(input.Dependencies, ref)
	}
	for _, a := range desc.ArtifactReads {
		input.ArtifactReads = append(input.ArtifactReads, mcpgit.ArtifactRef{
			Path:      a.Path,
			CommitSHA: a.CommitSHA,
		})
	}

	resolved, err := proj.Resolve(input)
	if err != nil {
		return nil, err
	}

	// Shape the output to match the legacy /inputs response so
	// existing formatters (format.ClaimResult, etc.) keep working.
	out := map[string]interface{}{
		"task_id":         meta.ID,
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
	// inline in the claim response. The reviewer shouldn't need a
	// separate enju_get_task round-trip just to see what they're
	// evaluating. The target is always in Dependencies (the
	// parser auto-inserts the reviews: edge), and the fat-client
	// has already pulled the commit to the local clone above, so
	// this is a plain file read.
	//
	// ReviewsTarget is stored in one of two shapes:
	//   - Non-for_each review: bare def id, e.g. "draft".
	//   - Per-instance review (static or dynamic for_each): the
	//     instance-matched short form "instanceKey:defID", e.g.
	//     "alpha:expand". This lets us pair each review with its
	//     matching target instance instead of collapsing to the
	//     task_def_id and matching the first one we see.
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
				// Non-fatal — we'd rather show a partial claim
				// response than fail the claim over a formatter
				// nicety. Log and move on.
				c.logger.Warn("reading reviewed target content",
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
				"commit_sha":    d.CommitSHA,
				"result_path":   d.ResultPath,
				"content":       string(data),
			}
			if targetInstanceKey != "" {
				reviewingBlock["instance_key"] = targetInstanceKey
			}
			// Fetch the target task to pick up the claimer's
			// username so the block can render "(by @alice)".
			// One extra GET per review claim — negligible, and
			// the output is much more useful with it.
			runPrefix := fmt.Sprintf("%d:%d:", meta.ProjectID, meta.RunSeq)
			targetFullID := runPrefix + meta.ReviewsTarget
			if targetData, terr := c.get(ctx, "/api/v1/tasks/"+targetFullID); terr == nil {
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
	TaskDefID      string            `json:"task_def_id"`
	InstanceKey    string            `json:"instance_key"`
	InstanceParams map[string]string `json:"instance_params"`
	CommitSHA      string            `json:"commit_sha"`
	ResultPath     string            `json:"result_path"`
	// State is the upstream's lifecycle state. Lets the
	// client-side resolver render a visible marker for
	// terminal-without-content states (skipped / failed)
	// instead of trying to read nonexistent result files.
	State string `json:"state,omitempty"`
	// VoteChoice is the upstream vote task's winning option id
	// (Phase E.2). Empty for non-vote upstreams.
	VoteChoice string `json:"vote_choice,omitempty"`
	// Responses is the per-citizen submission list for
	// multi-citizen upstreams (Phase E.2 session 2b). Each
	// entry has a username + option; the client-side resolver
	// reads the per-citizen result.md from the local clone
	// and attaches the content before substituting into the
	// downstream prompt via {{task.responses}}.
	Responses []descResponseRef `json:"responses,omitempty"`
}

type descResponseRef struct {
	Username     string `json:"username"`
	RealUsername string `json:"real_username,omitempty"` // git-path component; hidden when anonymize is on
	Option       string `json:"option"`
	CommitSHA    string `json:"commit_sha,omitempty"`
}

type descArtifactRef struct {
	Path      string `json:"path"`
	CommitSHA string `json:"commit_sha"`
}
