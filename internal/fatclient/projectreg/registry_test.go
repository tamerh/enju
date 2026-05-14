package projectreg

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTempRegistry(t *testing.T) *Registry {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), "projects.json"))
}

func TestLoad_Missing(t *testing.T) {
	r := newTempRegistry(t)
	idx, err := r.Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if idx.Version != CurrentVersion {
		t.Errorf("missing file should default to current version, got %d", idx.Version)
	}
	if len(idx.Projects) != 0 {
		t.Errorf("missing file should yield zero projects, got %d", len(idx.Projects))
	}
}

func TestUpsert_NewEntry(t *testing.T) {
	r := newTempRegistry(t)
	dir := t.TempDir()
	if err := r.Upsert(Entry{
		ID:        7,
		LocalPath: dir,
		Name:      "Alpha",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 7 || got[0].Name != "Alpha" {
		t.Fatalf("unexpected entries: %+v", got)
	}
	if got[0].LastTouched.IsZero() {
		t.Errorf("LastTouched should be auto-set on insert, got zero")
	}
}

func TestUpsert_MergeFields(t *testing.T) {
	r := newTempRegistry(t)
	dir := t.TempDir()

	// First upsert — full info.
	if err := r.Upsert(Entry{
		ID:            7,
		LocalPath:     dir,
		Name:          "Alpha",
		RemoteURL:     "git@example.com:alpha.git",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	// Second upsert — only LocalPath. Should preserve other fields.
	newDir := t.TempDir()
	if err := r.Upsert(Entry{
		ID:        7,
		LocalPath: newDir,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].LocalPath != newDir {
		t.Errorf("LocalPath should update, got %q", got[0].LocalPath)
	}
	if got[0].Name != "Alpha" || got[0].RemoteURL == "" || got[0].DefaultBranch == "" {
		t.Errorf("merged entry lost fields: %+v", got[0])
	}
}

func TestList_FiltersStalePaths(t *testing.T) {
	r := newTempRegistry(t)
	livePath := t.TempDir()
	stalePath := filepath.Join(t.TempDir(), "deleted-clone")

	for _, e := range []Entry{
		{ID: 7, LocalPath: livePath, Name: "Alpha"},
		{ID: 11, LocalPath: stalePath, Name: "Beta"},
	} {
		if err := r.Upsert(e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 7 {
		t.Fatalf("stale-path filter mismatch: %+v", got)
	}

	// File should still hold both — Remove is the only thing
	// that drops entries permanently.
	idx, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Projects) != 2 {
		t.Errorf("Load should still see both entries, got %d", len(idx.Projects))
	}
}

func TestTouch(t *testing.T) {
	r := newTempRegistry(t)
	dir := t.TempDir()
	if err := r.Upsert(Entry{ID: 7, LocalPath: dir, Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}

	idx0, _ := r.Load()
	originalTouched := idx0.Projects[0].LastTouched

	time.Sleep(10 * time.Millisecond)
	if err := r.Touch(7); err != nil {
		t.Fatal(err)
	}

	idx1, _ := r.Load()
	if !idx1.Projects[0].LastTouched.After(originalTouched) {
		t.Errorf("Touch should advance LastTouched: before=%v after=%v",
			originalTouched, idx1.Projects[0].LastTouched)
	}
}

func TestTouch_MissingEntryNoOp(t *testing.T) {
	r := newTempRegistry(t)
	if err := r.Touch(999); err != nil {
		t.Errorf("Touch on missing entry should be no-op, got %v", err)
	}
}

func TestRemove(t *testing.T) {
	r := newTempRegistry(t)
	dir := t.TempDir()
	if err := r.Upsert(Entry{ID: 7, LocalPath: dir}); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(7); err != nil {
		t.Fatal(err)
	}
	idx, _ := r.Load()
	if len(idx.Projects) != 0 {
		t.Errorf("Remove should drop entry, got %+v", idx.Projects)
	}
}

func TestRemove_MissingEntryNoOp(t *testing.T) {
	r := newTempRegistry(t)
	if err := r.Remove(999); err != nil {
		t.Errorf("Remove on missing entry should be no-op, got %v", err)
	}
}

func TestSave_AtomicWrite(t *testing.T) {
	// Write succeeds → no leftover .tmp file.
	r := newTempRegistry(t)
	if err := r.Upsert(Entry{ID: 7, LocalPath: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file should not survive successful write")
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	// Save into a directory that doesn't yet exist (~/.enju on
	// fresh install).
	parent := filepath.Join(t.TempDir(), "fresh", "deep", "nested")
	r := Open(filepath.Join(parent, "projects.json"))
	if err := r.Upsert(Entry{ID: 7, LocalPath: t.TempDir()}); err != nil {
		t.Fatalf("Upsert should create parent dirs, got %v", err)
	}
	if _, err := os.Stat(r.path); err != nil {
		t.Errorf("file not at expected path: %v", err)
	}
}

func TestLoad_MalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(path, []byte("not json {{{"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Open(path)
	if _, err := r.Load(); err == nil {
		t.Errorf("Load on malformed JSON should error")
	}
}

func TestFindContaining(t *testing.T) {
	r := newTempRegistry(t)
	outer := t.TempDir()
	inner := filepath.Join(outer, "nested")
	if err := os.MkdirAll(inner, 0755); err != nil {
		t.Fatal(err)
	}

	for _, e := range []Entry{
		{ID: 1, LocalPath: outer},
		{ID: 2, LocalPath: inner},
	} {
		if err := r.Upsert(e); err != nil {
			t.Fatal(err)
		}
	}

	// Case 1: prefers deepest match
	got, err := r.FindContaining(filepath.Join(inner, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != 2 {
		t.Errorf("expected deepest match (ID=2), got %+v", got)
	}

	// Case 2: falls back to outer
	got, err = r.FindContaining(filepath.Join(outer, "other.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != 1 {
		t.Errorf("expected outer match (ID=1), got %+v", got)
	}

	// Case 3: no match
	got, err = r.FindContaining("/elsewhere/entirely")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for no match, got %+v", got)
	}

	// Case 4: entry whose LocalPath was removed — os.Stat filter
	// should exclude it. To isolate the filter, create a fourth entry
	// with an isolated path (not a parent of anything), delete it,
	// and verify FindContaining filters it out.
	isolated := filepath.Join(t.TempDir(), "isolated-project")
	if err := os.MkdirAll(isolated, 0755); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(Entry{ID: 4, LocalPath: isolated}); err != nil {
		t.Fatal(err)
	}
	// Delete the isolated directory
	if err := os.RemoveAll(isolated); err != nil {
		t.Fatal(err)
	}
	// Now try to find containing for a path under the deleted directory.
	// The os.Stat filter should exclude the deleted entry.
	got, err = r.FindContaining(filepath.Join(isolated, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for entry whose dir was deleted, got %+v", got)
	}
}
