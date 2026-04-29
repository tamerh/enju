package mcpserver

// Project-lifecycle handlers. Creating (from scratch or by
// adopting an existing folder via enju_init), listing, reading
// remote status + sync state, setting a remote URL, and
// leaving a project (deleting its local clone). The non-init
// paths are mostly thin REST proxies; init is a larger affair
// because it validates + scaffolds an existing folder into a
// proper Enju project.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/engine"
	"github.com/enju-ai/enju/internal/mcpgit"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/projects")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Decorate the coordinator's project list with local push-status
	// info from the MCP workspace — but only for projects whose clone
	// already exists on disk. This keeps list_projects cheap (no fresh
	// clones triggered as a side effect of a listing call) while
	// restoring the at-a-glance `✓ last push: ...` indicator that was
	// server-side in Phase 1 / iteration 4.
	decorated := c.decorateProjectListWithPushStatus(data)
	return mcp.NewToolResultText(formatProjectList(decorated)), nil
}
// decorateProjectListWithPushStatus reads the coordinator's JSON
// project list and injects per-project `last_push_at` fields
// pulled from the MCP workspace's local clones. Clones that don't
// exist on disk get no decoration (the `remote: ...` line simply
// omits the check-mark suffix). If decoration fails for any
// reason, the original bytes are returned unchanged so the
// formatter still renders the list.
func (c *apiClient) decorateProjectListWithPushStatus(data []byte) []byte {
	if c.workspace == nil {
		return data
	}
	var projects []map[string]interface{}
	if err := json.Unmarshal(data, &projects); err != nil {
		return data
	}
	changed := false
	for _, p := range projects {
		remoteURL, _ := p["remote_url"].(string)
		if remoteURL == "" {
			continue
		}
		var projectID int64
		switch v := p["id"].(type) {
		case float64:
			projectID = int64(v)
		}
		if projectID == 0 {
			continue
		}
		if !c.workspace.HasLocalClone(projectID) {
			continue
		}
		pName, _ := p["name"].(string)
		proj, err := c.workspace.ForProject(projectID, remoteURL, pName)
		if err != nil {
			continue
		}
		if t := proj.LastPushAt(); !t.IsZero() {
			p["last_push_at"] = t.Format(time.RFC3339)
			changed = true
		}
		if e := proj.LastPushError(); e != "" {
			p["last_push_error"] = e
			changed = true
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(projects)
	if err != nil {
		return data
	}
	return out
}
func (c *apiClient) handleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	description := req.GetString("description", "")
	remoteURL := req.GetString("remote_url", "")
	defaultBranch := req.GetString("default_branch", "")

	body := map[string]string{
		"name":        name,
		"description": description,
		"remote_url":  remoteURL,
	}
	if defaultBranch != "" {
		body["default_branch"] = defaultBranch
	}
	data, err := c.post(ctx, "/api/v1/projects", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Eagerly initialize the workspace so the project directory
	// exists immediately after creation — not lazily on first
	// claim. Two paths:
	//
	//   - remote_url set: ForProject clones from the remote.
	//     Confirms the git remote is reachable at creation time
	//     instead of failing at first task.
	//
	//   - remote_url empty: ForProject's local-only path
	//     init's the working tree and seeds it with one commit
	//     (README + enju/templates/.gitkeep). No shadow bare —
	//     async reconciliation works via the scanner's
	//     refs/heads/<branch> fallback. The user sees a single
	//     git folder, behaves like `git init` would.
	//
	// Failures are non-fatal at this point: the project record
	// is registered, and the next tool call will retry the
	// init/clone. Logged as a warning so the user knows.
	if c.workspace != nil {
		var result map[string]interface{}
		if json.Unmarshal(data, &result) == nil {
			if projectID := int64(jsonFloat(result["id"])); projectID > 0 {
				remote, projName, _ := c.fetchProjectMetaFull(ctx, projectID)
				if _, err := c.workspace.ForProject(projectID, remote, projName); err != nil {
					c.logger.Warn("eager workspace init failed (will retry on first task)",
						"project_id", projectID, "remote", remote, "error", err)
				}
			}
		}
	}

	return mcp.NewToolResultText(formatCreateProjectResult(data)), nil
}
// handleInit adopts an existing folder as an Enju project. It:
// 1. Validates the path exists
// 2. Initializes git if not present
// 3. Writes enju/ + enju/templates/ scaffold if missing
// 4. Commits the scaffold
// 5. Registers the project with the coordinator
// 6. Sets the local path as the remote
// 7. Eagerly clones into the workspace
func (c *apiClient) handleInit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	dirPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Validate path exists.
	stat, err := os.Stat(dirPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("path %q does not exist: %v", dirPath, err)), nil
	}
	if !stat.IsDir() {
		return mcp.NewToolResultError(fmt.Sprintf("path %q is not a directory", dirPath)), nil
	}

	// Detect git state.
	repo, openErr := gogit.PlainOpen(dirPath)
	if openErr != nil {
		// No git — initialize.
		var initErr error
		repo, initErr = gogit.PlainInitWithOptions(dirPath, &gogit.PlainInitOptions{
			InitOptions: gogit.InitOptions{
				DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
			},
		})
		if initErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("git init failed: %v", initErr)), nil
		}
		c.logger.Info("initialized git in existing folder", "path", dirPath)
	}

	// Write scaffold if missing.
	wt, err := repo.Worktree()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("getting worktree: %v", err)), nil
	}
	scaffoldWritten := false
	enjuDir := filepath.Join(dirPath, "enju")
	if _, err := os.Stat(enjuDir); os.IsNotExist(err) {
		os.MkdirAll(enjuDir, 0755)
		scaffoldWritten = true
	}
	templatesDir := filepath.Join(dirPath, engine.DefaultTemplatesDir)
	if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
		os.MkdirAll(templatesDir, 0755)
		// Write a .gitkeep so the empty dir is tracked.
		os.WriteFile(filepath.Join(templatesDir, ".gitkeep"), []byte(""), 0644)
		scaffoldWritten = true
	}

	// Commit scaffold (and any existing uncommitted files).
	if scaffoldWritten {
		if err := wt.AddGlob("."); err != nil {
			c.logger.Warn("staging scaffold", "error", err)
		}
		status, _ := wt.Status()
		if !status.IsClean() {
			sig := &object.Signature{
				Name:  "Enju",
				Email: "enju@localhost",
				When:  time.Now(),
			}
			_, commitErr := wt.Commit("Initialize Enju orchestration", &gogit.CommitOptions{
				Author:    sig,
				Committer: sig,
			})
			if commitErr != nil {
				c.logger.Warn("scaffold commit", "error", commitErr)
			} else {
				c.logger.Info("committed Enju scaffold", "path", dirPath)
			}
		}
	}

	// Ensure the repo has at least one commit (needed for clone).
	if _, err := repo.Head(); err != nil {
		// No commits yet — commit everything.
		wt.AddGlob(".")
		sig := &object.Signature{
			Name:  "Enju",
			Email: "enju@localhost",
			When:  time.Now(),
		}
		wt.Commit("initial commit", &gogit.CommitOptions{
			Author:    sig,
			Committer: sig,
		})
	}

	// Pick up the folder's current HEAD branch as the
	// project's default_branch. If HEAD can't be read (shouldn't
	// happen after the scaffold commit above but we handle it
	// just in case), fall back to "main" via omission. Adopted
	// repos that already run on "trunk" / "develop" / "enju/work"
	// get their existing branch honored instead of being forced
	// onto "main".
	adoptedBranch := ""
	if head, herr := repo.Head(); herr == nil && head.Name().IsBranch() {
		adoptedBranch = head.Name().Short()
	}

	// Register project with coordinator. The working-tree path
	// goes in remote_url so the local fat-client opens it
	// directly via RegisterExternalDir below. If the folder
	// already has an origin (github clone case), pushes still
	// route to that origin via the working tree's git config —
	// nothing to coordinate at the Enju layer. If the folder
	// has NO origin, async tasks still work via the scanner's
	// refs/heads fallback (no shadow bare needed). The user
	// can later upgrade to a shared remote with
	// enju_set_project_remote.
	body := map[string]string{
		"name":       name,
		"remote_url": dirPath,
	}
	if adoptedBranch != "" {
		body["default_branch"] = adoptedBranch
	}
	data, err := c.post(ctx, "/api/v1/projects", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Register the folder as an external workspace so ForProject
	// opens it directly instead of cloning into ~/.enju/workspaces.
	if c.workspace != nil {
		var result map[string]interface{}
		if json.Unmarshal(data, &result) == nil {
			if projectID := int64(jsonFloat(result["id"])); projectID > 0 {
				c.workspace.RegisterExternalDir(projectID, dirPath)
				// Open it immediately to verify it works.
				if _, perr := c.workspace.ForProject(projectID, ""); perr != nil {
					c.logger.Warn("opening init'd folder", "error", perr)
				}
			}
		}
	}

	return mcp.NewToolResultText(fmt.Sprintf("✓ Initialized Enju in %s\n  Project registered as: %s", dirPath, name)), nil
}
// handleProjectRemoteStatus runs the remote-status diagnostic
// entirely on the client side. Phase 1 ran this in the coordinator
// against a server-owned clone; iteration A moves the clone to the
// client, so this tool now opens the MCP workspace's local clone
// and calls mcpgit.Project.CompareToRemote. The output shape is
// unchanged from the Phase 1 tool so formatters keep working.
func (c *apiClient) handleProjectRemoteStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("remote status is only available in MCP client mode"), nil
	}
	proj, remoteURL, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
	}
	if remoteURL == "" {
		resp["status"] = string(mcpgit.RemoteNoRemote)
		data, _ := json.Marshal(resp)
		return mcp.NewToolResultText(formatProjectRemoteStatus(data)), nil
	}

	cmp, err := proj.CompareToRemote()
	if err != nil {
		return mcp.NewToolResultError("comparing to remote: " + err.Error()), nil
	}

	// For init'd projects, show both the workspace path and the
	// actual git origin URL (if different).
	if gitOrigin := proj.GitOriginURL(); gitOrigin != "" && gitOrigin != remoteURL {
		resp["workspace"] = remoteURL
		resp["remote_url"] = gitOrigin
	}

	resp["status"] = string(cmp.Status)
	resp["local_head"] = cmp.LocalHead
	resp["remote_head"] = cmp.RemoteHead
	resp["ahead_by"] = cmp.AheadBy
	resp["behind_by"] = cmp.BehindBy
	if cmp.Unreachable != "" {
		resp["remote_error"] = cmp.Unreachable
	}
	// A.5 polish: surface the in-memory push-status bookkeeping
	// so the formatter can render "last push: <time>" / "last
	// push failed: <error>" the same way iteration 4 did.
	if t := proj.LastPushAt(); !t.IsZero() {
		resp["last_push_at"] = t.Format(time.RFC3339)
	}
	if e := proj.LastPushError(); e != "" {
		resp["last_push_error"] = e
	}
	data, _ := json.Marshal(resp)
	return mcp.NewToolResultText(formatProjectRemoteStatus(data)), nil
}
// handleProjectSync force-syncs the client's local clone to its
// remote. Runs entirely client-side: open the clone, preflight via
// CompareToRemote, refuse diverged state unless force=true, push.
// The coordinator is not involved beyond the initial project
// lookup.
func (c *apiClient) handleProjectSync(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	if c.workspace == nil {
		return mcp.NewToolResultError("project sync is only available in MCP client mode"), nil
	}
	force := req.GetBool("force", false)

	proj, remoteURL, _, _, err := c.openProject(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if remoteURL == "" {
		return mcp.NewToolResultError("project has no remote configured"), nil
	}

	proj.Lock()
	defer proj.Unlock()

	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
		"force":      force,
	}

	// Preflight comparison so we can refuse destructive force-less
	// pushes to a diverged remote.
	cmp, cmpErr := proj.CompareToRemote()
	if cmpErr == nil && cmp != nil {
		resp["status"] = string(cmp.Status)
		resp["local_head"] = cmp.LocalHead
		resp["remote_head"] = cmp.RemoteHead
		resp["ahead_by"] = cmp.AheadBy
		resp["behind_by"] = cmp.BehindBy

		switch cmp.Status {
		case mcpgit.RemoteInSync:
			resp["result"] = "noop"
			resp["message"] = "already in sync"
			data, _ := json.Marshal(resp)
			return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
		case mcpgit.RemoteBehind:
			resp["result"] = "noop"
			resp["message"] = fmt.Sprintf("local is behind remote by %d commit(s); nothing to push — fetch+merge to catch up", cmp.BehindBy)
			data, _ := json.Marshal(resp)
			return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
		case mcpgit.RemoteDiverged, mcpgit.RemoteUnrelated:
			if !force {
				resp["result"] = "refused"
				resp["message"] = fmt.Sprintf(
					"remote has diverged (local ahead by %d, behind by %d) — refuse to push without force=true; re-run with force=true to overwrite remote, or reconcile manually",
					cmp.AheadBy, cmp.BehindBy,
				)
				data, _ := json.Marshal(resp)
				return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
			}
		}
	}

	var pushErr error
	if force {
		pushErr = proj.PushForce()
	} else {
		pushErr = proj.Push()
	}
	if pushErr != nil {
		resp["result"] = "failed"
		resp["error"] = pushErr.Error()
		data, _ := json.Marshal(resp)
		return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
	}
	if force {
		resp["result"] = "force_pushed"
	} else {
		resp["result"] = "pushed"
	}
	data, _ := json.Marshal(resp)
	return mcp.NewToolResultText(formatProjectSyncResult(data)), nil
}
// handleLeaveProject removes the caller's membership on the
// coordinator and wipes their local clone of a project. The
// remote repo is untouched. Refused when the caller is the last
// owner — promote another member first.
//
// Pass keep_membership=true to wipe just the local clone while
// staying a member (the original Phase A behavior).
func (c *apiClient) handleLeaveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	keepMembership := req.GetBool("keep_membership", false)
	if c.workspace == nil {
		return mcp.NewToolResultError("leave project is only available in MCP client mode"), nil
	}
	// Existence check. fetchProjectMeta returns an error if the
	// coordinator's GET /projects/{id} responds with 404 (or any
	// other error); surface it verbatim.
	if _, err := c.fetchProjectMeta(ctx, int64(projectID)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("✗ Project #%d not found", projectID)), nil
	}
	// Remove the coordinator-side membership row before wiping
	// the clone — if the remove is refused (last-owner), we
	// want to bail out with a clear error, not orphan the local
	// clone on top. When keepMembership is set, skip this.
	var membershipMsg string
	if !keepMembership && c.username != "" {
		path := fmt.Sprintf("/api/v1/projects/%d/members/by-username/%s", projectID, c.username)
		data, err := c.delete(ctx, path)
		if err != nil {
			return mcp.NewToolResultError("leaving project: " + err.Error()), nil
		}
		if len(data) > 0 {
			var resp map[string]interface{}
			if json.Unmarshal(data, &resp) == nil {
				if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
					return mcp.NewToolResultError(errMsg), nil
				}
			}
		}
		membershipMsg = fmt.Sprintf("✓ Project #%d: membership removed. ", projectID)
	}
	hadClone := c.workspace.HasLocalClone(int64(projectID))
	if err := c.workspace.LeaveProject(int64(projectID)); err != nil {
		return mcp.NewToolResultError("removing local clone: " + err.Error()), nil
	}
	var cloneMsg string
	if hadClone {
		cloneMsg = "local clone removed"
	} else {
		cloneMsg = "no clone to remove (already absent)"
	}
	if keepMembership {
		return mcp.NewToolResultText(fmt.Sprintf("✓ Project #%d: %s — membership kept (keep_membership=true)", projectID, cloneMsg)), nil
	}
	return mcp.NewToolResultText(membershipMsg + cloneMsg), nil
}

// handleAddProjectMember grants membership to another citizen.
// Maps directly onto POST /projects/{id}/members.
func (c *apiClient) handleAddProjectMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	username, err := req.RequireString("username")
	if err != nil {
		return mcp.NewToolResultError("username is required"), nil
	}
	role := req.GetString("role", "")
	body := map[string]string{"username": username}
	if role != "" {
		body["role"] = role
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/members", projectID), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) == nil {
		if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}
		addedRole, _ := resp["role"].(string)
		if addedRole == "" {
			addedRole = "member"
		}
		return mcp.NewToolResultText(fmt.Sprintf("✓ Added %s to project #%d as %s", username, projectID, addedRole)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Added %s to project #%d", username, projectID)), nil
}

// handleRemoveProjectMember removes another citizen from a project.
// Owner-only on the coordinator side.
func (c *apiClient) handleRemoveProjectMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	username, err := req.RequireString("username")
	if err != nil {
		return mcp.NewToolResultError("username is required"), nil
	}
	path := fmt.Sprintf("/api/v1/projects/%d/members/by-username/%s", projectID, username)
	data, err := c.delete(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(data) > 0 {
		var resp map[string]interface{}
		if json.Unmarshal(data, &resp) == nil {
			if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
				return mcp.NewToolResultError(errMsg), nil
			}
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Removed %s from project #%d", username, projectID)), nil
}

// handleListProjectMembers lists the project's members. Members
// only on the coordinator side.
func (c *apiClient) handleListProjectMembers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	data, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/members", projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// GET endpoints return the coordinator body verbatim on
	// 4xx — detect the error envelope and surface it as a
	// tool-level error rather than rendering it as a listing.
	var envelope map[string]interface{}
	if json.Unmarshal(data, &envelope) == nil {
		if msg, ok := envelope["error"].(string); ok && msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
	}
	return mcp.NewToolResultText(formatProjectMemberList(data, int64(projectID))), nil
}

// handlePromoteMember sets a member's role to owner.
func (c *apiClient) handlePromoteMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return c.setMemberRole(ctx, req, "owner", "promoted")
}

// handleDemoteOwner sets an owner's role back to member.
func (c *apiClient) handleDemoteOwner(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return c.setMemberRole(ctx, req, "member", "demoted")
}

// setMemberRole is the shared body of promote + demote — both PUT
// the target role to the same role-change endpoint; only the
// role value and result verb differ.
func (c *apiClient) setMemberRole(ctx context.Context, req mcp.CallToolRequest, newRole, verb string) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	username, err := req.RequireString("username")
	if err != nil {
		return mcp.NewToolResultError("username is required"), nil
	}
	path := fmt.Sprintf("/api/v1/projects/%d/members/by-username/%s/role", projectID, username)
	data, err := c.put(ctx, path, map[string]string{"role": newRole})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) == nil {
		if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}
		if changed, _ := resp["changed"].(bool); !changed {
			return mcp.NewToolResultText(fmt.Sprintf("• %s is already %s on project #%d (no change)", username, newRole, projectID)), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ %s %s on project #%d (now: %s)", username, verb, projectID, newRole)), nil
}
// handleSetProjectDefaultBranch changes a project's default
// branch. Thin REST pass-through; the coordinator enforces
// owner-only + branch shape validation.
func (c *apiClient) handleSetProjectDefaultBranch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	branch, err := req.RequireString("branch")
	if err != nil {
		return mcp.NewToolResultError("branch is required"), nil
	}
	data, err := c.put(ctx, fmt.Sprintf("/api/v1/projects/%d/default_branch", projectID), map[string]string{
		"branch": branch,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) == nil {
		if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Project #%d default branch set to %q", projectID, branch)), nil
}

// handleSetProjectRemote updates a project's remote URL in the
// coordinator DB and, if a local clone exists, reconfigures its
// origin remote to match. Kept as a single tool (not split between
// coordinator and client) because the DB update and the local
// clone reconfiguration must stay consistent.
func (c *apiClient) handleSetProjectRemote(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	remoteURL, err := req.RequireString("remote_url")
	if err != nil {
		return mcp.NewToolResultError("remote_url is required"), nil
	}
	// Reject empty remote_url. With the scanner's refs/heads
	// fallback (Option B), clearing the remote no longer breaks
	// async reconciliation locally — the caller's own machine
	// keeps working. But on a multi-machine project, clearing
	// silently forks the team: Alice's local commits stop
	// pushing anywhere, Bob's machine has no way to see them,
	// and the project quietly bifurcates. The original "clear
	// remote" semantics had no legitimate use case in either
	// solo or shared mode: migrating to a different remote uses
	// the URL-replace path (just call this tool with the new
	// URL); deleting a project uses enju_leave_project. Closing
	// the door here removes a footgun whose blast radius scales
	// with how many citizens depend on the project.
	if strings.TrimSpace(remoteURL) == "" {
		return mcp.NewToolResultError(
			"remote_url cannot be empty — clearing a project's remote breaks async reconciliation. " +
				"To migrate to a different remote, pass the new URL directly. " +
				"To stop using this project on this machine, call enju_leave_project.",
		), nil
	}
	data, err := c.put(ctx, fmt.Sprintf("/api/v1/projects/%d/remote", projectID), map[string]string{
		"remote_url": remoteURL,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) == nil {
		if errMsg, ok := resp["error"].(string); ok {
			return mcp.NewToolResultError(errMsg), nil
		}
	}

	// Mirror the remote change into the existing local clone:
	//   1. Update the on-disk origin URL so future pushes/fetches
	//      hit the right place.
	//   2. Push every local branch to it so the new bare contains
	//      all the work that accumulated while the project was
	//      originless. The typical late-add scenario is "project
	//      ran async compute with no remote, commits stranded on
	//      local refs/heads/*" — without seeding,
	//      refs/remotes/origin/<branch> stays empty and the
	//      scanner can't see those commits.
	//   3. Reset scan cursors for every local branch to the
	//      sentinel that forces full-history rescans on next
	//      reconcile. This is what surfaces the historical
	//      trailers (TP53 Bug 2): without the reset, branches that
	//      previously baseline-tipped would skip history; with it,
	//      ScanBranchSince re-emits every trailer commit, the
	//      coordinator processes them (idempotent), and the
	//      artifact index catches up.
	//
	// remoteURL is guaranteed non-empty here — the validation at
	// the top of this handler rejects empty input.
	pushWarning := ""
	if c.workspace != nil {
		if proj, err := c.workspace.ForProject(int64(projectID), remoteURL); err == nil {
			proj.Lock()
			_ = proj.SetRemote(remoteURL)
			if pushErr := proj.PushAllLocalBranches(); pushErr != nil {
				// Non-fatal: the remote is set, but seeding
				// failed. Surface so the user knows manual
				// pushes may be needed before the cursor reset
				// can find anything to walk.
				pushWarning = fmt.Sprintf("\n⚠ Pushing local branches to new remote failed: %v", pushErr)
				c.logger.Warn("set_project_remote: push to new remote failed",
					"project_id", projectID, "remote", remoteURL, "error", pushErr)
			}
			// Cursor reset runs even on push failure: a partial
			// push (some branches landed) still wants
			// retroactive scans on those branches, and
			// resetting branches whose remote refs don't yet
			// exist is harmless (the scanner returns empty
			// when the remote ref is missing, leaving the
			// sentinel in place for the next attempt).
			if branches, lerr := proj.LocalBranches(); lerr == nil {
				cursorMu := mcpgit.CursorMutexFor(c.stateDir(), int64(projectID))
				cursorMu.Lock()
				cursors, _ := mcpgit.LoadCursors(c.stateDir(), int64(projectID))
				for _, b := range branches {
					cursors.Set(b, mcpgit.RescanSentinelSHA)
				}
				if serr := cursors.Save(); serr != nil {
					c.logger.Warn("set_project_remote: cursor reset save failed",
						"project_id", projectID, "error", serr)
				}
				cursorMu.Unlock()
			}
			proj.Unlock()
		}
	}

	return mcp.NewToolResultText(fmt.Sprintf("✓ Set remote for project %d to %s%s", projectID, remoteURL, pushWarning)), nil
}
