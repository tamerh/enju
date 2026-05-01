package store

// smoke tests for the EventStore interface +
// SQLiteEventStore backend. Covers:
//
//  - Open/close lifecycle on a fresh file.
//  - Record + QueryByRun round-trip.
//  - Kill-switch: SetEnabled(false) makes Record a no-op
//   and reads return ErrEventStoreDisabled.
//  - Stats counters increment correctly.
//  - noopEventStore returned by Store.Events() before
//   AttachEventStore is called.
//
// adds tests for the async writer (queue depth, drop
// on full queue, graceful shutdown). adds tests for
// the per-project monotone seq + gap detection.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// waitForPersisted polls until the EventStore's persisted
// counter reaches `want`, or fails the test after `budget`.
// Async writer means tests can't read-immediately-after-write
// without waiting for the writer goroutine to drain. Polling
// (rather than sleeping a fixed duration) keeps tests fast
// when the writer is keeping up and gives them a clean
// failure mode when something's stuck.
func waitForPersisted(t *testing.T, es *SQLiteEventStore, want int64, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if es.Stats().Persisted >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waited %v for persisted >= %d, got stats %+v", budget, want, es.Stats())
}

func TestEventStoreOpenCloseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	es, err := NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer es.Close()

	if !es.Enabled() {
		t.Fatal("default state should be enabled")
	}

	now := time.Now()
	es.Record(Event{
		CitizenID: 42,
		EventType: "task_completed",
		EventSubtype: "answer",
		TaskID:  "1:1:dev",
		RunID:   1,
		ProjectID: 1,
		Metadata: `{"estimated_tokens": 100}`,
		CreatedAt: now,
	})

	// Async writer: wait for the event to land before reading.
	waitForPersisted(t, es, 1, time.Second)

	got, err := es.QueryByRun(context.Background(), 1, time.Time{}, 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].EventType != "task_completed" || got[0].EventSubtype != "answer" {
		t.Errorf("event fields wrong: %+v", got[0])
	}

	// Stats reflects the write.
	st := es.Stats()
	if st.Enqueued != 1 || st.Persisted != 1 || st.Dropped != 0 {
		t.Errorf("stats: enqueued=%d persisted=%d dropped=%d (want 1/1/0)",
			st.Enqueued, st.Persisted, st.Dropped)
	}
}

func TestEventStoreKillSwitch(t *testing.T) {
	dir := t.TempDir()
	es, err := NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer es.Close()

	// Disable.
	es.SetEnabled(false)
	if es.Enabled() {
		t.Fatal("kill-switch should be off")
	}

	// Record while disabled — silent no-op.
	es.Record(Event{CitizenID: 1, EventType: "task_completed", RunID: 1, ProjectID: 1})

	// Counter doesn't increment when disabled.
	if got := es.Stats().Enqueued; got != 0 {
		t.Errorf("disabled Record should not increment enqueued, got %d", got)
	}

	// Reads return the sentinel error.
	_, err = es.QueryByRun(context.Background(), 1, time.Time{}, 10)
	if !errors.Is(err, ErrEventStoreDisabled) {
		t.Errorf("expected ErrEventStoreDisabled, got %v", err)
	}

	// Re-enable; new writes flow.
	es.SetEnabled(true)
	es.Record(Event{CitizenID: 1, EventType: "task_completed", RunID: 1, ProjectID: 1, CreatedAt: time.Now()})

	waitForPersisted(t, es, 1, time.Second)

	got, err := es.QueryByRun(context.Background(), 1, time.Time{}, 10)
	if err != nil {
		t.Fatalf("query after re-enable: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 event after re-enable (no backfill of disabled-window writes), got %d", len(got))
	}
}

func TestStoreEventsReturnsNoopBeforeAttach(t *testing.T) {
	// Store with no EventStore attached should hand back a
	// no-op that's safe to call. The shared newTestStore
	// helper attaches a real store (so most tests can read
	// events back), so this test bypasses it and constructs
	// the bare Store directly.
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	es := s.Events()
	if es == nil {
		t.Fatal("Events() should never return nil")
	}
	if es.Enabled() {
		t.Error("noop store should report disabled")
	}
	// Record on a noop store is a silent drop; verify it
	// doesn't panic and doesn't error.
	es.Record(Event{CitizenID: 1, EventType: "task_completed"})

	// Reads return ErrEventStoreDisabled — same shape as
	// the operator-killed real store.
	_, err = es.QueryByRun(context.Background(), 1, time.Time{}, 10)
	if !errors.Is(err, ErrEventStoreDisabled) {
		t.Errorf("expected ErrEventStoreDisabled from noop store, got %v", err)
	}
}

// TestEventStoreDropsOnFullQueue exercises the back-pressure
// contract: when the writer can't keep up and the queue is
// saturated, Record() drops the event (atomic counter
// increments, log warning fires) instead of blocking the
// caller. Constructed by holding the SQLite write lock from
// outside the store so the writer goroutine stalls, then
// flooding Record() until the channel is full.
func TestEventStoreDropsOnFullQueue(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")
	es, err := NewSQLiteEventStore(dbPath, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer es.Close()

	// Stall the writer by holding a transaction on the
	// underlying DB connection. While this is held, the
	// writer goroutine's INSERT will block on the writer
	// lock and the queue will fill behind it.
	tx, err := es.db.Begin()
	if err != nil {
		t.Fatalf("begin stall tx: %v", err)
	}
	// Force the tx to acquire the write lock immediately
	// (matches our IMMEDIATE _txlock setting in production).
	// removed the event_seq_counters table — using
	// a no-op INSERT into events to grab the
	// writer lock instead. Rollback at the end means the row
	// never lands.
	if _, err := tx.Exec(
		`INSERT INTO events (citizen_id, event_type, created_at)
		 VALUES (0, 'stall_holder', ?)`,
		time.Now(),
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed stall tx: %v", err)
	}
	// Don't commit yet — keep the writer blocked.

	// Flood the queue with more events than its capacity.
	// queueSize+50 ensures we overflow even with ~10 events
	// drained while the writer was acquiring the lock.
	flood := eventQueueSize + 50
	for i := 0; i < flood; i++ {
		es.Record(Event{
			CitizenID: 1, EventType: "task_completed", RunID: 1, ProjectID: 1,
			CreatedAt: time.Now(),
		})
	}

	// Some events queued, some dropped. Drops must be > 0
	// because we sent more than queueSize while the writer
	// was stuck.
	stats := es.Stats()
	if stats.Dropped == 0 {
		t.Errorf("expected drops > 0 when flooding past queueSize while writer stalled, got stats %+v", stats)
	}
	// Cumulative invariant: every event we tried to Record
	// is accounted for (enqueued + dropped). Persisted may
	// be 0-N depending on whether the writer drained any
	// before the stall.
	if int(stats.Enqueued+stats.Dropped) < flood {
		t.Errorf("counter accounting drift: enqueued=%d dropped=%d (sum %d) < flood %d",
			stats.Enqueued, stats.Dropped, stats.Enqueued+stats.Dropped, flood)
	}

	// Release the stall so the test can clean up cleanly.
	_ = tx.Rollback()
}

// TestEventStoreGracefulShutdownDrainsQueue verifies that
// Close() drains queued events to disk before the underlying
// DB is closed (bounded by shutdownDrainTimeout). Without
// this drain, late events queued just before shutdown would
// be silently lost.
func TestEventStoreGracefulShutdownDrainsQueue(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")
	es, err := NewSQLiteEventStore(dbPath, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const N = 50
	for i := 0; i < N; i++ {
		es.Record(Event{
			CitizenID: 1, EventType: "task_completed", RunID: 1, ProjectID: 1,
			CreatedAt: time.Now(),
		})
	}
	// Close immediately — writer must drain the queue on its
	// way out. wg.Wait() inside Close() blocks until the
	// drain goroutine exits.
	if err := es.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen and read — all N events should be on disk.
	es2, err := NewSQLiteEventStore(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer es2.Close()
	got, err := es2.QueryByRun(context.Background(), 1, time.Time{}, 1000)
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if len(got) != N {
		t.Errorf("graceful shutdown drained %d events, expected %d", len(got), N)
	}
}

// TestEventStoreLateRecordAfterCloseIsNoOp verifies the
// closed-flag short-circuit. A Record() call that arrives
// after Close() begins must not panic (channel send on a
// closed channel) and must not increment counters.
func TestEventStoreLateRecordAfterCloseIsNoOp(t *testing.T) {
	dir := t.TempDir()
	es, err := NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// One pre-close write to establish a baseline.
	es.Record(Event{CitizenID: 1, EventType: "test", RunID: 1, CreatedAt: time.Now()})
	waitForPersisted(t, es, 1, time.Second)
	baselinePersisted := es.Stats().Persisted

	if err := es.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Late Records are silent no-ops. No panic, no
	// counter movement.
	for i := 0; i < 10; i++ {
		es.Record(Event{CitizenID: 1, EventType: "test", RunID: 1, CreatedAt: time.Now()})
	}
	st := es.Stats()
	if st.Persisted != baselinePersisted {
		t.Errorf("persisted moved after close: baseline=%d now=%d", baselinePersisted, st.Persisted)
	}
	if st.Enqueued != 1 {
		t.Errorf("enqueued moved after close: expected baseline 1, got %d", st.Enqueued)
	}
}

// TestEventStoreCloseIsIdempotent verifies that calling
// Close() multiple times doesn't panic (closed channel,
// double-close DB) — defensive for shutdown-path code that
// may have multiple sites unsure about ownership.
// TestEventStoreOrderingFIFO pins the FIFO-emission-order
// guarantee. formalized this in the schema: per-
// project seq is claimed atomically at Record() entry, so
// emission order is observable as seq ASC regardless of any
// future writer-goroutine sharding. The dual id ASC + seq ASC
// assertions are mutually-redundant TODAY (single writer +
// monotone seq) but become independent contracts under any
// future write-path concurrency: id stops matching emission
// order if writers shard, but seq still tracks Record() entry
// order per project.
func TestEventStoreOrderingFIFO(t *testing.T) {
	dir := t.TempDir()
	es, err := NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer es.Close()

	const N = 20
	for i := 0; i < N; i++ {
		es.Record(Event{
			CitizenID: 1, EventType: "task_completed", RunID: 1, ProjectID: 1,
			TaskID:  fmt.Sprintf("task-%02d", i),
			CreatedAt: time.Now(),
		})
	}
	waitForPersisted(t, es, N, time.Second)

	got, err := es.QueryByRun(context.Background(), 1, time.Time{}, N+10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != N {
		t.Fatalf("got %d rows, want %d", len(got), N)
	}
	for i, ev := range got {
		want := fmt.Sprintf("task-%02d", i)
		if ev.TaskID != want {
			t.Errorf("row %d: TaskID=%q, want %q (FIFO emission order broken)", i, ev.TaskID, want)
		}
	}

	// every persisted row should have seq > 0
	// (project_id=1 in this test, so claimSeq returns 1..N).
	// Each successive row's seq must be strictly greater than
	// the prior row's; combined with the FIFO TaskID assertion
	// above, this proves seq tracks Record() entry order.
	var prevSeq int64
	for i, ev := range got {
		if ev.Seq <= 0 {
			t.Errorf("row %d: seq=%d, expected > 0 (per-project monotone seq is future contract)", i, ev.Seq)
		}
		if ev.Seq <= prevSeq {
			t.Errorf("row %d: seq=%d not strictly greater than prior seq=%d", i, ev.Seq, prevSeq)
		}
		prevSeq = ev.Seq
	}
}

// TestEventStoreSeqMonotonePerProject verifies the per-
// project counter is independent across projects. Two
// projects emitting concurrently each get their own 1..N
// sequence — interleaving doesn't conflate them. .
func TestEventStoreSeqMonotonePerProject(t *testing.T) {
	dir := t.TempDir()
	es, err := NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer es.Close()

	// Interleave emissions: P1, P2, P1, P2, P1, P2.
	for i := 0; i < 3; i++ {
		es.Record(Event{ProjectID: 1, EventType: "x", CreatedAt: time.Now()})
		es.Record(Event{ProjectID: 2, EventType: "y", CreatedAt: time.Now()})
	}
	waitForPersisted(t, es, 6, time.Second)

	got1, _ := es.QueryByRun(context.Background(), 0, time.Time{}, 100)
	_ = got1 // run-scoped query returns []; we want project queries

	// Use Query (generic) to filter by project.
	p1, err := es.Query(context.Background(), EventQuery{ProjectID: 1, Limit: 100})
	if err != nil {
		t.Fatalf("Query P1: %v", err)
	}
	p2, err := es.Query(context.Background(), EventQuery{ProjectID: 2, Limit: 100})
	if err != nil {
		t.Fatalf("Query P2: %v", err)
	}
	if len(p1) != 3 || len(p2) != 3 {
		t.Fatalf("expected 3 events per project, got P1=%d P2=%d", len(p1), len(p2))
	}
	// Each project should have seqs {1,2,3} regardless of
	// interleaving order. id DESC means latest first → seq 3
	// should be first.
	for projectName, events := range map[string][]Event{"P1": p1, "P2": p2} {
		seqs := []int64{events[0].Seq, events[1].Seq, events[2].Seq}
		want := []int64{3, 2, 1}
		for i, s := range seqs {
			if s != want[i] {
				t.Errorf("%s seq[%d]=%d, want %d (each project should have its own 1..N)", projectName, i, s, want[i])
			}
		}
	}
}

// TestEventStoreGapsInProjectDetectsDrops verifies that drops
// produce a detectable gap. Force drops by stalling the writer,
// then assert GapsInProject returns the missing seq numbers.
func TestEventStoreGapsInProjectDetectsDrops(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")
	es, err := NewSQLiteEventStore(dbPath, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer es.Close()

	// Emit a few events normally.
	for i := 0; i < 3; i++ {
		es.Record(Event{ProjectID: 1, EventType: "x", CreatedAt: time.Now()})
	}
	waitForPersisted(t, es, 3, time.Second)
	// Should have no gaps yet: seqs 1,2,3 all present.
	gaps, err := es.GapsInProject(context.Background(), 1)
	if err != nil {
		t.Fatalf("GapsInProject: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("expected no gaps after clean emission, got %v", gaps)
	}

	// Stall the writer by holding the write lock, then flood.
	tx, err := es.db.Begin()
	if err != nil {
		t.Fatalf("begin stall: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO events (citizen_id, event_type, created_at) VALUES (0, 'stall', ?)`,
		time.Now(),
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed stall: %v", err)
	}
	flood := eventQueueSize + 50
	for i := 0; i < flood; i++ {
		es.Record(Event{ProjectID: 1, EventType: "x", CreatedAt: time.Now()})
	}
	if es.Stats().Dropped == 0 {
		_ = tx.Rollback()
		t.Fatal("expected drops under stall + flood")
	}
	_ = tx.Rollback()

	// Drain whatever the writer eventually persists.
	es.WaitForDrain(2 * time.Second)
	gaps, err = es.GapsInProject(context.Background(), 1)
	if err != nil {
		t.Fatalf("GapsInProject after drops: %v", err)
	}
	if len(gaps) == 0 {
		t.Fatal("expected GapsInProject to return non-empty after forced drops (audit-detectable signal)")
	}
}

// TestEventStoreSeqRecoveryAfterRestart verifies that closing
// and reopening the store rebuilds the seq counter from
// MAX(seq), so post-restart seqs don't collide with persisted
// ones.
func TestEventStoreSeqRecoveryAfterRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")

	es1, err := NewSQLiteEventStore(dbPath, nil)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	for i := 0; i < 5; i++ {
		es1.Record(Event{ProjectID: 1, EventType: "x", CreatedAt: time.Now()})
	}
	waitForPersisted(t, es1, 5, time.Second)
	if err := es1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	// Reopen — seq counter should resume at 6, not reset to 1.
	es2, err := NewSQLiteEventStore(dbPath, nil)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer es2.Close()
	es2.Record(Event{ProjectID: 1, EventType: "x", CreatedAt: time.Now()})
	// Per-store Persisted counter starts at 0 — wait for 1
	// (this store's contribution), not 6 (cumulative). Then
	// query and assert the seq.
	waitForPersisted(t, es2, 1, time.Second)

	got, err := es2.Query(context.Background(), EventQuery{ProjectID: 1, Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Latest event (id DESC first) should have seq=6.
	if len(got) == 0 || got[0].Seq != 6 {
		t.Fatalf("expected post-restart seq=6, got %d (events: %d)",
			func() int64 {
				if len(got) > 0 {
					return got[0].Seq
				}
				return 0
			}(), len(got))
	}
}

func TestEventStoreCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	es, err := NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := es.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := es.Close(); err != nil {
		t.Errorf("second close should be no-op, got %v", err)
	}
}

func TestEventStoreAttachWiresThrough(t *testing.T) {
	// AttachEventStore + Events() returns the real store.
	s := newTestStore(t)
	dir := t.TempDir()
	real, err := NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer real.Close()
	s.AttachEventStore(real)

	es := s.Events()
	if !es.Enabled() {
		t.Error("after Attach, Events() should return the enabled real store")
	}
	es.Record(Event{
		CitizenID: 99, EventType: "test", RunID: 1,
		CreatedAt: time.Now(),
	})
	waitForPersisted(t, real, 1, time.Second)
	got, err := es.QueryByRun(context.Background(), 1, time.Time{}, 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
}
