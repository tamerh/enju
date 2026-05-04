package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// Issue status values. Kept in code (not enforced by SQL CHECK) so
// future additions don't require a schema migration. Validated at
// the handler layer.
//
// State transitions:
//
//	open → triaged    (TriageIssue, manual)
//	open → in_progress  (MarkIssueInProgress, auto-triage spawn)
//	open → closed/wontfix (CloseIssue, manual close)
//	triaged → in_progress (MarkIssueInProgress)
//	triaged → closed/wontfix
//	in_progress → closed (auto on linked task accept, or manual)
//	in_progress → wontfix (manual)
const (
	IssueStatusOpen    = "open"
	IssueStatusTriaged  = "triaged"
	IssueStatusInProgress = "in_progress"
	IssueStatusClosed   = "closed"
	IssueStatusWontfix  = "wontfix"
	IssueSeverityLow   = "low"
	IssueSeverityMedium  = "medium"
	IssueSeverityHigh   = "high"
	IssueSeverityCrit   = "critical"
)

// IssueFilter narrows ListIssues. ProjectID is the only required
// field — issues are project-scoped by construction.
type IssueFilter struct {
	ProjectID int64
	Status  []string // OR-matched if non-empty
	Severity []string
	Limit   int
}

// GetIssueBySeq fetches an issue by its (project, seq) pair —
// the way the URL addresses it. Use this for HTTP handlers.
func (s *Store) GetIssueBySeq(projectID int64, seq int) (*IssueRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, project_id, seq, title, body, status, severity, found_in_run_id, found_in_task_id,
		    filed_by, filed_at, triaged_by, triaged_at, closed_by_task_id, closed_at, updated_at
		 FROM issues WHERE project_id = ? AND seq = ?`,
		projectID, seq,
	)
	return scanIssue(row)
}

// GetIssue fetches by global DB id.
func (s *Store) GetIssue(id int64) (*IssueRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, project_id, seq, title, body, status, severity, found_in_run_id, found_in_task_id,
		    filed_by, filed_at, triaged_by, triaged_at, closed_by_task_id, closed_at, updated_at
		 FROM issues WHERE id = ?`,
		id,
	)
	return scanIssue(row)
}

func scanIssue(row *sql.Row) (*IssueRecord, error) {
	var r IssueRecord
	var triagedAt, closedAt sql.NullTime
	err := row.Scan(
		&r.ID, &r.ProjectID, &r.Seq, &r.Title, &r.Body, &r.Status, &r.Severity,
		&r.FoundInRunID, &r.FoundInTaskID, &r.FiledBy, &r.FiledAt,
		&r.TriagedBy, &triagedAt, &r.ClosedByTaskID, &closedAt, &r.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if triagedAt.Valid {
		t := triagedAt.Time
		r.TriagedAt = &t
	}
	if closedAt.Valid {
		t := closedAt.Time
		r.ClosedAt = &t
	}
	return &r, nil
}

// ListIssues returns issues matching the filter, newest-first.
// Status / severity slices are OR-matched; an empty filter pair
// means "any value." Limit defaults to 100, capped at 1000 to
// keep responses bounded.
func (s *Store) ListIssues(f IssueFilter) ([]IssueRecord, error) {
	if f.ProjectID == 0 {
		return nil, fmt.Errorf("project_id is required")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	conds := []string{"project_id = ?"}
	args := []interface{}{f.ProjectID}
	if len(f.Status) > 0 {
		conds = append(conds, "status IN ("+placeholders(len(f.Status))+")")
		for _, st := range f.Status {
			args = append(args, st)
		}
	}
	if len(f.Severity) > 0 {
		conds = append(conds, "severity IN ("+placeholders(len(f.Severity))+")")
		for _, sv := range f.Severity {
			args = append(args, sv)
		}
	}

	args = append(args, limit)
	rows, err := s.db.Query(
		`SELECT id, project_id, seq, title, body, status, severity, found_in_run_id, found_in_task_id,
		    filed_by, filed_at, triaged_by, triaged_at, closed_by_task_id, closed_at, updated_at
		 FROM issues
		 WHERE `+strings.Join(conds, " AND ")+`
		 ORDER BY filed_at DESC, id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []IssueRecord
	for rows.Next() {
		var r IssueRecord
		var triagedAt, closedAt sql.NullTime
		if err := rows.Scan(
			&r.ID, &r.ProjectID, &r.Seq, &r.Title, &r.Body, &r.Status, &r.Severity,
			&r.FoundInRunID, &r.FoundInTaskID, &r.FiledBy, &r.FiledAt,
			&r.TriagedBy, &triagedAt, &r.ClosedByTaskID, &closedAt, &r.UpdatedAt,
		); err != nil {
			continue
		}
		if triagedAt.Valid {
			t := triagedAt.Time
			r.TriagedAt = &t
		}
		if closedAt.Valid {
			t := closedAt.Time
			r.ClosedAt = &t
		}
		out = append(out, r)
	}
	return out, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := "?"
	for i := 1; i < n; i++ {
		out += ", ?"
	}
	return out
}

// FindOldestOpenIssue returns the lowest-seq issue in `open`
// status for the project, or nil if none exists. Used by the
// auto-triage hook to pick the next issue to fix. Issues in
// triaged or in_progress are excluded (already in flight or
// pending operator decision); explicit ordering by seq gives
// the deterministic "oldest first" behavior the design notes
// promise.
func (s *Store) FindOldestOpenIssue(projectID int64) (*IssueRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, project_id, seq, title, body, status, severity, found_in_run_id, found_in_task_id,
		    filed_by, filed_at, triaged_by, triaged_at, closed_by_task_id, closed_at, updated_at
		 FROM issues
		 WHERE project_id = ? AND status = 'open'
		 ORDER BY seq ASC LIMIT 1`,
		projectID,
	)
	return scanIssue(row)
}

