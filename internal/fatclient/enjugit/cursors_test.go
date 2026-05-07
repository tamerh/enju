package enjugit

import (
	"path/filepath"
	"testing"
)

func TestCursorsLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadCursors(dir, 7)
	if err != nil {
		t.Fatalf("LoadCursors: %v", err)
	}
	if c.Get("main") != "" {
		t.Errorf("fresh project should have empty cursor, got %q", c.Get("main"))
	}
}

func TestCursorsSetGetSaveRoundtrip(t *testing.T) {
	dir := t.TempDir()
	c, _ := LoadCursors(dir, 7)
	c.Set("main", "abc123")
	c.Set("topic/x", "deadbeef")
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Reload and verify.
	c2, err := LoadCursors(dir, 7)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := c2.Get("main"); got != "abc123" {
		t.Errorf("main cursor: got %q, want abc123", got)
	}
	if got := c2.Get("topic/x"); got != "deadbeef" {
		t.Errorf("topic/x cursor: got %q, want deadbeef", got)
	}
}

func TestCursorsCorruptIsTreatedAsEmpty(t *testing.T) {
	dir := t.TempDir()
	// Write garbage at the cursor path.
	corruptPath := filepath.Join(dir, "project-7-cursors.json")
	if err := writeFile(t, corruptPath, "not json"); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCursors(dir, 7)
	if err != nil {
		t.Fatalf("LoadCursors: %v", err)
	}
	if c.Get("main") != "" {
		t.Error("corrupt file should be treated as empty")
	}
}

func TestAdvanceScanCursor(t *testing.T) {
	dir := t.TempDir()
	AdvanceScanCursor(7, dir, "main", "tipsha")
	c, _ := LoadCursors(dir, 7)
	if got := c.Get("main"); got != "tipsha" {
		t.Errorf("AdvanceScanCursor didn't persist: got %q", got)
	}
}

func TestAdvanceScanCursorNoOpOnIncomplete(t *testing.T) {
	dir := t.TempDir()
	// Empty branch → no-op.
	AdvanceScanCursor(7, dir, "", "tipsha")
	c, _ := LoadCursors(dir, 7)
	if c.Get("main") != "" {
		t.Error("AdvanceScanCursor should be no-op on empty branch")
	}
	// Empty stateDir + zero projectID → no panic, no error.
	AdvanceScanCursor(0, "", "main", "tipsha")
}

func TestCursorMutexFor_SamePerProject(t *testing.T) {
	a := CursorMutexFor("/tmp/x", 1)
	b := CursorMutexFor("/tmp/x", 1)
	if a != b {
		t.Error("same (stateDir, projectID) should yield same mutex pointer")
	}
	c := CursorMutexFor("/tmp/x", 2)
	if a == c {
		t.Error("different projectID should yield different mutex")
	}
	d := CursorMutexFor("/tmp/y", 1)
	if a == d {
		t.Error("different stateDir should yield different mutex")
	}
}

func TestRescanSentinelSHA(t *testing.T) {
	if RescanSentinelSHA == "" {
		t.Error("RescanSentinelSHA must be non-empty")
	}
	if len(RescanSentinelSHA) != 40 {
		t.Errorf("RescanSentinelSHA should be 40 chars (SHA-1 length), got %d", len(RescanSentinelSHA))
	}
}

// writeFile is a tiny test helper so the test code stays focused.
func writeFile(t *testing.T, path, contents string) error {
	t.Helper()
	return writeFileImpl(path, []byte(contents))
}
