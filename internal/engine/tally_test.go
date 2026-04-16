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

func TestEvaluateReviewTallyUnanimousApprove(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {
				{Option: "approve"},
				{Option: "approve"},
				{Option: "approve"},
			},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID: "t1", Citizens: 3, VoteThreshold: "unanimous-approve",
	}
	out, err := e.EvaluateReviewTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Verdict != "approve" {
		t.Errorf("expected resolved=approve, got %+v", out)
	}
}

func TestEvaluateReviewTallyUnanimousRejectOnAnyReject(t *testing.T) {
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
		ID: "t1", Citizens: 3, VoteThreshold: "unanimous-approve",
	}
	out, err := e.EvaluateReviewTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Verdict != "reject" {
		t.Errorf("expected resolved=reject, got %+v", out)
	}
}

func TestEvaluateReviewTallyPercentMet(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {
				{Option: "approve"},
				{Option: "approve"},
				{Option: "approve"},
				{Option: "reject"},
			},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID: "t1", Citizens: 4, VoteThreshold: "percent:75",
	}
	out, err := e.EvaluateReviewTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved || out.Verdict != "approve" {
		t.Errorf("expected 75%% met (3/4=75%%), got %+v", out)
	}
}

func TestEvaluateReviewTallyPercentShortCircuitReject(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {
				{Option: "reject"},
				{Option: "reject"},
			},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID: "t1", Citizens: 3, VoteThreshold: "percent:75",
	}
	out, err := e.EvaluateReviewTally(task)
	if err != nil {
		t.Fatal(err)
	}
	// 2 rejects out of 3 needed. Even if the third approves,
	// only 1/3 = 33% < 75%. Should short-circuit reject.
	if !out.Resolved || out.Verdict != "reject" {
		t.Errorf("expected short-circuit reject (can't reach 75%%), got %+v", out)
	}
}

func TestEvaluateReviewTallyQuorumNotMetYet(t *testing.T) {
	ms := &mockStore{
		submissions: map[string][]store.TaskClaimRecord{
			"t1": {
				{Option: "approve"},
			},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID: "t1", Citizens: 3, VoteThreshold: "any-reject-kills",
	}
	out, err := e.EvaluateReviewTally(task)
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolved {
		t.Errorf("expected not resolved (1 of 3 quorum), got %+v", out)
	}
	if out.Reason == "" {
		t.Error("expected a reason for not resolving")
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
