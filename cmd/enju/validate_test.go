package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateOneAcceptsCleanWorkflow exercises the happy path:
// a minimally well-formed YAML parses without errors or warnings.
// Tests run with --strict to assert that no warnings sneak in
// from the validator defaults (a regression would surface as a
// surprise false-positive).
func TestValidateOneAcceptsCleanWorkflow(t *testing.T) {
	p := writeTempYAML(t, `
name: smoke
description: trivial pipeline
version: 1
tasks:
  - id: t1
    action: answer
    prompt: hello
`)
	if !validateOne(p, true, true) {
		t.Fatalf("expected validateOne to accept a clean workflow")
	}
}

func TestValidateOneRejectsParseError(t *testing.T) {
	p := writeTempYAML(t, "::: not valid yaml :::")
	if validateOne(p, false, true) {
		t.Fatalf("expected validateOne to reject malformed YAML")
	}
}

func TestValidateOneMissingFile(t *testing.T) {
	if validateOne(filepath.Join(t.TempDir(), "nonexistent.yaml"), false, true) {
		t.Fatalf("expected validateOne to reject a missing file")
	}
}

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}
