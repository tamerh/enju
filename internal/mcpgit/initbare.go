package mcpgit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// InitBareWithSeed creates a bare git repo at the given path
// with one initial commit (a README.md). This is needed so
// PlainClone can clone from it — an empty bare repo with no
// refs/HEAD fails to clone.
//
// Used by the MCP client's auto-local-repo feature: when a
// project is created without a remote_url, we auto-create
// ~/.enju/repos/{id}.git as the backing store.
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

	// Write a README and seed the enju_templates/ dir with a
	// .gitkeep so a fresh project clone has the same layout
	// enju_init produces for adopted folders. Without this,
	// enju_list_templates returns empty until the first
	// manual commit under enju_templates/, which is a
	// confusing "looks like the tool is broken" surprise.
	readme := filepath.Join(tmpDir, "README.md")
	readmeBody := "# Enju project\n\n" +
		"Task results live under `.enju/runs/`.\n\n" +
		"Templates live under `enju_templates/`, one directory per template:\n\n" +
		"```\n" +
		"enju_templates/\n" +
		"  my-template/\n" +
		"    template.yaml   # run definition (required)\n" +
		"    scripts/        # bundled scripts referenced by compute tasks\n" +
		"    README.md       # author-facing docs (optional)\n" +
		"```\n"
	if err := os.WriteFile(readme, []byte(readmeBody), 0644); err != nil {
		return fmt.Errorf("write readme: %w", err)
	}
	templatesDir := filepath.Join(tmpDir, "enju_templates")
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		return fmt.Errorf("create templates dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(templatesDir, ".gitkeep"), []byte(""), 0644); err != nil {
		return fmt.Errorf("write .gitkeep: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		return fmt.Errorf("add README: %w", err)
	}
	if _, err := wt.Add("enju_templates/.gitkeep"); err != nil {
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
