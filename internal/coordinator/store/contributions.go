package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync/atomic"
	"time"
)

// eventDrainBudget is the per-aggregation-read window we let
// the async event writer drain. Profile/audit reads are user-
// facing; the read-after-write race "submit then immediately
// read your own counters" is rare enough in human time but
// trivial to hit in tests and bots that immediately verify.
// 100ms is generous given the writer drains at ~1000ev/sec —
// covers any normal queue depth without blocking visibly.
//
// Failure mode: if the writer is stalled (disk full, lock
// contention) the drain returns after the budget elapses
// regardless of completion, and the aggregation reads stale
// data. Callers see the result as "best-effort up-to-date":
// counts reflect everything persisted at read time, missing
// only events still in the in-flight queue. The Stats()
// counter (Persisted vs Enqueued) is the authoritative signal
// for monitoring; this knob is the user-experience tradeoff.
// Bumping it past 1s starts being perceptible; lower it under
// 10ms and tests start flaking.
//
// Stored as nanoseconds in an atomic.Int64 so SetEventDrainBudget
// can mutate it at runtime (SIGHUP-driven config reload) without
// a mutex on the read path.
var eventDrainBudgetNs atomic.Int64

func init() {
	eventDrainBudgetNs.Store(int64(100 * time.Millisecond))
}

func eventDrainBudget() time.Duration {
	return time.Duration(eventDrainBudgetNs.Load())
}

// SetEventDrainBudget overrides the read-after-write wait window
// at runtime. Used by the SIGHUP reload path to apply config
// changes without restart. Values <= 0 are ignored.
func SetEventDrainBudget(d time.Duration) {
	if d <= 0 {
		return
	}
	eventDrainBudgetNs.Store(int64(d))
}

// RecordContributionEvent enqueues an event into the
// EventStore (events.db, separate connection pool, async
// writer). this used to write directly to the
// state-DB events table inside the same SQL
// connection — events now live in a strict-consumer
// subsystem that can fail without taking down state.
//
// The function still returns an error for backward
// compatibility with existing call sites that check it,
// but the EventStore.Record contract is "never returns an
// error to caller" — so this always returns nil. Callers
// can drop the error check at their convenience; leaving
// it in place is harmless.
//
// Caveat for callers that were previously relying on the
// in-transaction guarantee (issues.go's "issue_filed
// emitted in same tx as the issue row, can't drift under
// partial failure"): that contract is gone. Events are now
// best-effort observability. A successful state mutation
// followed by an event-emit failure leaves an audit gap,
// detectable future via gaps in the per-project monotone
// seq. State correctness is preserved either way.
func (s *Store) RecordContributionEvent(e *ContributionEvent) error {
	s.Events().Record(Event{
		CitizenID:  e.CitizenID,
		EventType:  e.EventType,
		EventSubtype: e.EventSubtype,
		TaskID:    e.TaskID,
		RunID:    e.RunID,
		ProjectID:  e.ProjectID,
		Metadata:   e.Metadata,
		CreatedAt:  e.CreatedAt,
	})
	return nil
}

// ContributionSummary is a per-citizen aggregate across all
// their contribution events. Used by enju_my_profile to
// render the factual breakdown without a scoring formula.
type ContributionSummary struct {
	TasksCompleted int
	TasksRejected int
	TasksTimedOut int
	TasksReleased int
	ReviewsGiven  int
	ReviewApproves int
	ReviewRejects int
	VotesCast   int
	RunsCreated  int
	TokensTotal  int64
	ProjectCount  int
}

// GetContributionSummary aggregates a citizen's contribution
// events into a display-ready summary. No scoring formula —
// just factual counts.
//
// All three component queries delegate to the EventStore. When
// the kill-switch is engaged the EventStore returns
// ErrEventStoreDisabled; we swallow it and return a zero
// summary + nil so the profile page renders "no contributions"
// instead of an error. Operators flipping the kill-switch
// expect the audit surface to soft-degrade.
func (s *Store) GetContributionSummary(citizenID int64) (*ContributionSummary, error) {
	summary := &ContributionSummary{}
	ctx := context.Background()
	es := s.Events()
	// Read-after-write consistency: a profile fetched
	// immediately after a submit must see that submit's
	// events. Brief wait covers the async-writer hop without
	// blocking the request perceptibly. Callers willing to
	// accept eventual consistency can pre-flush at a higher
	// level; this helper is the safe default.
	es.WaitForDrain(eventDrainBudget())

	counts, err := es.CountByCitizenAndType(ctx, citizenID)
	if err != nil && !errors.Is(err, ErrEventStoreDisabled) {
		return summary, err
	}
	for eventType, subtypes := range counts {
		for subtype, count := range subtypes {
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
				switch ReviewDecision(subtype) {
				case ReviewDecisionApprove:
					summary.ReviewApproves += count
				case ReviewDecisionReject:
					summary.ReviewRejects += count
				}
			case "vote_cast":
				summary.VotesCast += count
			case "run_created":
				summary.RunsCreated += count
			}
		}
	}

	if tokens, err := es.SumTokensForCitizen(ctx, citizenID); err == nil {
		summary.TokensTotal = tokens
	}
	if projects, err := es.CountDistinctProjectsForCitizen(ctx, citizenID); err == nil {
		summary.ProjectCount = projects
	}

	return summary, nil
}

// CountContributionEvents returns the total number of
// events for a citizen (the "Contribution #N" counter).
// Disabled EventStore → 0, nil so callers (profile cards)
// render "0" rather than an error.
func (s *Store) CountContributionEvents(citizenID int64) (int, error) {
	s.Events().WaitForDrain(eventDrainBudget())
	n, err := s.Events().CountContributionEvents(context.Background(), citizenID)
	if errors.Is(err, ErrEventStoreDisabled) {
		return 0, nil
	}
	return n, err
}

// CountProjectsThisMonth returns the distinct project count
// for a citizen since the start of the current calendar month
// (the "X projects this month" display).
func (s *Store) CountProjectsThisMonth(citizenID int64) (int, error) {
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	n, err := s.Events().CountProjectsThisMonth(context.Background(), citizenID, monthStart)
	if errors.Is(err, ErrEventStoreDisabled) {
		return 0, nil
	}
	return n, err
}

// ListTaskHistory returns all task_claims rows for a task,
// ordered by claimed_at. Includes completed, invalidated,
// released, and active claims — the full audit trail.
func (s *Store) ListTaskHistory(taskID string) ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT task_id, citizen_id, claimed_at, deadline, outcome, submitted_at, option, branch, commit_sha, decision
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
		if err := rows.Scan(&r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline, &outcome, &submittedAt, &r.Option, &r.Branch, &r.CommitSHA, &r.Decision); err != nil {
			continue
		}
		r.Outcome = ClaimOutcome(outcome.String)
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
		  tc.submitted_at, tc.option, tc.model,
		  COALESCE(c.username, '') AS username,
		  COALESCE(tc.commit_sha, '') AS commit_sha,
		  COALESCE(tc.decision, '') AS decision,
		  COALESCE(tc.branch, '') AS branch,
		  COALESCE(tc.iter_seq, 0) AS iter_seq
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
		var model sql.NullString
		if err := rows.Scan(
			&r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline,
			&r.Outcome, &submittedAt, &r.Option, &model,
			&r.Username, &r.CommitSHA, &r.ReviewDecision, &r.Branch,
			&r.IterSeq,
		); err != nil {
			continue
		}
		r.Seq = seq
		if submittedAt.Valid {
			t := submittedAt.Time
			r.SubmittedAt = &t
		}
		if model.Valid {
			r.Model = model.String
		}
		out = append(out, r)
	}
	return out, nil
}

// GetTemplateReuseCount returns how many runs were
// instantiated from templates committed by this citizen.
//
// the original was a single events↔runs JOIN. With
// events in a separate DB the correlation moves to two stages:
// fetch this citizen's run_created events from the EventStore,
// then ask the state DB which of those run_ids have a non-empty
// source_path. The "project-wide template adoption" metric is
// state-only and unchanged.
//
// EventStore disabled → authored=0; the project-wide reused
// count still computes from state. Profile renders the
// project metric without misclaiming personal authorship.
//
// Caveats: (1) async-writer drops can cause undercount —
// missing events mean missing run_ids, mean lower authored.
// (2) The QueryByCitizen limit (1000) caps how far back we
// scan; a power user with >1000 lifetime events will see a
// further undercount. Both are acceptable for a profile-page
// metric; replace with a state-only counter (e.g.,
// runs.authored_template_by) if either becomes load-bearing.
func (s *Store) GetTemplateReuseCount(citizenID int64) (authored int, reused int, err error) {
	ctx := context.Background()
	events, qerr := s.Events().QueryByCitizen(ctx, citizenID, 1000)
	if qerr != nil && !errors.Is(qerr, ErrEventStoreDisabled) {
		return 0, 0, nil
	}
	// Dedupe run_ids of run_created events.
	runIDs := map[int64]struct{}{}
	for _, e := range events {
		if e.EventType == "run_created" && e.RunID > 0 {
			runIDs[e.RunID] = struct{}{}
		}
	}
	if len(runIDs) > 0 {
		// Build IN clause.
		ids := make([]interface{}, 0, len(runIDs))
		placeholders := ""
		i := 0
		for id := range runIDs {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += "?"
			ids = append(ids, id)
			i++
		}
		s.db.QueryRow(
			`SELECT COUNT(*) FROM runs WHERE source_path != '' AND id IN (`+placeholders+`)`,
			ids...,
		).Scan(&authored)
	}
	// Project-wide template adoption (state-only).
	s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE source_path != ''`).Scan(&reused)
	return authored, reused, nil
}

// GetEventMetadataForTask returns the metadata JSON from the
// most recent contribution event of a given type for a task.
// Disabled EventStore → "", nil; missing event → "", nil
// (caller treats empty as soft-fail and proceeds).
func (s *Store) GetEventMetadataForTask(taskID, eventType string) (string, error) {
	meta, err := s.Events().LatestMetadataForTask(context.Background(), taskID, eventType)
	if errors.Is(err, ErrEventStoreDisabled) {
		return "", nil
	}
	return meta, err
}

// RunEventRecord is one line of a run's synthesized timeline.
// The fields are chosen to be JSONL-friendly — each becomes
// a key in the exported `enju/runs/{seq}/events/{phase}.jsonl`
// file. Metadata is passed through as a raw JSON string so
// the exporter can embed it without a double-decode round
// trip.
type RunEventRecord struct {
	Seq       int64 // per-project monotone seq, surfaced for client-side cursoring
	Timestamp time.Time
	Type      string
	Subtype   string
	TaskID    string
	Citizen   string
	Metadata  string
}

// ListRunEvents returns the chronological event timeline for
// a run, sorted by timestamp. Reads directly from the
// EventStore — no synthesis, no UNION with task_claims.
//
// Used to fold task_claims rows in here as synthetic
// task_claimed events because the old claim path didn't write
// to the event log. Phase 6c+ emits iteration_started at
// claim time, which carries the same "who claimed when"
// signal plus iter_seq, branch, and reopen flag — strict
// superset. Synthesis would have caused a divergence with
// enju_show_events (which reads the EventStore directly), so
// the cleanup converges both views on iteration_started as
// the canonical claim event.
//
// Disabled EventStore → returns an empty slice + nil error.
// Matches the rest of the read API: audit emission off means
// the run timeline shows nothing, not a 5xx.
func (s *Store) ListRunEvents(projectID, runID int64) ([]RunEventRecord, error) {
	var events []RunEventRecord

	ctx := context.Background()
	rawEvents, err := s.Events().QueryByRun(ctx, projectID, runID, time.Time{}, 0)
	if err != nil {
		if errors.Is(err, ErrEventStoreDisabled) {
			return nil, nil
		}
		return nil, err
	}
	if len(rawEvents) > 0 {
		usernames := s.lookupUsernames(rawEvents)
		for _, e := range rawEvents {
			events = append(events, RunEventRecord{
				Seq:       e.Seq,
				Timestamp: e.CreatedAt,
				Type:      e.EventType,
				Subtype:   e.EventSubtype,
				TaskID:    e.TaskID,
				Citizen:   usernames[e.CitizenID],
				Metadata:  e.Metadata,
			})
		}
	}

	// Sort by timestamp for determinism. EventStore queries
	// return in seq order which is monotonically increasing
	// per project at emission time but can diverge slightly
	// from wall-clock for events emitted across goroutines
	// in flight at once. Stable sort so equal-timestamp events
	// keep their seq order.
	sortRunEvents(events)
	return events, nil
}

// sortRunEvents orders the timeline by timestamp, stable so
// events with identical timestamps keep their original (seq)
// order.
func sortRunEvents(events []RunEventRecord) {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})
}

// EventQuery is a filter for ListEvents — the projection layer
// over events. All fields are optional; Limit caps
// at 1000 to keep responses bounded. At least one of ProjectID
// or RunID should be set in practice, otherwise the query is
// "every event ever recorded" which is rarely what callers want.
type EventQuery struct {
	ProjectID  int64
	RunID      int64
	CitizenID  int64
	EventTypes []string  // OR-matched if non-empty
	Since      time.Time // zero value = no lower bound
	// SinceSeq is a strict-`>` filter on per-project monotone seq.
	// Preferred over Since for streaming clients (notify, replay
	// consumers) — integer comparison eliminates the timestamp
	// edge cases that bit us with `>=` semantics + nanosecond
	// rounding on the wire. Zero = no filter. Composes with
	// Since (both filters AND together when both set, but
	// callers typically pick one).
	SinceSeq int64
	Limit    int // default 100, max 1000
}

// ListEvents returns the projection layer over the
// events table — the read-only counterpart to the
// `enju_export_run_events` git-tracked snapshot. Ordered newest-
// first so log-tailing UX is natural; reverse on the client when
// you want chronological order.
//
// Living-workflow phase 2: the events table is the
// canonical event log (per design notes). This method exposes
// it with filters; the MCP tool `enju_show_events` formats the
// result as JSONL.
func (s *Store) ListEvents(q EventQuery) ([]RunEventRecord, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	raw, err := s.Events().Query(context.Background(), q)
	if err != nil {
		if errors.Is(err, ErrEventStoreDisabled) {
			return nil, nil
		}
		return nil, err
	}
	usernames := s.lookupUsernames(raw)
	out := make([]RunEventRecord, 0, len(raw))
	for _, e := range raw {
		out = append(out, RunEventRecord{
			Seq:       e.Seq,
			Timestamp: e.CreatedAt,
			Type:      e.EventType,
			Subtype:   e.EventSubtype,
			TaskID:    e.TaskID,
			Citizen:   usernames[e.CitizenID],
			Metadata:  e.Metadata,
		})
	}
	return out, nil
}

// lookupUsernames batch-resolves citizen_id → username from
// the state DB for a slice of events. Cross-DB JOIN replacement
// since events live in events.db and citizens in state.db.
// Missing citizens map to "".
//
// Chunked at 500 ids per query to stay well below SQLite's
// SQLITE_MAX_VARIABLE_NUMBER default of 999. ListEvents is
// capped at Limit=1000, so a project-scoped query covering
// many distinct citizens could otherwise blow the parameter
// budget. Chunking keeps the contract simple — N round-trips
// in the worst case, all small.
func (s *Store) lookupUsernames(events []Event) map[int64]string {
	const chunk = 500
	out := map[int64]string{}
	if len(events) == 0 {
		return out
	}
	idSet := map[int64]struct{}{}
	for _, e := range events {
		if e.CitizenID > 0 {
			idSet[e.CitizenID] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return out
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]interface{}, len(batch))
		placeholders := ""
		for i, id := range batch {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += "?"
			args[i] = id
		}
		rows, err := s.db.Query(
			`SELECT id, COALESCE(username, '') FROM citizens WHERE id IN (`+placeholders+`)`,
			args...,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err == nil {
				out[id] = name
			}
		}
		rows.Close()
	}
	return out
}

// GetDownstreamImpact counts how many tasks transitively
// depended on tasks this citizen completed. This is the
// "Your outputs were used by N downstream tasks" metric.
//
// the "tasks completed by citizen" set comes from
// the EventStore; "tasks that depend on those" stays in
// state DB. Disabled EventStore → 0/0/nil (profile renders
// the metric as zero rather than an error).
func (s *Store) GetDownstreamImpact(citizenID int64) (int, int, error) {
	completedIDs, err := s.Events().DistinctTaskIDsForCitizenAndType(
		context.Background(), citizenID, "task_completed",
	)
	if err != nil {
		if errors.Is(err, ErrEventStoreDisabled) {
			return 0, 0, nil
		}
		return 0, 0, err
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
