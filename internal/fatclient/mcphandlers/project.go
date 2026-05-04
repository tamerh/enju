package mcphandlers

// Project-lifecycle handlers. Creating (from scratch or by
// adopting an existing folder via enju_init), listing, reading
// remote status + sync state, setting a remote URL, and
// leaving a project (deleting its local clone). Workspace-heavy
// orchestration (git scaffold, compare-to-remote, push +
// cursor-reset, eager clone init) lives in
// internal/fatclient/service/project_ops.go; this file is the
// transport-layer translator: parse args, call coord, dispatch
// to service, format the response.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/projects")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Decorate the coordinator's project list with local push-status
	// info from the MCP workspace — but only for projects whose clone
	// already exists on disk. Cheap by design: no fresh clones get
	// triggered as a side effect of a listing call.
	decorated := c.fc.DecorateProjectListWithPushStatus(data)
	return mcp.NewToolResultText(format.ProjectList(decorated)), nil
}

func (c *apiClient) handleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	description := req.GetString("description", "")
	remoteURL := req.GetString("remote_url", "")
	defaultBranch := req.GetString("default_branch", "")
	customPath := req.GetString("path", "")

	// Validate optional `path`: must be absolute, must be empty
	// or non-existent. The "fresh" guarantee on this tool means
	// callers can trust we won't overwrite anything — populated
	// directories must go through enju_init instead.
	if customPath != "" {
		// path + remote_url combined would be ambiguous: the
		// custom-path code path seeds a fresh local working tree
		// rather than cloning, so the project record would persist
		// a remote_url it never actually cloned from. Refuse loudly
		// rather than create that drift silently.
		if remoteURL != "" {
			return mcp.NewToolResultError("path and remote_url are mutually exclusive — enju_create_project with path= seeds a fresh local working tree, it does not clone. To use a remote, either omit path= (workspace lands at ~/.enju/workspaces/) or git-clone the remote yourself and run enju_init on the resulting directory."), nil
		}
		if !filepath.IsAbs(customPath) {
			return mcp.NewToolResultError(fmt.Sprintf("path must be absolute, got %q", customPath)), nil
		}
		// Lstat (not Stat) so symlinks surface as symlinks rather
		// than being silently followed. Following symlinks would
		// be a footgun: a user passing path=/home/me/proj where
		// proj is a symlink to a populated repo would either get
		// "refused, not empty" (confusing — they thought proj was
		// fresh) or, worse if the target is empty, end up with
		// the project's working tree dual-rooted via the symlink.
		info, lstatErr := os.Lstat(customPath)
		switch {
		case lstatErr == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return mcp.NewToolResultError(fmt.Sprintf("path %q is a symlink — pass a real directory path. If you intended the link target, resolve it with `readlink -f` and pass that instead.", customPath)), nil
			}
			if !info.IsDir() {
				return mcp.NewToolResultError(fmt.Sprintf("path %q exists but is not a directory", customPath)), nil
			}
			entries, readErr := os.ReadDir(customPath)
			if readErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("reading %q: %v", customPath, readErr)), nil
			}
			if len(entries) > 0 {
				return mcp.NewToolResultError(fmt.Sprintf(
					"path %q exists and is not empty — enju_create_project requires an empty or non-existent directory. To adopt a populated folder, use enju_init with the same path.",
					customPath,
				)), nil
			}
		case os.IsNotExist(lstatErr):
			// Doesn't exist — fall through to MkdirAll, which
			// handles non-existent parent chains too.
		default:
			return mcp.NewToolResultError(fmt.Sprintf("checking path %q: %v", customPath, lstatErr)), nil
		}
		if mkErr := os.MkdirAll(customPath, 0755); mkErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("creating %q: %v", customPath, mkErr)), nil
		}
	}

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
	// claim. Failures are non-fatal at this point: the project
	// record is registered, and the next tool call will retry
	// the init/clone.
	if c.fc.Workspace() != nil {
		var result map[string]interface{}
		if json.Unmarshal(data, &result) == nil {
			if projectID := int64(format.JsonFloat(result["id"])); projectID > 0 {
				if ierr := c.fc.EagerInitProjectClone(ctx, projectID, customPath); ierr != nil {
					c.fc.Logger().Warn("eager workspace init failed (will retry on first task)",
						"project_id", projectID, "path", customPath, "error", ierr)
				}
				// Auto-subscribe notifications to the just-created
				// project. Nil supervisor (notify-disabled session)
				// no-ops cleanly.
				c.notifySess.Switch(projectID)
			}
		}
	}

	return mcp.NewToolResultText(format.CreateProjectResult(data)), nil
}

// handleInit adopts an existing folder as an Enju project. It:
// 1. Validates the path exists and refuses populated unrelated repos.
// 2. Hands off to service.FatClient.InitDirAsProject for git init +
//    scaffold + commit (returns the adopted branch name).
// 3. Registers the project with the coordinator, passing the
//    adopted branch as default_branch.
// 4. Calls service.FatClient.RegisterAdoptedDir to wire the folder
//    into the workspace as external, then opens it once to verify.
func (c *apiClient) handleInit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	dirPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	force := req.GetBool("force", false)

	stat, err := os.Stat(dirPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("path %q does not exist: %v", dirPath, err)), nil
	}
	if !stat.IsDir() {
		return mcp.NewToolResultError(fmt.Sprintf("path %q is not a directory", dirPath)), nil
	}

	// Safety check: refuse populated unrelated git repos unless
	// force=true. The footgun this catches: a calling LLM running
	// inside /repo/A passes path=/repo/B but typos to /repo/A —
	// without this gate, Enju silently writes its scaffold + a
	// commit into the wrong repo.
	if !force {
		if reason := service.DetectPopulatedUnrelatedRepo(dirPath); reason != "" {
			return mcp.NewToolResultError(fmt.Sprintf(
				"%s. To adopt this directory anyway, re-invoke enju_init with force=true. To initialize a fresh project elsewhere, pass a different path or use enju_create_project.",
				reason,
			)), nil
		}
	}

	adoptedBranch, err := c.fc.InitDirAsProject(dirPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Register project with coordinator. The working-tree path
	// goes in remote_url so the local fat-client opens it
	// directly via RegisterExternalDir below. If the folder
	// already has an origin (github clone case), pushes still
	// route to that origin via the working tree's git config.
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

	if c.fc.Workspace() != nil {
		var result map[string]interface{}
		if json.Unmarshal(data, &result) == nil {
			if projectID := int64(format.JsonFloat(result["id"])); projectID > 0 {
				if rerr := c.fc.RegisterAdoptedDir(projectID, dirPath); rerr != nil {
					c.fc.Logger().Warn("opening init'd folder", "error", rerr)
				}
				// Auto-subscribe notifications. Same rationale as
				// create_project — init signals "I'm working here
				// now" so the cross-restart record updates.
				c.notifySess.Switch(projectID)
			}
		}
	}

	return mcp.NewToolResultText(fmt.Sprintf("✓ Initialized Enju in %s\n  Project registered as: %s", dirPath, name)), nil
}

func (c *apiClient) handleProjectRemoteStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	resp, err := c.fc.RemoteStatusReport(ctx, int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(resp)
	return mcp.NewToolResultText(format.ProjectRemoteStatus(data)), nil
}

func (c *apiClient) handleProjectSync(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	force := req.GetBool("force", false)
	resp, err := c.fc.SyncProjectToRemote(ctx, int64(projectID), force)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, _ := json.Marshal(resp)
	return mcp.NewToolResultText(format.ProjectSyncResult(data)), nil
}

// handleLeaveProject removes the caller's membership on the
// coordinator and wipes their local clone of a project. The
// remote repo is untouched. Refused when the caller is the last
// owner — promote another member first.
//
// Pass keep_membership=true to wipe just the local clone while
// staying a member.
func (c *apiClient) handleLeaveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	keepMembership := req.GetBool("keep_membership", false)
	if c.fc.Workspace() == nil {
		return mcp.NewToolResultError("leave project is only available in MCP client mode"), nil
	}
	// Existence check.
	if _, err := c.fc.FetchProjectMeta(ctx, int64(projectID)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("✗ Project #%d not found", projectID)), nil
	}
	// Remove the coordinator-side membership row before wiping
	// the clone — if the remove is refused (last-owner), bail
	// out with a clear error rather than orphan the local clone
	// on top.
	var membershipMsg string
	if !keepMembership && c.username() != "" {
		path := fmt.Sprintf("/api/v1/projects/%d/members/by-username/%s", projectID, c.username())
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
	hadClone, err := c.fc.LocalLeaveProject(int64(projectID))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
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
	var envelope map[string]interface{}
	if json.Unmarshal(data, &envelope) == nil {
		if msg, ok := envelope["error"].(string); ok && msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
	}
	return mcp.NewToolResultText(format.ProjectMemberList(data, int64(projectID))), nil
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
	// fallback, clearing the remote no longer breaks async
	// reconciliation locally — the caller's own machine keeps
	// working. But on a multi-machine project, clearing silently
	// forks the team: Alice's local commits stop pushing
	// anywhere, Bob's machine has no way to see them, and the
	// project quietly bifurcates.
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

	// Mirror the remote change into the existing local clone
	// (origin URL update + push every local branch to seed the
	// new bare + cursor reset to force full-history rescan).
	pushWarning := c.fc.MirrorRemoteAfterSet(int64(projectID), remoteURL)

	return mcp.NewToolResultText(fmt.Sprintf("✓ Set remote for project %d to %s%s", projectID, remoteURL, pushWarning)), nil
}
