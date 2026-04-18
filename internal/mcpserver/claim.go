package mcpserver

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
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/enju-ai/enju/internal/mcpgit"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
		"username": c.username,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Decide which inputs path to take based on whether the
	// project has a remote_url configured. Fat clients pull their
	// own clone and resolve templates locally; legacy clients get
	// a fully-resolved prompt from the coordinator.
	meta, metaErr := c.fetchTaskMeta(ctx, taskID)
	if metaErr != nil {
		c.logger.Warn("fetchTaskMeta after claim failed", "task_id", taskID, "error", metaErr)
	}
	var inputs []byte
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
	var reviewFeedback []byte
	if meta != nil && meta.ProjectID > 0 {
		reviewFeedback = c.fetchReviewFeedback(ctx, meta)
	}

	// Read the previous submission if it still exists on disk.
	// After request_changes, the DB clears result_path/commit_sha
	// but the file stays in the working tree.
	var previousSubmission []byte
	if reviewFeedback != nil && meta != nil && c.workspace != nil {
		remoteURL, projName, _ := c.fetchProjectMetaFull(ctx, meta.ProjectID)
		if proj, perr := c.workspace.ForProject(meta.ProjectID, remoteURL, projName); perr == nil {
			resultDir := mcpgit.ResultDir(meta.RunSeq, meta.InstanceKey, meta.TaskDefID)
			contentPath := filepath.Join(resultDir, "result.md")
			if content, rerr := proj.ReadFile(contentPath); rerr == nil && len(content) > 0 {
				prev := map[string]string{"content": string(content)}
				previousSubmission, _ = json.Marshal(prev)
			}
		}
	}

	return mcp.NewToolResultText(formatClaimResult(data, inputs, c.username, reviewFeedback, previousSubmission)), nil
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
		taskDefID, _ := t["task_def_id"].(string)
		instanceKey, _ := t["instance_key"].(string)
		if reviewID == "" {
			continue
		}
		// Try to read the review's metadata.json from the workspace.
		reviewResultDir := mcpgit.ResultDir(meta.RunSeq, instanceKey, taskDefID)
		metaPath := filepath.Join(reviewResultDir, "metadata.json")
		metaBytes, merr := proj.ReadFile(metaPath)
		if merr != nil {
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
		// Read the review content.
		contentPath := filepath.Join(reviewResultDir, "result.md")
		content, rerr := proj.ReadFile(contentPath)
		if rerr != nil {
			continue
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

	proj, err := c.workspace.ForProject(meta.ProjectID, meta.ProjectRemoteURL, meta.ProjectName)
	if err != nil {
		return nil, err
	}
	proj.Lock()
	defer proj.Unlock()
	if err := proj.Pull(); err != nil {
		return nil, fmt.Errorf("refreshing local clone before resolving task %s: %w", meta.ID, err)
	}

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
			ref.Responses = append(ref.Responses, mcpgit.CitizenResponseRef{
				Username: r.Username,
				Option:   r.Option,
				Content:  r.Content,
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
	// existing formatters (formatClaimResult, etc.) keep working.
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
	Username string `json:"username"`
	Option   string `json:"option"`
	Content  string `json:"content,omitempty"`
}

type descArtifactRef struct {
	Path      string `json:"path"`
	CommitSHA string `json:"commit_sha"`
}
