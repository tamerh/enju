package workspace

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestInitBareWithSeedScaffoldsTemplatesDir verifies that a
// freshly-initialized bare repo carries enju/templates/.gitkeep in
// its initial commit. Without this scaffolding, a new project's
// enju_list_templates call returns empty immediately after
// create_project — a confusing "looks broken" experience for
// users who expect the directory layout to already be present
// (as it is for enju_init-adopted folders).
func TestInitBareWithSeedScaffoldsTemplatesDir(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "seeded.git")
	if err := InitBareWithSeed(bareDir); err != nil {
		t.Fatalf("InitBareWithSeed: %v", err)
	}

	// Clone into a temp dir so we can see what the initial
	// commit actually contains without spelunking the bare.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	if _, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{
		URL:           bareDir,
		ReferenceName: plumbing.ReferenceName("refs/heads/main"),
	}); err != nil {
		t.Fatalf("clone seeded bare: %v", err)
	}

	// README.md — existing scaffold, verify we didn't regress.
	if _, err := os.Stat(filepath.Join(cloneDir, "README.md")); err != nil {
		t.Errorf("README.md missing from initial commit: %v", err)
	}

	// enju/templates/.gitkeep — the new scaffolding this test
	// locks in. Presence signals that enju_list_templates will
	// find a directory to scan (empty is a valid result; the
	// tool should never fail because the dir doesn't exist).
	if _, err := os.Stat(filepath.Join(cloneDir, "enju", "templates", ".gitkeep")); err != nil {
		t.Errorf("enju/templates/.gitkeep missing from initial commit: %v", err)
	}
}
