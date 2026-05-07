package project

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
//
// What it does, step by step:
//
//  1. Bare-clone the working tree: `git clone --bare workTreePath
//     barePath`. The bare gets every branch the working tree
//     currently knows about (main + any topic branches in the
//     `.git/refs/heads/` of the working tree).
//  2. Open the working tree and set its `origin` remote to
//     `barePath`. Replaces an existing `origin` if there is one
//     (rare for adopted dirs; common if the operator had been
//     using github at some point and we're now layering a local
//     bare in addition — caller's responsibility to confirm this
//     is the desired wiring).
//
// What it does NOT do:
//
//   - Does not push the working tree to the bare a second time.
//     The bare-clone in step 1 already captured every ref.
//   - Does not pull from the bare back into the working tree.
//     The trees are identical by construction; the working tree
//     just needs `origin` set so future pulls/pushes route
//     through the bare.
//   - Does not update the coord's project record. The caller
//     (service.SetupBareForBots) handles that — this primitive
//     stays workspace-pure.
func PromoteWorkingTreeToBare(workTreePath, barePath string) error {
	if !IsLocalWorkingTree(workTreePath) {
		return fmt.Errorf("workTreePath %q is not a local git working tree", workTreePath)
	}

	// Idempotency: a valid existing bare at this path means an
	// earlier call already promoted. The HEAD file is what gogit
	// needs to PlainOpen the bare, so its presence is a
	// reasonable "this is a real bare" signal. A half-formed dir
	// (mkdir-ed but not init-ed) returns an error from PlainOpen
	// which we surface as-is so the operator notices.
	if _, err := os.Stat(filepath.Join(barePath, "HEAD")); err == nil {
		if _, err := gogit.PlainOpen(barePath); err == nil {
			// Bare exists and is openable. Even so, ensure the
			// working tree's origin points at it — the bare
			// could have been created in a prior run while
			// origin-rewiring failed mid-flight.
			return setOrReplaceOrigin(workTreePath, barePath)
		}
	}

	// Init the bare, then fetch with a mirror refspec from the
	// working tree. We don't use PlainClone here because gogit's
	// PlainClone(bare=true) only brings the HEAD branch by
	// default, even with SingleBranch=false. The bot's flow
	// needs every branch (main + run branches + leftover topics
	// — anything `git branch -a` shows in the operator's tree)
	// so it can fork its own topic branches and FF-merge cleanly.
	// `+refs/heads/*:refs/heads/*` is the standard "mirror all
	// branches" refspec; the leading `+` allows non-FF updates,
	// which is fine here because we're populating an empty bare
	// from scratch.
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
	// Drop the `source` remote — once mirroring is done the bare
	// stands on its own. The working tree's `origin` will point
	// at the bare (set below); the bare needs no upstream.
	_ = bareRepo.DeleteRemote("source")

	if err := setOrReplaceOrigin(workTreePath, barePath); err != nil {
		// Bare exists; origin-rewiring failed. Don't roll back
		// the bare — a retry of PromoteWorkingTreeToBare will
		// hit the idempotent branch above and re-attempt the
		// origin-rewire.
		return fmt.Errorf("setting origin on %q: %w", workTreePath, err)
	}
	return nil
}

// setOrReplaceOrigin sets the `origin` remote on a working tree
// to bareURL, replacing any existing `origin`. Used by
// PromoteWorkingTreeToBare to wire the operator's tree to the
// new bare. Idempotent: if `origin` already points at bareURL
// the function is a no-op.
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
		// Already pointing at the right place — done.
		for _, u := range existing.URLs {
			if u == bareURL {
				return nil
			}
		}
		// Different URL — replace. DeleteRemote is the gogit
		// way; CreateRemote refuses if the name already exists.
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
// path with no refs and no commits. Useful when you have an
// existing working tree whose commits will be pushed in as
// the initial state — a separate seed would just create a
// divergent root that can't merge with what's already there.
//
// Production callers used to be enju_init's auto-bare path;
// after Option B (solo-mode default), no production code path
// creates bares anymore — the scanner falls back to local
// refs/heads. This helper is retained for test fixtures that
// need to construct fake remotes (set_project_remote tests,
// integration scenarios that exercise the upgrade path).
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
// Production callers used to be the autoLocal path of
// enju_create_project; after Option B (solo-mode default),
// no production code path creates bares anymore. This helper
// is retained for test fixtures that need to construct fake
// remotes (eager-clone tests, integration scenarios that
// exercise multi-machine sharing). The seed contents mirror
// what enjugit's local-init seed writes into a working tree,
// so remote-backed and solo projects share the same starting
// layout.
func InitBareWithSeed(bareDir string) error {
	// Init a temp working tree, commit, push to bare.
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		return fmt.Errorf("creating bare dir: %w", err)
	}

	// Init the bare repo first.
	_, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	})
	if err != nil {
		return fmt.Errorf("init bare: %w", err)
	}

	// Init a temp working tree, seed it, push to bare.
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

	// Write a README and seed the default templates/ dir with a
	// .gitkeep so a fresh project clone has the same layout
	// enju_init produces for adopted folders. Without this,
	// enju_list_templates returns empty until the first
	// manual commit under enju/templates/, which is a
	// confusing "looks like the tool is broken" surprise.
	readme := filepath.Join(tmpDir, "README.md")
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
	if err := os.WriteFile(readme, []byte(readmeBody), 0644); err != nil {
		return fmt.Errorf("write readme: %w", err)
	}
	templatesDir := filepath.Join(tmpDir, corelayout.DefaultTemplatesDir)
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		return fmt.Errorf("create templates dir: %w", err)
	}
	gitkeepRel := filepath.ToSlash(filepath.Join(corelayout.DefaultTemplatesDir, ".gitkeep"))
	if err := os.WriteFile(filepath.Join(tmpDir, gitkeepRel), []byte(""), 0644); err != nil {
		return fmt.Errorf("write .gitkeep: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		return fmt.Errorf("add README: %w", err)
	}
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
