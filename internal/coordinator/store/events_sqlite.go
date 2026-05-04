package store

// SQLite implementation of EventStore.
//
// Lives in its own database file (events.db) with its own
// connection pool, completely independent from the state Store.
// See eventstore.go for the architectural contract.
//
// This file ships the synchronous read API + schema. The async
// writer goroutine + queue + drop policy + observability ride
// in events_writer.go. Until 7b lands, Record() is
// a placeholder that writes synchronously — sufficient for unit
// tests of the schema + reads, but not the production path.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Tunables. Currently const; if hosted-mode metrics show
// these mid-fitting they become operator config in a v2 pass.
//
// queueSize: bounded buffer between the emit-side and the
// writer goroutine. With 1000 slots and a write that takes
// ~1ms on commodity disks, the writer drains ~1000 events/sec
// — far above expected emission rate at v1 scale. Drops only
// kick in when the writer is genuinely stuck (disk-full,
// lock contention, fsync stall).
//
// shutdownDrainTimeout: bounds Close()'s wait on the writer
// goroutine. Prefers losing a few queued events to leaving a
// stuck shutdown — operator can grep logs for the leftover
// count.
//
// dropLogRateLimit: max one drop log per second to keep
// /var/log/* from filling up under sustained pressure. The
// atomic counter still increments on every drop, so total
// loss is observable via Stats() even when individual log
// lines are rate-limited.
//
// statsInterval: 30s window for the aggregate "drops in last
// 30s" log. Silent when no drops happen; loud when degradation
// is in progress.
const (
	eventQueueSize    = 1000
	shutdownDrainTimeout = 5 * time.Second
	dropLogRateLimit   = 1 * time.Second
	statsInterval    = 30 * time.Second
)

// SQLiteEventStore is the v1 backend. Future EventStore impls
// (Postgres, log files, OTLP sink) implement the same interface.
type SQLiteEventStore struct {
	db   *sql.DB
	logger *slog.Logger

	// Kill-switch. Read-mostly; updates via SetEnabled are
	// rare. atomic.Bool keeps the hot Record() path lock-free.
	enabled atomic.Bool

	// closed flips at Close() entry to short-circuit late
	// Record() calls (channel send on a torn-down store
	// would either block or panic depending on order). Read
	// before any queue interaction in Record().
	closed atomic.Bool

	// Observability counters. All monotone since process
	// start. Snapshotted by Stats().
	enqueued atomic.Int64
	persisted atomic.Int64
	dropped  atomic.Int64

	// Queue depth tracks the current in-flight count in the
	// channel. Maintained by Record() (++) and the writer
	// loop (--). Reading is lock-free; the value can be off
	// by one transiently but never drifts unboundedly.
	queueDepth atomic.Int32

	// Async writer plumbing.
	queue chan Event  // bounded buffer; full → drop on send
	done chan struct{} // closed by Close() to signal shutdown
	wg  sync.WaitGroup

	// notifyMu protects notifyCh, the broadcast channel for
	// long-poll consumers ("wake me when any event lands").
	// Pattern: Wait() returns the current channel; Broadcast()
	// (called after each successful persist) closes it and
	// installs a fresh one. Closing fan-outs to all waiters at
	// once; new waiters block on the fresh channel for the next
	// broadcast. No subscription registry, no per-listener state
	// — just a single shared channel rotated on each event.
	//
	// Use case: handleShowEvents long-poll mode (?wait=30s).
	// Caller patterns: subscribe-then-query, so a broadcast that
	// races with the query is observed on the *next* iteration's
	// query, not missed.
	//
	// Scale ceiling (post-v1): broadcast fires on every persist
	// regardless of project, so every long-poller wakes for
	// every event. Spurious wakeups cost (events/sec) ×
	// (active long-pollers) × (one DB query each). Fine while
	// event rate stays bounded by Phase 7's writer throughput;
	// when it doesn't, shard into per-project channels
	// (notifyChByProject map[int64]chan struct{}). Tracked as
	// scale work, not a v1 blocker.
	notifyMu sync.Mutex
	notifyCh chan struct{}

	// Rate-limited drop logging. Prevents log floods under
	// sustained pressure while keeping the first drop visible.
	lastDropLogMu sync.Mutex
	lastDropLog  time.Time

	// per-project monotone seq counters.
	// Initialized at startup from MAX(seq) per project and
	// then advanced atomically inside Record(). Storing
	// *atomic.Int64 in the map (rather than int64) lets us
	// take a stable pointer to each project's counter; the
	// map itself is read-mostly with a sync.Mutex covering
	// the rare add-new-project path.
	//
	// Trade-off vs a DB counter: we lose the durability of
	// "every claimed seq is persisted somewhere" — process
	// crash loses any in-flight seqs that were claimed but
	// not yet persisted. Recovery on restart resyncs to
	// MAX(seq), so post-restart seqs never collide with
	// pre-restart ones, but there's no record of the
	// claimed-and-lost seq numbers either. The audit-detect-
	// drops contract still holds: drops produce gaps; the
	// gap-detector can't always distinguish "queue dropped"
	// from "process crashed mid-write," but Stats().Dropped
	// disambiguates that for live operations.
	seqMu    sync.Mutex
	seqCounters map[int64]*atomic.Int64

	// Close() is idempotent.
	closeOnce sync.Once
}

// EventStoreOption tunes EventStore construction. Use the
// With* helpers below; passing none yields the production
// defaults.
type EventStoreOption func(*eventStoreOptions)

type eventStoreOptions struct {
	queueSize int
}

// WithQueueSize overrides the bounded async-write buffer size.
// Default is eventQueueSize (1000). Operators raise this for
// high-throughput hosted multi-tenant deployments where 1000
// events isn't enough headroom across all tenants combined.
// Values <= 0 fall back to the default.
func WithQueueSize(n int) EventStoreOption {
	return func(o *eventStoreOptions) {
		if n > 0 {
			o.queueSize = n
		}
	}
}

// NewSQLiteEventStore opens (or creates) the events database
// at dbPath, runs the schema migration, and returns a ready
// EventStore. Default state: enabled.
//
// dbPath is typically `<state_dir>/events.db`. Caller is
// responsible for picking the location (operator config) and
// creating parent directories.
//
// The DSN parameters mirror the state Store's choices —
// busy_timeout for writer-vs-writer contention,
// _txlock=immediate for SELECT-then-INSERT atomicity. WAL is
// enabled at startup for concurrent reads.
func NewSQLiteEventStore(dbPath string, logger *slog.Logger, opts ...EventStoreOption) (*SQLiteEventStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	options := eventStoreOptions{queueSize: eventQueueSize}
	for _, fn := range opts {
		fn(&options)
	}
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening events database: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("setting busy_timeout: %w", err)
	}

	s := &SQLiteEventStore{
		db:     db,
		logger:   logger,
		queue:    make(chan Event, options.queueSize),
		done:    make(chan struct{}),
		seqCounters: map[int64]*atomic.Int64{},
		notifyCh:   make(chan struct{}),
	}
	s.enabled.Store(true)
	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("events schema init: %w", err)
	}
	// recover per-project seq counters. New events
	// will start above the highest persisted seq for each
	// project; projects with no events yet bootstrap to 1 on
	// first emit (handled by claimSeq's lazy init).
	if err := s.recoverSeqCounters(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("events seq recovery: %w", err)
	}

	// Spawn the writer + stats goroutines. They drain the
	// queue and emit periodic observability respectively.
	// Both block on s.done at shutdown.
	s.wg.Add(2)
	go s.writerLoop()
	go s.statsLoop()

	return s, nil
}

// recoverSeqCounters reads MAX(seq) per project from
// events and seeds the in-memory counter map.
// Idempotent — safe to call from initSchema() if needed in the
// future, though today only NewSQLiteEventStore invokes it.
//
// Bootstrap semantics: empty events table → empty map. The
// first Record() for an unseen project lazily creates a counter
// starting at 0; claimSeq pre-increments to 1. So a fresh
// install yields seq=1,2,3,... per project as expected.
func (s *SQLiteEventStore) recoverSeqCounters() error {
	rows, err := s.db.Query(
		`SELECT project_id, COALESCE(MAX(seq), 0) FROM events
		 WHERE project_id > 0 GROUP BY project_id`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var projectID, maxSeq int64
		if err := rows.Scan(&projectID, &maxSeq); err != nil {
			return err
		}
		c := &atomic.Int64{}
		c.Store(maxSeq)
		s.seqCounters[projectID] = c
	}
	return rows.Err()
}

// claimSeq atomically claims the next seq for a project. Lazy-
// inits the counter on first use. Returns 0 for project_id <= 0
// (system-attributed events with no project context — they
// bypass the gap-detection invariant since they're not scoped
// to a project anyway).
func (s *SQLiteEventStore) claimSeq(projectID int64) int64 {
	if projectID <= 0 {
		return 0
	}
	// Fast path: counter already exists.
	s.seqMu.Lock()
	c, ok := s.seqCounters[projectID]
	if !ok {
		c = &atomic.Int64{}
		s.seqCounters[projectID] = c
	}
	s.seqMu.Unlock()
	return c.Add(1)
}

// migrate creates the events schema. Idempotent.
//
// The events table mirrors the pre-events shape (so
// existing event_type / event_subtype / metadata semantics
// carry forward) plus a `seq` column for per-project monotone
// sequence numbering — gap-detectable when the async writer
// drops events under load.
//
// chose an in-memory atomic counter (recovered from
// MAX(seq) at startup) over a DB-side counter table — the
// "Record() never blocks" architectural contract trumps the
// added durability of a persisted next-seq. The event_seq_counters
// table from earlier drafts is gone.
// initSchema bootstraps the events.db schema. Same lifecycle
// position + chokepoint reasoning as Store.initSchema: pure DDL
// run at startup, not subject to the chokepoint contract.
func (s *SQLiteEventStore) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		seq INTEGER NOT NULL DEFAULT 0,
		citizen_id INTEGER NOT NULL,
		event_type TEXT NOT NULL,
		event_subtype TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '',
		run_id INTEGER NOT NULL DEFAULT 0,
		project_id INTEGER NOT NULL DEFAULT 0,
		metadata TEXT NOT NULL DEFAULT '{}',
		created_at TIMESTAMP NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_events_citizen
	 ON events(citizen_id);
	CREATE INDEX IF NOT EXISTS idx_events_type
	 ON events(event_type);
	CREATE INDEX IF NOT EXISTS idx_events_run
	 ON events(run_id, seq);
	CREATE INDEX IF NOT EXISTS idx_events_project_seq
	 ON events(project_id, seq);
	`
	_, err := s.db.Exec(schema)
	return err
}

// Close gracefully shuts down the event store: stop accepting
// new events, signal the writer goroutine to drain remaining
// queued events (bounded by shutdownDrainTimeout), wait for
// both goroutines to exit, then close the underlying DB.
// Idempotent — safe to call multiple times. Late Record()
// calls after Close() begins are silent no-ops (observed via
// the closed flag).
func (s *SQLiteEventStore) Close() error {
	var err error
	s.closeOnce.Do(func() {
		// Stop accepting new events FIRST. Any concurrent
		// Record() that already passed the closed check and
		// is mid-channel-send is bounded by the channel's
		// existing capacity; the writer will drain those.
		s.closed.Store(true)
		// Signal both goroutines to exit (writer drains,
		// stats stops).
		close(s.done)
		// Wait for them to finish their drain/exit. wg.Wait
		// is bounded by shutdownDrainTimeout inside the
		// writer's drain loop — Close() itself doesn't time
		// out separately.
		s.wg.Wait()
		err = s.db.Close()
	})
	return err
}

// Enabled reports the current kill-switch state.
func (s *SQLiteEventStore) Enabled() bool {
	return s.enabled.Load()
}

// SetEnabled flips the kill-switch. No backfill on re-enable.
// Document this in the operator runbook.
func (s *SQLiteEventStore) SetEnabled(enabled bool) {
	s.enabled.Store(enabled)
}

// WaitForDrain polls the queue depth at small intervals until
// it reaches zero or the timeout elapses. Used by read paths
// that need read-after-write consistency. Implementation is
// poll-based rather than condition-variable-signaled because
// the writer goroutine doesn't currently signal idleness, and
// adding that machinery for a v1 timing knob would be
// over-engineered. ~5ms granularity is fine: aggregation reads
// are user-facing latency, not hot-path.
func (s *SQLiteEventStore) WaitForDrain(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.queueDepth.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stats returns a runtime observability snapshot.
func (s *SQLiteEventStore) Stats() Stats {
	return Stats{
		Enabled:       s.enabled.Load(),
		Enqueued:      s.enqueued.Load(),
		Persisted:     s.persisted.Load(),
		Dropped:       s.dropped.Load(),
		QueueDepth:    int(s.queueDepth.Load()),
		QueueCapacity: cap(s.queue),
	}
}

// WaitForEvent returns a channel that is closed the next time
// any event is successfully persisted. Used by long-poll
// handlers (?wait= on the events endpoint) to block until new
// events arrive without polling the database.
//
// Caller pattern:
//
//	for {
//	  waitCh := es.WaitForEvent()        // subscribe FIRST
//	  events := query()                  // then check
//	  if len(events) > 0 { return events }
//	  select {
//	  case <-waitCh:                     // woken by new event
//	  case <-ctx.Done():                 // client gone
//	  case <-time.After(remaining):      // timeout
//	  }
//	}
//
// Subscribing before querying closes the missed-event race: if
// a broadcast fires between Wait and query, the query sees the
// committed event; if it fires after, the channel is observed
// closed and the next iteration's query catches it.
//
// All waiters share one channel — broadcast-by-close fans out
// in O(1) regardless of waiter count.
func (s *SQLiteEventStore) WaitForEvent() <-chan struct{} {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifyCh
}

// broadcastNotify wakes all current WaitForEvent subscribers
// (close fans out instantly) and rotates in a fresh channel
// for the next round of waiters. Called from the writer
// goroutine after each successful persist.
func (s *SQLiteEventStore) broadcastNotify() {
	s.notifyMu.Lock()
	close(s.notifyCh)
	s.notifyCh = make(chan struct{})
	s.notifyMu.Unlock()
}

// Record enqueues an event for async persistence by the
// writer goroutine.
//
// Contract (load-bearing for the "events never on critical
// path" architectural claim):
//
//  - Never blocks. Channel send uses a default branch; full
//   queue → drop with rate-limited warning.
//  - Never returns an error. Caller cannot fail because of
//   event-store state.
//  - When disabled (kill-switch) or closed (shutdown), silent
//   no-op. Counters don't increment.
//  - Latency: a single non-blocking channel send (~100ns)
//   plus the atomic counter increment.
func (s *SQLiteEventStore) Record(event Event) {
	if s.closed.Load() {
		return
	}
	if !s.enabled.Load() {
		return
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	// claim seq BEFORE the queue-send decision so
	// dropped events still consume a seq. This is what makes
	// gap detection work: a missing seq in the persisted log
	// means "an event was attempted but didn't make it." The
	// caller-supplied event.Seq is overwritten — Seq is the
	// EventStore's namespace, not the caller's.
	event.Seq = s.claimSeq(event.ProjectID)
	select {
	case s.queue <- event:
		s.enqueued.Add(1)
		s.queueDepth.Add(1)
	default:
		// Queue full. Drop the event, increment the counter,
		// rate-limit the log line. dropped_seq is the seq we
		// just claimed for this event — appears as a gap in
		// GapsInProject, and the log line lets operators
		// correlate "log says we dropped seq=N at time T"
		// with the gap reader's "seq N is missing for
		// project P." For project-less events (seq=0) the
		// field is meaningful only in aggregate.
		s.dropped.Add(1)
		s.logRateLimited("event queue full, dropped",
			"event_type", event.EventType,
			"project_id", event.ProjectID,
			"dropped_seq", event.Seq,
			"queue_depth", s.queueDepth.Load(),
			"dropped_total", s.dropped.Load(),
		)
	}
}

// writerLoop is the single-writer goroutine that drains the
// queue. Persists events one at a time (batching is a v2
// extension — see docs/event-log.md). On shutdown signal,
// drains remaining queued events bounded by
// shutdownDrainTimeout, then exits.
//
// Single-writer keeps event ordering FIFO globally. If we
// later need higher throughput, sharding by run_id (one
// writer per N runs) maintains per-run ordering while
// parallelizing.
func (s *SQLiteEventStore) writerLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			s.drainOnShutdown()
			return
		case event := <-s.queue:
			// Decrement queueDepth only after the event reaches a
			// terminal state (persisted or dropped). WaitForDrain
			// polls queueDepth==0 for read-after-write — if we
			// decremented before persistOne, callers could observe
			// queueDepth==0 with the row still mid-INSERT.
			if err := s.persistOne(event); err != nil {
				s.dropped.Add(1)
				s.queueDepth.Add(-1)
				s.logRateLimited("event persist failed",
					"event_type", event.EventType,
					"error", err,
				)
				continue
			}
			s.persisted.Add(1)
			s.queueDepth.Add(-1)
			s.broadcastNotify()
		}
	}
}

// drainOnShutdown pulls remaining events from the queue,
// bounded by shutdownDrainTimeout. Anything left in the
// queue after the timeout is counted as dropped.
//
// Bias: we'd rather lose late events than block coordinator
// shutdown. Operators see the leftover count in the warning
// log and can correlate with the dropped counter.
func (s *SQLiteEventStore) drainOnShutdown() {
	deadline := time.After(shutdownDrainTimeout)
	for {
		select {
		case event := <-s.queue:
			if err := s.persistOne(event); err != nil {
				s.dropped.Add(1)
			} else {
				s.persisted.Add(1)
			}
			s.queueDepth.Add(-1)
		case <-deadline:
			leftover := len(s.queue)
			if leftover > 0 {
				s.dropped.Add(int64(leftover))
				s.logger.Warn("event store shutdown drain timeout, leftover events dropped",
					"count", leftover,
					"timeout", shutdownDrainTimeout,
				)
			}
			return
		default:
			// Queue empty, all drained.
			return
		}
	}
}

// statsLoop emits a periodic aggregate-drop log line — silent
// when no drops happened in the window, loud when degradation
// is in progress. The atomic counters give operators the
// total-since-start figure; this gives them the recent-window
// figure for trend detection.
func (s *SQLiteEventStore) statsLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()
	var lastDrops int64
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			cur := s.dropped.Load()
			if cur > lastDrops {
				s.logger.Warn("event store dropped events in window",
					"window", statsInterval,
					"delta", cur-lastDrops,
					"total_dropped", cur,
					"queue_depth", s.queueDepth.Load(),
					"persisted_total", s.persisted.Load(),
				)
			}
			lastDrops = cur
		}
	}
}

// logRateLimited writes a warning iff the last call was more
// than dropLogRateLimit ago. Atomic counter still increments
// on every drop regardless — log lines are an operator
// convenience, the counter is the authoritative signal.
func (s *SQLiteEventStore) logRateLimited(msg string, kv ...any) {
	s.lastDropLogMu.Lock()
	now := time.Now()
	shouldLog := now.Sub(s.lastDropLog) >= dropLogRateLimit
	if shouldLog {
		s.lastDropLog = now
	}
	s.lastDropLogMu.Unlock()
	if shouldLog {
		s.logger.Warn(msg, kv...)
	}
}

// timestampLayout pins a fixed-width UTC layout for the
// created_at column. Critical for two reasons:
//
//  1. Default time.Time → SQL conversion via the modernc/sqlite
//     driver renders via time.Time.String() — which embeds
//     monotonic-clock garbage ("m=+0.073...") and the local
//     timezone abbreviation ("CEST"). Lexicographic comparison
//     against a Go-side parameter formatted differently
//     (different TZ, different layout) silently picks the wrong
//     side.
//  2. .999999999 layouts strip trailing zeros — a stored
//     `.387655150` would serialize as `.38765515`, making a
//     re-parse return a different time. Fixed-width .000000000
//     pins all 9 digits.
//
// Format choice: T-separator + Z suffix (UTC). Comparison-stable
// across timezones; sorts identically lexically and temporally.
const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTimestamp normalizes a time.Time for storage and SQL
// parameter binding. UTC + fixed-width nanos → comparison-safe.
func formatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

// persistOne is the actual INSERT — used by both the placeholder
// Record() above and by the writer goroutine.
//
// Seq is currently 0 ( will assign it from
// event_seq_counters). Read-side gap detection lights up only
// once 7k ships.
func (s *SQLiteEventStore) persistOne(event Event) error {
	_, err := s.db.Exec(
		`INSERT INTO events
		 (seq, citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Seq, event.CitizenID, event.EventType, event.EventSubtype,
		event.TaskID, event.RunID, event.ProjectID, event.Metadata, formatTimestamp(event.CreatedAt),
	)
	return err
}

// --- Read API ---

// QueryByRun returns events for a run, ordered by seq.
// Disabled state → ErrEventStoreDisabled (UI translates
// to "audit emission disabled by operator").
//
// Edge case worth flagging: events with project_id=0 get
// seq=0 from claimSeq (system-attributed events bypass the
// per-project sequence). The `ORDER BY seq ASC, id ASC`
// sort floats those rows to the TOP of the timeline —
// before any project-scoped event. Today every emit site
// that sets RunID also resolves ProjectID from the runs
// table, so this is unreachable. If a future code path
// introduces a project-less but run-attributed event, its
// timeline placement will look surprising. Either populate
// project_id at emit time, or change the sort to
// `id ASC` when project_id=0 events become a real case.
func (s *SQLiteEventStore) QueryByRun(ctx context.Context, projectID, runID int64, since time.Time, limit int) ([]Event, error) {
	if !s.enabled.Load() {
		return nil, ErrEventStoreDisabled
	}
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, seq, citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at
		 FROM events
		 WHERE project_id = ? AND run_id = ? AND created_at >= ?
		 ORDER BY seq ASC, id ASC
		 LIMIT ?`,
		projectID, runID, formatTimestamp(since), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// QueryByCitizen returns events for a citizen, ordered by seq.
func (s *SQLiteEventStore) QueryByCitizen(ctx context.Context, citizenID int64, limit int) ([]Event, error) {
	if !s.enabled.Load() {
		return nil, ErrEventStoreDisabled
	}
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, seq, citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at
		 FROM events
		 WHERE citizen_id = ?
		 ORDER BY id DESC
		 LIMIT ?`,
		citizenID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Query is the generic projection backing Store.ListEvents.
// Filter fields are AND-composed; EventTypes is OR-matched.
//
// Ordering: `id DESC` is persistence order, not creation order.
// In v1 these collapse to the same thing because the single-
// writer goroutine drains the queue in FIFO send order and
// SQLite assigns autoincrement ids monotonically — so for any
// pair of events whose CreatedAt is set at emission time,
// persistence order equals creation order.
//
// The invariant breaks only if a producer backdates CreatedAt
// (e.g., feeding a batch of historical events with explicit
// timestamps). No call site does this today; if a future
// consumer needs strict creation-order reads, switch to
// `ORDER BY created_at DESC, id DESC` and accept that the
// created_at column needs an index for sub-linear scans.
func (s *SQLiteEventStore) Query(ctx context.Context, q EventQuery) ([]Event, error) {
	if !s.enabled.Load() {
		return nil, ErrEventStoreDisabled
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	conds := []string{}
	args := []interface{}{}
	if q.ProjectID > 0 {
		conds = append(conds, "project_id = ?")
		args = append(args, q.ProjectID)
	}
	if q.RunID > 0 {
		conds = append(conds, "run_id = ?")
		args = append(args, q.RunID)
	}
	if q.CitizenID > 0 {
		conds = append(conds, "citizen_id = ?")
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
		conds = append(conds, "event_type IN ("+placeholders+")")
	}
	if !q.Since.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, formatTimestamp(q.Since))
	}
	if q.SinceSeq > 0 {
		// Strict `>` (vs Since's `>=`). seq is monotone within
		// (project_id), so this gives clients an exact resume
		// point: passing the last-seen seq returns "everything
		// strictly after," no overlap, no skipped events.
		conds = append(conds, "seq > ?")
		args = append(args, q.SinceSeq)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + conds[0]
		for i := 1; i < len(conds); i++ {
			where += " AND " + conds[i]
		}
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, seq, citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at
		 FROM events
		 `+where+`
		 ORDER BY id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// CountByCitizenAndType returns the (event_type, event_subtype)
// → count map for a citizen. Profile-display query;
// events-only, no state JOIN.
func (s *SQLiteEventStore) CountByCitizenAndType(ctx context.Context, citizenID int64) (map[string]map[string]int, error) {
	if !s.enabled.Load() {
		return nil, ErrEventStoreDisabled
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_type, event_subtype, COUNT(*)
		 FROM events
		 WHERE citizen_id = ?
		 GROUP BY event_type, event_subtype`,
		citizenID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var t, sub string
		var n int
		if err := rows.Scan(&t, &sub, &n); err != nil {
			return nil, err
		}
		if out[t] == nil {
			out[t] = map[string]int{}
		}
		out[t][sub] = n
	}
	return out, rows.Err()
}

// SumTokensForCitizen sums the estimated_tokens metadata
// field across a citizen's events.
func (s *SQLiteEventStore) SumTokensForCitizen(ctx context.Context, citizenID int64) (int64, error) {
	if !s.enabled.Load() {
		return 0, ErrEventStoreDisabled
	}
	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(CAST(json_extract(metadata, '$.estimated_tokens') AS INTEGER)), 0)
		 FROM events
		 WHERE citizen_id = ?
		  AND json_extract(metadata, '$.estimated_tokens') IS NOT NULL`,
		citizenID,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// CountDistinctProjectsForCitizen returns the project count
// across a citizen's events.
func (s *SQLiteEventStore) CountDistinctProjectsForCitizen(ctx context.Context, citizenID int64) (int, error) {
	if !s.enabled.Load() {
		return 0, ErrEventStoreDisabled
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT project_id)
		 FROM events
		 WHERE citizen_id = ? AND project_id > 0`,
		citizenID,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountContributionEvents returns the total event count for
// a citizen.
func (s *SQLiteEventStore) CountContributionEvents(ctx context.Context, citizenID int64) (int, error) {
	if !s.enabled.Load() {
		return 0, ErrEventStoreDisabled
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE citizen_id = ?`,
		citizenID,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountProjectsThisMonth returns the distinct project count
// for a citizen since the given timestamp.
func (s *SQLiteEventStore) CountProjectsThisMonth(ctx context.Context, citizenID int64, since time.Time) (int, error) {
	if !s.enabled.Load() {
		return 0, ErrEventStoreDisabled
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT project_id)
		 FROM events
		 WHERE citizen_id = ? AND project_id > 0 AND created_at >= ?`,
		citizenID, formatTimestamp(since),
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// LatestMetadataForTask returns the metadata of the most
// recent event of a given type for a task. Returns an empty
// string + nil error when no event matches (caller treats
// missing as a soft-fail).
func (s *SQLiteEventStore) LatestMetadataForTask(ctx context.Context, taskID, eventType string) (string, error) {
	if !s.enabled.Load() {
		return "", ErrEventStoreDisabled
	}
	var meta string
	err := s.db.QueryRowContext(ctx,
		`SELECT metadata FROM events
		 WHERE task_id = ? AND event_type = ?
		 ORDER BY id DESC LIMIT 1`,
		taskID, eventType,
	).Scan(&meta)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return meta, nil
}

// DistinctTaskIDsForCitizenAndType returns the distinct task
// IDs a citizen has events of a given type for.
func (s *SQLiteEventStore) DistinctTaskIDsForCitizenAndType(ctx context.Context, citizenID int64, eventType string) ([]string, error) {
	if !s.enabled.Load() {
		return nil, ErrEventStoreDisabled
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT task_id
		 FROM events
		 WHERE citizen_id = ? AND event_type = ?`,
		citizenID, eventType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GapsInProject computes the missing seq numbers for a
// project's persisted event sequence. .
//
// Algorithm: read all persisted seqs ASC, walk the list and
// emit any integer in [1, max] that isn't present. Cheap for
// small N; if a project ever accumulates millions of events
// this'll need a windowed approach (e.g. project-only +
// since-seq query), but at v1 scale a single SELECT is fine.
//
// Edge cases:
//  - Project with zero events → returns nil, nil. No gap by
//   definition.
//  - Project where the highest claimed seq is in-flight (queued
//   but not persisted yet) — that seq shows up as a gap until
//   the writer drains. Callers that want a stable read should
//   WaitForDrain first.
func (s *SQLiteEventStore) GapsInProject(ctx context.Context, projectID int64) ([]int64, error) {
	if !s.enabled.Load() {
		return nil, ErrEventStoreDisabled
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq FROM events
		 WHERE project_id = ? AND seq > 0
		 ORDER BY seq ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[int64]struct{}{}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		have[seq] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reference max: the highest seq the in-memory counter
	// has ever claimed for this project. Using MAX(persisted)
	// would miss end-of-sequence drops — claimed seqs that
	// never made it to disk because the queue was full
	// would simply be invisible (no row to mark MAX above).
	// The in-memory counter remembers the last claimed seq,
	// so gaps at the high end show up correctly.
	//
	// At restart the counter is rebuilt from MAX(persisted),
	// so any pre-restart end-of-sequence drops become
	// permanent invisible gaps. That's the documented trade-
	// off of the in-memory counter design (option 2 in 7k):
	// gap detection is perfect within a single process
	// lifetime; restart resyncs to disk truth.
	var maxClaimed int64
	s.seqMu.Lock()
	if c, ok := s.seqCounters[projectID]; ok {
		maxClaimed = c.Load()
	}
	s.seqMu.Unlock()
	if maxClaimed == 0 && len(have) == 0 {
		return nil, nil
	}
	var gaps []int64
	for i := int64(1); i <= maxClaimed; i++ {
		if _, ok := have[i]; !ok {
			gaps = append(gaps, i)
		}
	}
	return gaps, nil
}

// scanEvents is the shared row-to-Event scanner.
func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(
			&e.ID, &e.Seq, &e.CitizenID, &e.EventType, &e.EventSubtype,
			&e.TaskID, &e.RunID, &e.ProjectID, &e.Metadata, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
