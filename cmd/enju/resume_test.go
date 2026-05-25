package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// newResumeSession wires a cliSession over a registry holding one
// project (id 42) at a real path, the same shape project_test.go uses.
func newResumeSession(t *testing.T) *cliSession {
	t.Helper()
	dir := t.TempDir()
	reg := projectreg.Open(filepath.Join(dir, "projects.json"))
	if err := reg.Register(projectreg.Entry{ID: 42, LocalPath: dir}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	return &cliSession{FC: service.New(service.Config{ProjectRegistry: reg})}
}

// A non-positive / non-numeric seq is a usage error caught before any
// project resolution or coord round-trip.
func TestResolveResumeTarget_BadSeq(t *testing.T) {
	sess := newResumeSession(t)
	for _, bad := range []string{"abc", "0", "-3", ""} {
		projID, seq, err := resolveResumeTarget(sess, 42, bad)
		if err == nil {
			t.Errorf("seq %q: expected error, got (%d,%d)", bad, projID, seq)
		}
		if projID != 0 || seq != 0 {
			t.Errorf("seq %q: a parse failure must report (0,0), got (%d,%d)", bad, projID, seq)
		}
	}
}

// An unregistered --project id surfaces a clear resolution error
// BEFORE any coord call (mirrors runProjectDefaultBranch's contract).
func TestResolveResumeTarget_UnregisteredProject(t *testing.T) {
	sess := newResumeSession(t)
	projID, _, err := resolveResumeTarget(sess, 99999, "7")
	if err == nil {
		t.Fatal("expected an error for an unregistered --project id")
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Errorf("error should name the missing id, got: %v", err)
	}
	if projID != 0 {
		t.Errorf("a resolution failure must report project id 0, got %d", projID)
	}
}

// A valid seq + resolvable project returns the resolved id and the
// parsed seq, ready to thread into ExecuteRunParams.
func TestResolveResumeTarget_Valid(t *testing.T) {
	sess := newResumeSession(t)
	projID, seq, err := resolveResumeTarget(sess, 42, "7")
	if err != nil {
		t.Fatalf("resolveResumeTarget(42, \"7\"): %v", err)
	}
	if projID != 42 || seq != 7 {
		t.Errorf("got (project=%d, seq=%d), want (42, 7)", projID, seq)
	}
}

// TestValidateRetryFrom pins the two coord-recognized retry modes;
// the empty default is filled by the flag, so "" is not valid here.
func TestValidateRetryFrom(t *testing.T) {
	for _, v := range []string{"head", "snapshot"} {
		if err := validateRetryFrom(v); err != nil {
			t.Errorf("validateRetryFrom(%q): expected nil, got %v", v, err)
		}
	}
	for _, v := range []string{"", "HEAD", "Snapshot", "tip", "latest"} {
		if err := validateRetryFrom(v); err == nil {
			t.Errorf("validateRetryFrom(%q): expected error, got nil", v)
		}
	}
}

// TestFailedTaskIDs pins the keep-going failure-collection that drives
// both the report block and the exit-1 rule: only failure-class entries
// (failed/git_failed/error/unknown) are collected; successes, skips,
// and async-pending are not.
func TestFailedTaskIDs(t *testing.T) {
	res := &service.ExecuteRunResult{Entries: []service.ExecuteRunEntry{
		{TaskID: "p:1:a", Status: "completed"},
		{TaskID: "p:1:b", Status: "failed"},
		{TaskID: "p:1:c", Status: "skipped"},
		{TaskID: "p:1:d", Status: "git_failed"},
		{TaskID: "p:1:e", Status: "async_started"},
		{TaskID: "p:1:f", Status: "error"},
	}}
	got := failedTaskIDs(res)
	want := map[string]bool{"p:1:b": true, "p:1:d": true, "p:1:f": true}
	if len(got) != len(want) {
		t.Fatalf("failedTaskIDs = %v, want the 3 failure-class ids %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected id %q in failedTaskIDs (success/skip/async must not count)", id)
		}
	}
}
