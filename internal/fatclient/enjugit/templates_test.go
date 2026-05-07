package enjugit

import (
	"os"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// makeWorkflow + fakeOps already defined in state_prep_test.go.

func TestListTemplates_DefaultBranchEmpty(t *testing.T) {
	wf, _ := makeWorkflow(t)
	// resolveMap has no entry for default branch → no commits → empty list.
	out, err := wf.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty list when default branch has no commits, got %v", out)
	}
}

func TestListTemplates_FindsBundleSubdirs(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.treeEntries["tipsha:enju/templates"] = []git.TreeEntry{
		{Name: "alpha", IsDir: true},
		{Name: "beta", IsDir: true},
		{Name: "README.md", IsDir: false},
	}
	fake.readContent["tipsha:enju/templates/alpha/enju.yaml"] = []byte("name: Alpha\ndescription: first\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")
	fake.readContent["tipsha:enju/templates/beta/enju.yaml"] = []byte("name: Beta\ndescription: second\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")

	out, err := wf.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 templates, got %d (%+v)", len(out), out)
	}
	// Sorted alphabetically.
	if out[0].Path != "enju/templates/alpha/enju.yaml" {
		t.Errorf("first template: got %q", out[0].Path)
	}
	if out[0].Name != "Alpha" {
		t.Errorf("first name: got %q", out[0].Name)
	}
}

func TestListTemplates_SkipsSubdirsWithoutManifest(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.treeEntries["tipsha:enju/templates"] = []git.TreeEntry{
		{Name: "real-bundle", IsDir: true},
		{Name: "scratch-dir", IsDir: true},
	}
	fake.readContent["tipsha:enju/templates/real-bundle/enju.yaml"] = []byte("name: real\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")
	// scratch-dir has no manifest.

	out, err := wf.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("expected only the bundle with manifest, got %+v", out)
	}
}

func TestListTemplates_LegacyYamlEmitsMigrationHint(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.treeEntries["tipsha:enju/templates"] = []git.TreeEntry{
		{Name: "old-style.yaml", IsDir: false},
	}

	out, err := wf.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected one migration-hint entry, got %+v", out)
	}
	if out[0].ParseError == "" || !strings.Contains(out[0].ParseError, "legacy") {
		t.Errorf("expected migration-hint ParseError, got %q", out[0].ParseError)
	}
}

func TestListTemplates_BadManifestSurfacesParseError(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.treeEntries["tipsha:enju/templates"] = []git.TreeEntry{
		{Name: "broken", IsDir: true},
	}
	fake.readContent["tipsha:enju/templates/broken/enju.yaml"] = []byte("not: : valid: yaml::")

	out, err := wf.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatal("expected the broken bundle with ParseError")
	}
	if out[0].ParseError == "" {
		t.Errorf("expected ParseError populated for broken YAML, got %+v", out[0])
	}
}

func TestLoadTemplate_RejectsPathEscapes(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.LoadTemplate("enju/templates/../../etc/passwd")
	if err == nil {
		t.Error("expected error for path with .. components")
	}
}

func TestLoadTemplate_OutsideRoots(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.LoadTemplate("totally/elsewhere")
	if err == nil || !strings.Contains(err.Error(), "must live under") {
		t.Errorf("expected 'must live under' error, got %v", err)
	}
}

func TestLoadTemplate_DirForm(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	manifestBytes := []byte("name: gwas\ndescription: gwas template\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")
	fake.readContent["tipsha:enju/templates/gwas/enju.yaml"] = manifestBytes

	loaded, err := wf.LoadTemplate("enju/templates/gwas")
	if err != nil {
		t.Fatalf("LoadTemplate: %v", err)
	}
	if loaded.Path != "enju/templates/gwas/enju.yaml" {
		t.Errorf("Path: got %q", loaded.Path)
	}
	if loaded.BundleDir != "enju/templates/gwas" {
		t.Errorf("BundleDir: got %q", loaded.BundleDir)
	}
}

func TestLoadTemplate_ManifestForm(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.readContent["tipsha:enju/templates/x/enju.yaml"] = []byte("name: x\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")

	loaded, err := wf.LoadTemplate("enju/templates/x/enju.yaml")
	if err != nil {
		t.Fatalf("LoadTemplate: %v", err)
	}
	if loaded.BundleDir != "enju/templates/x" {
		t.Errorf("BundleDir: got %q", loaded.BundleDir)
	}
}

func TestLoadTemplate_ManifestAtRootRejected(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.LoadTemplate("enju/templates/enju.yaml")
	if err == nil || !strings.Contains(err.Error(), "must live inside a bundle subdirectory") {
		t.Errorf("expected bundle-subdir error, got %v", err)
	}
}

func TestLoadTemplate_LegacySingleFile(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.LoadTemplate("enju/templates/old-style.yaml")
	if err == nil || !strings.Contains(err.Error(), "legacy single-file") {
		t.Errorf("expected legacy migration hint, got %v", err)
	}
}

func TestLoadTemplate_WorktreeFallback(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Default branch resolves to nothing; tree empty.
	tmp := t.TempDir()
	manifestPath := tmp + "/enju/templates/draft/enju.yaml"
	if err := os.MkdirAll(tmp+"/enju/templates/draft", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("name: draft\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Workflow.WorkDir() reads from the underlying git.Ops via
	// type assertion; fakeOps doesn't implement WorkDir, so the
	// fallback path won't see tmp. Wire workdir through a helper
	// override so we can exercise the worktree-fallback path.
	fake.workDir = tmp
	loaded, err := wf.LoadTemplate("enju/templates/draft")
	if err != nil {
		t.Fatalf("worktree fallback: %v", err)
	}
	if loaded.Summary.Name != "draft" {
		t.Errorf("expected name 'draft', got %q", loaded.Summary.Name)
	}
}

func TestReadBundleFiles_Empty(t *testing.T) {
	wf, _ := makeWorkflow(t)
	// No commits on default branch → friendly error.
	_, err := wf.ReadBundleFiles("enju/templates/x", "snapshot/x")
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Errorf("expected 'no commits' error, got %v", err)
	}
}

func TestReadBundleFiles_RebasePathsToTarget(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.walkBlobs["tipsha:enju/templates/demo"] = map[string]struct {
		Mode    os.FileMode
		Content []byte
	}{
		"enju.yaml":      {Mode: 0o644, Content: []byte("name: demo")},
		"scripts/run.sh": {Mode: 0o755, Content: []byte("#!/bin/bash")},
	}

	files, err := wf.ReadBundleFiles("enju/templates/demo", "enju/runs/1/templates/demo")
	if err != nil {
		t.Fatalf("ReadBundleFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	paths := map[string]os.FileMode{}
	for _, f := range files {
		paths[f.RepoRelPath] = f.Mode
	}
	if _, ok := paths["enju/runs/1/templates/demo/enju.yaml"]; !ok {
		t.Errorf("manifest path not rebased: %v", paths)
	}
	if got := paths["enju/runs/1/templates/demo/scripts/run.sh"]; got != 0o755 {
		t.Errorf("script should preserve +x via 0o755 mode, got %v", got)
	}
}

func TestResolveBundlePathShape_DirAndManifest(t *testing.T) {
	roots := []string{"enju/templates"}
	bundleDir, manifestPath, err := resolveBundlePathShape("enju/templates/foo", roots)
	if err != nil {
		t.Fatal(err)
	}
	if bundleDir != "enju/templates/foo" || manifestPath != "enju/templates/foo/enju.yaml" {
		t.Errorf("dir form: got bundleDir=%q manifestPath=%q", bundleDir, manifestPath)
	}
	bundleDir, manifestPath, err = resolveBundlePathShape("enju/templates/bar/enju.yaml", roots)
	if err != nil {
		t.Fatal(err)
	}
	if bundleDir != "enju/templates/bar" || manifestPath != "enju/templates/bar/enju.yaml" {
		t.Errorf("manifest form: got bundleDir=%q manifestPath=%q", bundleDir, manifestPath)
	}
}
