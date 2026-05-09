package git

import (
	"fmt"
	"os"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// InitEmptyBare creates an empty bare git repo at barePath with
// the given default branch. defaultBranch is a short name like
// "main" — the function adds the refs/heads/ prefix.
//
// Idempotent: an existing bare at the path is left intact.
//
// Used by enjugit's bootstrap helpers to produce a non-working-
// tree push target for fat-client / bot flows. Lives in the git
// package (not enjugit) so the enjugit layer never imports
// go-git directly — backend swaps stay at the seam.
func InitEmptyBare(barePath, defaultBranch string) error {
	if err := os.MkdirAll(barePath, 0o755); err != nil {
		return fmt.Errorf("creating bare dir: %w", err)
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	_, err := gogit.PlainInitWithOptions(barePath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/" + defaultBranch),
		},
		Bare: true,
	})
	if err != nil {
		return fmt.Errorf("init bare: %w", err)
	}
	return nil
}

// InitBareWithMirrorFetch initializes a bare repo at barePath and
// mirror-fetches every branch from sourceWorkTree (treated as a
// local-path remote) into it. Used by PromoteWorkingTreeToBare to
// bridge an adopted working tree into a bare-backed shape so bots
// have a non-working-tree push target.
//
// Steps:
//  1. InitEmptyBare(barePath, "main")
//  2. Add a temporary "source" remote on the bare pointing at
//     sourceWorkTree.
//  3. bare.Fetch from "source" with mirror refspec
//     +refs/heads/*:refs/heads/*.
//  4. Remove the "source" remote.
//
// Idempotent for the init step (InitEmptyBare is). The fetch is
// repeatable — repeated calls just re-fetch any new branches.
func InitBareWithMirrorFetch(barePath, sourceWorkTree string) error {
	if err := InitEmptyBare(barePath, "main"); err != nil {
		return err
	}
	bareRepo, err := gogit.PlainOpen(barePath)
	if err != nil {
		return fmt.Errorf("open bare after init: %w", err)
	}
	// Mirror refspec is `+refs/heads/*:refs/heads/*` — every
	// branch on the source lands as the same name on the bare.
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
// at workTreePath to bareURL, replacing any existing `origin`.
// Idempotent: a no-op when origin already points at bareURL.
//
// Used after PromoteWorkingTreeToBare so the operator's adopted
// working tree pushes back to the freshly-promoted bare.
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
// The detection uses presence of the HEAD file, which is the
// minimum gogit needs to PlainOpen a bare. A half-initialized
// stub directory returns false.
func HasBare(barePath string) bool {
	if _, err := os.Stat(barePath + "/HEAD"); err != nil {
		return false
	}
	if _, err := gogit.PlainOpen(barePath); err != nil {
		return false
	}
	return true
}
