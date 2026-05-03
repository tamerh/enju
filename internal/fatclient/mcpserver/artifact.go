package mcpserver

// Artifact-read handlers. Writes happen through the submit
// path (artifacts_json on enju_submit_result); these three are
// the read-only surface: list every artifact in a project
// (optionally filtered by prefix), read one by path at the
// coordinator's current commit pointer, or walk its write
// history via git log.

import (
	"github.com/enju-ai/enju/internal/core/mcptools/format"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/mcpgit"
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
	return mcp.NewToolResultText(format.ArtifactList(data, int64(projectID))), nil
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
	return mcp.NewToolResultText(format.ArtifactDetail(out)), nil
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
	return mcp.NewToolResultText(format.ArtifactHistory(out)), nil
}

// handleListUntrackedArtifacts filters the coordinator's artifact
// index down to entries with tracked=false and reports their
// local-workspace visibility. Intended as a debugging aid for
// "cannot claim — task reads untracked artifact(s) not in your
// workspace" errors, and as a quick audit of which outputs this
// project keeps out of git.
//
// For each untracked entry:
//   - Runs EnsureSharedSymlink as a best-effort (materializes the
//     link if $ENJU_SHARED_ROOT is configured and the workspace
//     path isn't a live symlink yet). This means running this tool
//     can fix the downstream claim error in-place when shared
//     storage is available but the symlink wasn't yet created on
//     this workspace.
//   - os.Stat's the workspace path and records present/missing
//     plus, when the path resolved to a symlink, the target so the
//     user can see whether they're reading from local disk or
//     shared storage.
func (c *apiClient) handleListUntrackedArtifacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("enju_list_untracked_artifacts requires a local workspace (MCP client mode)"), nil
	}
	branch := req.GetString("branch", "")

	// Resolve project metadata once — the default_branch is
	// load-bearing for the symlink materializer below (the
	// producer's bytes live at
	// $SHARED/<slug>/<project-default-branch>/<path>, not at
	// a hard-coded "main"). For projects using default_branch
	// like "enju/work" a hardcoded fallback would make the
	// tool silently report "missing" even when bytes exist.
	remoteURL, projName, defaultBranch, _ := c.fetchProjectMetaExpanded(ctx, int64(projectID))
	if branch == "" {
		branch = defaultBranch
	}
	listPath := fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID)
	if branch != "" {
		listPath += "?branch=" + branch
	}
	data, err := c.get(ctx, listPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var rows []map[string]interface{}
	if json.Unmarshal(data, &rows) != nil {
		return mcp.NewToolResultError("unable to parse artifact index"), nil
	}

	// Open project workspace so we can stat paths + run the
	// symlink materializer. Non-fatal: if the open fails, we
	// still report what the index says; the local visibility
	// column just says "(workspace unavailable)".
	var workDir string
	if proj, perr := c.workspace.ForProject(int64(projectID), remoteURL, projName); perr == nil {
		workDir = proj.WorkDir()
	}

	type untrackedRow struct {
		Path       string
		Producer   string
		LocalState string // "present", "missing", "(unavailable)"
		Target     string // symlink target if applicable
	}
	var out []untrackedRow
	for _, r := range rows {
		if _, ok := r["tracked"].(bool); !ok {
			continue
		}
		if r["tracked"].(bool) {
			continue
		}
		path, _ := r["path"].(string)
		if path == "" {
			continue
		}
		ur := untrackedRow{
			Path:     path,
			Producer: firstNonEmpty(r, "last_writer", "last_task_id"),
		}
		if workDir == "" {
			ur.LocalState = "(workspace unavailable)"
			out = append(out, ur)
			continue
		}
		// Best-effort shared-root materialization — same logic
		// the pre-claim check uses. A missing ENJU_SHARED_ROOT
		// makes this a noop, so local-only users pay nothing.
		// `branch` is guaranteed resolved (caller value or
		// project's default_branch) so the symlink target
		// matches what the producer actually wrote.
		_ = mcpgit.EnsureSharedSymlink(mcpgit.ArtifactPath(path), workDir,
			int64(projectID), projName, branch, path)
		full := filepath.Join(workDir, mcpgit.ArtifactPath(path))
		fi, serr := os.Lstat(full)
		if os.IsNotExist(serr) {
			ur.LocalState = "missing"
		} else if serr != nil {
			ur.LocalState = fmt.Sprintf("error: %v", serr)
		} else {
			if fi.Mode()&os.ModeSymlink != 0 {
				tgt, _ := os.Readlink(full)
				ur.Target = tgt
			}
			ur.LocalState = "present"
		}
		out = append(out, ur)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	var b strings.Builder
	fmt.Fprintf(&b, "Untracked artifacts in project %d", projectID)
	if branch != "" {
		fmt.Fprintf(&b, " (branch: %s)", branch)
	}
	b.WriteString("\n\n")
	if len(out) == 0 {
		b.WriteString("(none — no artifacts flagged track:false in this project)\n")
		return mcp.NewToolResultText(b.String()), nil
	}
	shared := mcpgit.SharedRoot()
	for _, u := range out {
		marker := "?"
		switch u.LocalState {
		case "present":
			marker = "✓"
		case "missing":
			marker = "✗"
		}
		fmt.Fprintf(&b, "%s %s\n", marker, u.Path)
		if u.Producer != "" {
			fmt.Fprintf(&b, "   Producer: %s\n", u.Producer)
		}
		fmt.Fprintf(&b, "   Local: %s", u.LocalState)
		if u.Target != "" {
			fmt.Fprintf(&b, " (symlink → %s)", u.Target)
		}
		b.WriteByte('\n')
		b.WriteByte('\n')
	}
	if shared == "" {
		b.WriteString("(ENJU_SHARED_ROOT not configured — missing entries can be fixed by re-running the producer task locally, or by pointing $ENJU_SHARED_ROOT at a mount the producer wrote to.)\n")
	} else {
		fmt.Fprintf(&b, "(ENJU_SHARED_ROOT=%s — missing entries mean the producer never wrote to this mount, or the mount is unavailable.)\n", shared)
	}
	return mcp.NewToolResultText(b.String()), nil
}

// firstNonEmpty returns the first string value among the given
// keys, falling back through the list. Used for producer
// provenance where the artifact-index row has both last_writer
// (username) and last_task_id — either form is informative.
func firstNonEmpty(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, _ := m[k].(string); v != "" {
			return v
		}
	}
	return ""
}
