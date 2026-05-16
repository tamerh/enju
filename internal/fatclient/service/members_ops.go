package service

// Project-membership write methods. The MCP handlers
// (mcphandlers/project.go) drive these endpoints with raw
// apiClient coord calls; webui can't reach apiClient (it only
// imports internal/fatclient/service), so the coord calls get a
// thin home here. 1:1 with the coord HTTP surface — no extra
// policy; the coordinator enforces owner-only / membership.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// AddProjectMember adds a citizen to a project (mirror of
// enju_add_project_member). role is optional — pass "" to let
// the coordinator default it ("member"). Coordinator enforces
// owner-only.
func (s *FatClient) AddProjectMember(ctx context.Context, projectID int64, username, role string) error {
	body := map[string]string{"username": username}
	if role != "" {
		body["role"] = role
	}
	data, err := s.coord.Post(ctx, fmt.Sprintf("/api/v1/projects/%d/members", projectID), body)
	if err != nil {
		return err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// RemoveProjectMember removes a citizen from a project (mirror
// of enju_remove_project_member). Coordinator enforces
// owner-only. A 200 with an empty body is success.
func (s *FatClient) RemoveProjectMember(ctx context.Context, projectID int64, username string) error {
	path := fmt.Sprintf("/api/v1/projects/%d/members/by-username/%s",
		projectID, url.PathEscape(username))
	data, err := s.coord.Delete(ctx, path)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		if msg := coord.ExtractError(data); msg != "" {
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}

// SetProjectMemberRole changes a member's role (mirror of
// enju_promote_member / enju_demote_owner): role="owner" to
// promote, role="member" to demote. changed is false when the
// member already held that role (coord reports a no-op) so the
// UI can say "no change" rather than imply a mutation.
func (s *FatClient) SetProjectMemberRole(ctx context.Context, projectID int64, username, role string) (changed bool, err error) {
	path := fmt.Sprintf("/api/v1/projects/%d/members/by-username/%s/role",
		projectID, url.PathEscape(username))
	data, err := s.coord.Put(ctx, path, map[string]string{"role": role})
	if err != nil {
		return false, err
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) == nil {
		if errMsg, _ := resp["error"].(string); errMsg != "" {
			return false, fmt.Errorf("%s", errMsg)
		}
		if c, ok := resp["changed"].(bool); ok {
			return c, nil
		}
	}
	// No structured `changed` field — coordinator applied it.
	return true, nil
}
