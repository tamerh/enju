package gitv6

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
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
// producing a merge commit with the supplied message and author.
// Used when the FF fast path is impossible (caller should have
// tried MergeFFOrFail first) but conflicts shouldn't be expected
// to be a hard failure.
//
// Phase 1 plumbing-merge: never touches the worktree. Uses
// `git merge-tree --write-tree --name-only` to compute the
// merged tree object, then writes a commit object directly via
// the go-git store and atomically advances the target ref. The
// bot's worktree can have any amount of unrelated state on disk;
// this method ignores it entirely.
//
// Pre-Phase-1 implementation shelled out to `git checkout target`
// + `git merge --no-ff` which read+wrote the worktree. That path
// failed with "untracked working tree files would be overwritten"
// any time the worktree had stragglers — the production load-test
// failure mode for parallel siblings sharing a clone.
//
// On conflict: returns *ErrConflict (errors.Is matches
// ErrMergeConflict; errors.As extracts paths). No partial advance
// — the target ref is left exactly as it was; the merged tree
// object git wrote with conflict markers is harmless garbage left
// in the object DB and gets reaped by `git gc`.
//
// Git operations performed:
//  1. Resolve target/source SHAs.
//  2. Idempotent: ensure local target ref matches the resolved tip.
//  3. `git merge-tree --write-tree --name-only target source`
//     → merged tree SHA on stdout, conflicting paths on subsequent
//     lines when exit code is 1.
//  4. On conflict: return *ErrConflict.
//  5. Build commit object: tree = merged-tree-sha,
//     parents = [target, source], author/committer from args.
//  6. Encode + store the commit object.
//  7. Atomic CAS update of refs/heads/<target> from targetSHA to
//     mergeCommitSHA.
//
// Worktree state: untouched. Caller's responsibility if they want
// to materialize the new tip via Checkout afterwards (the
// MergeAcceptedTopic verb in enjugit/producing.go does that).
//
// Errors:
//   - ErrRefNotFound: target or source can't be resolved.
//   - ErrMergeConflict (via *ErrConflict): real file-level conflict.
//   - any: merge-tree shellout failure or object-store write failure.
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
	// expected tip. Defends against split-brain where a prior
	// aborted operation left the ref elsewhere.
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(refName, targetSHA)); err != nil {
		return "", fmt.Errorf("git: pre-merge SetReference %s: %w", target, err)
	}

	// Compute the merged tree without touching the worktree.
	// merge-tree --write-tree was added in git 2.38; --name-only
	// in 2.40. Both are well-established in production builds.
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

	// Output format:
	//   <merged-tree-sha>\n
	// followed (on conflict, exit code 1) by:
	//   <conflicted-path-1>\n
	//   <conflicted-path-2>\n
	//   ...
	//   \n
	//   <human-readable conflict messages>
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
		// Same shape as pre-Phase-1: ErrConflict carries paths,
		// errors.Is matches ErrMergeConflict.
		return "", &ErrConflict{Paths: conflicts}
	}

	// Clean merge — build + store the commit object.
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

	// Atomic ref advance from the resolved targetSHA to the new
	// merge commit. If the ref moved underneath us between the
	// resolve at the top of this function and now, the
	// SetReference here would silently clobber that — but since
	// we hold c.lock() across the whole operation no other
	// in-process caller can have moved it.
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return "", fmt.Errorf("git: advance %s to merge commit: %w", target, err)
	}
	return commitHash.String(), nil
}

// (resolveLocalRef + resolveLocalOrPlantFromOrigin live in
// branch.go in this package — moved during the v6 port.)

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
// stdout+stderr. Currently used only by MergeWithCommit's
// merge-tree call — kept narrow because once we've moved the
// merge off the worktree, there's no other place a workdir-rooted
// git invocation makes sense in the plumbing layer. Pre-Phase-1
// this also drove `git checkout` + `git merge --no-ff`, both of
// which are gone now.
func runGit(workDir string, args ...string) (string, error) {
	full := append([]string{"-C", workDir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
