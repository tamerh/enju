package gitcli

// bare.go — Phase 8 package-level helpers for bare-repo
// bootstrap. Distinct from Clone-method ops because the bare
// repo doesn't have an associated Clone handle at the time
// these run: they EXIST to create the bare so a Clone can
// later be opened against it (via initbare.go in the enjugit
// package).
//
// Four entry points:
//   - InitEmptyBare: create a fresh bare with a configured
//     default branch.
//   - InitBareWithMirrorFetch: create the bare AND mirror every
//     branch from an existing on-disk working tree into it
//     (used when an operator has a pre-existing repo and we're
//     wiring our managed-bare layer onto it).
//   - SetOriginOnWorkTree: point a working tree at the new
//     bare as its origin remote. Idempotent.
//   - HasBare: cheap existence probe.

import (
	"fmt"
	"os"
	"path/filepath"
)

// InitEmptyBare creates an empty bare git repo at barePath with
// the given default branch name (short form like "main").
//
// Idempotent: an existing valid bare at the path is left
// intact. We check via HasBare rather than `git init --bare`
// re-running unconditionally because re-running CAN change the
// initial branch ref on an old repo that was init'd differently
// — operationally a no-op we'd rather avoid.
func InitEmptyBare(barePath, defaultBranch string) error {
	if HasBare(barePath) {
		return nil
	}
	if err := os.MkdirAll(barePath, 0o755); err != nil {
		return fmt.Errorf("creating bare dir: %w", err)
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if _, err := runGit("", []string{"init", "--bare", "-b", defaultBranch, barePath}, runOpts{}); err != nil {
		return fmt.Errorf("init bare: %w", err)
	}
	return nil
}

// InitBareWithMirrorFetch initializes a bare repo at barePath
// and mirror-fetches every branch from sourceWorkTree (a
// local-path source) into it. Used when an operator has an
// existing project repo and we're wiring our managed-bare
// layer onto it: the bare captures the operator's branch
// history so future operations against the bare are
// indistinguishable from operations against the original.
//
// Mirror refspec `+refs/heads/*:refs/heads/*` copies every
// local branch verbatim. The fetch URL doesn't need to be a
// registered remote — git supports `fetch <url> <refspec>`
// inline, so we skip the add-remote / remove-remote dance
// gitv6 needed.
func InitBareWithMirrorFetch(barePath, sourceWorkTree string) error {
	if err := InitEmptyBare(barePath, "main"); err != nil {
		return err
	}
	// Inline fetch — no temporary remote to add+remove. The URL
	// is the source worktree path; git treats it as a local
	// file:// remote automatically.
	if _, err := runGit(barePath,
		[]string{"fetch", sourceWorkTree, "+refs/heads/*:refs/heads/*"},
		runOpts{}); err != nil {
		return fmt.Errorf("fetching branches from %q into bare: %w", sourceWorkTree, err)
	}
	return nil
}

// SetOriginOnWorkTree sets the `origin` remote on a working
// tree to bareURL, replacing any existing `origin`. Idempotent:
// when origin already points at bareURL, no-op.
//
// Distinct from Clone.EnsureOrigin: this is a package-level
// function called during bootstrap, before any Clone handle
// exists for the working tree. The behavior is identical
// otherwise.
func SetOriginOnWorkTree(workTreePath, bareURL string) error {
	out, err := runGit(workTreePath, []string{"remote", "get-url", "origin"}, runOpts{})
	if err == nil {
		// Origin already exists. Check if URL matches.
		current := trimTrailingNewline(out)
		if current == bareURL {
			return nil
		}
		if _, err := runGit(workTreePath, []string{"remote", "set-url", "origin", bareURL}, runOpts{}); err != nil {
			return fmt.Errorf("set-url origin %s: %w", bareURL, err)
		}
		return nil
	}
	// Origin missing — add.
	if _, err := runGit(workTreePath, []string{"remote", "add", "origin", bareURL}, runOpts{}); err != nil {
		return fmt.Errorf("add origin %s: %w", bareURL, err)
	}
	return nil
}

// HasBare reports whether barePath holds a valid bare git repo.
// Cheap probe — stat for the HEAD file (which every bare carries
// at the repo root) is enough; a fuller `git rev-parse
// --is-bare-repository` check would catch corrupt-bare cases
// but they're rare and the more expensive call isn't justified
// on a function this hot.
func HasBare(barePath string) bool {
	if _, err := os.Stat(filepath.Join(barePath, "HEAD")); err != nil {
		return false
	}
	// Belt-and-suspenders: confirm git agrees this is a bare.
	// Catches the "directory with a HEAD file but not actually
	// a git repo" pathological case.
	out, err := runGit(barePath, []string{"rev-parse", "--is-bare-repository"}, runOpts{})
	if err != nil {
		return false
	}
	return trimTrailingNewline(out) == "true"
}
