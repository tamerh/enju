package service

// Issue read/write methods consumed by the web UI's issues
// surface. Wraps the existing coord HTTP endpoints
// (POST/GET /api/v1/projects/{pid}/issues, GET/POST per-issue
// triage and close). No coord-side changes; pure HTTP
// pass-through.
//
// Wire shapes mirror coord/service/issues.go +
// coord/service/file_issue.go — kept local to fatclient/service
// so webui can import them through the published FatClient
// surface without crossing the coord/fatclient boundary.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// IssueResponse is one issue's wire shape. Matches the coord
// /issues/{seq} response field-for-field. Optional fields use
// pointer/empty conventions so the formatter / template can
// `{{if}}` cleanly.
type IssueResponse struct {
	ID             string `json:"id"`
	DBID           int64  `json:"db_id"`
	Seq            int    `json:"seq"`
	ProjectID      int64  `json:"project_id"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Status         string `json:"status"`
	Severity       string `json:"severity"`
	FiledBy        string `json:"filed_by"`
	FiledAt        string `json:"filed_at"`
	UpdatedAt      string `json:"updated_at"`
	FoundInRunSeq  *int   `json:"found_in_run_seq,omitempty"`
	FoundInTaskID  string `json:"found_in_task_id,omitempty"`
	TriagedBy      string `json:"triaged_by,omitempty"`
	TriagedAt      string `json:"triaged_at,omitempty"`
	ClosedByTaskID string `json:"closed_by_task_id,omitempty"`
	ClosedAt       string `json:"closed_at,omitempty"`
}

// FileIssueResponse is the wire shape returned from POST
// /issues. Lean on purpose — the caller usually re-reads
// detail to render the full record.
type FileIssueResponse struct {
	ID       int64  `json:"id"`
	Seq      int    `json:"seq"`
	Slug     string `json:"slug"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
}

// FileIssueParams bundles the new-issue fields. Title required;
// the rest are optional — coord defaults severity ("medium"
// today), found_in_run_seq=0 means "no run linked." Caller
// is responsible for caps/trim on prose; coord doesn't reject
// long bodies but the audit log gets noisy.
type FileIssueParams struct {
	Title         string
	Body          string
	Severity      string
	FoundInRunSeq int
	FoundInTaskID string
}

// IssueListOpts mirrors the coord query string. Empty values
// drop the filter. Status / Severity are CSV ("open,triaged"
// etc.) — coord parses them server-side.
type IssueListOpts struct {
	Status   string
	Severity string
	Limit    int
}

// FileIssue creates a new issue under projectID. Title is
// required. Returns the lean response shape; caller re-reads
// via GetIssue to render the full record.
func (s *FatClient) FileIssue(ctx context.Context, projectID int64, params FileIssueParams) (*FileIssueResponse, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("project_id is required")
	}
	if params.Title == "" {
		return nil, fmt.Errorf("title is required")
	}
	body := map[string]interface{}{
		"title": params.Title,
	}
	if params.Body != "" {
		body["body"] = params.Body
	}
	if params.Severity != "" {
		body["severity"] = params.Severity
	}
	if params.FoundInRunSeq > 0 {
		body["found_in_run_seq"] = params.FoundInRunSeq
	}
	if params.FoundInTaskID != "" {
		body["found_in_task_id"] = params.FoundInTaskID
	}
	data, err := s.coord.Post(ctx, fmt.Sprintf("/api/v1/projects/%d/issues", projectID), body)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out FileIssueResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode file_issue: %w", err)
	}
	return &out, nil
}

// ListIssues returns issues for a project with optional
// status/severity/limit filters. Empty slice when no matches —
// not an error.
func (s *FatClient) ListIssues(ctx context.Context, projectID int64, opts IssueListOpts) ([]IssueResponse, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("project_id is required")
	}
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Severity != "" {
		q.Set("severity", opts.Severity)
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	path := fmt.Sprintf("/api/v1/projects/%d/issues", projectID)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	data, err := s.coord.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out []IssueResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode issues: %w", err)
	}
	return out, nil
}

// GetIssue fetches one issue by (project, seq).
func (s *FatClient) GetIssue(ctx context.Context, projectID int64, seq int) (*IssueResponse, error) {
	if projectID <= 0 || seq <= 0 {
		return nil, fmt.Errorf("project_id and seq are required")
	}
	data, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/issues/%d", projectID, seq))
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out IssueResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode issue: %w", err)
	}
	return &out, nil
}

// TriageIssue updates an issue's severity (and marks it
// triaged). Empty severity keeps the current severity but still
// records the triage event.
func (s *FatClient) TriageIssue(ctx context.Context, projectID int64, seq int, severity string) (*IssueResponse, error) {
	body := map[string]string{}
	if severity != "" {
		body["severity"] = severity
	}
	data, err := s.coord.Post(ctx, fmt.Sprintf("/api/v1/projects/%d/issues/%d/triage", projectID, seq), body)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out IssueResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode triage: %w", err)
	}
	return &out, nil
}

// CloseIssue closes an issue. status defaults to "closed" when
// empty; "wontfix" is the other valid terminal status.
// closedByTaskID optionally links the resolving fix task.
func (s *FatClient) CloseIssue(ctx context.Context, projectID int64, seq int, status, closedByTaskID string) (*IssueResponse, error) {
	body := map[string]string{}
	if status != "" {
		body["status"] = status
	}
	if closedByTaskID != "" {
		body["closed_by_task_id"] = closedByTaskID
	}
	data, err := s.coord.Post(ctx, fmt.Sprintf("/api/v1/projects/%d/issues/%d/close", projectID, seq), body)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out IssueResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode close: %w", err)
	}
	return &out, nil
}

// errorMsg pulls a top-level "error" field out of a coord
// response if present. Returns "" otherwise. Matches the
// pattern used in run_ops.runStateAction.
func errorMsg(data []byte) string {
	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if msg, ok := result["error"].(string); ok && msg != "" {
			return msg
		}
	}
	return ""
}
