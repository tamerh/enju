package engine

import (
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

func TestComputeSubmissionSingleCitizen(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskClaimed, Citizens: 1, ClaimedBy: 1},
		},
	}
	e := New(ms, nil)
	out, err := e.ComputeSubmission("t1", 1, "path", "sha", "", "", "content", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved {
		t.Error("single-citizen should resolve")
	}
	if out.Collecting {
		t.Error("single-citizen should not be collecting")
	}
	if len(out.Plan.Mutations) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(out.Plan.Mutations))
	}
}

func TestComputeSubmissionMultiCitizen(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskReady, Citizens: 3},
		},
		activeClaims: map[string]map[int64]bool{
			"t1": {1: true},
		},
	}
	e := New(ms, nil)
	out, err := e.ComputeSubmission("t1", 1, "path", "sha", "", "", "content", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolved {
		t.Error("multi-citizen should not resolve on first submit")
	}
	if !out.Collecting {
		t.Error("multi-citizen should be collecting")
	}
}

func TestComputeSubmissionMultiCitizenNoClaim(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskReady, Citizens: 3},
		},
		// No active claims for citizen 1.
	}
	e := New(ms, nil)
	_, err := e.ComputeSubmission("t1", 1, "path", "sha", "", "", "content", 100, "")
	if err == nil {
		t.Fatal("expected no-claim error")
	}
	if !contains(err.Error(), "no open claim") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestComputeSubmissionRejectsAlreadyResolved(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskAccepted, Citizens: 3},
		},
	}
	e := New(ms, nil)
	_, err := e.ComputeSubmission("t1", 1, "path", "sha", "", "", "", 0, "")
	if err == nil {
		t.Fatal("expected already-resolved error")
	}
	if !contains(err.Error(), "already resolved") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestComputeSubmissionRejectsNonClaimable(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", State: store.TaskPending, Citizens: 1},
		},
	}
	e := New(ms, nil)
	_, err := e.ComputeSubmission("t1", 1, "path", "sha", "", "", "", 0, "")
	if err == nil {
		t.Fatal("expected non-claimable error")
	}
}

func TestValidateSubmitRequestDecision(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {ID: "t1", Action: "review", Citizens: 1, ClaimedBy: 1},
		},
	}
	e := New(ms, nil)
	task := ms.tasks["t1"]
	run := &store.RunRecord{ProjectID: 1, Seq: 1}

	// Missing decision should fail.
	_, _, _, _, err := e.ValidateSubmitRequest(task, run, &SubmitRequest{
		TaskID: "t1", Decision: "",
	})
	if err == nil {
		t.Fatal("expected missing-decision error")
	}

	// Invalid decision should fail.
	_, _, _, _, err = e.ValidateSubmitRequest(task, run, &SubmitRequest{
		TaskID: "t1", Decision: "maybe",
	})
	if err == nil {
		t.Fatal("expected invalid-decision error")
	}

	// Valid decision should pass.
	_, decision, _, _, err := e.ValidateSubmitRequest(task, run, &SubmitRequest{
		TaskID: "t1", Decision: "approve",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision != "approve" {
		t.Errorf("expected approve, got %q", decision)
	}
}

func TestValidateSubmitRequestVoteOption(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"t1": {
				ID: "t1", Action: "vote", Citizens: 1, ClaimedBy: 1,
				VoteOptions: `[{"id":"a"},{"id":"b"}]`,
			},
		},
	}
	e := New(ms, nil)
	task := ms.tasks["t1"]
	run := &store.RunRecord{ProjectID: 1, Seq: 1}

	// Invalid option should fail.
	_, _, _, _, err := e.ValidateSubmitRequest(task, run, &SubmitRequest{
		TaskID: "t1", Option: "c",
	})
	if err == nil {
		t.Fatal("expected invalid-option error")
	}

	// Valid option should pass.
	_, _, choice, _, err := e.ValidateSubmitRequest(task, run, &SubmitRequest{
		TaskID: "t1", Option: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if choice != "a" {
		t.Errorf("expected a, got %q", choice)
	}
}
