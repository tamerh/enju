package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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

// MergeWithCommit performs a non-FF merge of source onto target,
// producing a merge commit via worktree-free plumbing. See the
// v6 sibling in gitv6/merge.go for the full design rationale —
// this v5 backend is in lockstep so a backend swap stays mechanical.
//
// Uses `git merge-tree --write-tree --name-only` to compute the
// merged tree, then writes a commit object directly via the
// go-git store and atomically advances the target ref. The bot's
// worktree is never touched.
//
// On conflict: returns *ErrConflict (errors.Is matches
// ErrMergeConflict; errors.As extracts paths). No partial advance.
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
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(refName, targetSHA)); err != nil {
		return "", fmt.Errorf("git: pre-merge SetReference %s: %w", target, err)
	}

	out, mergeErr := runGit(c.workDir,
		"merge-tree", "--write-tree", "--name-only",
		targetSHA.String(), sourceSHA.String())
	exitCode := 0
	if exitErr, ok := mergeErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if mergeErr != nil {
		return "", fmt.Errorf("git merge-tree: %s (%w)",
			strings.TrimSpace(out), mergeErr)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
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

	if authorName == "" {
		authorName = "Enju git layer"
	}
	if authorEmail == "" {
		authorEmail = "enju-git@localhost"
	}
	sig := object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  time.Now(),
	}
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   message,
		TreeHash:  plumbing.NewHash(mergedTreeSHA),
		ParentHashes: []plumbing.Hash{
			targetSHA,
			sourceSHA,
		},
	}
	commitObj := c.repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return "", fmt.Errorf("git: encode merge commit: %w", err)
	}
	commitHash, err := c.repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		return "", fmt.Errorf("git: store merge commit: %w", err)
	}

	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return "", fmt.Errorf("git: advance %s to merge commit: %w", target, err)
	}
	return commitHash.String(), nil
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

// runGit shells out to system git in workDir and returns combined
// stdout+stderr. Used by MergeWithCommit's `merge-tree` invocation;
// the pre-Phase-1 `git checkout` + `git merge --no-ff` callers are
// gone with the worktree-touching merge path.
func runGit(workDir string, args ...string) (string, error) {
	full := append([]string{"-C", workDir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
