package enjugit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectBundleFiles_SkipsSymlinks pins the load-bearing
// caller-side half of the ISSUE-2 fix: ListBundleFiles (git) lists
// symlinks like any blob, so collectBundleFiles MUST drop them —
// both a file symlink and, critically, a directory symlink (the
// `current -> checkv-db-v1.5` case that crashed os.ReadFile under
// the old filepath.Walk). Also asserts the exec bit survives.
func TestCollectBundleFiles_SkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("enju.yaml", "name: x\n")
	write("scripts/run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "scripts", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("realdir/keep.txt", "k")
	if err := os.Symlink("enju.yaml", filepath.Join(dir, "filelink")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if err := os.Symlink("realdir", filepath.Join(dir, "dirlink")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	// rels exactly as ListBundleFiles would hand them over — git
	// lists the symlinks; dropping them is collectBundleFiles' job.
	rels := []string{"enju.yaml", "scripts/run.sh", "filelink", "dirlink"}
	got, err := collectBundleFiles(dir, rels)
	if err != nil {
		t.Fatalf("collectBundleFiles must not crash on a dir symlink: %v", err)
	}
	by := map[string]FileWrite{}
	for _, f := range got {
		by[f.RepoRelPath] = f
	}
	if _, ok := by["filelink"]; ok {
		t.Error("file symlink must be skipped")
	}
	if _, ok := by["dirlink"]; ok {
		t.Error("directory symlink must be skipped (the ISSUE-2 crash case)")
	}
	if f, ok := by["enju.yaml"]; !ok || string(f.Content) != "name: x\n" || f.Mode != 0o644 {
		t.Errorf("enju.yaml: ok=%v %+v", ok, f)
	}
	if f, ok := by["scripts/run.sh"]; !ok || f.Mode != 0o755 {
		t.Errorf("exec bit not preserved: ok=%v mode=%o", ok, f.Mode)
	}
	if len(got) != 2 {
		t.Errorf("expected exactly the 2 real files, got %d (%v)", len(got), got)
	}
}

// TestCollectBundleFiles_ExcludesEnjuAndGit pins the deeper
// ISSUE-2 facet: enju's own run bookkeeping under .enju/runs/ can
// become *tracked* on the default branch (merged through from a
// task's iteration branch), so ListBundleFiles' --cached arm
// lists it despite .gitignore. The collector must drop .enju/ and
// .git/ unconditionally — pinning runtime state back into the
// next bundle snowballs the snapshot. The files exist on disk
// here (tracked-file simulation): exclusion must NOT depend on
// gitignore or tracked-status, only on the path.
func TestCollectBundleFiles_ExcludesEnjuAndGit(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("enju.yaml", "name: x\n")
	write("scripts/run.sh", "#!/bin/sh\n")
	write(".enju/runs/3/t/result.md", "leaked run bookkeeping")
	write(".enju/runs/3/t/context.json", "{}")
	write(".git/config", "[core]\n")

	// rels exactly as ListBundleFiles --cached would hand them
	// over once the .enju paths are tracked on the branch.
	rels := []string{
		"enju.yaml",
		"scripts/run.sh",
		".enju/runs/3/t/result.md",
		".enju/runs/3/t/context.json",
		".git/config",
	}
	got, err := collectBundleFiles(dir, rels)
	if err != nil {
		t.Fatalf("collectBundleFiles: %v", err)
	}
	for _, f := range got {
		if strings.HasPrefix(f.RepoRelPath, ".enju/") || strings.HasPrefix(f.RepoRelPath, ".git/") {
			t.Errorf("runtime/git path leaked into bundle: %q", f.RepoRelPath)
		}
	}
	if len(got) != 2 {
		t.Errorf("expected only the 2 recipe files, got %d (%v)", len(got), got)
	}
}
