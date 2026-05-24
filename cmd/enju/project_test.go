package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// TestRunProjectDefaultBranch_UnregisteredProject pins the project
// resolution path for `enju project default-branch` — the same
// CLI-resolution surface where the H1 bughunt nil-deref lived. An
// unregistered --project id must surface a clear usage error (and a
// zero project id, so the command exits 2) BEFORE any coordinator
// call, rather than resolving to a nil entry.
func TestRunProjectDefaultBranch_UnregisteredProject(t *testing.T) {
	dir := t.TempDir()
	reg := projectreg.Open(filepath.Join(dir, "projects.json"))
	// A registered project at id 42 (path must exist for reg.Get).
	if err := reg.Register(projectreg.Entry{ID: 42, LocalPath: dir}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	fc := service.New(service.Config{ProjectRegistry: reg})
	sess := &cliSession{FC: fc}

	projID, _, err := runProjectDefaultBranch(sess, 99999, "main")
	if err == nil {
		t.Fatal("expected an error for an unregistered --project id")
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Errorf("error should name the missing id, got: %v", err)
	}
	if projID != 0 {
		t.Errorf("a resolution failure must report project id 0 (usage-error discriminator), got %d", projID)
	}
}
