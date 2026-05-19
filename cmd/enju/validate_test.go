package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestStrictPlainTextGlyphMatchesVerdict — C4. Under -strict a
// warning-bearing file FAILS (exit 4, ok:false in -json). The
// human-mode glyph must be ✗, not the success ✓ it used to print
// while exiting non-zero (a self-contradiction only visible via
// `echo $?`).
func TestStrictPlainTextGlyphMatchesVerdict(t *testing.T) {
	// `params:` with no description emits a non-fatal warning,
	// which -strict promotes to a failure.
	p := writeTempYAML(t, `
name: warns
version: 1
params:
  - { name: foo, type: string, required: true }
tasks:
  - id: t
    action: answer
    prompt: "use {{foo}}"
`)

	plain := captureStdout(t, func() {
		if validateOne(p, false, false) != true {
			t.Errorf("non-strict: warning-bearing file should still pass (ok=true)")
		}
	})
	if !strings.Contains(plain, "✓") || strings.Contains(plain, "✗") {
		t.Errorf("non-strict plain output should show ✓, got:\n%s", plain)
	}

	strict := captureStdout(t, func() {
		if validateOne(p, true, false) != false {
			t.Errorf("strict: warning-bearing file must fail (ok=false)")
		}
	})
	if strings.Contains(strict, "✓ "+filepath.Base(p)) || strings.Contains(strict, "✓ "+p) {
		t.Errorf("strict plain output must NOT show the success glyph for a failing file, got:\n%s", strict)
	}
	if !strings.Contains(strict, "✗") {
		t.Errorf("strict plain output must show ✗ for a failing file, got:\n%s", strict)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and
// returns everything it wrote. emitReport prints via fmt.Printf
// (package-level os.Stdout), so the redirect is the only seam.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
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
