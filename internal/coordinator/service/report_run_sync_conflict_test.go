package service

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/dagcache"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// newSyncConflictFixture builds a project + a COMPLETED run +
// the reporting citizen. The completed state is load-bearing:
// the run-branch → base sync runs AFTER the run is terminal, so
// the whole point of B-1 is that a `completed` run can still
// have silently lost its output to a merge conflict.
func newSyncConflictFixture(t *testing.T) (*store.Store, *store.SQLiteEventStore, int64, int, *store.CitizenRecord) {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// A real EventStore (the default Store hands back a noop
	// until AttachEventStore) so the run_sync_conflict event
	// actually persists and the timeline assertion is meaningful.
	es, err := store.NewSQLiteEventStore(t.TempDir()+"/events.db", nil)
	if err != nil {
		t.Fatalf("event store: %v", err)
	}
	st.AttachEventStore(es)
	t.Cleanup(func() { st.Close(); es.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_ = NewCoordinator(st, dagcache.New(st), logger)
	now := time.Now()

	res, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateProject{Project: store.ProjectRecord{Name: "p", CreatedAt: now, UpdatedAt: now}},
	}})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID := res.ProjectID

	res, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateRun{Run: store.RunRecord{
			ProjectID: projectID, Name: "sweep-2", YAMLData: "name: sweep-2",
			Branch: "load-test-sweep-2", Slug: "sweep-2",
			State: store.RunCompleted, CreatedAt: now, UpdatedAt: now,
		}},
	}})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runSeq := res.RunSeq

	res, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateCitizen{Citizen: store.CitizenRecord{
			Username: "alice", Name: "Alice", Email: "a@t.local",
			Kind: store.CitizenKindHuman, RegisteredAt: now, LastSeen: now,
		}, Token: "tok-alice"},
	}})
	if err != nil {
		t.Fatalf("create citizen: %v", err)
	}
	caller := &store.CitizenRecord{ID: res.CitizenID, Username: "alice"}
	return st, es, projectID, runSeq, caller
}

// waitForRunSyncConflictEvent polls the run timeline (the async
// EventStore writer drains in a background goroutine, so a
// read-immediately-after-report would flake) until a
// run_sync_conflict event appears, or fails after a budget.
// Asserting on the event TYPE — not a raw persisted count —
// avoids the false-positive where the fixture's
// CreateProject/Run/Citizen events already pushed the persisted
// counter past the threshold before the report ran.
func waitForRunSyncConflictEvent(t *testing.T, st *store.Store, projectID, runID int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := st.ListRunEvents(projectID, runID)
		if err != nil {
			t.Fatalf("ListRunEvents: %v", err)
		}
		for _, e := range events {
			if e.Type == "run_sync_conflict" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no run_sync_conflict event on the run timeline after 2s")
}

// TestReportRunSyncConflict_SurfacesOnCompletedRun is the B-1
// regression. The documented parallel `branch: auto` sweep makes
// the 2nd..Nth run's completion-sync conflict on shared output
// paths; before the fix the ONLY trace was an ERROR in the
// per-run operator log and every coordinator surface still said
// "completed 100%". This pins that a reported conflict (1) sets
// a durable runs.sync_status flag that SURVIVES the terminal
// completed state, (2) surfaces structured on the run-status
// projection, and (3) emits a run_sync_conflict event so the
// timeline carries it.
func TestReportRunSyncConflict_SurfacesOnCompletedRun(t *testing.T) {
	st, es, projectID, runSeq, caller := newSyncConflictFixture(t)

	conflictFiles := []string{"results/report.md", "results/summary.txt"}
	resp, err := ReportRunSyncConflict(st, caller, projectID, runSeq, ReportRunSyncConflictParams{
		RunBranch:     "load-test-sweep-2",
		BaseBranch:    "main",
		ConflictFiles: conflictFiles,
	})
	if err != nil {
		t.Fatalf("ReportRunSyncConflict: %v", err)
	}
	if resp.Status != "recorded" {
		t.Errorf("status = %q, want recorded", resp.Status)
	}

	// (1) Durable flag persists on the COMPLETED run.
	run, err := st.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		t.Fatalf("GetRunByProjectSeq: %v run=%v", err, run)
	}
	if run.State != store.RunCompleted {
		t.Fatalf("precondition: run should still be completed, got %s", run.State)
	}
	ss := store.ParseSyncStatus(run.SyncStatus)
	if ss == nil {
		t.Fatalf("sync_status not set on completed run (B-1 swallow regression): raw=%q", run.SyncStatus)
	}
	if ss.Kind != store.SyncStatusConflict {
		t.Errorf("sync_status kind = %q, want conflict", ss.Kind)
	}
	if len(ss.ConflictFiles) != 2 {
		t.Errorf("conflict_files = %v, want 2 entries", ss.ConflictFiles)
	}
	if ss.RunBranch != "load-test-sweep-2" || ss.BaseBranch != "main" {
		t.Errorf("branches = run:%q base:%q", ss.RunBranch, ss.BaseBranch)
	}
	if ss.Hint == "" {
		t.Errorf("hint should be populated with a manual-merge command")
	}

	// (2) Run-status projection surfaces it (the surface the
	// operator uses as ground truth) even though State=completed.
	status, err := GetRunStatus(st, caller, projectID, runSeq)
	if err != nil {
		t.Fatalf("GetRunStatus: %v", err)
	}
	if status.Run.State != "completed" {
		t.Fatalf("run state should read completed, got %q", status.Run.State)
	}
	if store.ParseSyncStatus(status.Run.SyncStatus) == nil {
		t.Errorf("GetRunStatus must carry sync_status so the renderer can stop saying unqualified 'completed 100%%' (raw=%q)", status.Run.SyncStatus)
	}

	// (3) The event timeline carries run_sync_conflict (drains
	// async; poll on the event type, not a raw persisted count).
	_ = es
	waitForRunSyncConflictEvent(t, st, projectID, run.ID)
}

// TestReportRunSyncConflict_Validation pins the input contract
// (matches the sibling report-* handlers): missing branches /
// empty conflict_files / unknown run are rejected, not silently
// recorded.
func TestReportRunSyncConflict_Validation(t *testing.T) {
	st, _, projectID, runSeq, caller := newSyncConflictFixture(t)

	if _, err := ReportRunSyncConflict(st, caller, projectID, runSeq, ReportRunSyncConflictParams{
		BaseBranch: "main", ConflictFiles: []string{"x"},
	}); err == nil {
		t.Errorf("expected error for empty run_branch")
	}
	if _, err := ReportRunSyncConflict(st, caller, projectID, runSeq, ReportRunSyncConflictParams{
		RunBranch: "b", BaseBranch: "main",
	}); err == nil {
		t.Errorf("expected error for empty conflict_files")
	}
	if _, err := ReportRunSyncConflict(st, caller, projectID, 9999, ReportRunSyncConflictParams{
		RunBranch: "b", BaseBranch: "main", ConflictFiles: []string{"x"},
	}); err == nil {
		t.Errorf("expected ErrNotFound for unknown run")
	}
}
