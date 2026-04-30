package store

import (
	"database/sql"
	"sort"
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
		`SELECT task_id, citizen_id, claimed_at, deadline, outcome, submitted_at, option, content, branch, commit_sha, decision
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
		if err := rows.Scan(&r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline, &outcome, &submittedAt, &r.Option, &r.Content, &r.Branch, &r.CommitSHA, &r.Decision); err != nil {
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

// ListTaskIterations returns the iteration history of a task —
// the living-workflow phase 5 projection over task_claims +
// tasks + citizens. Ordered by claimed_at ascending; Seq is
// 1-based.
//
// One row per task_claims row. A future iteration model that
// keeps multiple submissions inside one claim (request_changes
// staying on the same claim) would change Submissions from
// scalar to list; the wire shape leaves the door open by
// surfacing CommitSHA as a single field today.
//
// CommitSHA is the task-level CommitSHA at the moment this
// iteration's outcome landed. For active iterations (no
// outcome yet) it's the task's current CommitSHA, which may
// be empty. ReviewDecision is similarly the task-level
// decision; in single-citizen review tasks every iteration's
// decision overwrites; in multi-citizen tasks the per-claim
// option/content fields carry the per-citizen contribution.
func (s *Store) ListTaskIterations(taskID string) ([]IterationRecord, error) {
	// commit_sha / decision / branch are read from task_claims
	// directly, not joined from tasks — the per-claim columns
	// preserve historical fidelity (iter-1's commit doesn't
	// vanish when iter-2 starts and clears the task-level
	// fields). The tasks join is dropped entirely.
	rows, err := s.db.Query(
		`SELECT
		   tc.task_id, tc.citizen_id, tc.claimed_at, tc.deadline,
		   COALESCE(tc.outcome, '') AS outcome,
		   tc.submitted_at, tc.option, tc.content, tc.model_id,
		   COALESCE(c.username, '') AS username,
		   COALESCE(tc.commit_sha, '') AS commit_sha,
		   COALESCE(tc.decision, '') AS decision,
		   COALESCE(tc.branch, '') AS branch
		 FROM task_claims tc
		 LEFT JOIN citizens c ON tc.citizen_id = c.id
		 WHERE tc.task_id = ?
		 ORDER BY tc.claimed_at ASC, tc.id ASC`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []IterationRecord
	seq := 0
	for rows.Next() {
		seq++
		var r IterationRecord
		var submittedAt sql.NullTime
		var modelID sql.NullInt64
		if err := rows.Scan(
			&r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline,
			&r.Outcome, &submittedAt, &r.Option, &r.Content, &modelID,
			&r.Username, &r.CommitSHA, &r.ReviewDecision, &r.Branch,
		); err != nil {
			continue
		}
		r.Seq = seq
		if submittedAt.Valid {
			t := submittedAt.Time
			r.SubmittedAt = &t
		}
		if modelID.Valid {
			id := modelID.Int64
			r.ModelID = &id
		}
		out = append(out, r)
	}
	return out, nil
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

// RunEventRecord is one line of a run's synthesized timeline.
// The fields are chosen to be JSONL-friendly — each becomes
// a key in the exported `enju/runs/{seq}/events/{phase}.jsonl`
// file. Metadata is passed through as a raw JSON string so
// the exporter can embed it without a double-decode round
// trip.
type RunEventRecord struct {
	Timestamp time.Time
	Type      string
	Subtype   string
	TaskID    string
	Citizen   string
	Metadata  string
}

// ListRunEvents synthesizes a chronological timeline for a
// run by unioning two sources:
//
//  1. contribution_events scoped to this run — already a
//     typed event log (task_completed, review_given,
//     vote_cast, task_failed, task_invalidated, run_created).
//  2. task_claims for this run's tasks — synthesized into
//     task_claimed events since the claim path doesn't
//     write contribution_events today.
//
// Result is sorted by timestamp. Caller (the export tool)
// formats each record as a single JSONL line. Matches the
// "git is the ledger" pattern: no ambient file writes,
// authoritative data lives in the DB, snapshot materializes
// to git on demand.
func (s *Store) ListRunEvents(runID int64) ([]RunEventRecord, error) {
	var events []RunEventRecord

	// Source 1: typed events already in contribution_events.
	ceRows, err := s.db.Query(
		`SELECT ce.created_at, ce.event_type, ce.event_subtype, ce.task_id, ce.metadata,
		        COALESCE(c.username, '') AS citizen
		 FROM contribution_events ce
		 LEFT JOIN citizens c ON ce.citizen_id = c.id
		 WHERE ce.run_id = ?
		 ORDER BY ce.created_at ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	for ceRows.Next() {
		var r RunEventRecord
		var metadata sql.NullString
		if err := ceRows.Scan(&r.Timestamp, &r.Type, &r.Subtype, &r.TaskID, &metadata, &r.Citizen); err != nil {
			continue
		}
		r.Metadata = metadata.String
		events = append(events, r)
	}
	ceRows.Close()

	// Source 2: claims synthesized as task_claimed events.
	// JOIN to citizens for the username, and to tasks to
	// scope by run_id. Outcome carries the follow-up event
	// (completed / invalidated / released / timed_out) so
	// we only emit the *claimed* moment — the resolution
	// moment is already in contribution_events.
	clRows, err := s.db.Query(
		`SELECT tc.claimed_at, tc.task_id, COALESCE(c.username, '') AS citizen
		 FROM task_claims tc
		 JOIN tasks t ON tc.task_id = t.id
		 LEFT JOIN citizens c ON tc.citizen_id = c.id
		 WHERE t.run_id = ?
		 ORDER BY tc.claimed_at ASC`,
		runID,
	)
	if err != nil {
		return events, nil // best-effort; degrade rather than fail the whole export
	}
	for clRows.Next() {
		var r RunEventRecord
		if err := clRows.Scan(&r.Timestamp, &r.TaskID, &r.Citizen); err != nil {
			continue
		}
		r.Type = "task_claimed"
		events = append(events, r)
	}
	clRows.Close()

	// Merge-sort by timestamp. Small N (per-run scope), so
	// a single sort.Slice is cheaper than maintaining two
	// cursors during the scan.
	sortRunEvents(events)
	return events, nil
}

// sortRunEvents orders the merged timeline by timestamp,
// stable so events with identical timestamps keep their
// source-order (contribution events before synthesized
// claim events, matching the intuitive "result recorded
// moments before a new claim" reading when they collide).
func sortRunEvents(events []RunEventRecord) {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})
}

// EventQuery is a filter for ListEvents — the projection layer
// over contribution_events. All fields are optional; Limit caps
// at 1000 to keep responses bounded. At least one of ProjectID
// or RunID should be set in practice, otherwise the query is
// "every event ever recorded" which is rarely what callers want.
type EventQuery struct {
	ProjectID  int64
	RunID      int64
	CitizenID  int64
	EventTypes []string  // OR-matched if non-empty
	Since      time.Time // zero value = no lower bound
	Limit      int       // default 100, max 1000
}

// ListEvents returns the projection layer over the
// contribution_events table — the read-only counterpart to the
// `enju_export_run_events` git-tracked snapshot. Ordered newest-
// first so log-tailing UX is natural; reverse on the client when
// you want chronological order.
//
// Living-workflow phase 2: the contribution_events table is the
// canonical event log (per design notes). This method exposes
// it with filters; the MCP tool `enju_show_events` formats the
// result as JSONL.
func (s *Store) ListEvents(q EventQuery) ([]RunEventRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	conds := []string{}
	args := []interface{}{}
	if q.ProjectID > 0 {
		conds = append(conds, "ce.project_id = ?")
		args = append(args, q.ProjectID)
	}
	if q.RunID > 0 {
		conds = append(conds, "ce.run_id = ?")
		args = append(args, q.RunID)
	}
	if q.CitizenID > 0 {
		conds = append(conds, "ce.citizen_id = ?")
		args = append(args, q.CitizenID)
	}
	if len(q.EventTypes) > 0 {
		placeholders := ""
		for i := range q.EventTypes {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += "?"
			args = append(args, q.EventTypes[i])
		}
		conds = append(conds, "ce.event_type IN ("+placeholders+")")
	}
	if !q.Since.IsZero() {
		conds = append(conds, "ce.created_at >= ?")
		args = append(args, q.Since)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + conds[0]
		for i := 1; i < len(conds); i++ {
			where += " AND " + conds[i]
		}
	}

	args = append(args, limit)
	rows, err := s.db.Query(
		`SELECT ce.created_at, ce.event_type, ce.event_subtype, ce.task_id, ce.metadata,
		        COALESCE(c.username, '') AS citizen
		 FROM contribution_events ce
		 LEFT JOIN citizens c ON ce.citizen_id = c.id
		 `+where+`
		 ORDER BY ce.created_at DESC, ce.id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []RunEventRecord
	for rows.Next() {
		var r RunEventRecord
		var metadata sql.NullString
		if err := rows.Scan(&r.Timestamp, &r.Type, &r.Subtype, &r.TaskID, &metadata, &r.Citizen); err != nil {
			continue
		}
		r.Metadata = metadata.String
		events = append(events, r)
	}
	return events, nil
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
