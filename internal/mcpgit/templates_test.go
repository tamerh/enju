package mcpgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestListTemplatesEmpty — an empty / missing templates/
// directory returns an empty slice, not an error. "No
// templates yet" is a normal state and ListTemplates is
// safe to call on any project clone.
func TestListTemplatesEmpty(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, err := NewWorkspace(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	templates, err := proj.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates on empty project: %v", err)
	}
	if len(templates) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(templates))
	}
}

// TestListAndLoadTemplate — drop a template bundle into the
// clone's enju_templates/ directory and verify ListTemplates
// surfaces its metadata, LoadTemplate returns the raw bytes,
// and InstantiateTemplate substitutes supplied params. Also
// covers the caller-supplied forms (dir path and full
// manifest path).
func TestListAndLoadTemplate(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	bundleDir := filepath.Join(proj.WorkDir(), "enju_templates", "gwas")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	template := []byte(`name: "GWAS analysis"
description: "Analyze GWAS summary stats for a disease."
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "The disease to analyze (e.g. endometriosis, PCOS)"
  - name: tissue
    type: string
    default: "whole blood"
    description: "Primary tissue"
tasks:
  - id: gwas
    action: answer
    prompt: "Analyze GWAS data for {{disease}} in {{tissue}}"
`)
	if err := os.WriteFile(filepath.Join(bundleDir, "template.yaml"), template, 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	// ListTemplates surfaces the metadata.
	templates, err := proj.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected 1 template, got %d: %+v", len(templates), templates)
	}
	if templates[0].Name != "GWAS analysis" {
		t.Errorf("name: got %q", templates[0].Name)
	}
	if len(templates[0].Params) != 2 {
		t.Errorf("expected 2 params, got %d", len(templates[0].Params))
	}
	if templates[0].Path != "enju_templates/gwas/template.yaml" {
		t.Errorf("expected summary path to be the manifest, got %q", templates[0].Path)
	}

	// LoadTemplate accepts both the dir form and the full
	// manifest path — both resolve to the same bundle.
	for _, ref := range []string{"enju_templates/gwas", "enju_templates/gwas/template.yaml"} {
		loaded, err := proj.LoadTemplate(ref)
		if err != nil {
			t.Fatalf("LoadTemplate(%q): %v", ref, err)
		}
		if len(loaded.Raw) == 0 {
			t.Errorf("LoadTemplate(%q): expected raw bytes", ref)
		}
		if loaded.BundleDir != "enju_templates/gwas" {
			t.Errorf("LoadTemplate(%q): BundleDir = %q, want enju_templates/gwas", ref, loaded.BundleDir)
		}
		if loaded.Path != "enju_templates/gwas/template.yaml" {
			t.Errorf("LoadTemplate(%q): Path = %q, want the manifest", ref, loaded.Path)
		}
	}

	// InstantiateTemplate substitutes supplied values.
	parsed, _, err := proj.InstantiateTemplate("enju_templates/gwas", map[string]interface{}{
		"disease": "PCOS",
	})
	if err != nil {
		t.Fatalf("InstantiateTemplate: %v", err)
	}
	got := parsed.Run.Tasks[0].Prompt
	want := "Analyze GWAS data for PCOS in whole blood"
	if got != want {
		t.Errorf("prompt substitution wrong\n  got:  %q\n  want: %q", got, want)
	}
}

// TestInstantiateTemplateMissingRequired — errors from
// ParseWithParams propagate up with their natural-language
// phrasing so the MCP handler can forward them to the LLM.
func TestInstantiateTemplateMissingRequired(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	bundleDir := filepath.Join(proj.WorkDir(), "enju_templates", "r")
	_ = os.MkdirAll(bundleDir, 0o755)
	_ = os.WriteFile(filepath.Join(bundleDir, "template.yaml"), []byte(`name: "R"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "The disease to analyze (e.g. endometriosis, PCOS)"
tasks:
  - id: t
    action: answer
    prompt: "x {{disease}}"
`), 0o644)

	err := proj.ValidateTemplateParams("enju_templates/r", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected missing-required error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required parameter") {
		t.Errorf("expected 'missing required parameter', got: %v", err)
	}
	if !strings.Contains(err.Error(), "The disease to analyze") {
		t.Errorf("expected description in error, got: %v", err)
	}
}

// TestListTemplatesLegacyFileShape — a loose .yaml directly
// under enju_templates/ (the pre-bundle convention) shows up
// with a migration-hint ParseError rather than being silently
// skipped. Users who upgrade their enju install keep getting
// a visible, actionable menu entry until they migrate.
func TestListTemplatesLegacyFileShape(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	templatesDir := filepath.Join(proj.WorkDir(), "enju_templates")
	_ = os.MkdirAll(templatesDir, 0o755)
	_ = os.WriteFile(filepath.Join(templatesDir, "legacy.yaml"),
		[]byte("name: legacy\nversion: 1\ntasks: [{id: t, action: answer, prompt: x}]\n"),
		0o644)

	templates, err := proj.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected 1 migration-hint entry, got %d: %+v", len(templates), templates)
	}
	if templates[0].ParseError == "" {
		t.Error("expected ParseError with migration hint, got empty")
	}
	if !strings.Contains(templates[0].ParseError, "legacy single-file template") {
		t.Errorf("expected migration hint in ParseError, got %q", templates[0].ParseError)
	}
}

// TestLoadTemplateRejectsPathEscape — a template path with
// `../` is blocked so a malicious caller can't use the
// template tool as a file-read primitive for anything
// outside templates/.
func TestLoadTemplateRejectsPathEscape(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	_, err := proj.LoadTemplate("enju_templates/../.git/config")
	if err == nil {
		t.Fatal("expected path-escape rejection, got nil")
	}
	if !strings.Contains(err.Error(), "disallowed") && !strings.Contains(err.Error(), "must live under") {
		t.Errorf("expected disallowed-path error, got: %v", err)
	}
}

// TestLoadTemplateRejectsOutsideTemplatesDir — paths that
// don't start with templates/ are rejected. The templates
// directory is the only on-disk namespace the tool will
// read from.
func TestLoadTemplateRejectsOutsideTemplatesDir(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	_, err := proj.LoadTemplate("runs/1/result.md")
	if err == nil {
		t.Fatal("expected outside-templates-dir rejection, got nil")
	}
	if !strings.Contains(err.Error(), "templates/") {
		t.Errorf("expected templates/ prefix error, got: %v", err)
	}
}
