package enjugit

import (
	"os"
	"strings"
	"testing"
)

// makeWorkflow + fakeOps already defined in state_prep_test.go.

func TestListWorkflows_DefaultBranchEmpty(t *testing.T) {
	wf, _ := makeWorkflow(t)
	// resolveMap has no entry for default branch → no commits → empty list.
	out, err := wf.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty list when default branch has no commits, got %v", out)
	}
}

func TestListWorkflows_ReturnsEveryYAMLInRepo(t *testing.T) {
	// Verb-level contract: returns every *.yaml / *.yml in the
	// default-branch tree. No content sniffing — paths only.
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.blobPaths["tipsha"] = []string{
		"README.md",
		"enju.yaml",
		"workflows/gwas/enju.yaml",
		"workflows/eval/recipe.yaml",
		"unrelated.yml",
		"scripts/run.py",
		"data/notes.txt",
	}

	out, err := wf.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	// All four YAML paths, sorted; non-YAML excluded.
	want := []string{
		"enju.yaml",
		"unrelated.yml",
		"workflows/eval/recipe.yaml",
		"workflows/gwas/enju.yaml",
	}
	if len(out) != len(want) {
		t.Fatalf("got %d entries, want %d (%+v)", len(out), len(want), out)
	}
	for i, w := range want {
		if out[i].Path != w {
			t.Errorf("entry %d: got %q, want %q", i, out[i].Path, w)
		}
	}
}

func TestListWorkflows_ExcludesHiddenDirectories(t *testing.T) {
	// Any path whose component starts with '.' is dropped:
	// .git/, .enju/, .github/, .vscode/, and a deep .hidden/sub.
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.blobPaths["tipsha"] = []string{
		"enju.yaml",
		".github/workflows/ci.yaml",
		".enju/runs/1/snapshot/enju.yaml",
		"tools/.hidden/secret.yaml",
		"workflows/visible.yaml",
		".enju.yaml", // leading-dot file at root — also excluded by the symmetric rule.
	}

	out, err := wf.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	want := []string{"enju.yaml", "workflows/visible.yaml"}
	if len(out) != len(want) {
		t.Fatalf("got %d entries, want %d (%+v)", len(out), len(want), out)
	}
	for i, w := range want {
		if out[i].Path != w {
			t.Errorf("entry %d: got %q, want %q", i, out[i].Path, w)
		}
	}
}

func TestListWorkflows_NoContentRead(t *testing.T) {
	// Path-only contract: ListWorkflows must NOT read blob contents.
	// We assert that by injecting paths the fake has no readContent
	// for; if the verb tried to read, the fake would return an error
	// or empty content silently — either way, ListWorkflows must not
	// surface name/description/params (those fields don't exist on
	// WorkflowSummary at all).
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.blobPaths["tipsha"] = []string{"enju.yaml", "workflows/foo/enju.yaml"}
	// Deliberately leave fake.readContent empty.

	out, err := wf.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows must not depend on blob content: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 paths, got %+v", out)
	}
}

func TestLoadWorkflow_RejectsPathEscapes(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.LoadWorkflow("workflows/../../etc/passwd")
	if err == nil {
		t.Error("expected error for path with .. components")
	}
}

func TestLoadWorkflow_DirForm(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	manifestBytes := []byte("name: gwas\ndescription: gwas workflow\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")
	fake.readContent["tipsha:workflows/gwas/enju.yaml"] = manifestBytes

	loaded, err := wf.LoadWorkflow("workflows/gwas")
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if loaded.Path != "workflows/gwas/enju.yaml" {
		t.Errorf("Path: got %q", loaded.Path)
	}
	if loaded.BundleDir != "workflows/gwas" {
		t.Errorf("BundleDir: got %q", loaded.BundleDir)
	}
	if loaded.Details.Name != "gwas" {
		t.Errorf("Details.Name: got %q", loaded.Details.Name)
	}
}

func TestLoadWorkflow_ManifestForm(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.readContent["tipsha:workflows/x/enju.yaml"] = []byte("name: x\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")

	loaded, err := wf.LoadWorkflow("workflows/x/enju.yaml")
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if loaded.BundleDir != "workflows/x" {
		t.Errorf("BundleDir: got %q", loaded.BundleDir)
	}
}

func TestLoadWorkflow_RootLevelYAML(t *testing.T) {
	// A workflow YAML can live at the repo root (e.g. enju.yaml).
	// LoadWorkflow treats the path as the manifest and resolves
	// bundleDir to "" (root).
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.readContent["tipsha:my-workflow.yaml"] = []byte("name: root-workflow\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n")

	loaded, err := wf.LoadWorkflow("my-workflow.yaml")
	if err != nil {
		t.Fatalf("LoadWorkflow root-level yaml: %v", err)
	}
	if loaded.Path != "my-workflow.yaml" {
		t.Errorf("Path: got %q, want my-workflow.yaml", loaded.Path)
	}
	if loaded.BundleDir != "" {
		t.Errorf("BundleDir for root-level yaml: got %q, want empty string", loaded.BundleDir)
	}
}

func TestLoadWorkflow_WorktreeFallback(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Default branch resolves to nothing; tree empty.
	tmp := t.TempDir()
	manifestPath := tmp + "/workflows/draft/enju.yaml"
	if err := os.MkdirAll(tmp+"/workflows/draft", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("name: draft\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.workDir = tmp
	loaded, err := wf.LoadWorkflow("workflows/draft")
	if err != nil {
		t.Fatalf("worktree fallback: %v", err)
	}
	if loaded.Details.Name != "draft" {
		t.Errorf("expected name 'draft', got %q", loaded.Details.Name)
	}
}

// TestLoadWorkflow_RejectsUncommittedDivergence pins the
// showcase_v16 fix: when the worktree's copy of the workflow YAML
// differs from the default-branch commit, LoadWorkflow must
// refuse rather than silently use the committed bytes. Path=
// runs execute against the committed tree, so quietly validating
// the OLD bytes is a trap.
func TestLoadWorkflow_RejectsUncommittedDivergence(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.readContent["tipsha:enju.yaml"] = []byte("name: old-version\nversion: 1\ntasks:\n  - id: t\n    action: answer\n    prompt: hi\n")

	tmp := t.TempDir()
	if err := os.WriteFile(tmp+"/enju.yaml",
		[]byte("name: new-version\nversion: 1\ntasks:\n  - id: t\n    action: answer\n    prompt: hi\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	fake.workDir = tmp

	_, err := wf.LoadWorkflow("enju.yaml")
	if err == nil {
		t.Fatal("LoadWorkflow should refuse when worktree diverges from committed bytes")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("error should mention uncommitted changes, got: %v", err)
	}
}

// TestLoadWorkflow_AcceptsMatchingWorktreeAndCommit pins the
// no-false-positive case: when the worktree exactly matches the
// committed bytes, LoadWorkflow succeeds.
func TestLoadWorkflow_AcceptsMatchingWorktreeAndCommit(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	body := []byte("name: matched\nversion: 1\ntasks:\n  - id: t\n    action: answer\n    prompt: hi\n")
	fake.readContent["tipsha:enju.yaml"] = body

	tmp := t.TempDir()
	if err := os.WriteFile(tmp+"/enju.yaml", body, 0o644); err != nil {
		t.Fatal(err)
	}
	fake.workDir = tmp

	loaded, err := wf.LoadWorkflow("enju.yaml")
	if err != nil {
		t.Fatalf("matching worktree/commit should load cleanly: %v", err)
	}
	if loaded.Details.Name != "matched" {
		t.Errorf("expected name=matched, got %q", loaded.Details.Name)
	}
}

// TestLoadTemplate_DeprecatedAliasStillWorks pins the back-compat
// wrapper so webui (and any other lingering caller) keeps building
// while the rename ripples through. Once the other agent migrates
// webui to LoadWorkflow, both this test and the wrapper can go.
func TestLoadTemplate_DeprecatedAliasStillWorks(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.readContent["tipsha:enju.yaml"] = []byte("name: legacy\nversion: 1\ntasks:\n  - id: t\n    action: answer\n    prompt: hi\n")

	loaded, err := wf.LoadTemplate("enju.yaml")
	if err != nil {
		t.Fatalf("deprecated LoadTemplate alias: %v", err)
	}
	if loaded.Summary.Name != "legacy" {
		t.Errorf("expected Summary.Name=legacy, got %q", loaded.Summary.Name)
	}
}

func TestReadBundleFiles_Empty(t *testing.T) {
	wf, _ := makeWorkflow(t)
	// No commits on default branch → friendly error.
	_, err := wf.ReadBundleFiles("workflows/x", "snapshot/x")
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Errorf("expected 'no commits' error, got %v", err)
	}
}

func TestReadBundleFiles_RebasePathsToTarget(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "tipsha"
	fake.walkBlobs["tipsha:workflows/demo"] = map[string]struct {
		Mode    os.FileMode
		Content []byte
	}{
		"enju.yaml":      {Mode: 0o644, Content: []byte("name: demo")},
		"scripts/run.sh": {Mode: 0o755, Content: []byte("#!/bin/bash")},
	}

	files, err := wf.ReadBundleFiles("workflows/demo", ".enju/runs/1/snapshot")
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
	if _, ok := paths[".enju/runs/1/snapshot/enju.yaml"]; !ok {
		t.Errorf("manifest path not rebased: %v", paths)
	}
	if got := paths[".enju/runs/1/snapshot/scripts/run.sh"]; got != 0o755 {
		t.Errorf("script should preserve +x via 0o755 mode, got %v", got)
	}
}

func TestResolveBundlePathShape(t *testing.T) {
	cases := []struct {
		in            string
		wantBundleDir string
		wantManifest  string
	}{
		// Dir form — manifest assumed at <dir>/enju.yaml.
		{"workflows/foo", "workflows/foo", "workflows/foo/enju.yaml"},
		// Manifest form — explicit *.yaml path is the manifest.
		// bundleDir = its containing dir.
		{"workflows/bar/enju.yaml", "workflows/bar", "workflows/bar/enju.yaml"},
		{"tools/lint/lint-workflow.yaml", "tools/lint", "tools/lint/lint-workflow.yaml"},
		// Root-level YAML — bundleDir collapses to empty string
		// (the project root).
		{"workflow.yaml", "", "workflow.yaml"},
		{"my-pipeline.yml", "", "my-pipeline.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			bundleDir, manifestPath, err := resolveBundlePathShape(tc.in)
			if err != nil {
				t.Fatalf("resolveBundlePathShape(%q): %v", tc.in, err)
			}
			if bundleDir != tc.wantBundleDir {
				t.Errorf("bundleDir: got %q, want %q", bundleDir, tc.wantBundleDir)
			}
			if manifestPath != tc.wantManifest {
				t.Errorf("manifestPath: got %q, want %q", manifestPath, tc.wantManifest)
			}
		})
	}
}
