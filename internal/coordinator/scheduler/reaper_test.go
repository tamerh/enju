package scheduler

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore uses a file-based SQLite so multiple connections from the
// pool (reaper goroutine + test goroutine) share the same schema. The
// `:memory:` driver scopes the DB per-connection, which races with the
// background loop.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedClaimedTask creates a project+run+task+citizen and claims the task
// with the given deadline. Returns the task ID and citizen ID.
func seedClaimedTask(t *testing.T, s *store.Store, taskID string, deadline time.Time) (string, int64) {
	t.Helper()
	now := time.Now()

	createRes, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateProject{Project: store.ProjectRecord{
				Name: "test-project", CreatedAt: now, UpdatedAt: now,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := createRes.ProjectID

	runRes, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateRun{Run: store.RunRecord{
				ProjectID: projectID, Name: "Test Run",
				YAMLData:  "name: test",
				State:     store.RunActive,
				CreatedAt: now, UpdatedAt: now,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := runRes.RunID

	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateTask{Task: store.TaskRecord{
				ID: taskID, RunID: runID, Seq: 1, TaskDefID: "t",
				Action: "answer", ResultType: "text",
				State:  store.TaskReady, CreatedAt: now,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	citRes, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateCitizen{
				Citizen: store.CitizenRecord{
					Username:     "alice-" + taskID,
					Name:         "alice",
					Email:        "alice-" + taskID + "@test.local",
					RegisteredAt: now,
					LastSeen:     now,
				},
				Token: "tok-" + taskID,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	citizenID := citRes.CitizenID

	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetClaim{TaskID: taskID, CitizenID: citizenID, Deadline: deadline},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return taskID, citizenID
}

func TestReaperSweepExpiresPastDeadlineClaim(t *testing.T) {
	s := newTestStore(t)
	taskID, citizenID := seedClaimedTask(t, s, "task-expired", time.Now().Add(-1*time.Minute))

	r := NewReaper(s, time.Hour, discardLogger())
	r.sweep()

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != store.TaskReady {
		t.Fatalf("expected task back to ready, got %s", task.State)
	}
	if task.ClaimedBy != 0 {
		t.Fatalf("expected ClaimedBy cleared, got %d", task.ClaimedBy)
	}

	cit, err := s.GetCitizen(citizenID)
	if err != nil {
		t.Fatal(err)
	}
	if cit.TasksTimedOut != 1 {
		t.Fatalf("expected TasksTimedOut=1, got %d", cit.TasksTimedOut)
	}
}

func TestReaperSweepLeavesActiveClaimAlone(t *testing.T) {
	s := newTestStore(t)
	taskID, citizenID := seedClaimedTask(t, s, "task-active", time.Now().Add(30*time.Minute))

	r := NewReaper(s, time.Hour, discardLogger())
	r.sweep()

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != store.TaskClaimed {
		t.Fatalf("expected task still claimed, got %s", task.State)
	}

	cit, err := s.GetCitizen(citizenID)
	if err != nil {
		t.Fatal(err)
	}
	if cit.TasksTimedOut != 0 {
		t.Fatalf("expected TasksTimedOut=0, got %d", cit.TasksTimedOut)
	}
}

func TestReaperSweepEmptyStoreNoop(t *testing.T) {
	s := newTestStore(t)
	r := NewReaper(s, time.Hour, discardLogger())
	// Must not panic or error-log loudly; just verify it completes.
	r.sweep()
}

func TestReaperStartStopLifecycle(t *testing.T) {
	s := newTestStore(t)
	seedClaimedTask(t, s, "task-loop", time.Now().Add(-1*time.Minute))

	// Short interval so the ticker fires during the test.
	r := NewReaper(s, 20*time.Millisecond, discardLogger())
	r.Start()

	// Wait for at least one tick to process the expired claim.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, err := s.GetTask("task-loop")
		if err != nil {
			t.Fatal(err)
		}
		if task.State == store.TaskReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	task, err := s.GetTask("task-loop")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != store.TaskReady {
		t.Fatalf("reaper loop did not expire task within 2s, state=%s", task.State)
	}

	r.Stop()

	// After Stop, Start's goroutine should exit promptly. Attempting a
	// second Stop would panic on the closed channel — the invariant we
	// care about here is that Stop is safe to call once.
}
