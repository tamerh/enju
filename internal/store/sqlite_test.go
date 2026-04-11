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

func TestCreateAndGetProblem(t *testing.T) {
	s := newTestStore(t)

	now := time.Now()
	err := s.CreateProblem(&ProblemRecord{
		ID:        "prob-1",
		Name:      "Test Problem",
		YAMLData:  "name: test",
		RepoURL:   "git@example.com:repo.git",
		State:     ProblemActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	p, err := s.GetProblem("prob-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Test Problem" {
		t.Fatalf("expected 'Test Problem', got %q", p.Name)
	}
}

func TestCreateAndClaimTask(t *testing.T) {
	s := newTestStore(t)

	now := time.Now()
	s.CreateProblem(&ProblemRecord{
		ID: "prob-1", Name: "Test", State: ProblemActive,
		CreatedAt: now, UpdatedAt: now,
	})

	// Create a ready task
	s.CreateTask(&TaskRecord{
		ID: "task-1", ProblemID: "prob-1", TaskDefID: "step1",
		Type: "llm_prompt", Mode: "autonomous", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})

	// Register participant
	s.CreateParticipant(&ParticipantRecord{
		ID: "user-1", Name: "alice", Token: "tok-123",
		RegisteredAt: now, LastSeen: now,
	})

	// Claim the task
	deadline := now.Add(30 * time.Minute)
	err := s.ClaimTask("task-1", "user-1", deadline)
	if err != nil {
		t.Fatal(err)
	}

	// Verify state changed
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
	now := time.Now()

	s.CreateProblem(&ProblemRecord{
		ID: "prob-1", Name: "Test", State: ProblemActive,
		CreatedAt: now, UpdatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "task-1", ProblemID: "prob-1", TaskDefID: "step1",
		Type: "llm_prompt", Mode: "autonomous", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	s.CreateParticipant(&ParticipantRecord{
		ID: "user-1", Name: "alice", Token: "tok-123",
		RegisteredAt: now, LastSeen: now,
	})

	s.ClaimTask("task-1", "user-1", now.Add(30*time.Minute))

	err := s.SubmitTaskResult("task-1", "results/step1.json", 1500)
	if err != nil {
		t.Fatal(err)
	}

	task, _ := s.GetTask("task-1")
	if task.State != TaskAccepted {
		t.Fatalf("expected accepted, got %s", task.State)
	}
	if task.ResultPath != "results/step1.json" {
		t.Fatalf("expected result path, got %q", task.ResultPath)
	}

	// Check participant stats updated
	p, _ := s.GetParticipantByToken("tok-123")
	if p.TasksCompleted != 1 {
		t.Fatalf("expected 1 task completed, got %d", p.TasksCompleted)
	}
	if p.TokensContrib != 1500 {
		t.Fatalf("expected 1500 tokens contributed, got %d", p.TokensContrib)
	}
}

func TestUpdateReadyTasks(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	s.CreateProblem(&ProblemRecord{
		ID: "prob-1", Name: "Test", State: ProblemActive,
		CreatedAt: now, UpdatedAt: now,
	})

	// Create: a (ready) -> b (pending) -> c (pending)
	s.CreateTask(&TaskRecord{
		ID: "a", ProblemID: "prob-1", TaskDefID: "a",
		Type: "llm_prompt", Mode: "autonomous", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "b", ProblemID: "prob-1", TaskDefID: "b",
		Type: "llm_prompt", Mode: "autonomous", ResultType: "text",
		State: TaskPending, DependsOn: "a", CreatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "c", ProblemID: "prob-1", TaskDefID: "c",
		Type: "llm_prompt", Mode: "autonomous", ResultType: "text",
		State: TaskPending, DependsOn: "a,b", CreatedAt: now,
	})

	// Nothing accepted yet — no tasks should become ready
	count, err := s.UpdateReadyTasks("prob-1")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 newly ready, got %d", count)
	}

	// Accept task a — b should become ready, c still pending (needs a AND b)
	s.CreateParticipant(&ParticipantRecord{
		ID: "user-1", Name: "alice", Token: "tok-123",
		RegisteredAt: now, LastSeen: now,
	})
	s.ClaimTask("a", "user-1", now.Add(30*time.Minute))
	s.SubmitTaskResult("a", "results/a.json", 100)

	count, err = s.UpdateReadyTasks("prob-1")
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
	now := time.Now()

	s.CreateProblem(&ProblemRecord{
		ID: "prob-1", Name: "Test", State: ProblemActive,
		CreatedAt: now, UpdatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "task-1", ProblemID: "prob-1", TaskDefID: "step1",
		Type: "llm_prompt", Mode: "autonomous", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	s.CreateParticipant(&ParticipantRecord{
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
	now := time.Now()

	s.CreateProblem(&ProblemRecord{
		ID: "prob-1", Name: "Test", State: ProblemActive,
		CreatedAt: now, UpdatedAt: now,
	})

	// a -> b -> c, all accepted
	for _, id := range []string{"a", "b", "c"} {
		s.CreateTask(&TaskRecord{
			ID: id, ProblemID: "prob-1", TaskDefID: id,
			Type: "llm_prompt", Mode: "autonomous", ResultType: "text",
			State: TaskAccepted, CreatedAt: now,
		})
	}

	// Invalidate a, cascade to b and c
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

	taskC, _ := s.GetTask("c")
	if taskC.State != TaskInvalidated {
		t.Fatalf("expected c invalidated, got %s", taskC.State)
	}
}
