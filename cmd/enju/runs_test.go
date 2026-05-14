package main

import (
	"testing"

	"github.com/enju-ai/enju/internal/common/wire"
)

func TestParseStatusFilter_Empty(t *testing.T) {
	if got := parseStatusFilter(""); got != nil {
		t.Errorf("empty: want nil, got %v", got)
	}
	if got := parseStatusFilter("   "); got != nil {
		t.Errorf("whitespace: want nil, got %v", got)
	}
}

func TestParseStatusFilter_ActiveAlias(t *testing.T) {
	got := parseStatusFilter("active")
	for _, want := range []string{"active", "waiting", "idle"} {
		if !got[want] {
			t.Errorf("active alias: missing %q in %v", want, got)
		}
	}
}

func TestParseStatusFilter_DoneAlias(t *testing.T) {
	got := parseStatusFilter("done")
	if !got["completed"] {
		t.Errorf("done alias should map to completed; got %v", got)
	}
}

func TestParseStatusFilter_LiteralStates(t *testing.T) {
	got := parseStatusFilter("failed,terminated")
	if !got["failed"] || !got["terminated"] {
		t.Errorf("literal states: got %v", got)
	}
}

func TestFilterRuns_OrderingAndCap(t *testing.T) {
	runs := []wire.Run{
		{Seq: 1, State: "completed"},
		{Seq: 3, State: "active"},
		{Seq: 2, State: "active"},
		{Seq: 5, State: "failed"},
		{Seq: 4, State: "completed"},
	}
	got := filterRuns(runs, nil, 3)
	if len(got) != 3 {
		t.Fatalf("cap 3: got %d", len(got))
	}
	// Descending seq order.
	if got[0].Seq != 5 || got[1].Seq != 4 || got[2].Seq != 3 {
		t.Errorf("order: got %v %v %v", got[0].Seq, got[1].Seq, got[2].Seq)
	}
}

func TestFilterRuns_ZeroLimitReturnsAll(t *testing.T) {
	runs := []wire.Run{
		{Seq: 1, State: "completed"},
		{Seq: 2, State: "completed"},
	}
	got := filterRuns(runs, nil, 0)
	if len(got) != 2 {
		t.Errorf("limit 0 should return all: got %d", len(got))
	}
}

func TestFilterRuns_StateFilter(t *testing.T) {
	runs := []wire.Run{
		{Seq: 1, State: "completed"},
		{Seq: 2, State: "failed"},
		{Seq: 3, State: "active"},
	}
	got := filterRuns(runs, map[string]bool{"failed": true}, 0)
	if len(got) != 1 || got[0].Seq != 2 {
		t.Errorf("state filter: got %+v", got)
	}
}
