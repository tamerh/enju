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
	"encoding/json"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// Type aliases re-export the workspace template shapes through
// the service surface. Lets in-process consumers (web UI) use
// service.TemplateSummary without importing workspace —
// keeps the published FatClient API self-contained at the
// fatclient/service boundary.
type TemplateSummary = enjugit.TemplateSummary
type ParamSummary = enjugit.ParamSummary
type LoadedTemplate = enjugit.LoadedTemplate

// ListTemplates opens the project clone, best-effort pulls,
// and returns the list of template entries. A failed pull is
// logged at Debug and the scan proceeds against whatever's on
// disk — the user still gets a menu, and the error surfaces
// on the next branch-touching tool call if it's load-bearing.
func (s *FatClient) ListTemplates(ctx context.Context, projectID int64) ([]TemplateSummary, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("enju_list_templates requires a local workspace (MCP client mode)")
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if perr := wf.PullBranch(""); perr != nil {
		s.logger.Debug("list_templates pull failed, scanning local state", "err", perr)
	}
	return wf.ListTemplates()
}

// DescribeTemplate opens the project clone, best-effort pulls,
// and loads one template by path.
func (s *FatClient) DescribeTemplate(ctx context.Context, projectID int64, templatePath string) (*LoadedTemplate, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("enju_describe_template requires a local workspace (MCP client mode)")
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if perr := wf.PullBranch(""); perr != nil {
		s.logger.Debug("describe_template pull failed, reading local state", "err", perr)
	}
	return wf.LoadTemplate(templatePath)
}

// CreateRunFromTemplateResult bundles the coord response body
// and any non-fatal snapshot warning.
//
// CoordResponse is the raw bytes from POST /runs — handlers can
// JSON-decode for `seq`, `name`, `branch`, etc. We don't define
// a typed struct here because the run-create response is
// already exposed as `wire.Run` shape elsewhere; keep this
// generic to avoid a dual-type-maintenance burden.
//
// SnapshotWarning is non-empty when the run was created on the
// coordinator but the post-create template-bundle snapshot
// failed (e.g. push refused on the run branch). The run is
// usable — just lacks the per-run frozen template copy.
type CreateRunFromTemplateResult struct {
	CoordResponse   []byte
	SnapshotWarning string
}

// CreateRunFromTemplate creates a run by snapshotting a
// template bundle. Mirrors the body of
// mcphandlers.handleCreateRun for in-process consumers (web UI).
//
// Steps, all best-effort except the coord POST:
//
//  1. PrepareRunTemplate — open clone, pull, load bundle,
//     compute slug, pin to default branch
//  2. POST /api/v1/projects/{pid}/runs with the loaded YAML +
//     params + source_path + source_commit_sha
//  3. CommitRunTemplateSnapshot — freeze a copy of the bundle
//     into enju/runs/{seq}-{slug}/template-snapshot/ (failures
//     here surface as SnapshotWarning, not error)
//  4. TouchProject — bump the workspace cache stamp so other
//     surfaces see the new run on next read
//
// authorName / authorEmail come from the caller's commit
// identity (typically Session.CommitAuthor).
func (s *FatClient) CreateRunFromTemplate(ctx context.Context, projectID int64, templatePath string, params map[string]interface{}, branch, authorName, authorEmail string) (*CreateRunFromTemplateResult, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("project_id is required")
	}
	if templatePath == "" {
		return nil, fmt.Errorf("template_path is required")
	}

	prep, err := s.PrepareRunTemplate(ctx, projectID, templatePath, authorName, authorEmail)
	if err != nil {
		return nil, err
	}

	body := map[string]interface{}{
		"yaml":              prep.YAMLContent,
		"username":          s.coord.Username(),
		"source_path":       templatePath,
		"source_commit_sha": prep.SourceCommit,
	}
	if len(params) > 0 {
		body["params"] = params
	}
	if branch != "" {
		body["branch"] = branch
	}

	data, err := s.coord.Post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs", projectID), body)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}

	// Best-effort snapshot — the run is already created on the
	// coord; errors here surface as a warning, not a fatal.
	snapshotWarning := s.CommitRunTemplateSnapshot(prep, data, templatePath, authorName, authorEmail)

	s.TouchProject(projectID)

	return &CreateRunFromTemplateResult{
		CoordResponse:   data,
		SnapshotWarning: snapshotWarning,
	}, nil
}

// runSeqFromCreateResponse extracts the per-project run seq
// from a /runs POST response. Returns 0 on parse failure (the
// caller decides whether to fall back to a list page or surface
// a generic "created" message). Helper exposed for the web UI
// which needs to redirect to /p/{pid}/r/{seq} on success.
func RunSeqFromCreateResponse(data []byte) int {
	var r map[string]interface{}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0
	}
	if seqF, ok := r["seq"].(float64); ok {
		return int(seqF)
	}
	return 0
}
