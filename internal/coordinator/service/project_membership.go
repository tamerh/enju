package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// SetDefaultBranchResponse is the wire shape for
// enju_set_project_default_branch.
type SetDefaultBranchResponse struct {
	ProjectID     int64  `json:"project_id"`
	DefaultBranch string `json:"default_branch"`
}

// SetProjectDefaultBranch updates the project's default branch.
// Owner-only. Validates the branch name.
func SetProjectDefaultBranch(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, branch string) (*SetDefaultBranchResponse, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, fmt.Errorf("%w: branch is required", ErrInvalidArgument)
	}
	if err := validateBranchName(branch); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}
	if err := requireOwner(s, projectID, caller.ID); err != nil {
		return nil, err
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetProjectDefaultBranch{ProjectID: projectID, Branch: branch},
		},
	}); err != nil {
		return nil, err
	}
	return &SetDefaultBranchResponse{
		ProjectID:     projectID,
		DefaultBranch: branch,
	}, nil
}

// AddMemberResponse is the wire shape for enju_add_project_member.
type AddMemberResponse struct {
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role"`
	AddedAt  string `json:"added_at"`
	AddedBy  string `json:"added_by,omitempty"`
}

// AddProjectMember grants membership to a citizen. Any member
// can add as 'member'; only owners can add as 'owner'.
func AddProjectMember(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, username, roleStr string) (*AddMemberResponse, error) {
	if username == "" {
		return nil, fmt.Errorf("%w: username is required", ErrInvalidArgument)
	}
	callerMember, err := requireMembership(s, projectID, caller.ID)
	if err != nil {
		return nil, err
	}
	target, err := s.GetCitizenByUsername(username)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("%w: citizen %q not found", ErrInvalidArgument, username)
	}
	if existing, _ := s.GetProjectMember(projectID, target.ID); existing != nil {
		return nil, fmt.Errorf("%w: %q is already a member of this project", ErrConflict, username)
	}
	role := store.ProjectRole(roleStr)
	switch role {
	case "":
		role = store.ProjectRoleMember
	case store.ProjectRoleOwner:
		// Adding-as-owner shortcut bypasses the promote-only-by-
		// owner rule unless we gate it.
		if callerMember == nil || callerMember.Role != store.ProjectRoleOwner {
			return nil, fmt.Errorf("%w: only project owners can add members as 'owner' — ask an owner, or add as 'member' and request promotion", ErrForbidden)
		}
	case store.ProjectRoleMember:
		// default
	default:
		return nil, fmt.Errorf("%w: unknown role %q (expected 'member' or 'owner')", ErrInvalidArgument, roleStr)
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.AddProjectMember{
				ProjectID: projectID,
				CitizenID: target.ID,
				Role:      role,
				AddedBy:   caller.ID,
			},
		},
	}); err != nil {
		return nil, err
	}
	return &AddMemberResponse{
		Username: target.Username,
		Name:     target.Name,
		Role:     string(role),
		AddedAt:  time.Now().Format(time.RFC3339),
		AddedBy:  CitizenUsername(s, caller.ID),
	}, nil
}

// RemoveMemberResponse is the wire shape for
// enju_remove_project_member / enju_leave_project.
type RemoveMemberResponse struct {
	ProjectID int64  `json:"project_id"`
	Citizen   string `json:"citizen"`
	Removed   bool   `json:"removed"`
	SelfLeave bool   `json:"self_leave"`
}

// RemoveProjectMember removes a citizen from the project.
// Caller must be an owner OR removing themselves. Refuses to
// drop owner count to zero.
func RemoveProjectMember(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, targetUsername string) (*RemoveMemberResponse, error) {
	if targetUsername == "" {
		return nil, fmt.Errorf("%w: username is required", ErrInvalidArgument)
	}
	target, err := s.GetCitizenByUsername(targetUsername)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("%w: citizen %q not found", ErrInvalidArgument, targetUsername)
	}
	callerMember, err := requireMembership(s, projectID, caller.ID)
	if err != nil {
		return nil, err
	}
	isSelf := caller.ID == target.ID
	if !isSelf && (callerMember == nil || callerMember.Role != store.ProjectRoleOwner) {
		return nil, fmt.Errorf("%w: only project owners can remove other members — or remove yourself to leave", ErrForbidden)
	}
	targetMember, err := s.GetProjectMember(projectID, target.ID)
	if err != nil {
		return nil, err
	}
	if targetMember == nil {
		return nil, fmt.Errorf("%w: member not found on this project", ErrNotFound)
	}
	// Last-owner invariant.
	if targetMember.Role == store.ProjectRoleOwner {
		owners, _ := s.CountProjectOwners(projectID)
		if owners <= 1 {
			if isSelf {
				return nil, fmt.Errorf("%w: you are the last owner — promote another member to owner first, then leave", ErrConflict)
			}
			return nil, fmt.Errorf("%w: cannot remove the last owner — promote another member to owner first", ErrConflict)
		}
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.RemoveProjectMember{ProjectID: projectID, CitizenID: target.ID},
		},
	}); err != nil {
		return nil, err
	}
	return &RemoveMemberResponse{
		ProjectID: projectID,
		Citizen:   target.Username,
		Removed:   true,
		SelfLeave: isSelf,
	}, nil
}

// LeaveProject is a convenience wrapper around
// RemoveProjectMember(caller.Username) — the most common self-
// remove path. Used by enju_leave_project. Native MCP doesn't
// touch the local clone (no workspace); the fat-client tool
// adds the workspace.LeaveProject step on top.
func LeaveProject(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64) (*RemoveMemberResponse, error) {
	return RemoveProjectMember(s, caller, projectID, caller.Username)
}

// SetMemberRoleResponse is the wire shape for promote/demote.
type SetMemberRoleResponse struct {
	ProjectID int64  `json:"project_id"`
	Citizen   string `json:"citizen"`
	Role      string `json:"role"`
	Changed   bool   `json:"changed"`
}

// SetProjectMemberRole promotes or demotes a member.
// Owner-only. Refuses to demote the last owner.
func SetProjectMemberRole(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, targetUsername, roleStr string) (*SetMemberRoleResponse, error) {
	if targetUsername == "" {
		return nil, fmt.Errorf("%w: username is required", ErrInvalidArgument)
	}
	if err := requireOwner(s, projectID, caller.ID); err != nil {
		return nil, err
	}
	newRole := store.ProjectRole(roleStr)
	if newRole != store.ProjectRoleOwner && newRole != store.ProjectRoleMember {
		return nil, fmt.Errorf("%w: unknown role %q (expected 'owner' or 'member')", ErrInvalidArgument, roleStr)
	}
	target, err := s.GetCitizenByUsername(targetUsername)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("%w: citizen %q not found", ErrInvalidArgument, targetUsername)
	}
	targetMember, err := s.GetProjectMember(projectID, target.ID)
	if err != nil {
		return nil, err
	}
	if targetMember == nil {
		return nil, fmt.Errorf("%w: member not found on this project", ErrNotFound)
	}
	if targetMember.Role == newRole {
		return &SetMemberRoleResponse{
			ProjectID: projectID,
			Citizen:   target.Username,
			Role:      string(newRole),
			Changed:   false,
		}, nil
	}
	// Last-owner invariant on demotion.
	if targetMember.Role == store.ProjectRoleOwner && newRole == store.ProjectRoleMember {
		owners, _ := s.CountProjectOwners(projectID)
		if owners <= 1 {
			return nil, fmt.Errorf("%w: cannot demote the last owner — promote another member to owner first", ErrConflict)
		}
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetProjectMemberRole{
				ProjectID: projectID,
				CitizenID: target.ID,
				Role:      newRole,
			},
		},
	}); err != nil {
		return nil, err
	}
	return &SetMemberRoleResponse{
		ProjectID: projectID,
		Citizen:   target.Username,
		Role:      string(newRole),
		Changed:   true,
	}, nil
}

// requireMembership fetches the caller's member row. Returns
// (nil, nil) for legacy zero-member projects (open access);
// (record, nil) for explicit members; ErrNotMember otherwise.
func requireMembership(s store.CoordinatorStore, projectID, citizenID int64) (*store.ProjectMemberRecord, error) {
	total, err := s.CountProjectMembers(projectID)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		// Legacy zero-member project — open. No member record to
		// return; callers that need owner-only gating should use
		// requireOwner instead, which treats this as denied.
		return nil, nil
	}
	m, err := s.GetProjectMember(projectID, citizenID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrNotMember
	}
	return m, nil
}

// requireOwner enforces the owner-only gate for membership
// mutations. Returns ErrForbidden when the caller is a member
// but not an owner. Legacy zero-member projects (no member
// rows) pass — they predate the membership model and stay
// open, matching the api's pre-service behavior. Once the
// project has any explicit member, owner-only gating kicks in
// normally.
func requireOwner(s store.CoordinatorStore, projectID, citizenID int64) error {
	m, err := requireMembership(s, projectID, citizenID)
	if err != nil {
		return err
	}
	if m == nil {
		// Legacy zero-member project — open. Anyone
		// authenticated may perform owner-only operations.
		return nil
	}
	if m.Role != store.ProjectRoleOwner {
		return fmt.Errorf("%w: only project owners can perform this action", ErrForbidden)
	}
	return nil
}

// validateBranchName enforces git's branch-name rules — same
// validation the legacy api.validateBranchName applied. Lifted
// to service so the gate runs uniformly across transports.
func validateBranchName(s string) error {
	if s == "" {
		return errors.New("branch name cannot be empty")
	}
	if s == "HEAD" {
		return fmt.Errorf("branch name %q is reserved", s)
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return fmt.Errorf("branch name %q: must not start with '-' or '/' and must not end with '/'", s)
	}
	if strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{") {
		return fmt.Errorf("branch name %q contains a forbidden sequence (.., //, or @{)", s)
	}
	for _, r := range s {
		switch r {
		case ' ', '\t', '~', '^', ':', '?', '*', '[', '\\', '\x7f':
			return fmt.Errorf("branch name %q contains a forbidden character %q", s, r)
		}
		if r < 0x20 {
			return fmt.Errorf("branch name %q contains a control character", s)
		}
	}
	return nil
}
