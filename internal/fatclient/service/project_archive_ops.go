package service

// Project archive/restore — the webui side of
// enju_archive_project / enju_restore_project. Same rationale as
// members_ops.go / project_settings_ops.go: the MCP handler
// drives these coord endpoints with raw apiClient calls; webui
// may only import service, so the thin coord wrappers live here.
// 1:1 with the coord HTTP surface — owner-gating, the
// non-terminal-run precondition, and idempotency are all
// enforced coordinator-side.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// ProjectArchiveResult is the coord's archive/restore reply.
// Status is one of: "archived", "restored", "already_archived",
// "already_restored" (idempotent no-ops) — the UI banners the
// difference between a real transition and a no-op.
type ProjectArchiveResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// SetProjectArchived archives (archive=true) or restores
// (archive=false) a project — mirror of enju_archive_project /
// enju_restore_project. The coordinator refuses an archive while
// the project has non-terminal runs (compose with
// enju_terminate_run); that refusal surfaces as the returned
// error so the handler can banner it.
func (s *FatClient) SetProjectArchived(ctx context.Context, projectID int64, archive bool) (*ProjectArchiveResult, error) {
	action := "restore"
	if archive {
		action = "archive"
	}
	data, err := s.coord.Post(ctx,
		fmt.Sprintf("/api/v1/projects/%d/%s", projectID, action),
		map[string]any{})
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var res ProjectArchiveResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("decode archive response: %w", err)
	}
	return &res, nil
}

// ListArchivedProjects returns only the caller's archived
// projects. The coord excludes archived from the default list;
// ?include_archived=true returns active+archived, so we filter
// to Archived here (the archived-projects view wants just those).
func (s *FatClient) ListArchivedProjects(ctx context.Context) ([]wire.Project, error) {
	data, err := s.coord.Get(ctx, "/api/v1/projects?include_archived=true")
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var all []wire.Project
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	out := make([]wire.Project, 0)
	for _, p := range all {
		if p.Archived {
			out = append(out, p)
		}
	}
	return out, nil
}
