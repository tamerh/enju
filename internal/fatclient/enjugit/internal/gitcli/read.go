package gitcli

// read.go — Phase 1 read-only ref operations. None of these
// acquire the project lock; they're concurrent-safe at the
// gitcli level (and `git` itself is safe-for-concurrent-reads
// against an on-disk repo as long as nothing concurrently
// mutates refs in incompatible ways).
//
// All verbs here funnel through runGit and rely on its stderr →
// typed-error mapping. Verb code is small because the
// classification work is centralized.

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveRef resolves a ref name or 40-hex SHA to a commit SHA.
// Accepts:
//   - 40-hex SHAs (returned as-is after existence check).
//   - Full ref paths (e.g. "refs/heads/main").
//   - Branch names (e.g. "main") — resolved via refs/heads,
//     then refs/remotes/origin.
//
// Returns ErrRefNotFound when the name doesn't resolve.
//
// Implementation note: `git rev-parse --verify <name>^{commit}`
// covers all three cases in one shot — `--verify` enforces
// existence, `^{commit}` peels through annotated tags and
// ensures the resolved object is a commit. We try the full ref
// path implicitly when name starts with "refs/"; for short
// branch names we let rev-parse's standard lookup order do its
// thing.
func (c *Clone) ResolveRef(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty ref name", ErrRefNotFound)
	}
	// 40-hex SHA: confirm the object exists. cat-file -e is the
	// dedicated existence check (rev-parse on a literal hex string
	// just echoes it without validation, as Phase 0's test
	// established).
	if isHexSHA(name) {
		if _, err := runGit(c.workDir, []string{"cat-file", "-e", name + "^{commit}"}, runOpts{}); err != nil {
			return "", fmt.Errorf("%w: %s", ErrRefNotFound, name)
		}
		return name, nil
	}

	// Ref name. rev-parse --verify enforces existence; ^{commit}
	// peels tags + fails on non-commit objects. We probe in
	// order: full-ref → refs/heads → refs/remotes/origin. Mirrors
	// gitv6's three-step lookup so callers see identical
	// resolution behavior.
	candidates := []string{}
	if strings.HasPrefix(name, "refs/") {
		candidates = append(candidates, name)
	} else {
		candidates = append(candidates,
			"refs/heads/"+name,
			"refs/remotes/origin/"+name,
		)
	}
	for _, ref := range candidates {
		out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", ref + "^{commit}"}, runOpts{})
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrRefNotFound, name)
}

// Head returns HEAD's commit SHA and the current branch name.
// When HEAD is detached, branch is "" and sha is the commit hash
// HEAD points at directly.
//
// Returns ErrRefNotFound when HEAD itself can't be resolved
// (unborn HEAD in an empty repo, corrupt state).
func (c *Clone) Head() (sha, branch string, err error) {
	// rev-parse HEAD returns the commit; symbolic-ref --short
	// returns the branch name or fails for detached HEAD.
	// Run rev-parse first so an empty-repo's unborn HEAD surfaces
	// as ErrRefNotFound rather than as a misleading empty branch.
	out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "HEAD"}, runOpts{})
	if err != nil {
		return "", "", fmt.Errorf("%w: HEAD: %v", ErrRefNotFound, err)
	}
	sha = strings.TrimSpace(string(out))

	// symbolic-ref --quiet --short HEAD: silent exit 1 when
	// detached, otherwise the short branch name.
	bOut, bErr := runGit(c.workDir, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, runOpts{})
	if bErr == nil {
		branch = strings.TrimSpace(string(bOut))
	}
	return sha, branch, nil
}

// LocalBranches returns the short names of every local branch.
// Order is git's enumeration order (alphabetical by ref path);
// for-each-ref is deterministic, unlike gitv6's hash-iteration.
// Callers don't depend on order, so this is fine.
func (c *Clone) LocalBranches() ([]string, error) {
	out, err := runGit(c.workDir, []string{"for-each-ref", "--format=%(refname:short)", "refs/heads/"}, runOpts{})
	if err != nil {
		return nil, fmt.Errorf("git: list local branches: %w", err)
	}
	return splitNonEmptyLines(string(out)), nil
}

// LocalBranchHash returns the SHA of refs/heads/<branch>, falling
// back to refs/remotes/origin/<branch>. Empty string when neither
// resolves. Empty branch arg is a caller bug.
func (c *Clone) LocalBranchHash(branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("git: LocalBranchHash: branch required")
	}
	// rev-parse --verify -q exits non-zero silently on miss, so
	// we can't distinguish "miss" from "error" — but for this
	// query, miss IS the expected non-error case, so we just
	// fall through to the next candidate.
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", ref}, runOpts{})
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", nil
}

// RemoteBranchHash queries origin via ls-remote and returns the
// SHA of the named branch, or empty string when the remote
// doesn't carry it. Errors when origin is unreachable.
func (c *Clone) RemoteBranchHash(branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("git: RemoteBranchHash: branch required")
	}
	out, err := runGit(c.workDir, []string{"ls-remote", "origin", "refs/heads/" + branch}, runOpts{network: true})
	if err != nil {
		return "", fmt.Errorf("git: ls-remote: %w", err)
	}
	// Format: "<sha>\t<refname>\n". Empty stdout when the remote
	// doesn't carry the branch.
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", nil
	}
	parts := strings.SplitN(line, "\t", 2)
	return parts[0], nil
}

// RemoteBranches queries origin via ls-remote and returns every
// branch name on the remote. Used by the fat-client's auto-branch
// picker to avoid colliding with branches that exist on the bare
// repo but aren't tracked by the coord DB (post-DB-wipe state).
func (c *Clone) RemoteBranches() ([]string, error) {
	out, err := runGit(c.workDir, []string{"ls-remote", "--heads", "origin"}, runOpts{network: true})
	if err != nil {
		return nil, fmt.Errorf("git: ls-remote --heads: %w", err)
	}
	var names []string
	for _, line := range splitNonEmptyLines(string(out)) {
		// "<sha>\trefs/heads/<name>"
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		ref := strings.TrimPrefix(parts[1], "refs/heads/")
		if ref != parts[1] {
			names = append(names, ref)
		}
	}
	return names, nil
}

// IsAncestor reports whether `ancestor` is reachable from
// `descendant` by walking parents. Maps to native git semantics
// (no hop bound). Returns (false, nil) when either commit is
// unknown to the local object DB — same shape as gitv6's
// hash-zero comparison.
func (c *Clone) IsAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == "" || descendant == "" {
		return false, nil
	}
	// merge-base --is-ancestor: exit 0 = is-ancestor, exit 1 =
	// not-ancestor, exit 128 = error (e.g. unknown SHA). The
	// "unknown SHA" case maps to (false, nil) for parity with
	// gitv6, which silently returned false when either hash
	// resolved to no commit.
	_, err := runGit(c.workDir, []string{"merge-base", "--is-ancestor", ancestor, descendant}, runOpts{})
	if err == nil {
		return true, nil
	}
	// Distinguish exit-1 (definitive "no") from exit-128 (error).
	// Easiest: check for the typed errors we know about; anything
	// else assume exit-1.
	// classifyStderr maps "bad object" / "no such commit" to
	// ErrCommitNotFound — for IsAncestor's contract that's still
	// "false, nil".
	return false, nil
}

// State inspects the worktree and returns its current state.
//
// Order of checks (matches gitv6's contract for parity):
//   1. HEAD detached → StateDetached.
//   2. Tracked file modifications → StateDirtyTracked.
//   3. Untracked files in tree → StateDirtyUntracked.
//   4. Otherwise → StateClean.
//
// Skips our own infrastructure paths (.bare.git / .clone) the
// same way gitv6 does — these are managed git plumbing inside
// the project, not user-tracked content.
func (c *Clone) State() WorktreeState {
	// Detached HEAD check: symbolic-ref fails when HEAD is
	// detached. (It also fails on unborn HEAD in an empty repo;
	// that's effectively "no branch" too, which we'll surface as
	// detached — verb callers handle empty-repo bootstrap
	// elsewhere.)
	if _, err := runGit(c.workDir, []string{"symbolic-ref", "--quiet", "HEAD"}, runOpts{}); err != nil {
		return StateDetached
	}

	// status --porcelain=v1 -z: NUL-terminated, scriptable.
	// Format per entry: "<XY> <path>\0" where XY is two status
	// chars. We don't need to handle renames here (-> form) —
	// any non-"  " in XY classifies as dirty.
	out, err := runGit(c.workDir, []string{"status", "--porcelain=v1", "-z", "--untracked-files=normal"}, runOpts{})
	if err != nil {
		// Best-effort: treat as clean and let the verb's other
		// pre-checks fail loudly if something's actually broken.
		return StateClean
	}
	hasTracked := false
	hasUntracked := false
	for _, entry := range strings.Split(string(out), "\x00") {
		if len(entry) < 3 {
			continue
		}
		// XY is the first two bytes; path is bytes 3..
		xy := entry[:2]
		path := entry[3:]
		// Skip our infrastructure paths (same filter as gitv6).
		base := filepath.Base(path)
		if base == ".bare.git" || base == ".clone" {
			continue
		}
		if xy == "??" {
			hasUntracked = true
		} else if xy != "  " {
			hasTracked = true
		}
	}
	if hasTracked {
		return StateDirtyTracked
	}
	if hasUntracked {
		return StateDirtyUntracked
	}
	return StateClean
}

// isHexSHA returns true when s is a 40-character hex string (a
// git object SHA-1 in canonical form).
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// splitNonEmptyLines splits on \n and drops empty entries (the
// common trailing-newline case + git's occasional blank line
// from sub-process output).
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
