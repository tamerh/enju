package store

// EventStore architectural-contract tests: pin invariants the
// rest of the suite would let drift silently.
//
//  1. The state DB never INSERTs into events. This
//   guards against a future contributor "fixing" the audit-gap
//   window by re-introducing in-tx emission, which would
//   unwind the strict-consumer architecture without anything
//   else failing.
//  2. State mutations succeed even when event emission errors.
//   The "events are never on the critical path" claim is
//   load-bearing for the whole event-store design — break the
//   EventStore and assert state still commits.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNoContributionEventsInsertOutsideEventStore scans the
// store package for the string `INSERT INTO events`
// and asserts it appears only in events_sqlite.go. The state DB
// no longer carries this table; any new writer is a regression.
//
// Trade-off: a string-match test is brittle to legitimate
// formatting (e.g. wrapping the SQL in a string builder). But
// the rule it pins is narrow — "don't write events
// from a *Store transaction" — and the false-positive cost
// (rename a comment) is far below the false-negative cost
// (silently revert the architecture).
func TestNoContributionEventsInsertOutsideEventStore(t *testing.T) {
	const allowedFile = "events_sqlite.go"
	const banned = "INSERT INTO events"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read store dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || e.IsDir() {
			continue
		}
		if name == allowedFile {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			// The contract is about production code paths.
			// Tests may legitimately reference the string
			// (e.g., this very test).
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(body), banned) {
			t.Errorf("file %s contains %q — events must flow through "+
				"Store.Events().Record(...) so the strict-consumer "+
				"contract holds. Move the emission to a post-commit "+
				"Record() call.", name, banned)
		}
	}
}

// brokenEventStore wraps a real EventStore but force-fails every
// Record into the dropped-counter path by closing it first. Used
// to simulate a degraded events subsystem in the strict-consumer
// test below — Record must still be a no-op-shaped operation
// from the caller's perspective even when the backend is broken.
type brokenEventStore struct{}

func (brokenEventStore) Record(Event) {} // silent drop, mimics post-Close behavior

func (brokenEventStore) QueryByRun(context.Context, int64, int64, time.Time, int) ([]Event, error) {
	return nil, errors.New("simulated event store failure")
}
func (brokenEventStore) QueryByCitizen(context.Context, int64, int) ([]Event, error) {
	return nil, errors.New("simulated event store failure")
}
func (brokenEventStore) Query(context.Context, EventQuery) ([]Event, error) {
	return nil, errors.New("simulated event store failure")
}
func (brokenEventStore) CountByCitizenAndType(context.Context, int64) (map[string]map[string]int, error) {
	return nil, errors.New("simulated event store failure")
}
func (brokenEventStore) SumTokensForCitizen(context.Context, int64) (int64, error) {
	return 0, errors.New("simulated event store failure")
}
func (brokenEventStore) CountDistinctProjectsForCitizen(context.Context, int64) (int, error) {
	return 0, errors.New("simulated event store failure")
}
func (brokenEventStore) CountContributionEvents(context.Context, int64) (int, error) {
	return 0, errors.New("simulated event store failure")
}
func (brokenEventStore) CountProjectsThisMonth(context.Context, int64, time.Time) (int, error) {
	return 0, errors.New("simulated event store failure")
}
func (brokenEventStore) LatestMetadataForTask(context.Context, string, string) (string, error) {
	return "", errors.New("simulated event store failure")
}
func (brokenEventStore) DistinctTaskIDsForCitizenAndType(context.Context, int64, string) ([]string, error) {
	return nil, errors.New("simulated event store failure")
}
func (brokenEventStore) Stats() Stats       { return Stats{} }
func (brokenEventStore) Enabled() bool       { return true }
func (brokenEventStore) SetEnabled(bool)      {}
func (brokenEventStore) WaitForDrain(time.Duration) {}
func (brokenEventStore) WaitForEvent() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}
func (brokenEventStore) GapsInProject(context.Context, int64) ([]int64, error) {
	return nil, errors.New("simulated event store failure")
}
func (brokenEventStore) Close() error       { return nil }

// TestStateMutationsSurviveBrokenEventStore is the strict-
// consumer contract test: with a deliberately-broken EventStore
// attached, the load-bearing state mutations (CreateRun,
// SpawnTask, PauseRun, EvaluateRunState, CreateIssue, etc.)
// must complete successfully and produce the right state. Audit
// loss is acceptable; state loss is not.
//
// Without this test, a future refactor that quietly threaded
// EventStore errors back into the apply path would compile,
// pass the unit tests for individual mutations (which still
// see the green path), and only break in production.
func TestStateMutationsSurviveBrokenEventStore(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AttachEventStore(brokenEventStore{})

	// 1. CreateProject + CreateRun. Both fire run_created
	//  events that the broken store will refuse.
	now := time.Now()
	pid, err := helperCreateProject(s, &ProjectRecord{
		Name: "events-broken-project", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateProject failed despite broken events: %v", err)
	}
	runID, _, err := helperCreateRun(s, &RunRecord{
		ProjectID: pid, Name: "Test", YAMLData: "name: t",
		State: RunActive, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateRun failed despite broken events: %v", err)
	}

	// 2. CreateIssue — issues.go was migrated to post-commit
	//  Record(). State must survive the simulated emission
	//  failure.
	alice, err := helperCreateCitizen(s, &CitizenRecord{
		Username: "alice-broken", Name: "Alice", Email: "alice-broken@test.local",
		RegisteredAt: now, LastSeen: now,
	}, "tok-broken")
	if err != nil {
		t.Fatal(err)
	}
	_, issueSeq, err := helperCreateIssue(s, &IssueRecord{
		ProjectID: pid, Title: "broken-events finding",
		FiledBy: alice, Severity: IssueSeverityLow,
	})
	if err != nil {
		t.Fatalf("CreateIssue failed despite broken events: %v", err)
	}
	if issueSeq != 1 {
		t.Errorf("issue seq = %d, want 1 — state should be intact even with broken events", issueSeq)
	}

	// 3. SpawnTask — apply.go was migrated to staged events.
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref,
			action, prompt, user_prompt, script, outputs, requirements, result_type,
			timeout, state, depends_on, reads_artifacts, writes_artifacts,
			assign_to, require_role, citizens, run_slug,
			spawned_from, spawn_trigger, closes_issue_seq, created_at)
		 VALUES ('1:1:root', ?, 1, 'root', '', '', '',
			'answer', '', '', '', '', '', 'text',
			'', 'accepted', '', '[]', '[]',
			'', '', 1, '',
			'', '', 0, ?)`,
		runID, now,
	); err != nil {
		t.Fatal(err)
	}
	taskID, err := helperSpawnTask(s, SpawnSpec{
		RunID: runID, TaskDefID: "spawned", Action: "answer",
		SpawnedBy: alice, Trigger: "human",
	})
	if err != nil {
		t.Fatalf("SpawnTask failed despite broken events: %v", err)
	}
	if taskID == "" {
		t.Error("SpawnTask returned empty task id")
	}
	got, _ := s.GetTask(taskID)
	if got == nil || got.State != TaskReady {
		t.Errorf("spawned task missing or wrong state: %+v", got)
	}

	// 4. PauseRun — runs through recordRunLifecycleEvent which
	//  is best-effort by design.
	changed, err := helperPauseRun(s, runID, alice)
	if err != nil {
		t.Fatalf("PauseRun failed despite broken events: %v", err)
	}
	if !changed {
		t.Error("PauseRun should report changed=true on first call")
	}
	r, _ := s.GetRun(runID)
	if r.State != RunPaused {
		t.Errorf("run state = %s, want paused", r.State)
	}
}

// TestEventStoreFileFailureDoesNotPropagate complements the
// broken-store test above by exercising the real SQLiteEventStore
// in a degraded mode: open the store, then yank the underlying
// file out from under it. Subsequent persists fail; Record()
// never blocks or errors. State mutations through the parent
// Store keep working because they only call Record(), never
// observe its outcome.
func TestEventStoreFileFailureDoesNotPropagate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	es, err := NewSQLiteEventStore(dbPath, nil)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	s.AttachEventStore(es)

	// Close the events DB out from under the writer goroutine.
	// Subsequent Record() calls succeed (queue accepts) but the
	// writer's persistOne fails — drops counter increments,
	// caller never sees the error.
	if err := es.Close(); err != nil {
		t.Fatalf("close events: %v", err)
	}

	// Now drive a state mutation. It must succeed — the broken
	// downstream is invisible to the state path.
	now := time.Now()
	pid, err := helperCreateProject(s, &ProjectRecord{
		Name: "post-close-project", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateProject failed after EventStore close: %v", err)
	}
	if pid == 0 {
		t.Error("expected non-zero project id")
	}
}
