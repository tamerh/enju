package compute

import (
	"os"
	"path/filepath"
	"testing"
)

// M5: undeclaredSiblingOutputs flags a file written next to a declared
// output but not itself declared — the forgot-to-declare-a-sibling
// footgun. It must NOT flag files outside declared-output dirs, nor
// scratch bookkeeping.
func TestUndeclaredSiblingOutputs(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "results", "declared.txt"), "ok")
	mustWrite(t, filepath.Join(dir, "results", "undeclared.txt"), "oops")
	mustWrite(t, filepath.Join(dir, "results", "context.json"), "{}") // bookkeeping — excluded
	mustWrite(t, filepath.Join(dir, "tmp", "scratch.bin"), "junk")     // outside declared dir — ignored

	declared := map[string]bool{"results/declared.txt": true}
	got := undeclaredSiblingOutputs(dir, declared)

	if len(got) != 1 || got[0] != "results/undeclared.txt" {
		t.Fatalf("expected [results/undeclared.txt], got %v", got)
	}
}

// No declared writes → nothing scanned → no warnings.
func TestUndeclaredSiblingOutputs_NoDeclared(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "stuff.txt"), "x")
	if got := undeclaredSiblingOutputs(dir, map[string]bool{}); len(got) != 0 {
		t.Fatalf("no declared outputs should produce no warnings, got %v", got)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
