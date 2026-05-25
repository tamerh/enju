package service

import "testing"

// TestClassifyEntryStatus pins the status→class map that the CLI
// and MCP renderers now share. The bug it guards: the CLI used to
// only recognize "ok"/"accepted" as success, so the canonical
// "completed" fell to its ✗ default and every successful run
// printed as a failure. The critical invariants here are
//  1. "completed" → Success (the regression), and
//  2. an unrecognized status → Unknown, NEVER Success — a future
//     producer that adds a status string must not silently read
//     as ✓ in any renderer.
func TestClassifyEntryStatus(t *testing.T) {
	cases := []struct {
		status string
		want   EntryClass
	}{
		{"completed", EntryClassSuccess},
		{"failed", EntryClassFailed},
		{"git_failed", EntryClassGitFailed},
		{"error", EntryClassError},
		{"async_started", EntryClassPending},
		{"skipped", EntryClassSkipped},
		{"", EntryClassUnknown},
		{"ok", EntryClassUnknown},       // legacy alias: no producer; must not read as ✓
		{"accepted", EntryClassUnknown}, // legacy alias: no producer; must not read as ✓
		{"weird_new_status", EntryClassUnknown},
	}
	for _, c := range cases {
		if got := ClassifyEntryStatus(c.status); got != c.want {
			t.Errorf("ClassifyEntryStatus(%q) = %d, want %d", c.status, got, c.want)
		}
	}
	if ClassifyEntryStatus("") == EntryClassSuccess {
		t.Fatal("zero-value/unknown status must never classify as Success")
	}
}

// TestEntryFromOutcomeStatusesAreClassified is the by-construction
// link: every Status string EntryFromOutcome can stamp must map to
// a non-Unknown class. If a producer arm changes the status string
// without updating ClassifyEntryStatus, this fails — the two stay
// in lockstep instead of drifting like the original two switches.
func TestEntryFromOutcomeStatusesAreClassified(t *testing.T) {
	for _, st := range []string{"completed", "failed", "git_failed"} {
		e := EntryFromOutcome(&ExecuteOutcome{TaskID: "t", Status: st})
		if ClassifyEntryStatus(e.Status) == EntryClassUnknown {
			t.Errorf("EntryFromOutcome emits Status=%q but ClassifyEntryStatus says Unknown", st)
		}
	}
}

// TestStopReasonForOutcome pins the keep-going policy that the serial
// loop and runCascadeParallel both consult. The invariants:
//   - task-level failures (failed/git_failed) stop fail-fast but are
//     downgraded to "" (continue) under keepGoing,
//   - driver-level errors stop regardless of keepGoing,
//   - async launches always stop (the caller reaps),
//   - non-terminal statuses never stop.
func TestStopReasonForOutcome(t *testing.T) {
	cases := []struct {
		status    string
		keepGoing bool
		want      string
	}{
		{"failed", false, StopComputeFailed},
		{"failed", true, ""}, // keep-going: record + continue
		{"git_failed", false, StopGitOperationFailed},
		{"git_failed", true, ""},
		{"error", false, StopComputeErrored},
		{"error", true, StopComputeErrored}, // driver-level: fatal even under keep-going
		{"async_started", false, StopAsyncTaskStarted},
		{"async_started", true, StopAsyncTaskStarted},
		{"completed", false, ""},
		{"completed", true, ""},
		{"skipped", true, ""},
	}
	for _, c := range cases {
		if got := stopReasonForOutcome(c.status, c.keepGoing); got != c.want {
			t.Errorf("stopReasonForOutcome(%q, keepGoing=%v) = %q, want %q",
				c.status, c.keepGoing, got, c.want)
		}
	}
}
