package gitv6

import (
	"fmt"
	"os"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
)

// InitEmptyBare creates an empty bare git repo at barePath with
// the given default branch. defaultBranch is a short name like
// "main".
//
// Idempotent: an existing bare at the path is left intact.
func InitEmptyBare(barePath, defaultBranch string) error {
	if err := os.MkdirAll(barePath, 0o755); err != nil {
		return fmt.Errorf("creating bare dir: %w", err)
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	_, err := gogit.PlainInit(barePath, true,
		gogit.WithDefaultBranch(plumbing.ReferenceName("refs/heads/"+defaultBranch)),
	)
	if err != nil {
		return fmt.Errorf("init bare: %w", err)
	}
	return nil
}

// InitBareWithMirrorFetch initializes a bare repo at barePath and
// mirror-fetches every branch from sourceWorkTree (a local-path
// remote) into it. See git/bare.go in the v5 sibling for the full
// rationale; behaviour is identical.
func InitBareWithMirrorFetch(barePath, sourceWorkTree string) error {
	if err := InitEmptyBare(barePath, "main"); err != nil {
		return err
	}
	bareRepo, err := gogit.PlainOpen(barePath)
	if err != nil {
		return fmt.Errorf("open bare after init: %w", err)
	}
	mirror := []config.RefSpec{config.RefSpec("+refs/heads/*:refs/heads/*")}
	if _, err := bareRepo.CreateRemote(&config.RemoteConfig{
		Name:  "source",
		URLs:  []string{sourceWorkTree},
		Fetch: mirror,
	}); err != nil {
		return fmt.Errorf("adding source remote on bare: %w", err)
	}
	if err := bareRepo.Fetch(&gogit.FetchOptions{
		RemoteName: "source",
		RefSpecs:   mirror,
	}); err != nil {
		return fmt.Errorf("fetching branches from %q into bare: %w", sourceWorkTree, err)
	}
	_ = bareRepo.DeleteRemote("source")
	return nil
}

// SetOriginOnWorkTree sets the `origin` remote on a working tree
// to bareURL, replacing any existing `origin`. Idempotent.
func SetOriginOnWorkTree(workTreePath, bareURL string) error {
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

// HasBare reports whether barePath holds a valid bare git repo.
func HasBare(barePath string) bool {
	if _, err := os.Stat(barePath + "/HEAD"); err != nil {
		return false
	}
	if _, err := gogit.PlainOpen(barePath); err != nil {
		return false
	}
	return true
}
