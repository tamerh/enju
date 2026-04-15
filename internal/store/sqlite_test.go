package store

import (
	"strings"
	"testing"
	"time"
)

func TestValidateUsername(t *testing.T) {
	good := []string{"a", "tamer", "tamer-gur", "octocat-42", "x1", strings.Repeat("a", 39)}
	for _, u := range good {
		if err := ValidateUsername(u); err != nil {
			t.Errorf("expected %q to validate, got %v", u, err)
		}
	}

	bad := []string{
		"",                       // empty
		"-leading",               // leading hyphen
		"trailing-",              // trailing hyphen
		"With-Caps",              // uppercase
		"spaces in it",           // spaces
		"under_score",            // underscore
		"dot.ed",                 // dot
		strings.Repeat("a", 40),  // too long
	}
	for _, u := range bad {
		if err := ValidateUsername(u); err == nil {
			t.Errorf("expected %q to fail validation, got nil", u)
		}
	}
}

func TestSlugifyName(t *testing.T) {
	cases := map[string]string{
		"alice":              "alice",
		"Alice":              "alice",
		"Tamer Gur":          "tamer-gur",
		"  weird  spacing  ": "weird-spacing",
		"mixed_ _ underscores":  "mixed-underscores",
		"with.dots.here":     "with-dots-here",
		"UPPER CASE":         "upper-case",
		"trailing-":          "trailing",
		"---leading---":      "leading",
		"":                   "",
		"!!!":                "",
	}
	for in, want := range cases {
		if got := SlugifyName(in); got != want {
			t.Errorf("SlugifyName(%q) = %q, want %q", in, got, want)
		}
	}
}

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
		Name:      "test-project",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func createTestRun(t *testing.T, s *Store) int64 {
	t.Helper()
	projectID := createTestProject(t, s)
	now := time.Now()
	id, _, err := s.CreateRun(&RunRecord{
		ProjectID: projectID,
		Name:      "Test Run",
		YAMLData:  "name: test",
		State:     RunActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// createTestCitizen creates a citizen with the given username and a
// unique token, returning the generated int64 primary key.
func createTestCitizen(t *testing.T, s *Store, username, token string) int64 {
	t.Helper()
	now := time.Now()
	id, err := s.CreateCitizen(&CitizenRecord{
		Username:     username,
		Name:         username,
		Token:        token,
		RegisteredAt: now,
		LastSeen:     now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreateAndGetRun(t *testing.T) {
	s := newTestStore(t)
	pid := createTestRun(t, s)

	p, err := s.GetRun(pid)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Test Run" {
		t.Fatalf("expected 'Test Run', got %q", p.Name)
	}
	if p.ID != pid {
		t.Fatalf("expected id %d, got %d", pid, p.ID)
	}
}

func TestCreateAndClaimTask(t *testing.T) {
	s := newTestStore(t)
	pid := createTestRun(t, s)
	now := time.Now()

	s.CreateTask(&TaskRecord{
		ID: "task-1", RunID: pid, Seq: 1, TaskDefID: "step1",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})

	alice := createTestCitizen(t, s, "alice", "tok-123")
	bob := createTestCitizen(t, s, "bob", "tok-456")

	deadline := now.Add(30 * time.Minute)
	err := s.ClaimTask("task-1", alice, deadline)
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
	if task.ClaimedBy != alice {
		t.Fatalf("expected claimed by %d, got %d", alice, task.ClaimedBy)
	}

	// Can't claim again
	err = s.ClaimTask("task-1", bob, deadline)
	if err == nil {
		t.Fatal("expected error claiming already claimed task")
	}
}

func TestSubmitResult(t *testing.T) {
	s := newTestStore(t)
	pid := createTestRun(t, s)
	now := time.Now()

	s.CreateTask(&TaskRecord{
		ID: "task-1", RunID: pid, Seq: 1, TaskDefID: "step1",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	alice := createTestCitizen(t, s, "alice", "tok-123")

	s.ClaimTask("task-1", alice, now.Add(30*time.Minute))

	_, err := s.SubmitTaskResult("task-1", alice, "results/step1", "", "", "", "", 1500)
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
	pid := createTestRun(t, s)
	now := time.Now()

	// a (ready) -> b (pending) -> c (pending, depends on a,b)
	s.CreateTask(&TaskRecord{
		ID: "a", RunID: pid, Seq: 1, TaskDefID: "a",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "b", RunID: pid, Seq: 2, TaskDefID: "b",
		Action: "answer", ResultType: "text",
		State: TaskPending, DependsOn: "a", CreatedAt: now,
	})
	s.CreateTask(&TaskRecord{
		ID: "c", RunID: pid, Seq: 3, TaskDefID: "c",
		Action: "answer", ResultType: "text",
		State: TaskPending, DependsOn: "a,b", CreatedAt: now,
	})

	alice := createTestCitizen(t, s, "alice", "tok-123")

	// Nothing accepted — no tasks should become ready
	count, err := s.UpdateReadyTasks(pid)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 newly ready, got %d", count)
	}

	// Accept a → b should become ready, c still pending
	s.ClaimTask("a", alice, now.Add(30*time.Minute))
	s.SubmitTaskResult("a", alice, "results/a", "", "", "", "", 100)

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
	pid := createTestRun(t, s)
	now := time.Now()

	s.CreateTask(&TaskRecord{
		ID: "task-1", RunID: pid, Seq: 1, TaskDefID: "step1",
		Action: "answer", ResultType: "text",
		State: TaskReady, CreatedAt: now,
	})
	alice := createTestCitizen(t, s, "alice", "tok-123")

	s.ClaimTask("task-1", alice, now.Add(30*time.Minute))

	err := s.ReleaseTask("task-1", alice)
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
	pid := createTestRun(t, s)
	now := time.Now()

	// Three accepted tasks with descendants a → b → c (modeled via
	// the descendantIDs argument rather than a real DAG).
	for i, id := range []string{"a", "b", "c"} {
		s.CreateTask(&TaskRecord{
			ID: id, RunID: pid, Seq: i + 1, TaskDefID: id,
			Action: "answer", ResultType: "text",
			State: TaskAccepted, CreatedAt: now,
		})
	}

	changed, err := s.InvalidateTask("a", []string{"b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	// 1 target + 2 descendants = 3
	if changed != 3 {
		t.Fatalf("expected 3 tasks changed, got %d", changed)
	}

	// Target transitions ACCEPTED → READY (not INVALID). It's ready to
	// re-claim now that the bad result is invalidated.
	taskA, _ := s.GetTask("a")
	if taskA.State != TaskReady {
		t.Fatalf("expected a to be READY after invalidation, got %s", taskA.State)
	}

	// Descendants transition ACCEPTED → PENDING, waiting for a to
	// re-complete. They'll be promoted back to READY by
	// UpdateReadyTasks once a is re-accepted.
	for _, id := range []string{"b", "c"} {
		task, _ := s.GetTask(id)
		if task.State != TaskPending {
			t.Fatalf("expected %s to be PENDING after cascade, got %s", id, task.State)
		}
	}
}

// TestInvalidateTaskRejectsNonAcceptedTarget verifies you can't
// invalidate a task that isn't in the ACCEPTED state.
func TestInvalidateTaskRejectsNonAcceptedTarget(t *testing.T) {
	s := newTestStore(t)
	pid := createTestRun(t, s)
	now := time.Now()

	s.CreateTask(&TaskRecord{
		ID: "p", RunID: pid, Seq: 1, TaskDefID: "p",
		Action: "answer", ResultType: "text",
		State: TaskPending, CreatedAt: now,
	})

	_, err := s.InvalidateTask("p", nil)
	if err == nil {
		t.Fatal("expected error invalidating pending task, got nil")
	}
}

// TestInvalidateTaskClearsClaims verifies that a cascade wipes claim
// fields on both the target and any descendants that were
// claimed/running when the cascade happened.
func TestInvalidateTaskClearsClaims(t *testing.T) {
	s := newTestStore(t)
	pid := createTestRun(t, s)
	now := time.Now()

	alice := createTestCitizen(t, s, "alice", "tok-a")

	// Target is accepted by alice.
	acceptedAt := now
	s.CreateTask(&TaskRecord{
		ID: "target", RunID: pid, Seq: 1, TaskDefID: "target",
		Action: "answer", ResultType: "text",
		State:       TaskAccepted,
		ClaimedBy:   alice,
		ClaimedAt:   &acceptedAt,
		SubmittedAt: &acceptedAt,
		ResultPath:  "runs/1/target",
		CreatedAt:   now,
	})
	// Descendant is currently claimed by alice (in-progress).
	s.CreateTask(&TaskRecord{
		ID: "descendant", RunID: pid, Seq: 2, TaskDefID: "descendant",
		Action: "answer", ResultType: "text",
		State:     TaskClaimed,
		ClaimedBy: alice,
		ClaimedAt: &acceptedAt,
		CreatedAt: now,
	})

	if _, err := s.InvalidateTask("target", []string{"descendant"}); err != nil {
		t.Fatal(err)
	}

	target, _ := s.GetTask("target")
	if target.State != TaskReady {
		t.Fatalf("expected target READY, got %s", target.State)
	}
	if target.ClaimedBy != 0 || target.ClaimedAt != nil || target.ResultPath != "" {
		t.Fatalf("expected target claim fields cleared, got %+v", target)
	}

	desc, _ := s.GetTask("descendant")
	if desc.State != TaskPending {
		t.Fatalf("expected descendant PENDING, got %s", desc.State)
	}
	if desc.ClaimedBy != 0 || desc.ClaimedAt != nil {
		t.Fatalf("expected descendant claim fields cleared, got %+v", desc)
	}
}
