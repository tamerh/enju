package engine

import (
	"testing"

	"github.com/enju-ai/enju/internal/store"
)

func TestEvaluateReviewTallyAnyRejectKills(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {
				{Option: "approve"},
				{Option: "reject"},
			},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID:            "t1",
		Citizens:      3,
		VoteThreshold: "any-reject-kills",
	}
	out, err := e.EvaluateReviewTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Verdict != "reject" {
		t.Errorf("expected resolved=reject, got %+v", out)
	}
}

func TestEvaluateReviewTallyMajorityShortCircuit(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {
				{Option: "approve"},
				{Option: "approve"},
			},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID:            "t1",
		Citizens:      3,
		VoteThreshold: "majority-approve",
	}
	out, err := e.EvaluateReviewTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Verdict != "approve" {
		t.Errorf("expected short-circuit approve (2 of 3), got %+v", out)
	}
}

func TestEvaluateVoteTallyPlurality(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {
				{Option: "alpha"},
				{Option: "beta"},
				{Option: "alpha"},
			},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID:            "t1",
		Citizens:      3,
		VoteThreshold: "plurality",
		VoteOptions:   `[{"id":"alpha"},{"id":"beta"}]`,
	}
	out, err := e.EvaluateVoteTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.WinningOption != "alpha" {
		t.Errorf("expected alpha wins, got %+v", out)
	}
}

func TestEvaluateVoteTallyQuorumNotMet(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {{Option: "alpha"}},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID:            "t1",
		Citizens:      3,
		VoteThreshold: "plurality",
		VoteOptions:   `[{"id":"alpha"},{"id":"beta"}]`,
	}
	out, err := e.EvaluateVoteTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolved {
		t.Errorf("expected not resolved (quorum 3, got 1), got %+v", out)
	}
}
