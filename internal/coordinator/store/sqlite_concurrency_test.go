package store

// Concurrency-stress tests for the SQLite store. Targets the
// parallel enju_execute_run path where N goroutines hit
// write-mutating operations (set_claim, etc.) simultaneously.
//
// Why a separate file: these tests need a FILE-backed DB.
// :memory: databases don't expose pool contention because each
// new connection would see a different empty in-memory DB —
// modernc.org/sqlite serializes them onto a single connection
// internally, hiding any pool-wide PRAGMA issues. File DBs
// reflect production reality.

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBusyTimeoutAppliesToAllPoolConnections is the foundation
// probe for the SQLITE_BUSY-under-parallel-claims bug. The
// fix at sqlite.go:84 puts busy_timeout in the DSN
// (?_pragma=busy_timeout(5000)) so every new pool connection
// inherits it. This test verifies that's actually what happens.
//
// Strategy: open a file-backed Store, hold one connection busy
// (force the pool to allocate a second), then query
// `PRAGMA busy_timeout` on the second connection. If the DSN
// is honored it returns 5000; if not it returns 0 (default).
//
// Without this test, a subtle DSN-syntax regression (driver
// upgrade, typo, URL-encoding bug) would silently degrade
// concurrent writes to fail-fast SQLITE_BUSY mode and only
// surface under live parallel load — exactly the bug the user
// hit in production.
func TestBusyTimeoutAppliesToAllPoolConnections(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()

	// Force at least 2 concurrent connections so the pool
	// has to allocate a second one. database/sql allocates
	// lazily; without this, the test could pass against a
	// 1-connection pool that happens to have run the
	// startup PRAGMA.
	s.db.SetMaxOpenConns(4)
	s.db.SetMaxIdleConns(0) // discourage reuse → force fresh conns

	const probes = 4
	results := make(chan int64, probes)
	errs := make(chan error, probes)
	var wg sync.WaitGroup
	for i := 0; i < probes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Hold each connection for a moment so the next
			// goroutine forces a NEW pool connection rather
			// than reusing this one.
			conn, err := s.db.Conn(t.Context())
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			var timeout int64
			row := conn.QueryRowContext(t.Context(), "PRAGMA busy_timeout")
			if err := row.Scan(&timeout); err != nil {
				errs <- err
				return
			}
			time.Sleep(20 * time.Millisecond) // hold the conn
			results <- timeout
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("probe error: %v", err)
	}
	got := []int64{}
	for v := range results {
		got = append(got, v)
	}
	if len(got) != probes {
		t.Fatalf("expected %d probes, got %d", probes, len(got))
	}
	// Every probe MUST report 5000ms. A 0 means that
	// connection doesn't have the timeout, which is the bug.
	for i, v := range got {
		if v != 5000 {
			t.Errorf("probe %d: busy_timeout=%d, want 5000 (DSN _pragma not applied to this connection)", i, v)
		}
	}
}

// TestConcurrentWritesDoNotHitSQLITE_BUSY is the user's repro,
// distilled to the minimum: many goroutines run write
// transactions simultaneously against a file-backed DB. With
// busy_timeout properly applied, each write either acquires the
// lock immediately or waits for it — none should see
// "database is locked (5) (SQLITE_BUSY)".
//
// Without this test, the parallel enju_execute_run path
// regresses silently to "1 of N writes succeeds, N-1 fail" any
// time someone touches the SQLite open path (driver upgrade,
// DSN refactor, journal-mode change).
func TestConcurrentWritesDoNotHitSQLITE_BUSY(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent-writes.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	s.db.SetMaxOpenConns(8)

	// Each goroutine creates its own citizen — a small write
	// transaction that doesn't depend on any other state. Hammer
	// it with N goroutines to provoke writer-lock contention
	// the same way parallel set_claim calls do in production.
	const goroutines = 32
	const opsPerGoroutine = 5
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*opsPerGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for op := 0; op < opsPerGoroutine; op++ {
				name := nextUniqueCitizenName(gid, op)
				_, err := s.CreateCitizen(&CitizenRecord{
					Username:     name,
					Name:         name,
					Token:        name + "-token",
					RegisteredAt: time.Now(),
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	var busyHits []string
	var otherErrs []string
	for err := range errCh {
		msg := err.Error()
		if strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY") {
			busyHits = append(busyHits, msg)
		} else {
			otherErrs = append(otherErrs, msg)
		}
	}
	if len(busyHits) > 0 {
		t.Errorf("got %d SQLITE_BUSY errors under %d-way concurrent writes — busy_timeout is not pool-safe; first error: %s",
			len(busyHits), goroutines, busyHits[0])
	}
	if len(otherErrs) > 0 {
		t.Errorf("got %d non-busy errors: %s", len(otherErrs), otherErrs[0])
	}
}

// nextUniqueCitizenName produces a deterministic-but-unique
// username per (goroutine, op) so concurrent CreateCitizen
// calls don't UNIQUE-conflict and mask the real busy-vs-not
// signal we're testing for.
func nextUniqueCitizenName(gid, op int) string {
	// "g0-o0", "g0-o1", "g1-o0", ... — short, slug-safe.
	return formatN(gid, op)
}

func formatN(gid, op int) string {
	// Avoid fmt import for this tiny helper; keeps the file's
	// dependency footprint matching its siblings.
	var b strings.Builder
	b.WriteByte('g')
	writeInt(&b, gid)
	b.WriteByte('-')
	b.WriteByte('o')
	writeInt(&b, op)
	return b.String()
}

func writeInt(b *strings.Builder, n int) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(buf[i:])
}

// Compile-time guard: the test depends on Store.db being
// reachable for SetMaxOpenConns and Conn() probes. If the
// field is renamed or unexported behind an accessor, this
// will fail to compile here, signaling the test needs to move
// (or expose a new test-only hook).
var _ = func() *sql.DB { return (&Store{}).db }

// TestIsSQLiteBusy covers the error-class detector used by
// ApplyPlan's retry wrapper. The driver produces two distinct
// message shapes for SQLITE_BUSY ("database is locked" vs
// "SQLITE_BUSY <detail>"); both must be recognized so the
// retry path doesn't degrade silently if a driver upgrade
// changes the wording.
func TestIsSQLiteBusy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"database is locked", &stringErr{"database is locked (5) (SQLITE_BUSY)"}, true},
		{"plain SQLITE_BUSY", &stringErr{"SQLITE_BUSY at commit time"}, true},
		{"unrelated error", &stringErr{"foreign key violation"}, false},
		{"wrapped busy", &stringErr{"set_claim: database is locked (5) (SQLITE_BUSY)"}, true},
	}
	for _, tc := range cases {
		if got := isSQLiteBusy(tc.err); got != tc.want {
			t.Errorf("%s: isSQLiteBusy(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }

// withInstantBackoff swaps applyWithRetry's per-attempt sleep for a
// no-op so retry tests don't pay the production backoff schedule
// (5+10+20+40+80 ≈ 155ms × 5 attempts). Restore the original on test
// exit. Tests that care about the backoff math itself (none today)
// would inspect sleepBusyBackoff directly instead.
func withInstantBackoff(t *testing.T) {
	t.Helper()
	prev := sleepBusyBackoff
	sleepBusyBackoff = func(int) {}
	t.Cleanup(func() { sleepBusyBackoff = prev })
}

// TestApplyWithRetrySucceedsAfterTransientBusy is the targeted
// retry-loop test. Independent of the live concurrency tests above,
// which exercise the DSN config; this one nails down the wrapper
// behavior itself: count, backoff, busy-vs-non-busy distinction.
//
// Why this exists: the existing concurrency tests
// (TestReadThenWriteInDeferredTxHitsSnapshotBusy,
// TestConcurrentWritesDoNotHitSQLITE_BUSY) hit SQLite directly via
// db.Begin() / CreateCitizen, NOT through ApplyPlan. The retry loop
// could regress (off-by-one count, jitter overflow, swallowed
// non-busy errors) and those tests would still pass — they don't go
// through the wrapper. This test stubs the per-attempt callback so
// the wrapper's contract is verified in isolation.
func TestApplyWithRetrySucceedsAfterTransientBusy(t *testing.T) {
	withInstantBackoff(t)
	calls := 0
	result, err := applyWithRetry(applyPlanMaxAttempts, func() (ApplyResult, error) {
		calls++
		if calls < 3 {
			return ApplyResult{}, errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		return ApplyResult{TasksCreated: 7}, nil
	})
	if err != nil {
		t.Fatalf("expected eventual success, got %v after %d calls", err, calls)
	}
	// 2 busy returns + 1 success = 3 calls total.
	if calls != 3 {
		t.Errorf("call count: got %d, want 3", calls)
	}
	// Result from the SUCCESSFUL attempt must propagate, not the
	// zero values from the failed attempts.
	if result.TasksCreated != 7 {
		t.Errorf("result not propagated from successful attempt: %+v", result)
	}
}

// TestApplyWithRetryStopsAfterMaxAttempts asserts the budget is
// exactly maxAttempts — no off-by-one — and that the wrapped error
// names the exhaustion case so log readers don't confuse it with a
// non-retried single failure.
func TestApplyWithRetryStopsAfterMaxAttempts(t *testing.T) {
	withInstantBackoff(t)
	calls := 0
	_, err := applyWithRetry(3, func() (ApplyResult, error) {
		calls++
		return ApplyResult{}, errors.New("SQLITE_BUSY at commit time")
	})
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
	if calls != 3 {
		t.Errorf("call count: got %d, want 3 (one per attempt, no off-by-one)", calls)
	}
	if !strings.Contains(err.Error(), "after 3 retries on SQLITE_BUSY") {
		t.Errorf("exhaustion error wording must name the budget; got: %v", err)
	}
	// Underlying error must be unwrapped-reachable so callers can
	// errors.Is / errors.As against it if they need to.
	if !strings.Contains(err.Error(), "SQLITE_BUSY at commit time") {
		t.Errorf("exhaustion error must wrap the underlying cause; got: %v", err)
	}
}

// TestApplyWithRetryDoesNotRetryNonBusy verifies the wrapper only
// retries SQLITE_BUSY-class errors. Validation failures, bad
// mutations, schema problems must surface on the first attempt —
// retrying them would mask real bugs and waste time on guaranteed-
// to-fail work.
func TestApplyWithRetryDoesNotRetryNonBusy(t *testing.T) {
	withInstantBackoff(t)
	calls := 0
	_, err := applyWithRetry(applyPlanMaxAttempts, func() (ApplyResult, error) {
		calls++
		return ApplyResult{TasksCreated: 1}, errors.New("validation: unknown task_def_id")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("non-busy errors must not retry; got %d calls", calls)
	}
	// Wrapper passes the original error through untouched (no
	// "after N retries" wrapping) for non-busy paths.
	if strings.Contains(err.Error(), "after") && strings.Contains(err.Error(), "retries") {
		t.Errorf("non-busy error must not be wrapped as exhaustion: %v", err)
	}
}

// TestApplyWithRetrySucceedsOnFirstTry covers the happy path: the
// wrapper must not impose any per-call overhead on the common case
// where the first attempt succeeds. One call, immediate return,
// result propagated.
func TestApplyWithRetrySucceedsOnFirstTry(t *testing.T) {
	withInstantBackoff(t)
	calls := 0
	result, err := applyWithRetry(applyPlanMaxAttempts, func() (ApplyResult, error) {
		calls++
		return ApplyResult{RunID: 42}, nil
	})
	if err != nil {
		t.Fatalf("happy path must not error: %v", err)
	}
	if calls != 1 {
		t.Errorf("happy path must invoke fn exactly once; got %d", calls)
	}
	if result.RunID != 42 {
		t.Errorf("result not propagated: %+v", result)
	}
}

// TestReadThenWriteInDeferredTxHitsSnapshotBusy reproduces
// the production failure pattern that the previous busy_timeout
// fix doesn't address. ApplyPlan uses s.db.Begin() which starts
// a DEFERRED transaction. applySetClaim (and similar mutations)
// then does a SELECT before its INSERT — the classic SQLite-WAL
// "reader-to-writer upgrade" pattern.
//
// What goes wrong under concurrent load:
//
//   1. T1: BEGIN DEFERRED, SELECT (acquires read snapshot at v=N).
//   2. T2: BEGIN DEFERRED, SELECT (also at snapshot v=N).
//   3. T1: INSERT (acquires writer lock, v advances to N+1), COMMIT.
//   4. T2: INSERT (tries to acquire writer lock).
//      - The lock is free, but T2's read snapshot is at v=N
//        while the DB is at v=N+1.
//      - SQLite returns SQLITE_BUSY_SNAPSHOT — distinct from
//        plain SQLITE_BUSY, and busy_timeout does NOT retry
//        this case. Application must roll back and retry the
//        whole transaction.
//
// This is exactly the user's production symptom: parallel=4
// over a 50-iteration for_each, two of four claims fail with
// "database is locked (5) (SQLITE_BUSY)" while busy_timeout=
// 5000ms is supposedly set. The timeout works for plain
// writer-vs-writer contention (which TestConcurrentWritesDoNot-
// HitSQLITE_BUSY exercises via INSERT-only operations) but
// not for snapshot conflicts.
//
// The test passes today (failing test = bug present) only
// because we haven't yet added the snapshot-aware retry. Once
// the fix lands (BEGIN IMMEDIATE in ApplyPlan, or explicit
// retry-on-SQLITE_BUSY_SNAPSHOT loop), this test will assert
// no busy errors regardless of contention.
func TestReadThenWriteInDeferredTxHitsSnapshotBusy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "snapshot-busy.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	s.db.SetMaxOpenConns(8)

	// Seed a table with one row that every transaction reads
	// before writing — mirrors applySetClaim's SELECT against
	// `tasks` followed by INSERT into `task_claims` and UPDATE
	// of `tasks`.
	if _, err := s.db.Exec(`CREATE TABLE counters (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatalf("create counters: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO counters (id, value) VALUES (1, 0)`); err != nil {
		t.Fatalf("seed counters: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE log (id INTEGER PRIMARY KEY AUTOINCREMENT, who INTEGER, value INTEGER)`); err != nil {
		t.Fatalf("create log: %v", err)
	}

	// readThenWriteDeferred is the production pattern: BEGIN
	// DEFERRED, SELECT, then INSERT/UPDATE. Run many of these
	// concurrently and SQLITE_BUSY_SNAPSHOT shows up on
	// commit/write attempts.
	readThenWriteDeferred := func(who int) error {
		tx, err := s.db.Begin() // DEFERRED — the bug
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var v int
		if err := tx.QueryRow(`SELECT value FROM counters WHERE id = 1`).Scan(&v); err != nil {
			return err
		}
		// Tiny delay between read and write enlarges the
		// window where another transaction can advance the
		// snapshot underneath us — makes the bug deterministic
		// in the test instead of timing-dependent.
		time.Sleep(2 * time.Millisecond)
		if _, err := tx.Exec(`INSERT INTO log (who, value) VALUES (?, ?)`, who, v); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE counters SET value = value + 1 WHERE id = 1`); err != nil {
			return err
		}
		return tx.Commit()
	}

	const goroutines = 16
	const opsPerGoroutine = 5
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*opsPerGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for op := 0; op < opsPerGoroutine; op++ {
				if err := readThenWriteDeferred(gid); err != nil {
					errCh <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	var busyHits []string
	for err := range errCh {
		msg := err.Error()
		if strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY") {
			busyHits = append(busyHits, msg)
		}
	}
	if len(busyHits) > 0 {
		// This is the bug: snapshot-conflict BUSY despite
		// busy_timeout=5000. The fix (BEGIN IMMEDIATE or
		// retry-on-snapshot-conflict in ApplyPlan) will turn
		// this assertion green.
		t.Errorf("got %d SQLITE_BUSY errors under deferred-tx read-then-write contention — busy_timeout does NOT retry SQLITE_BUSY_SNAPSHOT; first error: %s",
			len(busyHits), busyHits[0])
	}
}
