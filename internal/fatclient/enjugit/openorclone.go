package enjugit

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// OpenOrCloneShared resolves a workDir to a *SharedClone, cloning
// from remoteURL on first access. Used by the project package's
// openOrClone migration path so the same orchestration lives in one
// place.
//
// Behavior:
//
//  1. Recover leftover preserve dir from a prior crash (best-effort).
//  2. If workDir/.git exists: open it. When remoteURL is non-empty
//     and disagrees with the on-disk origin, wipe + re-clone (stale
//     workspace recovery — numeric workspace dirs get reused across
//     DB wipes).
//  3. If remoteURL is empty: init a local-only repo with seed.
//  4. Otherwise clone. Empty-remote bootstrap fallback: a fresh
//     remote with no initial commit (transport.ErrEmptyRemoteRepository)
//     becomes a local-init + EnsureOrigin so the first submit can
//     push the bootstrap commit.
//
// Surfaces FriendlyGitError on the clone path so the caller's UX
// gets a hint without re-wrapping. Other errors propagate as-is.
//
// Goes away with the project package.
func OpenOrCloneShared(workDir, remoteURL string, logger *slog.Logger) (*SharedClone, error) {
	if err := RecoverLeftoverSharedPreserve(workDir, logger); err != nil && logger != nil {
		logger.Warn("preserve-dir recovery failed; leaving for manual inspection",
			"error", err, "path", workDir+SharedPreserveDirSuffix)
	}

	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		gc, err := OpenSharedClone(workDir, "", logger)
		if err != nil {
			return nil, fmt.Errorf("opening existing clone at %s: %w", workDir, err)
		}
		if remoteURL != "" && gc.RemoteURL() != remoteURL {
			if logger != nil {
				logger.Warn("stale workspace — remote URL mismatch, re-cloning",
					"path", workDir, "expected", remoteURL, "found", gc.RemoteURL())
			}
			os.RemoveAll(workDir)
			// Fall through to fresh-clone path below.
		} else {
			return gc, nil
		}
	}

	if remoteURL == "" {
		gc, err := InitLocalShared(workDir, "", logger)
		if err != nil {
			return nil, fmt.Errorf("initializing local-only repo: %w", err)
		}
		if logger != nil {
			logger.Info("initialized local-only repo", "path", workDir)
		}
		return gc, nil
	}

	if stat, err := os.Stat(workDir); err == nil && stat.IsDir() {
		entries, _ := os.ReadDir(workDir)
		if len(entries) > 0 {
			if logger != nil {
				logger.Warn("removing existing non-repo directory before clone", "path", workDir)
			}
			if err := os.RemoveAll(workDir); err != nil {
				return nil, fmt.Errorf("cleaning work dir before clone: %w", err)
			}
		}
	}
	gc, err := CloneOrInitShared(workDir, remoteURL, "", logger)
	if err != nil {
		// Empty-remote bootstrap path. A fresh remote with no
		// initial commit surfaces as ErrEmptyRemoteRepository —
		// the common first-time scenario. Init an empty local
		// clone with origin configured and let the first submit
		// push the bootstrap commit.
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			if stat, statErr := os.Stat(workDir); statErr == nil && stat.IsDir() {
				_ = os.RemoveAll(workDir)
			}
			gc, ierr := InitLocalShared(workDir, "", logger)
			if ierr != nil {
				return nil, fmt.Errorf("initializing empty-remote clone: %w", ierr)
			}
			if eerr := gc.EnsureOrigin(remoteURL); eerr != nil {
				return nil, fmt.Errorf("configuring origin for empty remote: %w", eerr)
			}
			if logger != nil {
				logger.Info("bootstrapped empty remote", "url", remoteURL, "path", workDir)
			}
			return gc, nil
		}
		return nil, FriendlyGitError("clone", remoteURL, err)
	}
	if logger != nil {
		logger.Info("cloned project", "url", remoteURL, "path", workDir)
	}
	return gc, nil
}
