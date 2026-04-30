package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Issue status values. Kept in code (not enforced by SQL CHECK) so
// future additions don't require a schema migration. Validated at
// the handler layer.
//
// State transitions:
//
//	open → triaged       (TriageIssue, manual)
//	open → in_progress   (MarkIssueInProgress, auto-triage spawn)
//	open → closed/wontfix (CloseIssue, manual close)
//	triaged → in_progress (MarkIssueInProgress)
//	triaged → closed/wontfix
//	in_progress → closed (auto on linked task accept, or manual)
//	in_progress → wontfix (manual)
const (
	IssueStatusOpen       = "open"
	IssueStatusTriaged    = "triaged"
	IssueStatusInProgress = "in_progress"
	IssueStatusClosed     = "closed"
	IssueStatusWontfix    = "wontfix"
	IssueSeverityLow      = "low"
	IssueSeverityMedium   = "medium"
	IssueSeverityHigh     = "high"
	IssueSeverityCrit     = "critical"
)

// IssueFilter narrows ListIssues. ProjectID is the only required
// field — issues are project-scoped by construction.
type IssueFilter struct {
	ProjectID int64
	Status    []string // OR-matched if non-empty
	Severity  []string
	Limit     int
}

// CreateIssue inserts a new issue and returns its DB id + the
// per-project seq (the ISSUE-NNN counter). Atomic via the same
// MAX(seq)+1-in-tx pattern CreateRun uses; the UNIQUE
// (project_id, seq) constraint catches racing creates and
// surfaces them as an error the caller can retry.
//
// Defaults: status=open, severity=medium if either is empty. The
// caller passes filer (citizen_id), found_in_run_id, and
// found_in_task_id; downstream-facing fields like triaged_by /
// closed_by_task_id are populated by TriageIssue / CloseIssue.
//
// Emits issue_filed in the same transaction as the INSERT so
// the event log can't drift from issue state under partial
// failure. Matches the SpawnTask / applyCompleteRun pattern.
func (s *Store) CreateIssue(rec *IssueRecord) (int64, int, error) {
	if rec.ProjectID == 0 {
		return 0, 0, fmt.Errorf("project_id is required")
	}
	if rec.Title == "" {
		return 0, 0, fmt.Errorf("title is required")
	}
	if rec.FiledBy == 0 {
		return 0, 0, fmt.Errorf("filed_by (citizen id) is required")
	}
	if rec.Status == "" {
		rec.Status = IssueStatusOpen
	}
	if rec.Severity == "" {
		rec.Severity = IssueSeverityMedium
	}
	now := time.Now()
	if rec.FiledAt.IsZero() {
		rec.FiledAt = now
	}
	rec.UpdatedAt = now

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM issues WHERE project_id = ?`, rec.ProjectID).Scan(&maxSeq); err != nil {
		return 0, 0, err
	}
	rec.Seq = int(maxSeq.Int64) + 1

	res, err := tx.Exec(
		`INSERT INTO issues (project_id, seq, title, body, status, severity, found_in_run_id, found_in_task_id, filed_by, filed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ProjectID, rec.Seq, rec.Title, rec.Body, rec.Status, rec.Severity,
		rec.FoundInRunID, rec.FoundInTaskID, rec.FiledBy, rec.FiledAt, rec.UpdatedAt,
	)
	if err != nil {
		return 0, 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	// Emit issue_filed in the same tx so the event log can't
	// drift from the issue row under partial failure. Severity
	// rides on event_subtype so filters like
	// `event_types=issue_filed` can further narrow by severity
	// without metadata parsing.
	if _, err := tx.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, 'issue_filed', ?, ?, ?, ?, ?, ?)`,
		rec.FiledBy, rec.Severity, rec.FoundInTaskID, rec.FoundInRunID, rec.ProjectID,
		fmt.Sprintf(`{"issue_seq":%d,"title":%q,"severity":%q}`, rec.Seq, rec.Title, rec.Severity),
		rec.FiledAt,
	); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	rec.ID = id
	return id, rec.Seq, nil
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

// TriageIssue moves an open issue into "triaged" state and
// optionally updates severity. Refuses on non-open issues so a
// closed/wontfix issue can't be revived without an explicit
// re-open path (deferred). citizenID attributes the triage; pass
// 0 for system / automated triage. Emits issue_triaged in the
// same tx as the UPDATE.
func (s *Store) TriageIssue(issueID, citizenID int64, severity string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now()
	q := `UPDATE issues
	      SET status = 'triaged', triaged_by = ?, triaged_at = ?, updated_at = ?`
	args := []interface{}{citizenID, now, now}
	if severity != "" {
		q += `, severity = ?`
		args = append(args, severity)
	}
	q += ` WHERE id = ? AND status = 'open'`
	args = append(args, issueID)

	res, err := tx.Exec(q, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("issue %d cannot be triaged (already triaged/closed/wontfix or not found)", issueID)
	}

	// Re-read the updated row so the event metadata reflects
	// the post-triage severity (whether it was overridden or
	// kept). Single SELECT inside the tx — cheap.
	var seq int
	var newSeverity string
	var projectID int64
	if err := tx.QueryRow(
		`SELECT project_id, seq, severity FROM issues WHERE id = ?`, issueID,
	).Scan(&projectID, &seq, &newSeverity); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, 'issue_triaged', ?, '', 0, ?, ?, ?)`,
		citizenID, newSeverity, projectID,
		fmt.Sprintf(`{"issue_seq":%d,"severity":%q}`, seq, newSeverity),
		now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkIssueInProgress transitions open|triaged → in_progress and
// links the issue to the task that's working on resolving it.
// Used by the auto-triage hook when an idle run spawns a fix
// task for an open issue. Refuses on terminal states. Emits
// issue_in_progress in the same tx as the UPDATE.
//
// citizenID = 0 marks system-initiated transitions (auto-triage
// is the only caller in v1; future manual paths can pass a real
// citizen).
func (s *Store) MarkIssueInProgress(issueID, citizenID int64, fixTaskID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now()
	res, err := tx.Exec(
		`UPDATE issues
		 SET status = 'in_progress', closed_by_task_id = ?, updated_at = ?
		 WHERE id = ? AND status IN ('open', 'triaged')`,
		fixTaskID, now, issueID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("issue %d cannot move to in_progress (already terminal/in_progress or not found)", issueID)
	}

	var seq int
	var projectID int64
	if err := tx.QueryRow(
		`SELECT project_id, seq FROM issues WHERE id = ?`, issueID,
	).Scan(&projectID, &seq); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, 'issue_in_progress', '', ?, 0, ?, ?, ?)`,
		citizenID, fixTaskID, projectID,
		fmt.Sprintf(`{"issue_seq":%d,"fix_task_id":%q}`, seq, fixTaskID),
		now,
	); err != nil {
		return err
	}
	return tx.Commit()
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

// CloseIssue moves an issue into terminal status. status must be
// "closed" or "wontfix". closedByTaskID is the optional fix-task
// reference — empty when closing without a fix (e.g. duplicate,
// wontfix, manually-resolved). Refuses if already terminal.
// Emits issue_closed in the same tx as the UPDATE.
func (s *Store) CloseIssue(issueID, citizenID int64, status, closedByTaskID string) error {
	if status != IssueStatusClosed && status != IssueStatusWontfix {
		return fmt.Errorf("close status must be 'closed' or 'wontfix', got %q", status)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now()
	// In_progress is included in the allowed-from set so the
	// auto-triage close-on-accept path can transition
	// in_progress → closed without going through a separate
	// "promote to closable" step.
	res, err := tx.Exec(
		`UPDATE issues
		 SET status = ?, closed_by_task_id = ?, closed_at = ?, updated_at = ?
		 WHERE id = ? AND status IN ('open', 'triaged', 'in_progress')`,
		status, closedByTaskID, now, now, issueID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("issue %d cannot be closed (already terminal or not found)", issueID)
	}

	var seq int
	var projectID int64
	if err := tx.QueryRow(
		`SELECT project_id, seq FROM issues WHERE id = ?`, issueID,
	).Scan(&projectID, &seq); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, 'issue_closed', ?, ?, 0, ?, ?, ?)`,
		citizenID, status, closedByTaskID, projectID,
		fmt.Sprintf(`{"issue_seq":%d,"status":%q,"closed_by_task_id":%q}`, seq, status, closedByTaskID),
		now,
	); err != nil {
		return err
	}
	return tx.Commit()
}
