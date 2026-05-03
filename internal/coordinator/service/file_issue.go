package service

import (
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// FileIssueParams bundles the new-issue fields. Empty Severity
// is allowed (the store defaults). FoundInRunSeq is project-
// scoped and resolved to the run's global ID; pass 0 to skip.
type FileIssueParams struct {
	ProjectID     int64
	Title         string
	Body          string
	Severity      string
	FoundInRunSeq int
	FoundInTaskID string
}

// FileIssueResponse is the wire shape for enju_file_issue.
type FileIssueResponse struct {
	ID       int64  `json:"id"`
	Seq      int    `json:"seq"`
	Slug     string `json:"slug"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
}

// FileIssue creates a new issue under a project. Membership-
// gated. Title is required. Returns ErrInvalidArgument when
// title is empty or found_in_run_seq names a missing run.
//
// Note: caller (api transport) is responsible for the
// post-create auto-triage hook (evaluateRunStateAndMaybeTriage)
// that re-evaluates completed runs in projects with
// auto_triage. Native MCP transport doesn't run the hook today
// — see the DAG/parsed-run cache extraction TODO.
func FileIssue(s *store.Store, caller *store.CitizenRecord, p FileIssueParams) (*FileIssueResponse, error) {
	if p.Title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidArgument)
	}
	if !CanReadProject(s, p.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	rec := &store.IssueRecord{
		ProjectID:     p.ProjectID,
		Title:         p.Title,
		Body:          p.Body,
		Severity:      p.Severity,
		FoundInTaskID: p.FoundInTaskID,
		FiledBy:       caller.ID,
	}
	if p.FoundInRunSeq > 0 {
		run, err := s.GetRunByProjectSeq(p.ProjectID, p.FoundInRunSeq)
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, fmt.Errorf("%w: found_in_run_seq %d does not exist in project %d", ErrInvalidArgument, p.FoundInRunSeq, p.ProjectID)
		}
		rec.FoundInRunID = run.ID
	}
	id, seq, err := s.CreateIssue(rec)
	if err != nil {
		return nil, err
	}
	return &FileIssueResponse{
		ID:       id,
		Seq:      seq,
		Slug:     fmt.Sprintf("ISSUE-%03d", seq),
		Status:   rec.Status,
		Severity: rec.Severity,
		Title:    rec.Title,
	}, nil
}

// TriageIssue moves an issue to triaged with optional severity
// update. Membership-gated. Returns ErrNotFound when the issue
// doesn't exist.
func TriageIssue(s *store.Store, caller *store.CitizenRecord, projectID int64, seq int, severity string) (*IssueResponse, error) {
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
	if err := s.TriageIssue(it.ID, caller.ID, severity); err != nil {
		return nil, err
	}
	updated, _ := s.GetIssue(it.ID)
	if updated == nil {
		return nil, ErrNotFound
	}
	resp := ToIssueResponse(s, updated)
	return &resp, nil
}

// CloseIssue moves an issue to closed (or wontfix). Optional
// closed_by_task_id links the resolving fix task. Membership-
// gated.
func CloseIssue(s *store.Store, caller *store.CitizenRecord, projectID int64, seq int, status, closedByTaskID string) (*IssueResponse, error) {
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, ErrNotMember
	}
	if status == "" {
		status = store.IssueStatusClosed
	}
	it, err := s.GetIssueBySeq(projectID, seq)
	if err != nil {
		return nil, err
	}
	if it == nil {
		return nil, ErrNotFound
	}
	if err := s.CloseIssue(it.ID, caller.ID, status, closedByTaskID); err != nil {
		return nil, err
	}
	updated, _ := s.GetIssue(it.ID)
	if updated == nil {
		return nil, ErrNotFound
	}
	resp := ToIssueResponse(s, updated)
	return &resp, nil
}

