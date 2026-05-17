package engine

import (
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

func TestComputeClaimSingleCitizen(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskReady, Citizens: 1},
		},
	}
	e := New(ms, nil)
	plan, err := e.ComputeClaim("t1", 1, time.Now().Add(30*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mutations) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(plan.Mutations))
	}
	if _, ok := plan.Mutations[0].(store.SetClaim); !ok {
		t.Errorf("expected SetClaim mutation, got %T", plan.Mutations[0])
	}
}

func TestComputeClaimRejectsNonReady(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskPending, Citizens: 1},
		},
	}
	e := New(ms, nil)
	_, err := e.ComputeClaim("t1", 1, time.Now(), "")
	if err == nil {
		t.Fatal("expected error for non-ready task")
	}
}

func TestComputeClaimMultiCitizenAcceptsReady(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskReady, Citizens: 3},
		},
		claimCounts: map[string]int{"t1": 1},
	}
	e := New(ms, nil)
	plan, err := e.ComputeClaim("t1", 2, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mutations) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(plan.Mutations))
	}
}

func TestComputeClaimMultiCitizenAcceptsCollecting(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskCollecting, Citizens: 3},
		},
		claimCounts: map[string]int{"t1": 1},
	}
	e := New(ms, nil)
	_, err := e.ComputeClaim("t1", 2, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestComputeClaimRejectsDuplicateSlot(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskReady, Citizens: 3},
		},
		activeClaims: map[string]map[int64]bool{
			"t1": {1: true},
		},
	}
	e := New(ms, nil)
	_, err := e.ComputeClaim("t1", 1, time.Now(), "")
	if err == nil {
		t.Fatal("expected duplicate-slot error")
	}
	if !contains(err.Error(), "already have an active claim") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestComputeClaimRejectsCapReached(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskReady, Citizens: 2},
		},
		claimCounts: map[string]int{"t1": 2},
	}
	e := New(ms, nil)
	_, err := e.ComputeClaim("t1", 3, time.Now(), "")
	if err == nil {
		t.Fatal("expected cap-reached error")
	}
	if !contains(err.Error(), "citizens cap") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestCheckTaskAccessAssignTo(t *testing.T) {
	task := &store.TaskRecord{AssignTo: `["alice","bob"]`}
	alice := &store.CitizenRecord{Username: "alice"}
	charlie := &store.CitizenRecord{Username: "charlie"}

	if err := CheckTaskAccess(task, alice); err != nil {
		t.Errorf("alice should pass: %v", err)
	}
	if err := CheckTaskAccess(task, charlie); err == nil {
		t.Error("charlie should fail")
	}
}

func TestCheckTaskAccessRequireRole(t *testing.T) {
	task := &store.TaskRecord{RequireRole: "reviewer"}
	reviewer := &store.CitizenRecord{Username: "a", Role: "reviewer"}
	citizen := &store.CitizenRecord{Username: "b", Role: "citizen"}

	if err := CheckTaskAccess(task, reviewer); err != nil {
		t.Errorf("reviewer should pass: %v", err)
	}
	if err := CheckTaskAccess(task, citizen); err == nil {
		t.Error("citizen should fail")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsAt(s, sub))
}

func containsAt(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
