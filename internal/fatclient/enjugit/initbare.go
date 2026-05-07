package enjugit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initbare.go — bare-repo initialization helpers.
//
// These produce or seed bare repos at a path. They sit in
// enjugit (not the git layer) because the seed contents
// (README.md text, enju/templates/.gitkeep) are an Enju policy
// — git knows nothing about templates dirs.
//
// PromoteWorkingTreeToBare bridges adopted working trees into a
// bare-backed shape so bots have a non-working-tree push target.
// InitBareEmpty / InitBareWithSeed are kept for test fixtures.

// PromoteWorkingTreeToBare creates a bare repo at barePath that
// mirrors the operator's working tree at workTreePath, and
// rewires the working tree's `origin` remote to point at the
// new bare. Used by `enju bot setup` when the project lacks a
// real remote (created via enju_init or enju_create_project
// --path) — bots need a non-working-tree push target, and this
// promotion produces one without disturbing the operator's
// existing folder layout.
//
// Idempotent: if barePath already exists with a valid bare
// repo, returns nil immediately. The "bare exists" detection is
// presence-of-`HEAD`; a half-initialized stub at the path is
// surfaced as an error so a subsequent retry sees a clear state.
func PromoteWorkingTreeToBare(workTreePath, barePath string) error {
	if !IsLocalWorkingTree(workTreePath) {
		return fmt.Errorf("workTreePath %q is not a local git working tree", workTreePath)
	}

	// Idempotency: a valid existing bare at this path means an
	// earlier call already promoted. The HEAD file is what gogit
	// needs to PlainOpen the bare, so its presence is a
	// reasonable "this is a real bare" signal.
	if _, err := os.Stat(filepath.Join(barePath, "HEAD")); err == nil {
		if _, err := gogit.PlainOpen(barePath); err == nil {
			return setOrReplaceOrigin(workTreePath, barePath)
		}
	}

	// Init the bare, then fetch with a mirror refspec from the
	// working tree. PlainClone(bare=true) only brings the HEAD
	// branch by default; we need every branch (main + run +
	// topics) so bots can fork their own topic branches.
	// `+refs/heads/*:refs/heads/*` is the standard mirror refspec.
	if err := os.MkdirAll(filepath.Dir(barePath), 0755); err != nil {
		return fmt.Errorf("creating parent dir for bare: %w", err)
	}
	bareRepo, err := gogit.PlainInitWithOptions(barePath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	})
	if err != nil {
		return fmt.Errorf("initializing bare at %q: %w", barePath, err)
	}
	if _, err := bareRepo.CreateRemote(&config.RemoteConfig{
		Name: "source",
		URLs: []string{workTreePath},
		Fetch: []config.RefSpec{
			config.RefSpec("+refs/heads/*:refs/heads/*"),
		},
	}); err != nil {
		return fmt.Errorf("adding source remote on bare: %w", err)
	}
	if err := bareRepo.Fetch(&gogit.FetchOptions{
		RemoteName: "source",
		RefSpecs: []config.RefSpec{
			config.RefSpec("+refs/heads/*:refs/heads/*"),
		},
	}); err != nil {
		return fmt.Errorf("fetching branches from %q into bare: %w", workTreePath, err)
	}
	_ = bareRepo.DeleteRemote("source")

	if err := setOrReplaceOrigin(workTreePath, barePath); err != nil {
		return fmt.Errorf("setting origin on %q: %w", workTreePath, err)
	}
	return nil
}

// setOrReplaceOrigin sets the `origin` remote on a working tree
// to bareURL, replacing any existing `origin`. Idempotent: if
// `origin` already points at bareURL the function is a no-op.
func setOrReplaceOrigin(workTreePath, bareURL string) error {
	repo, err := gogit.PlainOpen(workTreePath)
	if err != nil {
		return fmt.Errorf("open working tree: %w", err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if existing, ok := cfg.Remotes["origin"]; ok {
		for _, u := range existing.URLs {
			if u == bareURL {
				return nil
			}
		}
		if err := repo.DeleteRemote("origin"); err != nil {
			return fmt.Errorf("removing existing origin: %w", err)
		}
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bareURL},
	}); err != nil {
		return fmt.Errorf("creating origin: %w", err)
	}
	return nil
}

// InitBareEmpty creates an empty bare git repo at the given
// path with no refs and no commits. Useful when an existing
// working tree's commits will be pushed in as the initial state
// — a separate seed would create a divergent root that can't
// merge with what's already there.
//
// Production callers used to be enju_init's auto-bare path;
// after Option B (solo-mode default), no production path
// creates bares anymore — kept for test fixtures that need to
// construct fake remotes.
func InitBareEmpty(bareDir string) error {
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		return fmt.Errorf("creating bare dir: %w", err)
	}
	_, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	})
	if err != nil {
		return fmt.Errorf("init bare: %w", err)
	}
	return nil
}

// InitBareWithSeed creates a bare git repo at the given path
// with one initial commit (a README + enju/templates/.gitkeep).
// Needed so PlainClone can clone from it — an empty bare repo
// with no refs/HEAD fails to clone.
//
// Test-only: production paths don't create bares anymore.
func InitBareWithSeed(bareDir string) error {
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		return fmt.Errorf("creating bare dir: %w", err)
	}
	_, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	})
	if err != nil {
		return fmt.Errorf("init bare: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "enju-seed-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	repo, err := gogit.PlainInitWithOptions(tmpDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		return fmt.Errorf("init seed: %w", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	}); err != nil {
		return fmt.Errorf("create remote: %w", err)
	}

	if err := writeSeedFiles(tmpDir); err != nil {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		return fmt.Errorf("add README: %w", err)
	}
	gitkeepRel := filepath.ToSlash(filepath.Join(corelayout.DefaultTemplatesDir, ".gitkeep"))
	if _, err := wt.Add(gitkeepRel); err != nil {
		return fmt.Errorf("add .gitkeep: %w", err)
	}
	sig := &object.Signature{
		Name:  "Enju",
		Email: "enju@localhost",
		When:  time.Now(),
	}
	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{
		Author:    sig,
		Committer: sig,
	}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// writeSeedFiles writes the canonical README + enju/templates/
// .gitkeep into a fresh working tree. Shared by InitBareWithSeed
// and (in future) any local-init seeding helper that needs the
// same starting layout.
func writeSeedFiles(workDir string) error {
	readmeBody := "# Enju project\n\n" +
		"Everything Enju-owned lives under `enju/`:\n\n" +
		"```\n" +
		"enju/\n" +
		"  templates/              # reusable run recipes (edit these)\n" +
		"    my-template/\n" +
		"      enju.yaml           # run definition (required)\n" +
		"      scripts/            # bundled scripts for compute tasks\n" +
		"      README.md           # author-facing docs (optional)\n" +
		"  runs/                   # per-run results + audit trail (tool output)\n" +
		"  conf.yaml               # optional project config\n" +
		"```\n\n" +
		"Override the templates location by creating `enju/conf.yaml` with a\n" +
		"`templates:` list of repo-relative paths.\n"
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte(readmeBody), 0644); err != nil {
		return fmt.Errorf("write readme: %w", err)
	}
	templatesDir := filepath.Join(workDir, corelayout.DefaultTemplatesDir)
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		return fmt.Errorf("create templates dir: %w", err)
	}
	gitkeepRel := filepath.ToSlash(filepath.Join(corelayout.DefaultTemplatesDir, ".gitkeep"))
	if err := os.WriteFile(filepath.Join(workDir, gitkeepRel), []byte(""), 0644); err != nil {
		return fmt.Errorf("write .gitkeep: %w", err)
	}
	return nil
}
