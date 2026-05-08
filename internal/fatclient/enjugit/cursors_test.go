package enjugit

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// TestCursorsVersionMismatchTreatedAsEmpty: a cursor file from a
// future schema version is treated as empty, so an older client
// won't act on data it can't interpret. Originally project's
// TestCursorsVersionMismatch.
func TestCursorsVersionMismatchTreatedAsEmpty(t *testing.T) {
	stateDir := t.TempDir()
	path := cursorsPath(stateDir, 8)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":999,"branches":{"main":"xxx"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadCursors(stateDir, 8)
	if c.Get("main") != "" {
		t.Errorf("future version should be treated as empty; got main=%q", c.Get("main"))
	}
}

// TestCursorsSaveAtomicallyOverwrites: a second Save replaces the
// first cleanly via the temp-file + rename pattern. The reload
// must observe the second state, not a partial mix or the first
// state. Originally project's TestCursorsAtomicSaveSurvivesPartialWrite.
func TestCursorsSaveAtomicallyOverwrites(t *testing.T) {
	stateDir := t.TempDir()
	c := NewCursors(stateDir, 5)
	c.Set("main", "first")
	if err := c.Save(); err != nil {
		t.Fatalf("first save: %v", err)
	}

	c.Set("main", "second")
	if err := c.Save(); err != nil {
		t.Fatalf("second save: %v", err)
	}
	reloaded, _ := LoadCursors(stateDir, 5)
	if got := reloaded.Get("main"); got != "second" {
		t.Errorf("expected second after overwrite, got %q", got)
	}
}

// TestAdvanceScanCursor_SerializesConcurrentCallers covers the
// last-writer-wins race the project test originally flagged.
// Before CursorMutexFor, two callers could each do
// LoadCursors → Set → Save concurrently; the later writer's save
// carried its own older snapshot and silently overwrote the
// earlier writer's advance. Atomic-rename keeps the file from
// corrupting, but the cursor still goes BACKWARDS — next scan
// walks extra history. AdvanceScanCursor now serializes via
// CursorMutexFor so writes never trample each other.
//
// Test: N goroutines each advance a unique commit SHA on the
// same branch. After all finish, the saved cursor MUST be one
// of the N SHAs (never empty, never the seed, never corrupt).
// Originally project's TestAdvanceCursorIfConfiguredSerializesConcurrentCallers.
func TestAdvanceScanCursor_SerializesConcurrentCallers(t *testing.T) {
	stateDir := t.TempDir()
	const projectID int64 = 42
	const N = 20

	// Pre-seed with an initial cursor so the file exists before
	// the racers start (matches production's "scanner has been
	// running" steady state).
	seed := NewCursors(stateDir, projectID)
	seed.Set("main", "seed")
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	shas := make([]string, N)
	for i := 0; i < N; i++ {
		shas[i] = fmt.Sprintf("%040x", i+1)
	}

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(sha string) {
			defer wg.Done()
			AdvanceScanCursor(projectID, stateDir, "main", sha)
		}(shas[i])
	}
	wg.Wait()

	loaded, err := LoadCursors(stateDir, projectID)
	if err != nil {
		t.Fatalf("load after race: %v", err)
	}
	final := loaded.Get("main")
	if final == "" {
		t.Fatalf("cursor vanished after concurrent advances")
	}
	if final == "seed" {
		t.Fatalf("cursor stayed at seed — concurrent advances all lost")
	}
	found := false
	for _, sha := range shas {
		if sha == final {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("final cursor %q is not one of the advanced SHAs — concurrent save corrupted state", final)
	}
}
