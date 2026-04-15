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

// TestListAndLoadTemplate — drop a template file into the
// clone's templates/ directory and verify ListTemplates
// surfaces its metadata, LoadTemplate returns the raw bytes,
// and InstantiateTemplate substitutes supplied params.
func TestListAndLoadTemplate(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	templatesDir := filepath.Join(proj.WorkDir(), "templates")
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		t.Fatalf("mkdir templates: %v", err)
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
	if err := os.WriteFile(filepath.Join(templatesDir, "gwas.yaml"), template, 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	// ListTemplates surfaces the metadata.
	templates, err := proj.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected 1 template, got %d", len(templates))
	}
	if templates[0].Name != "GWAS analysis" {
		t.Errorf("name: got %q", templates[0].Name)
	}
	if len(templates[0].Params) != 2 {
		t.Errorf("expected 2 params, got %d", len(templates[0].Params))
	}

	// LoadTemplate returns raw bytes + summary.
	loaded, err := proj.LoadTemplate("templates/gwas.yaml")
	if err != nil {
		t.Fatalf("LoadTemplate: %v", err)
	}
	if len(loaded.Raw) == 0 {
		t.Error("expected raw bytes on LoadTemplate")
	}

	// InstantiateTemplate substitutes supplied values.
	parsed, _, err := proj.InstantiateTemplate("templates/gwas.yaml", map[string]interface{}{
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

	templatesDir := filepath.Join(proj.WorkDir(), "templates")
	_ = os.MkdirAll(templatesDir, 0o755)
	_ = os.WriteFile(filepath.Join(templatesDir, "r.yaml"), []byte(`name: "R"
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

	err := proj.ValidateTemplateParams("templates/r.yaml", map[string]interface{}{})
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

// TestLoadTemplateRejectsPathEscape — a template path with
// `../` is blocked so a malicious caller can't use the
// template tool as a file-read primitive for anything
// outside templates/.
func TestLoadTemplateRejectsPathEscape(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	_, err := proj.LoadTemplate("templates/../.git/config")
	if err == nil {
		t.Fatal("expected path-escape rejection, got nil")
	}
	if !strings.Contains(err.Error(), "disallowed") {
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
