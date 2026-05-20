package gitcli

// content.go — Phase 2 read-only content operations: file
// contents, tree entries, blob walks, commit log walks.
//
// All verbs here are read-only and do not acquire the project
// lock. They funnel through runGit and rely on its stderr →
// typed-error mapping. Output framing for log walks uses 0x1e
// (record separator) between commits because commit bodies can
// contain any character including newlines + NULs.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// recordSep is the 0x1e ASCII Record Separator. We use it to
// terminate per-commit records in --format strings because real
// commit bodies essentially never contain it (it's a control
// character with no syntactic purpose in code/prose).
const recordSep = "\x1e"

// ReadFile reads a file from the working tree at a repo-relative
// path. Pure OS read — no git involvement at all.
func (c *Clone) ReadFile(repoRelPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.workDir, repoRelPath))
}

// ListBundleFiles returns the repo-relative slash paths under
// pathspec that git considers in scope for a template bundle:
// tracked files PLUS untracked files that are not gitignored.
// It shells `git ls-files --cached --others --exclude-standard`,
// so .gitignore, the global excludes file, and .git/info/exclude
// are all honored exactly as the operator's git would — no
// hand-rolled ignore logic, and it reads the index rather than
// statting the (possibly multi-GB) data tree. An empty pathspec
// scopes the whole repository. ls-files reports symlinks as
// ordinary entries; the caller lstat-filters those out.
func (c *Clone) ListBundleFiles(pathspec string) ([]string, error) {
	args := []string{"ls-files", "--cached", "--others", "--exclude-standard", "-z"}
	if pathspec != "" {
		args = append(args, "--", pathspec)
	}
	out, err := runGit(c.workDir, args, runOpts{})
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	seen := make(map[string]struct{})
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue // --cached/--others are disjoint; defensive
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths, nil
}

// ReadFileAtCommit reads the contents of path at the given commit.
// ok=false (with nil err) when the commit exists but path doesn't.
// Returns ErrCommitNotFound when sha can't be resolved locally
// AND a lazy fetch from origin doesn't recover it.
//
// One lazy fetch attempt mirrors gitv6's behavior: callers up the
// stack pass commit SHAs that may have been pushed by a peer bot
// between our last fetch and this read. The fetch is gated on
// remoteURL being set so path-mode bootstrapping doesn't trip on
// a not-yet-wired origin.
func (c *Clone) ReadFileAtCommit(sha, path string) ([]byte, bool, error) {
	if err := c.ensureCommitLocal(sha); err != nil {
		return nil, false, err
	}
	// cat-file -e <sha>:<path> exits 0 iff the file exists in the
	// tree. Cheap existence probe before paying for content read.
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", sha + ":" + path}, runOpts{}); err != nil {
		// File-doesn't-exist surfaces as classifyStderr's
		// ErrCommitNotFound bucket too (git uses the same "bad
		// object" wording for missing-path-in-tree). Distinguish
		// by: commit existed (ensureCommitLocal above), so a miss
		// here means the file's absent, not the commit.
		return nil, false, nil
	}
	out, err := runGit(c.workDir, []string{"cat-file", "-p", sha + ":" + path}, runOpts{})
	if err != nil {
		return nil, false, fmt.Errorf("git: read %s at %s: %w", path, sha, err)
	}
	return out, true, nil
}

// ReadTreeEntriesAtCommit returns the direct entries of a
// directory at a specific commit's tree. ok=false when dirPath
// doesn't resolve to a subtree (missing dir, or path resolves
// to a blob); ok=true with empty entries means an empty dir.
//
// Lazy fetch on missing commit, same as ReadFileAtCommit.
func (c *Clone) ReadTreeEntriesAtCommit(sha, dirPath string) ([]TreeEntry, bool, error) {
	if err := c.ensureCommitLocal(sha); err != nil {
		return nil, false, err
	}
	dirPath = strings.Trim(dirPath, "/")
	// Confirm path resolves to a tree before listing. cat-file -t
	// returns "tree" / "blob" / "commit" etc., or errors when the
	// path doesn't exist.
	treeish := sha
	if dirPath != "" {
		treeish = sha + ":" + dirPath
	} else {
		treeish = sha + "^{tree}"
	}
	typOut, err := runGit(c.workDir, []string{"cat-file", "-t", treeish}, runOpts{})
	if err != nil {
		// "bad object" — path doesn't exist in tree. Not an error
		// for this verb's contract: ok=false signals "absent".
		return nil, false, nil
	}
	if strings.TrimSpace(string(typOut)) != "tree" {
		// Path resolves to a blob (or other non-tree).
		return nil, false, nil
	}
	// ls-tree -z gives NUL-terminated entries. Format per entry:
	// "<mode> <type> <sha>\t<name>\0"
	out, err := runGit(c.workDir, []string{"ls-tree", "-z", treeish}, runOpts{})
	if err != nil {
		return nil, false, fmt.Errorf("git: ls-tree %s: %w", treeish, err)
	}
	var entries []TreeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		// Split on \t to separate "<mode> <type> <sha>" from name.
		head, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(head)
		if len(fields) != 3 {
			continue
		}
		modeOctal, typ := fields[0], fields[1]
		entries = append(entries, TreeEntry{
			Name:  name,
			IsDir: typ == "tree",
			Mode:  parseGitMode(modeOctal),
		})
	}
	return entries, true, nil
}

// WalkSubtreeBlobsAtCommit recursively walks every blob under
// dirPath at sha and invokes visit(relPath, mode, content) for
// each. relPath is forward-slash separated and rooted at dirPath
// (i.e. doesn't include dirPath itself).
//
// Every blob in the git tree is materialized — including those
// whose path components start with `.` (`.gitignore`, `.mcp.json`,
// `.editorconfig`, `.github/workflows/*`, `.env.example`, etc.).
// Pre-fix the walker filtered dot-prefixed segments on the
// rationale of "skip .git, .DS_Store, etc.", but git's tree
// representation has no entry for `.git/` (that's the repo store,
// not the worktree) and `.DS_Store` only shows up here if the
// user explicitly committed it — same answer for any dotfile.
// Tracking is the user's decision; the materializer doesn't
// second-guess it. The earlier `.enju/` carve-out for the audit
// trail is now subsumed by this same principle.
//
// dirPath missing or resolving to a non-tree is a no-op (visitor
// not called); returns nil.
func (c *Clone) WalkSubtreeBlobsAtCommit(sha, dirPath string, visit BlobVisitor) error {
	if err := c.ensureCommitLocal(sha); err != nil {
		return err
	}
	dirPath = strings.Trim(dirPath, "/")
	treeish := sha + "^{tree}"
	if dirPath != "" {
		treeish = sha + ":" + dirPath
	}
	// Confirm the path resolves to a tree; ls-tree -r on a blob
	// or missing path produces no output (which we'd treat as
	// empty), so disambiguate explicitly.
	typOut, err := runGit(c.workDir, []string{"cat-file", "-t", treeish}, runOpts{})
	if err != nil || strings.TrimSpace(string(typOut)) != "tree" {
		return nil
	}
	// ls-tree -r -z lists all blobs recursively, NUL-terminated.
	out, err := runGit(c.workDir, []string{"ls-tree", "-r", "-z", treeish}, runOpts{})
	if err != nil {
		return fmt.Errorf("git: ls-tree -r %s: %w", treeish, err)
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		head, relPath, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(head)
		if len(fields) != 3 {
			continue
		}
		modeOctal, typ, blobSHA := fields[0], fields[1], fields[2]
		if typ != "blob" {
			// Submodule (commit) or other — skip.
			continue
		}
		body, err := runGit(c.workDir, []string{"cat-file", "-p", blobSHA}, runOpts{})
		if err != nil {
			return fmt.Errorf("git: read blob %s for %s: %w", blobSHA, relPath, err)
		}
		if vErr := visit(relPath, parseGitMode(modeOctal), body); vErr != nil {
			return vErr
		}
	}
	return nil
}

// ListBlobPathsAtCommit returns every blob path in the tree at
// sha, forward-slash separated, sorted by git's tree order.
// Path-only: no blob contents are read, so this is cheap on big
// repos. Submodules and other non-blob entries are skipped.
//
// Lazy fetch on missing commit, same as ReadFileAtCommit. A
// path resolving to a non-tree at the root is impossible (the
// root of a commit is always a tree), so no ok-flag is needed.
func (c *Clone) ListBlobPathsAtCommit(sha string) ([]string, error) {
	if err := c.ensureCommitLocal(sha); err != nil {
		return nil, err
	}
	treeish := sha + "^{tree}"
	out, err := runGit(c.workDir, []string{"ls-tree", "-r", "-z", treeish}, runOpts{})
	if err != nil {
		return nil, fmt.Errorf("git: ls-tree -r %s: %w", treeish, err)
	}
	var paths []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		head, relPath, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(head)
		if len(fields) != 3 {
			continue
		}
		if fields[1] != "blob" {
			// Submodule / commit — skip.
			continue
		}
		paths = append(paths, relPath)
	}
	return paths, nil
}

// LogFile returns commits that touched relPath, newest-first.
// Used by per-file history readers (enju_get_artifact_history).
//
// Returns []CommitInfo with Hash / Message / Author name /
// Author time per commit.
func (c *Clone) LogFile(relPath, branch string) ([]CommitInfo, error) {
	args := []string{"log", "--format=%H%n%ct%n%an%n%B" + recordSep}
	if branch != "" {
		if strings.HasPrefix(branch, "--") {
			return nil, fmt.Errorf("invalid branch name: %q", branch)
		}
		args = append(args, branch)
	}
	args = append(args, "--", relPath)
	// Per-commit format: SHA \n ctime \n authorname \n body \x1e
	out, err := runGit(c.workDir, args, runOpts{})
	if err != nil {
		return nil, fmt.Errorf("git: log %s: %w", relPath, err)
	}
	var infos []CommitInfo
	for _, rec := range splitOnRecordSep(string(out)) {
		// Per-commit shape: "sha\nctime\nauthor\nbody"
		// Body can contain newlines, so use SplitN with N=4.
		parts := strings.SplitN(rec, "\n", 4)
		if len(parts) < 4 {
			continue
		}
		sha := strings.TrimSpace(parts[0])
		if !isHexSHA(sha) {
			continue
		}
		ct, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		infos = append(infos, CommitInfo{
			Hash:    sha,
			Message: parts[3],
			Author:  strings.TrimSpace(parts[2]),
			Time:    time.Unix(ct, 0),
		})
	}
	return infos, nil
}

// WalkRecentCommits walks HEAD newest-first for up to maxWalk
// commits, calling visit(sha, message). visit returns false to
// stop early. maxWalk <= 0 walks the whole history.
//
// Note on early-exit: git CLI doesn't natively support a
// callback-driven early stop, so when visit returns false we
// simply stop calling it — we don't kill the subprocess
// mid-stream. The subprocess overhead is bounded at maxWalk
// commits anyway, which is the caller's bound regardless.
func (c *Clone) WalkRecentCommits(maxWalk int, visit func(sha, message string) bool) error {
	args := []string{"log", "--format=%H%n%B" + recordSep}
	if maxWalk > 0 {
		args = append(args, "-n", strconv.Itoa(maxWalk))
	}
	out, err := runGit(c.workDir, args, runOpts{})
	if err != nil {
		// Empty repo / unborn HEAD: log fails. Mirror gitv6's
		// behavior — silently return nil rather than surfacing
		// the error. Callers visiting "nothing" is the natural
		// no-op.
		return nil
	}
	return walkLogRecords(string(out), visit)
}

// WalkCommitsFrom walks ancestry first-parent-first starting at
// fromSHA, calling visit for up to maxWalk commits (<=0 = all).
// Returns ErrCommitNotFound when fromSHA isn't in the local
// object DB (no lazy fetch — this verb is used by diagnostics).
func (c *Clone) WalkCommitsFrom(fromSHA string, maxWalk int, visit func(sha, message string) bool) error {
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", fromSHA + "^{commit}"}, runOpts{}); err != nil {
		return ErrCommitNotFound
	}
	args := []string{"log", "--format=%H%n%B" + recordSep}
	if maxWalk > 0 {
		args = append(args, "-n", strconv.Itoa(maxWalk))
	}
	args = append(args, fromSHA)
	out, err := runGit(c.workDir, args, runOpts{})
	if err != nil {
		return fmt.Errorf("git: log %s: %w", fromSHA, err)
	}
	return walkLogRecords(string(out), visit)
}

// ScanBranchSince walks commits on refs/remotes/origin/<branch>
// (falling back to refs/heads/<branch>) newer than `since`
// (exclusive) up to tip (inclusive). Returns the new tip SHA —
// callers persist this as the next cursor.
//
// Semantics (parity with gitv6):
//   - since == "" → first scan: return tip + don't visit
//   - since == tip → no-op: return tip + don't visit
//   - since unreachable → walk from tip without stop (force-push
//     / rebase scenario): caller's reconcile is idempotent
//   - otherwise → walk since..tip exclusive of since
//
// Visits in chronological order (ancestor → tip).
func (c *Clone) ScanBranchSince(branch, since string, visit func(sha, message string)) (string, error) {
	defer c.lock()()
	if branch == "" {
		return since, fmt.Errorf("git: ScanBranchSince: branch required")
	}
	// Resolve tip: prefer origin tracking, fall back to local.
	tip := ""
	for _, ref := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
		out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", ref}, runOpts{})
		if err == nil {
			tip = strings.TrimSpace(string(out))
			break
		}
	}
	if tip == "" {
		return since, nil // unknown branch is a no-op, not an error
	}
	if since == "" || since == tip {
		return tip, nil
	}

	// stopOnSince: if `since` exists as a commit locally, walk
	// since..tip exclusive. Otherwise walk from tip back to root
	// (force-push / rebase scenario — caller is idempotent).
	stopOnSince := true
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", since + "^{commit}"}, runOpts{}); err != nil {
		stopOnSince = false
	}

	var args []string
	if stopOnSince {
		args = []string{"log", "--reverse", "--format=%H%n%B" + recordSep, since + ".." + tip}
	} else {
		args = []string{"log", "--reverse", "--format=%H%n%B" + recordSep, tip}
	}
	out, err := runGit(c.workDir, args, runOpts{})
	if err != nil {
		return since, fmt.Errorf("git: log %s: %w", branch, err)
	}
	// `--reverse` gives chronological order, so we visit directly
	// without a buffer-and-flip step (gitv6 had to do that
	// because gogit's iterator only walks newest-first).
	_ = walkLogRecords(string(out), func(sha, msg string) bool {
		visit(sha, msg)
		return true
	})
	return tip, nil
}

// --- helpers ---

// ensureCommitLocal verifies that sha resolves to a commit in
// the local object DB. On miss, attempts one fetch from origin
// (if remoteURL is set) and retries. Returns ErrCommitNotFound
// when the commit still can't be found.
func (c *Clone) ensureCommitLocal(sha string) error {
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", sha + "^{commit}"}, runOpts{}); err == nil {
		return nil
	}
	if c.remoteURL == "" {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
	}
	// One fetch attempt. We invoke `git fetch` directly rather
	// than going through this package's Fetch() (which doesn't
	// exist yet during Phase 2). When Phase 5 lands, this stays
	// independent because it intentionally bypasses the lock —
	// reads must not block on a held write lock.
	if _, err := runGit(c.workDir, []string{"fetch", "origin", "+refs/heads/*:refs/remotes/origin/*"}, runOpts{network: true}); err != nil {
		// Surface the fetch failure as part of ErrCommitNotFound
		// so callers see one error to handle, not two.
		return fmt.Errorf("%w (fetch failed: %v)", ErrCommitNotFound, err)
	}
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", sha + "^{commit}"}, runOpts{}); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
	}
	return nil
}

// walkLogRecords parses output produced by runGit("log ... --format=%H%n%B<RS>")
// and invokes visit for each. Tolerates the leading newline git
// emits between commits.
func walkLogRecords(out string, visit func(sha, message string) bool) error {
	for _, rec := range splitOnRecordSep(out) {
		// Per-record: "sha\nbody". Use Cut not Split so body's
		// own newlines are preserved.
		sha, body, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		sha = strings.TrimSpace(sha)
		if !isHexSHA(sha) {
			continue
		}
		if !visit(sha, body) {
			return nil
		}
	}
	return nil
}

// splitOnRecordSep splits an output blob on 0x1e and trims the
// leading/trailing whitespace that git's --format adds between
// records. Drops empty records.
func splitOnRecordSep(s string) []string {
	var out []string
	for _, rec := range strings.Split(s, recordSep) {
		// Git separates records with a "\n" between commits when
		// using --format. Trim only the leading \n we know is
		// from that boundary, but preserve internal body content.
		rec = strings.TrimPrefix(rec, "\n")
		rec = strings.TrimSuffix(rec, "\n")
		if rec == "" {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// parseGitMode converts an octal mode string from ls-tree (e.g.
// "100644", "100755", "40000") into an os.FileMode with just the
// permission bits. Symlinks (120000) and submodules (160000) get
// 0644 because callers don't materialize them as anything
// meaningful at this layer.
func parseGitMode(octalStr string) os.FileMode {
	n, err := strconv.ParseUint(octalStr, 8, 32)
	if err != nil {
		return 0o644
	}
	// Tree modes: 0o100644 (regular), 0o100755 (executable),
	// 0o40000 (subtree), 0o120000 (symlink), 0o160000 (gitlink).
	// Mask to keep only the 9-bit permission part.
	return os.FileMode(n & 0o777)
}


