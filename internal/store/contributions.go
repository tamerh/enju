package store

import (
	"database/sql"
	"time"
)

// RecordContributionEvent inserts an append-only event into
// the contribution log. Events are never deleted — even if
// the underlying task is invalidated, the original event
// stays and the invalidation gets its own event.
func (s *Store) RecordContributionEvent(e *ContributionEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.CitizenID, e.EventType, e.EventSubtype, e.TaskID, e.RunID, e.ProjectID, e.Metadata, e.CreatedAt,
	)
	return err
}

// ContributionSummary is a per-citizen aggregate across all
// their contribution events. Used by enju_my_profile to
// render the factual breakdown without a scoring formula.
type ContributionSummary struct {
	TasksCompleted int
	TasksRejected  int
	TasksTimedOut  int
	TasksReleased  int
	ReviewsGiven   int
	ReviewApproves int
	ReviewRejects  int
	VotesCast      int
	RunsCreated    int
	TokensTotal    int64
	ProjectCount   int
}

// GetContributionSummary aggregates a citizen's contribution
// events into a display-ready summary. No scoring formula —
// just factual counts.
func (s *Store) GetContributionSummary(citizenID int64) (*ContributionSummary, error) {
	summary := &ContributionSummary{}

	rows, err := s.db.Query(
		`SELECT event_type, event_subtype, COUNT(*) FROM contribution_events WHERE citizen_id = ? GROUP BY event_type, event_subtype`,
		citizenID,
	)
	if err != nil {
		return summary, err
	}
	defer rows.Close()

	for rows.Next() {
		var eventType, subtype string
		var count int
		if err := rows.Scan(&eventType, &subtype, &count); err != nil {
			continue
		}
		switch eventType {
		case "task_completed":
			summary.TasksCompleted += count
		case "task_rejected":
			summary.TasksRejected += count
		case "task_timed_out":
			summary.TasksTimedOut += count
		case "task_released":
			summary.TasksReleased += count
		case "review_given":
			summary.ReviewsGiven += count
			if subtype == "approve" {
				summary.ReviewApproves += count
			} else if subtype == "reject" {
				summary.ReviewRejects += count
			}
		case "vote_cast":
			summary.VotesCast += count
		case "run_created":
			summary.RunsCreated += count
		}
	}

	// Estimated tokens from metadata (prompt + content chars / 4).
	var estimatedTokens int64
	err = s.db.QueryRow(
		`SELECT COALESCE(SUM(CAST(json_extract(metadata, '$.estimated_tokens') AS INTEGER)), 0) FROM contribution_events WHERE citizen_id = ? AND json_extract(metadata, '$.estimated_tokens') IS NOT NULL`,
		citizenID,
	).Scan(&estimatedTokens)
	if err == nil {
		summary.TokensTotal = estimatedTokens
	}

	// Distinct project count.
	var projectCount int
	err = s.db.QueryRow(
		`SELECT COUNT(DISTINCT project_id) FROM contribution_events WHERE citizen_id = ? AND project_id > 0`,
		citizenID,
	).Scan(&projectCount)
	if err == nil {
		summary.ProjectCount = projectCount
	}

	return summary, nil
}

// CountContributionEvents returns the total number of
// events for a citizen (the "Contribution #N" counter).
func (s *Store) CountContributionEvents(citizenID int64) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM contribution_events WHERE citizen_id = ?`,
		citizenID,
	).Scan(&count)
	return count, err
}

// CountContributionEventsThisMonth returns the distinct
// project count for a citizen in the current calendar
// month (the "X projects this month" display).
func (s *Store) CountProjectsThisMonth(citizenID int64) (int, error) {
	var count int
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT project_id) FROM contribution_events WHERE citizen_id = ? AND project_id > 0 AND created_at >= ?`,
		citizenID, monthStart,
	).Scan(&count)
	return count, err
}

// ListTaskHistory returns all task_claims rows for a task,
// ordered by claimed_at. Includes completed, invalidated,
// released, and active claims — the full audit trail.
func (s *Store) ListTaskHistory(taskID string) ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT task_id, citizen_id, claimed_at, deadline, outcome, submitted_at, option, content
		 FROM task_claims WHERE task_id = ? ORDER BY claimed_at ASC`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []TaskClaimRecord
	for rows.Next() {
		var r TaskClaimRecord
		var outcome sql.NullString
		var submittedAt sql.NullTime
		if err := rows.Scan(&r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline, &outcome, &submittedAt, &r.Option, &r.Content); err != nil {
			continue
		}
		r.Outcome = outcome.String
		if submittedAt.Valid {
			t := submittedAt.Time
			r.SubmittedAt = &t
		}
		records = append(records, r)
	}
	return records, nil
}

// GetTemplateReuseCount returns how many runs were
// instantiated from templates committed by this citizen.
// Uses runs.source_path + contribution_events to correlate:
// the citizen authored the template file that other runs
// instantiate. For v1 simplicity, we count all runs with
// a non-empty source_path that match a template path this
// citizen has associated events for. This is approximate
// but useful for the profile display.
func (s *Store) GetTemplateReuseCount(citizenID int64) (authored int, reused int, err error) {
	// Count runs created by this citizen that used a template.
	err = s.db.QueryRow(
		`SELECT COUNT(DISTINCT e.task_id) FROM contribution_events e
		 JOIN runs r ON e.run_id = r.id
		 WHERE e.citizen_id = ? AND e.event_type = 'run_created' AND r.source_path != ''`,
		citizenID,
	).Scan(&authored)
	if err != nil {
		return 0, 0, nil
	}
	// Count total runs that used any template (project-wide
	// template adoption metric).
	s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE source_path != ''`).Scan(&reused)
	return authored, reused, nil
}

// GetEventMetadataForTask returns the metadata JSON from the
// most recent contribution event of a given type for a task.
func (s *Store) GetEventMetadataForTask(taskID, eventType string) (string, error) {
	var metadata string
	err := s.db.QueryRow(
		`SELECT metadata FROM contribution_events WHERE task_id = ? AND event_type = ? ORDER BY created_at DESC LIMIT 1`,
		taskID, eventType,
	).Scan(&metadata)
	if err != nil {
		return "", err
	}
	return metadata, nil
}

// GetDownstreamImpact counts how many tasks transitively
// depended on tasks this citizen completed. This is the
// "Your outputs were used by N downstream tasks" metric.
func (s *Store) GetDownstreamImpact(citizenID int64) (int, int, error) {
	// Find all task IDs this citizen completed.
	rows, err := s.db.Query(
		`SELECT DISTINCT task_id FROM contribution_events WHERE citizen_id = ? AND event_type = 'task_completed'`,
		citizenID,
	)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	var completedIDs []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		completedIDs = append(completedIDs, id)
	}
	if len(completedIDs) == 0 {
		return 0, 0, nil
	}

	// Count tasks whose depends_on contains any of these IDs.
	downstreamCount := 0
	projectSet := map[int64]bool{}
	for _, cid := range completedIDs {
		dRows, err := s.db.Query(
			`SELECT id, run_id FROM tasks WHERE depends_on LIKE ? AND id != ?`,
			"%"+cid+"%", cid,
		)
		if err != nil {
			continue
		}
		for dRows.Next() {
			var tid string
			var runID int64
			dRows.Scan(&tid, &runID)
			downstreamCount++
			// Get project_id from run.
			var pid int64
			s.db.QueryRow(`SELECT project_id FROM runs WHERE id = ?`, runID).Scan(&pid)
			if pid > 0 {
				projectSet[pid] = true
			}
		}
		dRows.Close()
	}

	return downstreamCount, len(projectSet), nil
}
