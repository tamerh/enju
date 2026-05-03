package service

// Template-discovery orchestration. enju_list_templates and
// enju_describe_template are pure client-side reads (the
// coordinator doesn't know about templates beyond a run's
// source_path provenance column), but each opens the project
// clone and runs a best-effort pull so templates pushed by
// other citizens since the last update show up. Bodies are
// nearly identical — extracted here so handlers stay one-liners
// and any future change to the open+pull dance has one home.

import (
	"context"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// ListTemplates opens the project clone, best-effort pulls,
// and returns the list of template entries. A failed pull is
// logged at Debug and the scan proceeds against whatever's on
// disk — the user still gets a menu, and the error surfaces
// on the next branch-touching tool call if it's load-bearing.
func (s *Session) ListTemplates(ctx context.Context, projectID int64) ([]workspace.TemplateSummary, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("enju_list_templates requires a local workspace (MCP client mode)")
	}
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	proj.Lock()
	if perr := proj.Pull(); perr != nil {
		s.logger.Debug("list_templates pull failed, scanning local state", "err", perr)
	}
	proj.Unlock()
	return proj.ListTemplates()
}

// DescribeTemplate opens the project clone, best-effort pulls,
// and loads one template by path.
func (s *Session) DescribeTemplate(ctx context.Context, projectID int64, templatePath string) (*workspace.LoadedTemplate, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("enju_describe_template requires a local workspace (MCP client mode)")
	}
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	proj.Lock()
	if perr := proj.Pull(); perr != nil {
		s.logger.Debug("describe_template pull failed, reading local state", "err", perr)
	}
	proj.Unlock()
	return proj.LoadTemplate(templatePath)
}
