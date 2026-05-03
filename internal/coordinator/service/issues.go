package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// IssueResponse is the wire shape for one issue. Both
// formatters (format.IssueList + format.IssueDetail) read from
// these JSON keys, so they're load-bearing.
//
// Map-typed in spirit to mirror the legacy api.issueToMap; we
// use a struct so static checking catches typos. Optional
// fields use omitempty so format.IssueDetail's per-key guard
// doesn't have to render "<nil>" for unset values.
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

// ToIssueResponse builds the wire shape from a store record.
// Resolves the run seq + citizen usernames best-effort (a
// hard-deleted run silently drops the field rather than
// blocking the render).
func ToIssueResponse(s *store.Store, it *store.IssueRecord) IssueResponse {
	resp := IssueResponse{
		ID:        fmt.Sprintf("ISSUE-%03d", it.Seq),
		DBID:      it.ID,
		Seq:       it.Seq,
		ProjectID: it.ProjectID,
		Title:     it.Title,
		Body:      it.Body,
		Status:    it.Status,
		Severity:  it.Severity,
		FiledBy:   CitizenUsername(s, it.FiledBy),
		FiledAt:   it.FiledAt.UTC().Format(time.RFC3339),
		UpdatedAt: it.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if it.FoundInRunID > 0 {
		if run, err := s.GetRun(it.FoundInRunID); err == nil && run != nil {
			seq := run.Seq
			resp.FoundInRunSeq = &seq
		}
	}
	if it.FoundInTaskID != "" {
		resp.FoundInTaskID = it.FoundInTaskID
	}
	if it.TriagedBy > 0 {
		resp.TriagedBy = CitizenUsername(s, it.TriagedBy)
	}
	if it.TriagedAt != nil {
		resp.TriagedAt = it.TriagedAt.UTC().Format(time.RFC3339)
	}
	if it.ClosedByTaskID != "" {
		resp.ClosedByTaskID = it.ClosedByTaskID
	}
	if it.ClosedAt != nil {
		resp.ClosedAt = it.ClosedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// IssueListParams bundles the optional filter knobs.
type IssueListParams struct {
	ProjectID int64
	Status    string // CSV; "" = no filter
	Severity  string // CSV; "" = no filter
	Limit     int
}

// ListIssues returns issues in the project matching the
// filters, gated on caller membership.
func ListIssues(s *store.Store, caller *store.CitizenRecord, p IssueListParams) ([]IssueResponse, error) {
	if !CanReadProject(s, p.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	f := store.IssueFilter{ProjectID: p.ProjectID}
	if p.Status != "" {
		f.Status = strings.Split(p.Status, ",")
	}
	if p.Severity != "" {
		f.Severity = strings.Split(p.Severity, ",")
	}
	if p.Limit > 0 {
		f.Limit = p.Limit
	}
	issues, err := s.ListIssues(f)
	if err != nil {
		return nil, err
	}
	out := make([]IssueResponse, 0, len(issues))
	for i := range issues {
		out = append(out, ToIssueResponse(s, &issues[i]))
	}
	return out, nil
}

// GetIssue returns one issue by (project, seq). Membership-
// gated; ErrNotFound when the issue doesn't exist.
func GetIssue(s *store.Store, caller *store.CitizenRecord, projectID int64, seq int) (*IssueResponse, error) {
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, ErrNotMember
	}
	it, err := s.GetIssueBySeq(projectID, seq)
	if err != nil {
		return nil, err
	}
	if it == nil {
		return nil, ErrNotFound
	}
	resp := ToIssueResponse(s, it)
	return &resp, nil
}
