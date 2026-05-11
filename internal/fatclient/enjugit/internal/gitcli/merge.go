package gitcli

// merge.go — Phase 6 merge verbs. Both operate purely at the
// ref/object level — neither touches the worktree, matching
// gitv6's Phase-1 plumbing-merge design (the worktree might
// have any amount of unrelated state from sibling executions
// on the same clone, so we never merge into it).
//
// MergeFFOrFail is a ref-only advance when target is an
// ancestor of source. MergeWithCommit handles non-FF cases via
// `git merge-tree --write-tree` to compute the merged tree
// without checking out, then writes a merge commit object
// directly and CAS-advances the target ref.

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// MergeFFOrFail tries to fast-forward target to source's tip.
// When target's tip IS an ancestor of source's tip, the merge
// is a pure ref update — no commit, no checkout. Otherwise
// returns ErrPushNonFF (literally non-FF; the FF can't happen).
//
// Returns the new tip SHA on success, "" on the non-FF error.
//
// Worktree state: unchanged (ref-only operation).
func (c *Clone) MergeFFOrFail(target, source string) (string, error) {
	defer c.lock()()
	targetSHA, err := c.resolveLocalOrPlantFromOriginSHALocked(target)
	if err != nil {
		return "", fmt.Errorf("%w: target %s", ErrRefNotFound, target)
	}
	sourceSHA, err := c.resolveAnyRefSHALocked(source)
	if err != nil {
		return "", fmt.Errorf("%w: source %s", ErrRefNotFound, source)
	}
	if targetSHA == sourceSHA {
		return targetSHA, nil
	}
	// merge-base --is-ancestor returns exit 0 if ancestor,
	// exit 1 if not. classifyStderr maps "bad object" to
	// ErrCommitNotFound; we've already validated both SHAs
	// exist via the resolve calls above, so a non-nil err here
	// genuinely means "not an ancestor."
	if _, ancErr := runGit(c.workDir, []string{"merge-base", "--is-ancestor", targetSHA, sourceSHA}, runOpts{}); ancErr != nil {
		return "", fmt.Errorf("%w: %s is not ancestor of %s",
			ErrPushNonFF, target, source)
	}
	// Atomic FF: update-ref with CAS from targetSHA → sourceSHA.
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + target, sourceSHA, targetSHA}, runOpts{}); err != nil {
		return "", fmt.Errorf("git: advance %s to %s: %w", target, sourceSHA, err)
	}
	return sourceSHA, nil
}

// MergeWithCommit performs a non-FF merge of source onto target,
// producing a merge commit with the supplied message + author.
// Used when MergeFFOrFail returned ErrPushNonFF but the caller
// is willing to land a real merge.
//
// Worktree-free implementation: uses `git merge-tree
// --write-tree --name-only` to compute the merged tree object,
// then writes the commit object directly via `git commit-tree`
// and atomically advances the target ref. The bot's worktree
// can have any amount of unrelated state on disk — this method
// ignores it.
//
// On conflict: returns *ErrConflict (errors.Is matches
// ErrMergeConflict, errors.As extracts paths). No partial
// advance — target ref untouched.
//
// Requires git ≥ 2.40 for `merge-tree --write-tree --name-only`.
func (c *Clone) MergeWithCommit(target, source, message, authorName, authorEmail string) (string, error) {
	defer c.lock()()
	targetSHA, err := c.resolveLocalOrPlantFromOriginSHALocked(target)
	if err != nil {
		return "", fmt.Errorf("%w: target %s", ErrRefNotFound, target)
	}
	sourceSHA, err := c.resolveAnyRefSHALocked(source)
	if err != nil {
		return "", fmt.Errorf("%w: source %s", ErrRefNotFound, source)
	}

	// Idempotent pre-merge ref reset: ensure local target ref
	// matches our resolved tip. Defends against split-brain
	// where a prior aborted op left the ref elsewhere.
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + target, targetSHA}, runOpts{}); err != nil {
		return "", fmt.Errorf("git: pre-merge update-ref %s: %w", target, err)
	}

	// merge-tree --write-tree --name-only:
	//   exit 0 → clean merge; stdout = merged-tree-SHA + \n
	//   exit 1 → conflicts; stdout = tree-SHA + conflict paths
	//          + blank line + "Auto-merging X / CONFLICT (...)"
	//          diagnostic lines. stderr is empty in both cases
	//          (verified empirically with git 2.43); runGit's
	//          trimStderr truncation isn't losing anything.
	//   exit 128 → error (bad SHA etc.) — propagates as runGit err
	out, mtErr := runGit(c.workDir,
		[]string{"merge-tree", "--write-tree", "--name-only", targetSHA, sourceSHA},
		runOpts{})

	// Detect exit code 1 (conflict) vs higher (real error).
	// runGit returns a wrapped error in both cases; we need to
	// inspect the underlying *exec.ExitError to distinguish.
	exitCode := 0
	if mtErr != nil {
		// runGit may have stripped to a typed sentinel
		// (ErrCommitNotFound for "bad object", etc.) — in that
		// case re-surface, since invalid SHAs shouldn't be
		// silently treated as conflicts.
		if errors.Is(mtErr, ErrCommitNotFound) {
			return "", fmt.Errorf("git merge-tree: %w", mtErr)
		}
		var exitErr *exec.ExitError
		if errors.As(mtErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if exitCode == 0 {
			// runGit doesn't unwrap to *exec.ExitError when it
			// wrapped via classifyStderr — but for merge-tree
			// the "conflict" exit-1 doesn't match any
			// classified pattern, so we should reach the
			// exec.ExitError path. If we somehow don't,
			// surface the raw error.
			return "", fmt.Errorf("git merge-tree: %w", mtErr)
		}
	}

	// Parse output. Format:
	//   <merged-tree-sha>\n
	// followed (on conflict only) by:
	//   <conflicted-path-1>\n
	//   ...
	//   \n
	//   <human-readable messages>
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 || !isHexSHA(lines[0]) {
		return "", fmt.Errorf("git merge-tree: unexpected output: %q", out)
	}
	mergedTreeSHA := lines[0]

	if exitCode != 0 {
		var conflicts []string
		for _, l := range lines[1:] {
			if l == "" {
				break
			}
			conflicts = append(conflicts, l)
		}
		return "", &ErrConflict{Paths: conflicts}
	}

	// Clean merge — write the commit object via commit-tree.
	when := time.Now()
	env := authorEnvVars(authorName, authorEmail, when)
	commitOut, err := runGit(c.workDir,
		[]string{"commit-tree", mergedTreeSHA, "-p", targetSHA, "-p", sourceSHA, "-m", message},
		runOpts{extraEnv: env})
	if err != nil {
		return "", fmt.Errorf("git: commit-tree merge: %w", err)
	}
	commitSHA := strings.TrimSpace(string(commitOut))

	// Atomic CAS advance from targetSHA → commitSHA. Holding
	// c.lock() means no other in-process caller moved the ref
	// between our resolve and this update, but the CAS is
	// belt-and-suspenders for cross-process safety via flock.
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + target, commitSHA, targetSHA}, runOpts{}); err != nil {
		return "", fmt.Errorf("git: advance %s to merge commit %s: %w", target, commitSHA, err)
	}
	return commitSHA, nil
}

// --- lock-internal helpers ---

// resolveLocalOrPlantFromOriginSHALocked returns the SHA of
// refs/heads/<branch>, planting from refs/remotes/origin/<branch>
// when the local ref doesn't exist. Cross-citizen flow: a
// merge target that's only known via origin gets a local ref
// created (and planted at origin's tip) before the merge so
// subsequent update-ref operations have something to advance.
func (c *Clone) resolveLocalOrPlantFromOriginSHALocked(branch string) (string, error) {
	if sha, err := c.localRefHashLocked(branch); err == nil && sha != "" {
		return sha, nil
	}
	originSHA, ok := c.resolveOriginRefHashLocked(branch)
	if !ok {
		return "", fmt.Errorf("ref not found locally or on origin: %s", branch)
	}
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + branch, originSHA}, runOpts{}); err != nil {
		return "", fmt.Errorf("plant local ref from origin: %w", err)
	}
	return originSHA, nil
}

// resolveAnyRefSHALocked resolves a ref name (branch, remote-
// tracking, or 40-hex SHA) to a commit SHA. Used for merge
// SOURCES where any reachable commit is valid.
func (c *Clone) resolveAnyRefSHALocked(name string) (string, error) {
	if isHexSHA(name) {
		if _, err := runGit(c.workDir, []string{"cat-file", "-e", name + "^{commit}"}, runOpts{}); err != nil {
			return "", fmt.Errorf("commit %s not found", name)
		}
		return name, nil
	}
	for _, ref := range []string{"refs/heads/" + name, "refs/remotes/origin/" + name} {
		if out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", ref}, runOpts{}); err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", fmt.Errorf("ref %s not resolvable", name)
}
