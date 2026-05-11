package gitcli

// branch.go — Phase 3 ref/branch mutations and origin remote
// management. All mutating; all acquire the project lock.
// IsAncestor is the one read-only branch op and lives in
// read.go (Phase 1) since it shares classification with the
// resolution path.

import (
	"fmt"
	"strings"
)

// CreateBranchAt creates a new local branch ref at the given
// commit SHA. Errors with ErrBranchExists if refs/heads/<name>
// already exists — caller must DeleteBranch first to replace.
//
// `git update-ref refs/heads/<name> <sha> ""` is the atomic
// create-only form: the empty-string oldvalue means "must not
// exist." Stderr "reference already exists" maps to
// ErrBranchExists via classifyStderr.
//
// Errors:
//   - ErrBranchExists: refs/heads/<name> already exists.
//   - ErrCommitNotFound: baseSHA isn't a real commit.
func (c *Clone) CreateBranchAt(name, baseSHA string) error {
	defer c.lock()()
	if name == "" {
		return fmt.Errorf("git: CreateBranchAt: name required")
	}
	if baseSHA == "" {
		return fmt.Errorf("git: CreateBranchAt: baseSHA required")
	}
	// Validate commit exists locally first — gives the right
	// typed error (ErrCommitNotFound) for the caller's
	// classification. Without this, update-ref would surface a
	// less specific "fatal: <sha> is not a valid SHA1".
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", baseSHA + "^{commit}"}, runOpts{}); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, baseSHA)
	}
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + name, baseSHA, ""}, runOpts{}); err != nil {
		// classifyStderr maps "reference already exists" to
		// ErrBranchExists — surface that directly.
		return err
	}
	return nil
}

// DeleteBranch removes the local branch ref. Idempotent — git's
// `update-ref -d` returns exit 0 when the ref is already missing.
func (c *Clone) DeleteBranch(name string) error {
	defer c.lock()()
	if name == "" {
		return fmt.Errorf("git: DeleteBranch: name required")
	}
	_, err := runGit(c.workDir, []string{"update-ref", "-d", "refs/heads/" + name}, runOpts{})
	if err != nil {
		return fmt.Errorf("git: delete branch %s: %w", name, err)
	}
	return nil
}

// SetBranchTo overwrites a local branch ref to point at sha. No
// CAS — caller takes any current value (or non-existent ref).
// Use UpdateRef for compare-and-swap semantics.
//
// Errors:
//   - ErrCommitNotFound: sha isn't a real commit in local object DB.
func (c *Clone) SetBranchTo(name, sha string) error {
	defer c.lock()()
	if name == "" {
		return fmt.Errorf("git: SetBranchTo: name required")
	}
	if sha == "" {
		return fmt.Errorf("git: SetBranchTo: sha required")
	}
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", sha + "^{commit}"}, runOpts{}); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
	}
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + name, sha}, runOpts{}); err != nil {
		return fmt.Errorf("git: set branch %s: %w", name, err)
	}
	return nil
}

// UpdateRef atomically sets refs/heads/<name> to newSHA. When
// expectedOldSHA is non-empty, the update only succeeds if the
// current value matches — compare-and-swap. Pass "" to skip the
// check (allow any current value, including non-existent).
//
// Native git `update-ref <ref> <newvalue> [<oldvalue>]` already
// gives us CAS for free, so the implementation is one
// invocation. We don't validate that newSHA is a real commit
// here (the caller is typically PlumbingCommit's downstream,
// where the SHA was JUST written and existence is implied).
// update-ref would refuse a totally-bogus SHA anyway.
func (c *Clone) UpdateRef(name, newSHA, expectedOldSHA string) error {
	defer c.lock()()
	if name == "" {
		return fmt.Errorf("git: UpdateRef: name required")
	}
	if newSHA == "" {
		return fmt.Errorf("git: UpdateRef: newSHA required")
	}
	args := []string{"update-ref", "refs/heads/" + name, newSHA}
	if expectedOldSHA != "" {
		args = append(args, expectedOldSHA)
	}
	if _, err := runGit(c.workDir, args, runOpts{}); err != nil {
		// CAS failure surfaces as "cannot lock ref ...: is at <X> but expected <Y>".
		// Wrap with a stable Go-side message; classifyStderr
		// doesn't have a typed error for CAS — by design, since
		// the caller checks for any non-nil error and retries.
		if expectedOldSHA != "" {
			return fmt.Errorf("git: UpdateRef: CAS check failed on %s: %w", name, err)
		}
		return fmt.Errorf("git: UpdateRef %s: %w", name, err)
	}
	return nil
}

// EnsureOrigin makes the on-disk .git/config carry an origin
// remote pointing at url. Idempotent:
//   - origin URL already equals url → no-op
//   - origin exists with a different URL → set-url to overwrite
//   - origin missing → add (with the default
//     +refs/heads/*:refs/remotes/origin/* fetch refspec)
//
// Caller passing url=="" is a no-op (matches gitv6 — used by
// the dual-handle self-heal path where empty means "don't
// touch").
func (c *Clone) EnsureOrigin(url string) error {
	defer c.lock()()
	if url == "" {
		return nil
	}
	out, getErr := runGit(c.workDir, []string{"remote", "get-url", "origin"}, runOpts{})
	if getErr == nil {
		// Origin exists. Check if URL matches.
		current := trimTrailingNewline(out)
		if current == url {
			c.remoteURL = url
			return nil
		}
		if _, err := runGit(c.workDir, []string{"remote", "set-url", "origin", url}, runOpts{}); err != nil {
			return fmt.Errorf("git: ensure-origin: set-url: %w", err)
		}
		c.remoteURL = url
		return nil
	}
	// Origin missing — add. `git remote add` configures the
	// default fetch refspec automatically, so we don't need an
	// extra `git config remote.origin.fetch` call.
	if _, err := runGit(c.workDir, []string{"remote", "add", "origin", url}, runOpts{}); err != nil {
		return fmt.Errorf("git: ensure-origin: add: %w", err)
	}
	c.remoteURL = url
	return nil
}

// RemoveOrigin deletes the origin remote when present.
// Idempotent — missing origin is a no-op, not an error.
func (c *Clone) RemoveOrigin() error {
	defer c.lock()()
	if _, err := runGit(c.workDir, []string{"remote", "get-url", "origin"}, runOpts{}); err != nil {
		return nil // already absent
	}
	if _, err := runGit(c.workDir, []string{"remote", "remove", "origin"}, runOpts{}); err != nil {
		return fmt.Errorf("git: remove-origin: %w", err)
	}
	c.remoteURL = ""
	return nil
}

// trimTrailingNewline strips a single trailing newline (or pair
// for CRLF) from byte output. Git commands always terminate
// output lines with \n; this helper exists so verbs aren't
// repeating strings.TrimSpace (which also strips other
// whitespace that might be syntactically meaningful in a URL,
// path, etc).
func trimTrailingNewline(b []byte) string {
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}
