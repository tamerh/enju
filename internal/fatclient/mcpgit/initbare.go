package mcpgit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	corelayout "github.com/enju-ai/enju/internal/core/layout"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// seedLocalWorkspace writes a README + enju/templates/.gitkeep
// into a freshly-init'd working tree and creates the first
// commit on the default branch. Used by openOrClone's local-
// only path so a fresh project without a remote still has the
// baseline state every other code path expects: at least one
// commit on refs/heads/main, the standard enju/templates/
// directory ready to receive templates, a recognizable README.
//
// Mirrors what InitBareWithSeed produces inside a bare repo,
// so the user-facing layout doesn't depend on whether a remote
// was configured at create time. Idempotent in the only sense
// that matters here (caller only invokes on fresh init); does
// not check for existing commits.
func seedLocalWorkspace(repo *gogit.Repository, workDir string) error {
	readme := filepath.Join(workDir, "README.md")
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
		return fmt.Errorf("write README: %w", err)
	}
	templatesDir := filepath.Join(workDir, corelayout.DefaultTemplatesDir)
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		return fmt.Errorf("create templates dir: %w", err)
	}
	gitkeepRel := filepath.ToSlash(filepath.Join(corelayout.DefaultTemplatesDir, ".gitkeep"))
	if err := os.WriteFile(filepath.Join(workDir, gitkeepRel), []byte(""), 0644); err != nil {
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
// what seedLocalWorkspace writes into a working tree, so
// remote-backed and solo projects share the same starting
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
