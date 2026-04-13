package store

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createTestProject(t *testing.T, s *Store) int64 {
	t.Helper()
	now := time.Now()
	id, err := s.CreateProject(&ProjectRecord{
		Name:      "Test Project",
		YAMLData:  "name: test",
		State:     ProjectActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreateAndGetProject(t *testing.T) {
	s := newTestStore(t)
	pid := createTestProject(t, s)

	p, err := s.GetProject(pid)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Test Project" {
		t.Fatalf("expected 'Test Project', got %q", p.Name)
	}
	if p.ID != pid {
		t.Fatalf("expected id %d, got %d", pid, p.ID)
	}
}

func TestCreateAndClaimTask(t *testing.T) {
	s := newTestStore(t)
	pid := createTestProject(t, s)
	now := time.Now()

	s.CreateTask(&TaskRecord{
		ID: "task-1", ProjectID: pid, Seq: 1, TaskDefID: "step1",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})

	s.CreateCitizen(&CitizenRecord{
		ID: "user-1", Name: "alice", Token: "tok-123",
		RegisteredAt: now, LastSeen: now,
	})

	deadline := now.Add(30 * time.Minute)
	err := s.ClaimTask("task-1", "user-1", deadline)
	if err != nil {
		t.Fatal(err)
	}

	task, err := s.GetTask("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != TaskClaimed {
		t.Fatalf("expected claimed, got %s", task.State)
	}
	if task.ClaimedBy != "user-1" {
		t.Fatalf("expected claimed by user-1, got %q", task.ClaimedBy)
	}

	// Can't claim again
	err = s.ClaimTask("task-1", "user-2", deadline)
	if err == nil {
		t.Fatal("expected error claiming already claimed task")
	}
}

func TestSubmitResult(t *testing.T) {
	s := newTestStore(t)
	pid := createTestProject(t, s)
	now := time.Now()

	s.CreateTask(&TaskRecord{
		ID: "task-1", ProjectID: pid, Seq: 1, TaskDefID: "step1",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	s.CreateCitizen(&CitizenRecord{
		ID: "user-1", Name: "alice", Token: "tok-123",
		RegisteredAt: now, LastSeen: now,
	})

	s.ClaimTask("task-1", "user-1", now.Add(30*time.Minute))

	err := s.SubmitTaskResult("task-1", "results/step1", 1500)
	if err != nil {
		t.Fatal(err)
	}

	task, _ := s.GetTask("task-1")
	if task.State != TaskAccepted {
		t.Fatalf("expected accepted, got %s", task.State)
	}

	p, _ := s.GetCitizenByToken("tok-123")
	if p.TasksCompleted != 1 {
		t.Fatalf("expected 1 completed, got %d", p.TasksCompleted)
	}
}

func TestUpdateReadyTasks(t *testing.T) {
	s := newTestStore(t)
	pid := createTestProject(t, s)
	now := time.Now()

	// a (ready) -> b (pending) -> c (pending, depends on a,b)
	s.CreateTask(&TaskRecord{
		ID: "a", ProjectID: pid, Seq: 1, TaskDefID: "a",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "b", ProjectID: pid, Seq: 2, TaskDefID: "b",
		Action: "answer", ResultType: "text",
		State: TaskPending, DependsOn: "a", CreatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "c", ProjectID: pid, Seq: 3, TaskDefID: "c",
		Action: "answer", ResultType: "text",
		State: TaskPending, DependsOn: "a,b", CreatedAt: now,
	})

	s.CreateCitizen(&CitizenRecord{
		ID: "user-1", Name: "alice", Token: "tok-123",
		RegisteredAt: now, LastSeen: now,
	})

	// Nothing accepted — no tasks should become ready
	count, err := s.UpdateReadyTasks(pid)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 newly ready, got %d", count)
	}

	// Accept a → b should become ready, c still pending
	s.ClaimTask("a", "user-1", now.Add(30*time.Minute))
	s.SubmitTaskResult("a", "results/a", 100)

	count, err = s.UpdateReadyTasks(pid)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 newly ready (b), got %d", count)
	}

	taskB, _ := s.GetTask("b")
	if taskB.State != TaskReady {
		t.Fatalf("expected b ready, got %s", taskB.State)
	}

	taskC, _ := s.GetTask("c")
	if taskC.State != TaskPending {
		t.Fatalf("expected c still pending, got %s", taskC.State)
	}
}

func TestReleaseTask(t *testing.T) {
	s := newTestStore(t)
	pid := createTestProject(t, s)
	now := time.Now()

	s.CreateTask(&TaskRecord{
		ID: "task-1", ProjectID: pid, Seq: 1, TaskDefID: "step1",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	s.CreateCitizen(&CitizenRecord{
		ID: "user-1", Name: "alice", Token: "tok-123",
		RegisteredAt: now, LastSeen: now,
	})

	s.ClaimTask("task-1", "user-1", now.Add(30*time.Minute))

	err := s.ReleaseTask("task-1", "user-1")
	if err != nil {
		t.Fatal(err)
	}

	task, _ := s.GetTask("task-1")
	if task.State != TaskReady {
		t.Fatalf("expected ready after release, got %s", task.State)
	}
}

func TestInvalidateTask(t *testing.T) {
	s := newTestStore(t)
	pid := createTestProject(t, s)
	now := time.Now()

	for i, id := range []string{"a", "b", "c"} {
		s.CreateTask(&TaskRecord{
			ID: id, ProjectID: pid, Seq: i + 1, TaskDefID: id,
			Action: "answer", ResultType: "text",
			State: TaskAccepted, CreatedAt: now,
		})
	}

	err := s.InvalidateTask("a", []string{"b", "c"})
	if err != nil {
		t.Fatal(err)
	}

	taskA, _ := s.GetTask("a")
	if taskA.State != TaskInvalid {
		t.Fatalf("expected a invalid, got %s", taskA.State)
	}

	taskB, _ := s.GetTask("b")
	if taskB.State != TaskInvalidated {
		t.Fatalf("expected b invalidated, got %s", taskB.State)
	}
}
