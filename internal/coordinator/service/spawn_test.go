package service

// Bug-hunt L4, spawn path: a spawned task bypasses the create-time
// assign_to-citizen check, so SpawnTask must validate assign_to
// itself or a typoed assignee lands an unclaimable task.

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

func newSpawnStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	es, err := store.NewSQLiteEventStore(filepath.Join(dir, "e.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("event store: %v", err)
	}
	st.AttachEventStore(es)
	t.Cleanup(func() { st.Close(); es.Close() })
	return st
}

func TestSpawnTask_AssignToCitizenExistence(t *testing.T) {
	st := newSpawnStore(t)
	now := time.Now()

	res, _ := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateProject{Project: store.ProjectRecord{Name: "p", CreatedAt: now, UpdatedAt: now}},
	}})
	projectID := res.ProjectID
	res, _ = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateRun{Run: store.RunRecord{ProjectID: projectID, Name: "r", YAMLData: "name: r", State: store.RunActive, CreatedAt: now, UpdatedAt: now}},
	}})
	runSeq := res.RunSeq
	res, _ = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateCitizen{Citizen: store.CitizenRecord{Username: "op", Name: "Op", Email: "o@t.local", Kind: store.CitizenKindHuman, RegisteredAt: now, LastSeen: now}, Token: "tok-op"},
	}})
	caller, _ := st.GetCitizen(res.CitizenID)

	// Unregistered assignee → rejected (no unclaimable task created).
	_, err := SpawnTask(st, caller, projectID, runSeq, SpawnTaskParams{
		TaskDefID: "ghost_assigned",
		Action:    "answer",
		Prompt:    "x",
		AssignTo:  []string{"ghost-agent"},
	})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected unregistered-assignee rejection, got: %v", err)
	}

	// Registered assignee → spawn succeeds.
	if _, err := SpawnTask(st, caller, projectID, runSeq, SpawnTaskParams{
		TaskDefID: "ok_assigned",
		Action:    "answer",
		Prompt:    "x",
		AssignTo:  []string{caller.Username},
	}); err != nil {
		t.Fatalf("registered assignee should spawn cleanly: %v", err)
	}
}
