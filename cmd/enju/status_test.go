package main

import (
	"testing"

	"github.com/enju-ai/enju/internal/common/wire"
)

func TestSplitRunsByStateActiveOrdering(t *testing.T) {
	runs := []wire.Run{
		{Seq: 3, State: "active"},
		{Seq: 1, State: "active"},
		{Seq: 2, State: "completed"},
		{Seq: 7, State: "completed"},
	}
	active, recent := splitRunsByState(runs)

	// Active sorted seq-ascending.
	if len(active) != 2 || active[0].Seq != 1 || active[1].Seq != 3 {
		t.Fatalf("active: got %+v", active)
	}

	// Recent sorted seq-descending and capped at 5.
	if len(recent) != 2 || recent[0].Seq != 7 || recent[1].Seq != 2 {
		t.Fatalf("recent: got %+v", recent)
	}
}

func TestSplitRunsByStateRecentCap(t *testing.T) {
	var runs []wire.Run
	for i := 1; i <= 10; i++ {
		runs = append(runs, wire.Run{Seq: i, State: "completed"})
	}
	_, recent := splitRunsByState(runs)
	if len(recent) != 5 {
		t.Fatalf("recent cap broken: len=%d", len(recent))
	}
	if recent[0].Seq != 10 {
		t.Fatalf("expected seq 10 first, got %d", recent[0].Seq)
	}
}
