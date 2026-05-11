package mcphandlers

// Project-lifecycle handlers. Creating (from scratch or by
// adopting an existing folder via path=), listing, reading
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
	customPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required — pass an absolute path to the folder you want to use as the project's working tree. The folder may be empty, populated (we'll git-init and commit your files), or already a git repo (we'll adopt it)."), nil
	}
	params := service.CreateProjectParams{
		Name:          name,
		Description:   req.GetString("description", ""),
		DefaultBranch: req.GetString("default_branch", ""),
		Path:          customPath,
		RemoteURL:     req.GetString("remote_url", ""),
		Force:         req.GetBool("force", false),
	}
	res, err := c.fc.CreateProject(ctx, params)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if res.ProjectID > 0 {
		// Auto-subscribe notifications to the just-created project.
		// Nil supervisor (notify-disabled session) no-ops cleanly.
		c.notifySess.Switch(res.ProjectID)
	}
	text := format.CreateProjectResult(res.CoordResponse)
	if res.InitWarning != "" {
		text += fmt.Sprintf("\n\n⚠ Local workspace not initialized — %s", res.InitWarning)
	}
	return mcp.NewToolResultText(text), nil
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
	if c.fc.Enjugit() == nil {
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
// branch. The coordinator enforces owner-only + branch shape
// validation; on success, the fat-client materializes the new
// default in git so the workspace doesn't drift from the coord
// setting (subsequent runs default-branch the new name —
// PrepareBranchForCommit + EnsureRunBranch resolve `branch` from
// origin/local refs, so the ref must actually exist).
func (c *apiClient) handleSetProjectDefaultBranch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	branch, err := req.RequireString("branch")
	if err != nil {
		return mcp.NewToolResultError("branch is required"), nil
	}
	// Capture the OLD default before the coord update so a
	// brand-new branch can fork from the prior default. If
	// fetch fails (project gone, network), EnsureRunBranch
	// gracefully no-ops with a warning instead of a hard fail.
	_, _, oldDefault, _ := c.fc.FetchProjectMetaExpanded(ctx, int64(projectID))

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

	// Materialize the new default in git so subsequent runs
	// can fork from it. Same idempotent verb used at create_run:
	// existing branch (local OR origin) is no-op'd; brand-new
	// branch forks from oldDefault and pushes. Errors are
	// non-fatal — coord update already landed.
	ensureWarning := c.fc.EnsureProjectDefaultBranch(ctx, int64(projectID), branch, oldDefault)

	text := fmt.Sprintf("✓ Project #%d default branch set to %q", projectID, branch)
	if ensureWarning != "" {
		text += fmt.Sprintf("\n⚠ %s", ensureWarning)
	}
	return mcp.NewToolResultText(text), nil
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
	// new bare + cursor reset to force full-history rescan). The
	// migrationNote tells the operator when their project is
	// graduating from a managed local bare to a real remote so the
	// "all your local branches are being mirrored" effect isn't
	// silent.
	migrationNote, pushWarning := c.fc.MirrorRemoteAfterSet(int64(projectID), remoteURL)

	return mcp.NewToolResultText(fmt.Sprintf("✓ Set remote for project %d to %s%s%s", projectID, remoteURL, migrationNote, pushWarning)), nil
}
