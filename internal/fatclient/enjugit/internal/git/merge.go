package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// MergeFFOrFail tries to fast-forward target to source's tip.
// When target's current tip IS an ancestor of source's tip, the
// merge is a pure ref update (no commit, no checkout). When it
// isn't, returns an error wrapping ErrPushNonFF — caller's choice
// whether to call MergeWithCommit instead.
//
// Git operations performed:
//   1. Resolve target and source SHAs.
//   2. If target == source → no-op.
//   3. If target is ancestor of source → SetReference(target → source).
//   4. Else → return ErrPushNonFF (literally non-FF; the merge can't be FF).
//
// Returns the new tip SHA on success, "" on the non-FF case.
//
// Worktree state: unchanged (this is a ref-only operation).
func (c *Clone) MergeFFOrFail(target, source string) (string, error) {
	defer c.lock()()
	targetSHA, err := c.resolveLocalOrPlantFromOrigin(target)
	if err != nil {
		return "", fmt.Errorf("%w: target %s", ErrRefNotFound, target)
	}
	sourceSHA, err := c.resolveAnyRef(source)
	if err != nil {
		return "", fmt.Errorf("%w: source %s", ErrRefNotFound, source)
	}
	if targetSHA == sourceSHA {
		return targetSHA.String(), nil
	}
	isAncestor, err := c.commitIsAncestor(targetSHA, sourceSHA)
	if err != nil {
		return "", fmt.Errorf("git: ancestor check: %w", err)
	}
	if !isAncestor {
		return "", fmt.Errorf("%w: %s is not ancestor of %s",
			ErrPushNonFF, target, source)
	}
	refName := plumbing.NewBranchReferenceName(target)
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(refName, sourceSHA)); err != nil {
		return "", fmt.Errorf("git: SetReference %s: %w", target, err)
	}
	return sourceSHA.String(), nil
}

// MergeWithCommit performs a `git merge --no-ff` of source onto
// target, producing a merge commit with the supplied message and
// author. Used when the FF fast path is impossible (caller should
// have tried MergeFFOrFail first) but conflicts shouldn't be
// expected to be a hard failure.
//
// Shells out to system git because go-git's merge support doesn't
// expose proper conflict-file reporting.
//
// On conflict: aborts the merge, parses unmerged files from
// `git status --porcelain`, returns ErrMergeConflict (carrying
// paths via ErrConflict). The target ref is restored to its
// pre-merge state — no partial advance.
//
// Git operations performed:
//   1. Resolve target/source SHAs.
//   2. SetReference target to current tip (idempotent guard).
//   3. git checkout target (worktree update).
//   4. git merge --no-ff --no-edit -m <msg> <source>.
//   5. On conflict: git merge --abort; restore ref; return error.
//   6. On success: read new HEAD SHA; return it.
//
// Worktree state: Pre any → Post StateClean (matches new merge tip).
//
// Errors:
//   - ErrRefNotFound: target or source can't be resolved.
//   - ErrMergeConflict: real file-level conflict (carries paths).
//   - any: shell-out failure wrapped.
func (c *Clone) MergeWithCommit(target, source, message, authorName, authorEmail string) (string, error) {
	defer c.lock()()
	targetSHA, err := c.resolveLocalOrPlantFromOrigin(target)
	if err != nil {
		return "", fmt.Errorf("%w: target %s", ErrRefNotFound, target)
	}
	sourceSHA, err := c.resolveAnyRef(source)
	if err != nil {
		return "", fmt.Errorf("%w: source %s", ErrRefNotFound, source)
	}

	refName := plumbing.NewBranchReferenceName(target)
	// Idempotent guard: ensure local target ref points at our
	// expected tip before checkout. Defends against split-brain
	// where a prior aborted merge left the ref elsewhere.
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(refName, targetSHA)); err != nil {
		return "", fmt.Errorf("git: pre-merge SetReference %s: %w", target, err)
	}

	if out, err := runGit(c.workDir, "checkout", target); err != nil {
		return "", fmt.Errorf("git checkout %s before merge: %s (%w)",
			target, strings.TrimSpace(out), err)
	}

	if authorName == "" {
		authorName = "Enju git layer"
	}
	if authorEmail == "" {
		authorEmail = "enju-git@localhost"
	}
	mergeOut, mergeErr := runGitWithEnv(c.workDir,
		[]string{
			"GIT_AUTHOR_NAME=" + authorName,
			"GIT_AUTHOR_EMAIL=" + authorEmail,
			"GIT_COMMITTER_NAME=" + authorName,
			"GIT_COMMITTER_EMAIL=" + authorEmail,
		},
		"merge", "--no-ff", "--no-edit", "-m", message, sourceSHA.String(),
	)
	if mergeErr != nil {
		conflicts := readUnmergedFiles(c.workDir)
		// Best-effort abort + ref restoration.
		_, _ = runGit(c.workDir, "merge", "--abort")
		_ = c.repo.Storer.SetReference(plumbing.NewHashReference(refName, targetSHA))
		if len(conflicts) > 0 {
			// Return *ErrConflict directly. Its Is() method
			// returns true for ErrMergeConflict so callers using
			// errors.Is(err, ErrMergeConflict) match, AND
			// errors.As(err, &*ErrConflict) extracts the paths.
			return "", &ErrConflict{Paths: conflicts}
		}
		return "", fmt.Errorf("git merge --no-ff: %s (%w)",
			strings.TrimSpace(mergeOut), mergeErr)
	}

	newRef, err := c.repo.Reference(refName, true)
	if err != nil {
		return "", fmt.Errorf("git: read %s post-merge: %w", target, err)
	}
	return newRef.Hash().String(), nil
}

// resolveLocalRef resolves a ref name to a hash, looking ONLY in
// refs/heads (not remote-tracking). Used by merge ops that mutate
// the local branch — we don't want to silently advance a local
// ref because origin/* says so.
func (c *Clone) resolveLocalRef(name string) (plumbing.Hash, error) {
	ref, err := c.repo.Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// resolveLocalOrPlantFromOrigin resolves a branch name to a hash:
// returns the local SHA if refs/heads/<name> exists, otherwise
// PLANTS refs/heads/<name> from refs/remotes/origin/<name> (when
// the origin tracking ref exists) and returns that SHA.
//
// Why plant from origin: a fresh clone (e.g. a bot's per-bot
// clone) sees the run branch as origin/<name> after fetch but
// never has a local ref unless someone explicitly creates one.
// Merge ops need a local ref to advance — without this fallback,
// MergeFFOrFail / MergeWithCommit fail with a misleading "ref
// not found" against a branch that DOES exist on origin.
//
// Returns ErrRefNotFound when neither local nor origin tracking
// has the branch (genuinely unknown).
func (c *Clone) resolveLocalOrPlantFromOrigin(name string) (plumbing.Hash, error) {
	if hash, err := c.resolveLocalRef(name); err == nil {
		return hash, nil
	}
	remoteRef, rerr := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", name), true)
	if rerr != nil {
		return plumbing.ZeroHash, rerr
	}
	localRef := plumbing.NewBranchReferenceName(name)
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(localRef, remoteRef.Hash())); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("planting local ref %s from origin: %w", name, err)
	}
	return remoteRef.Hash(), nil
}

// resolveAnyRef resolves a ref name to a hash, accepting:
// branches (refs/heads), remote-tracking refs (refs/remotes/origin),
// and 40-hex SHAs. Used for merge SOURCES where any reachable
// commit is valid.
func (c *Clone) resolveAnyRef(name string) (plumbing.Hash, error) {
	if isHexSHA(name) {
		hash := plumbing.NewHash(name)
		if _, err := c.repo.CommitObject(hash); err == nil {
			return hash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("commit %s not found", name)
	}
	if ref, err := c.repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
		return ref.Hash(), nil
	}
	if ref, err := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", name), true); err == nil {
		return ref.Hash(), nil
	}
	return plumbing.ZeroHash, fmt.Errorf("ref %s not resolvable", name)
}

// commitIsAncestor reports whether `ancestor` is reachable from
// `descendant` by walking parent links (i.e. "is X a fast-forward
// of Y"). Bounded internally by go-git; safe even for deep histories.
func (c *Clone) commitIsAncestor(ancestor, descendant plumbing.Hash) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}
	descCommit, err := c.repo.CommitObject(descendant)
	if err != nil {
		return false, fmt.Errorf("loading descendant: %w", err)
	}
	ancCommit, err := c.repo.CommitObject(ancestor)
	if err != nil {
		return false, fmt.Errorf("loading ancestor: %w", err)
	}
	// go-git: X.IsAncestor(Y) → "is X an ancestor of Y."
	return ancCommit.IsAncestor(descCommit)
}

// readUnmergedFiles parses `git status --porcelain` for unmerged
// entries and returns affected paths. Used to populate
// ErrConflict.Paths after a non-FF merge fails.
//
// Format dependency: relies on porcelain v1 (`XY <path>`).
func readUnmergedFiles(workDir string) []string {
	out, err := runGit(workDir, "status", "--porcelain")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		switch line[:2] {
		case "UU", "AA", "DD", "AU", "UA", "DU", "UD":
			files = append(files, strings.TrimSpace(line[3:]))
		}
	}
	return files
}

// runGit shells out to system git in workDir and returns combined
// stdout+stderr. Used for operations go-git doesn't support cleanly
// (merge with conflict reporting).
func runGit(workDir string, args ...string) (string, error) {
	full := append([]string{"-C", workDir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGitWithEnv is like runGit but with extra environment vars
// appended to os.Environ() — used for merge to set author/committer.
func runGitWithEnv(workDir string, extraEnv []string, args ...string) (string, error) {
	full := append([]string{"-C", workDir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
