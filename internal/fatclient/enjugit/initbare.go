package enjugit

import (
	"fmt"
	"os"
	"path/filepath"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitv6"
)

// initbare.go — bare-repo initialization helpers.
//
// These produce or seed bare repos at a path. They sit in
// enjugit (not the git layer) because the seed contents
// (README.md text, enju/templates/.gitkeep) are an Enju policy
// — git knows nothing about templates dirs. The git operations
// themselves live in the git package (git.InitEmptyBare,
// git.InitBareWithMirrorFetch, git.SetOriginOnWorkTree) so
// enjugit doesn't import go-git directly — that's the seam.
//
// PromoteWorkingTreeToBare bridges adopted working trees into a
// bare-backed shape so bots have a non-working-tree push target.
// InitBareEmpty / InitBareWithSeed are kept for test fixtures.

// PromoteWorkingTreeToBare creates a bare repo at barePath that
// mirrors the operator's working tree at workTreePath, and
// rewires the working tree's `origin` remote to point at the
// new bare. Used by `enju bot setup` when the project lacks a
// real remote.
//
// Idempotent: if barePath already holds a valid bare, returns
// nil after re-pointing the working tree's origin.
func PromoteWorkingTreeToBare(workTreePath, barePath string) error {
	if !IsLocalWorkingTree(workTreePath) {
		return fmt.Errorf("workTreePath %q is not a local git working tree", workTreePath)
	}
	if git.HasBare(barePath) {
		return git.SetOriginOnWorkTree(workTreePath, barePath)
	}
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		return fmt.Errorf("creating parent dir for bare: %w", err)
	}
	if err := git.InitBareWithMirrorFetch(barePath, workTreePath); err != nil {
		return err
	}
	if err := git.SetOriginOnWorkTree(workTreePath, barePath); err != nil {
		return fmt.Errorf("setting origin on %q: %w", workTreePath, err)
	}
	return nil
}

// InitBareEmpty creates an empty bare git repo at the given
// path with no refs and no commits. Test fixtures use this to
// construct fake remotes.
func InitBareEmpty(bareDir string) error {
	return git.InitEmptyBare(bareDir, "main")
}

// InitBareWithSeed creates a bare git repo at the given path
// with one initial commit (a README + enju/templates/.gitkeep)
// pushed in via a temporary working tree. Test fixtures use this
// when they need a clonable bare.
//
// Composition: InitEmptyBare + a temporary git.InitLocal
// (already seeds README + enju/templates/.gitkeep) + push.
func InitBareWithSeed(bareDir string) error {
	if err := git.InitEmptyBare(bareDir, "main"); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "enju-seed-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	tmpClone, err := git.InitLocal(tmpDir, "", nil)
	if err != nil {
		return fmt.Errorf("init temp seed working tree: %w", err)
	}
	if err := tmpClone.EnsureOrigin(bareDir); err != nil {
		return fmt.Errorf("set origin on temp seed: %w", err)
	}
	if err := tmpClone.Push("main"); err != nil {
		return fmt.Errorf("push seed to bare: %w", err)
	}
	return nil
}

