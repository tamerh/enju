package service

// Iter-2 revision-fix contract: WipeDeclaredWrites must clean
// every shape of `writes_artifacts` declaration the YAML grammar
// accepts — not just literal paths. A half-fix would leave glob/
// dir-declared tasks producing union-of-files commits when the
// LLM picks different filenames across iterations.

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(rel), err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func relPathsBelow(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	if err := filepath.Walk(dir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestWipeDeclaredWrites_LiteralRemovesNamedFile pins the
// simplest shape: a literal path declaration deletes that one
// file and leaves everything else alone.
func TestWipeDeclaredWrites_LiteralRemovesNamedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/keep.go", "kept")
	writeFile(t, dir, "src/foo/entities.go", "iter-1")

	writes := enjuYaml.WriteArtifacts{
		{Path: "src/foo/entities.go", Track: true},
	}
	if err := wipeDeclaredWritesInDir(dir, writes); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	got := relPathsBelow(t, dir)
	want := []string{"src/keep.go"}
	if !equalSlice(got, want) {
		t.Errorf("after wipe, files = %v, want %v", got, want)
	}
}

// TestWipeDeclaredWrites_GlobRemovesAllMatches pins the
// glob-shape contract — `src/*.go` must remove every `.go` file
// in `src/`, not just the literal pattern string. Pre-fix this
// case slipped through and produced the union-of-files mess on
// iter-2.
func TestWipeDeclaredWrites_GlobRemovesAllMatches(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/entities.go", "iter-1")
	writeFile(t, dir, "src/errors.go", "iter-1")
	writeFile(t, dir, "src/parse.go", "iter-1")
	writeFile(t, dir, "src/inner/keep.go", "kept (subdir, glob doesn't recurse)")

	writes := enjuYaml.WriteArtifacts{
		{Path: "src/*.go", Track: true},
	}
	if err := wipeDeclaredWritesInDir(dir, writes); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	got := relPathsBelow(t, dir)
	want := []string{"src/inner/keep.go"}
	if !equalSlice(got, want) {
		t.Errorf("after glob wipe, files = %v, want %v", got, want)
	}
}

// TestWipeDeclaredWrites_DirectoryRemovesAllChildren pins the
// directory-shape contract — `out/` must remove every file
// under `out/`, recursively. Pre-fix this case slipped through.
func TestWipeDeclaredWrites_DirectoryRemovesAllChildren(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "out/a.md", "iter-1")
	writeFile(t, dir, "out/b.md", "iter-1")
	writeFile(t, dir, "out/sub/c.md", "iter-1")
	writeFile(t, dir, "elsewhere/keep.md", "kept")

	writes := enjuYaml.WriteArtifacts{
		{Path: "out/", Track: true},
	}
	if err := wipeDeclaredWritesInDir(dir, writes); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	got := relPathsBelow(t, dir)
	want := []string{"elsewhere/keep.md"}
	if !equalSlice(got, want) {
		t.Errorf("after directory wipe, files = %v, want %v", got, want)
	}
}

// TestWipeDeclaredWrites_PreResolvedTemplateLooksLikeLiteral
// pins the templated-path contract: by the time the daemon sees
// a TaskMeta the templates have been resolved at materialization
// (e.g. `out/{{instance}}.md` → `out/alpha.md`), so the wipe just
// sees a literal. Test fakes the resolved form directly.
func TestWipeDeclaredWrites_PreResolvedTemplateLooksLikeLiteral(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "out/alpha.md", "iter-1 alpha")
	writeFile(t, dir, "out/beta.md", "kept (different instance)")

	// What the daemon actually receives — the post-substitution
	// literal for this instance.
	writes := enjuYaml.WriteArtifacts{
		{Path: "out/alpha.md", Track: true},
	}
	if err := wipeDeclaredWritesInDir(dir, writes); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	got := relPathsBelow(t, dir)
	want := []string{"out/beta.md"}
	if !equalSlice(got, want) {
		t.Errorf("after templated-literal wipe, files = %v, want %v", got, want)
	}
}

// TestWipeDeclaredWrites_MissingFileIsNoOp pins idempotency —
// a declaration that matches nothing on disk (because the
// previous iteration didn't write it) silently no-ops, doesn't
// error.
func TestWipeDeclaredWrites_MissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keep.md", "kept")

	writes := enjuYaml.WriteArtifacts{
		{Path: "absent.md", Track: true, Optional: true},
	}
	if err := wipeDeclaredWritesInDir(dir, writes); err != nil {
		t.Errorf("wipe of absent file should no-op, got %v", err)
	}
	got := relPathsBelow(t, dir)
	want := []string{"keep.md"}
	if !equalSlice(got, want) {
		t.Errorf("after no-op wipe, files = %v, want %v", got, want)
	}
}

// TestWipeDeclaredWrites_MixedShapesAllCleared confirms a
// realistic task that uses several declaration shapes at once
// (literal + glob + dir) gets fully cleaned in a single call.
// Without this composability, a task like
//
//	writes:
//	  - go.mod
//	  - "src/*.go"
//	  - docs/
//
// would only get partial coverage and iter-2 would still be
// poisoned.
func TestWipeDeclaredWrites_MixedShapesAllCleared(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module foo")
	writeFile(t, dir, "src/a.go", "iter-1")
	writeFile(t, dir, "src/b.go", "iter-1")
	writeFile(t, dir, "docs/intro.md", "iter-1")
	writeFile(t, dir, "docs/sub/deep.md", "iter-1")
	writeFile(t, dir, "untouched.txt", "kept")

	writes := enjuYaml.WriteArtifacts{
		{Path: "go.mod", Track: true},
		{Path: "src/*.go", Track: true},
		{Path: "docs/", Track: true},
	}
	if err := wipeDeclaredWritesInDir(dir, writes); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	got := relPathsBelow(t, dir)
	want := []string{"untouched.txt"}
	if !equalSlice(got, want) {
		t.Errorf("after mixed-shape wipe, files = %v, want %v", got, want)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
