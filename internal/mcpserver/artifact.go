package mcpserver

// Artifact-read handlers. Writes happen through the submit
// path (artifacts_json on enju_submit_result); these three are
// the read-only surface: list every artifact in a project
// (optionally filtered by prefix), read one by path at the
// coordinator's current commit pointer, or walk its write
// history via git log.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/mcpgit"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListArtifacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path := fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID)
	if prefix := req.GetString("prefix", ""); prefix != "" {
		path += "?prefix=" + prefix
	}
	data, err := c.get(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatArtifactList(data, int64(projectID))), nil
}
// handleGetArtifact reads an artifact's current content from the
// client's local clone. The coordinator provides the provenance
// metadata (via its artifact index), the client reads the actual
// bytes. This replaces the Phase 1 path where the coordinator
// served file contents from a server-side clone.
func (c *apiClient) handleGetArtifact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("get_artifact requires a local workspace (MCP client mode)"), nil
	}

	// Provenance metadata comes from the coordinator's artifact
	// index (last_writer, last_task_id, last_run_id, commit_sha,
	// updated_at). File bytes come from the local clone.
	metaRaw, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/artifacts/%s", projectID, path))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var meta map[string]interface{}
	_ = json.Unmarshal(metaRaw, &meta)
	if meta == nil {
		meta = map[string]interface{}{}
	}
	if errMsg, ok := meta["error"].(string); ok {
		return mcp.NewToolResultError(errMsg), nil
	}

	proj, _, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj.Lock()
	_ = proj.Pull()
	proj.Unlock()

	// Read at the indexed commit SHA if available so the content
	// matches what the coordinator's index points at. Fall back
	// to the working tree when no commit SHA is recorded.
	commitSHA, _ := meta["commit_sha"].(string)
	repoPath := mcpgit.ArtifactPath(path)
	var content []byte
	if commitSHA != "" {
		data, ok, rerr := proj.ReadFileAtCommit(commitSHA, repoPath)
		if rerr != nil || !ok {
			return mcp.NewToolResultError(fmt.Sprintf("artifact %q not found at commit %s", path, commitSHA)), nil
		}
		content = data
	} else {
		data, rerr := proj.ReadFile(repoPath)
		if rerr != nil {
			return mcp.NewToolResultError("reading artifact from working tree: not found"), nil
		}
		content = data
	}
	meta["path"] = path
	meta["content"] = string(content)
	out, _ := json.Marshal(meta)
	return mcp.NewToolResultText(formatArtifactDetail(out)), nil
}
// handleGetArtifactHistory walks the local clone's git log for a
// specific file, then enriches each commit with current-pointer
// and invalidation status by cross-referencing the coordinator's
// artifact index and the task state machine.
//
// A.5 polish: in the orchestrator model, a commit in history can
// correspond to an invalidated task (its content is in git forever
// but the DB pointer no longer references it). Marking each commit
// as `[current pointer]` or `[invalidated]` makes the "which
// version is actually in effect" question obvious from the tool
// output.
func (c *apiClient) handleGetArtifactHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("get_artifact_history requires a local workspace (MCP client mode)"), nil
	}

	proj, _, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	proj.Lock()
	_ = proj.Pull()
	proj.Unlock()

	// A.7 backward compat: try git log at the new namespaced
	// path, fall back to the pre-A.5 flat layout if the primary
	// lookup returns no history (which is what happens for
	// projects created before the namespacing).
	history, err := proj.LogFile(mcpgit.ArtifactPath(path))
	if err != nil {
		return mcp.NewToolResultError("reading git history: " + err.Error()), nil
	}

	// Fetch the coordinator's current artifact index pointer for
	// this path. The commit SHA it names is the "current pointer"
	// — the one the DB treats as the active version.
	currentCommitSHA := ""
	if artData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/artifacts/%s", projectID, path)); err == nil {
		var art map[string]interface{}
		if json.Unmarshal(artData, &art) == nil {
			if s, ok := art["commit_sha"].(string); ok {
				currentCommitSHA = s
			}
		}
	}

	// Build the set of unique task IDs in the history and fetch
	// each one's current state + current commit SHA. The latter
	// is needed to spot `superseded` commits: a commit whose
	// author task is currently ACCEPTED but whose hash differs
	// from the task's current commit (because the task was
	// invalidated and later re-submitted with a new version).
	// One GET per unique task — the history of one file is
	// rarely more than a handful of commits, so this is fine.
	type historyTaskMeta struct {
		state     string
		commitSHA string
	}
	taskMetas := map[string]historyTaskMeta{}
	for _, commit := range history {
		taskID, _ := parseTaskCommitMessage(commit.Message)
		if taskID == "" {
			continue
		}
		if _, have := taskMetas[taskID]; have {
			continue
		}
		if tdata, err := c.get(ctx, "/api/v1/tasks/"+taskID); err == nil {
			var t map[string]interface{}
			if json.Unmarshal(tdata, &t) == nil {
				m := historyTaskMeta{}
				if st, ok := t["state"].(string); ok {
					m.state = st
				}
				if cs, ok := t["commit_sha"].(string); ok {
					m.commitSHA = cs
				}
				taskMetas[taskID] = m
			}
		}
	}

	entries := make([]map[string]interface{}, 0, len(history))
	for _, commit := range history {
		subject := commit.Message
		if i := indexOfNewline(subject); i >= 0 {
			subject = subject[:i]
		}
		taskID, owner := parseTaskCommitMessage(commit.Message)

		// Annotation classification, in order of precedence:
		//
		//   1. current pointer — commit's SHA matches the
		//      artifact index's current value. This is the
		//      version the coordinator treats as live.
		//
		//   2. invalidated — commit's task is currently in a
		//      non-ACCEPTED state (READY / PENDING / CLAIMED).
		//      The task's result is being re-done.
		//
		//   3. superseded — commit's task is ACCEPTED but its
		//      hash doesn't match the task's current commit SHA.
		//      This happens when a task was invalidated, the
		//      artifact reverted to an earlier writer, and then
		//      the task was re-submitted with a new version —
		//      the old pre-invalidation commit is still in git
		//      history but is no longer what the task points at.
		//
		//   4. (none) — commit is accepted and its hash matches
		//      its task's current commit SHA but isn't the
		//      artifact's current pointer (e.g., this task
		//      wrote the file but a different task is the live
		//      writer now).
		annotation := ""
		tm, haveTaskMeta := taskMetas[taskID]
		switch {
		case commit.Hash == currentCommitSHA && taskID != "":
			annotation = "current pointer"
		case haveTaskMeta && taskID != "" && tm.state != "accepted":
			annotation = "invalidated — task " + taskID + " now " + tm.state
		case haveTaskMeta && taskID != "" && tm.state == "accepted" && tm.commitSHA != "" && tm.commitSHA != commit.Hash:
			short := tm.commitSHA
			if len(short) > 8 {
				short = short[:8]
			}
			annotation = "superseded — task re-submitted as " + short
		}

		entry := map[string]interface{}{
			"hash":    commit.Hash,
			"subject": subject,
			"time":    commit.Time.Format(time.RFC3339),
			"task_id": taskID,
			"owner":   owner,
		}
		if annotation != "" {
			entry["annotation"] = annotation
		}
		entries = append(entries, entry)
	}
	out, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"history": entries,
	})
	return mcp.NewToolResultText(formatArtifactHistory(out)), nil
}
