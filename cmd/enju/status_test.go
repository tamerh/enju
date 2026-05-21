package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// TestResolveActiveProjectMissingID pins bug-hunt H1: a syntactically
// valid but unregistered --project id used to return (nil, nil) from
// reg.Get, which sailed past cmdStatus's err check and segfaulted in
// renderProjectStatus on the nil entry. The resolver must turn that
// into a usage error (shared across status / runs / dag).
func TestResolveActiveProjectMissingID(t *testing.T) {
	dir := t.TempDir()
	reg := projectreg.Open(filepath.Join(dir, "projects.json"))
	// A registered entry whose LocalPath actually exists (reg.Get
	// stats the path and treats a missing dir as not-found).
	if err := reg.Upsert(projectreg.Entry{ID: 42, Name: "real", LocalPath: dir}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	fc := service.New(service.Config{ProjectRegistry: reg})
	sess := &cliSession{FC: fc}

	// Missing id → friendly error, not a nil entry.
	if _, err := resolveActiveProject(sess, 99999); err == nil {
		t.Fatal("expected error for an unregistered --project id, got nil")
	} else if !strings.Contains(err.Error(), "99999") {
		t.Errorf("error should name the missing id, got: %v", err)
	}

	// Real id → the entry, no error.
	entry, err := resolveActiveProject(sess, 42)
	if err != nil {
		t.Fatalf("resolveActiveProject(42): %v", err)
	}
	if entry == nil || entry.ID != 42 {
		t.Fatalf("expected entry id=42, got %+v", entry)
	}
}

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
