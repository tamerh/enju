package notify

import (
	"path/filepath"
	"testing"
)

// TestActiveProjectFileRoundTrip pins the on-disk record:
// SaveActiveProject → LoadActiveProject must round-trip the
// (coordinator-key, project-id) pair. Other coordinators'
// entries must survive an unrelated save.
func TestActiveProjectFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")

	// Missing file → 0, nil error (don't block startup).
	if got := LoadActiveProject(path, "local"); got != 0 {
		t.Errorf("missing file should return 0, got %d", got)
	}

	// Save for one coordinator.
	if err := SaveActiveProject(path, "http://prod", 42); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := LoadActiveProject(path, "http://prod"); got != 42 {
		t.Errorf("round-trip prod: got %d, want 42", got)
	}

	// A second coordinator's record doesn't touch the first.
	if err := SaveActiveProject(path, "local", 7); err != nil {
		t.Fatalf("save local: %v", err)
	}
	if got := LoadActiveProject(path, "http://prod"); got != 42 {
		t.Errorf("after local save, prod should still be 42, got %d", got)
	}
	if got := LoadActiveProject(path, "local"); got != 7 {
		t.Errorf("local: got %d, want 7", got)
	}

	// Save 0 clears the entry.
	if err := SaveActiveProject(path, "local", 0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := LoadActiveProject(path, "local"); got != 0 {
		t.Errorf("after clear, local should be 0, got %d", got)
	}
	// But prod is untouched.
	if got := LoadActiveProject(path, "http://prod"); got != 42 {
		t.Errorf("clearing local must not affect prod: got %d, want 42", got)
	}
}

// TestActiveProjectEmptyArgsAreNoOp pins the safe-degradation
// path: empty path or empty key skip persistence without
// erroring, matching the rest of the notify package's "missing
// path = no persistence" convention.
func TestActiveProjectEmptyArgsAreNoOp(t *testing.T) {
	if err := SaveActiveProject("", "local", 5); err != nil {
		t.Errorf("empty path should be no-op, got: %v", err)
	}
	if err := SaveActiveProject(filepath.Join(t.TempDir(), "x.json"), "", 5); err != nil {
		t.Errorf("empty key should be no-op, got: %v", err)
	}
	if got := LoadActiveProject("", "local"); got != 0 {
		t.Errorf("empty path load: got %d, want 0", got)
	}
}
