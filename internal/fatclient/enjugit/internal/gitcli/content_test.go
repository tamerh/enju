package gitcli

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// commitWithMessage writes a file and commits it with a custom
// message. Used by tests that want specific body content for log
// parsing.
func commitWithMessage(t *testing.T, dir, filename, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", filename)
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", message)
	return strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
}

// TestListBundleFiles reproduces ISSUE-2: the template-pin walker
// must honor .gitignore and must not crash on a directory symlink.
// The old filepath.Walk swept a gitignored multi-GB tree into
// `git add` (rejected) and os.ReadFile'd a dir-symlink ("is a
// directory"). git ls-files closes both: ignored paths are gone
// before the caller sees them, and ls-files never stats a symlink
// target so it can't crash on one.
func TestListBundleFiles(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	mk := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("enju.yaml", "name: x\n")
	mk(".gitignore", "resources/\n")
	mk("scripts/run.sh", "#!/bin/sh\n")
	mk("resources/big.db", "HUGE")           // gitignored
	mk("resources/raw/reads.fastq", "@SEQ")  // gitignored
	// Directory symlink inside the ignored tree — the phase-1
	// crash trigger under the old walker. ls-files must not stat it.
	if err := os.Symlink("raw", filepath.Join(dir, "resources", "current")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	// A non-ignored in-tree symlink: ls-files DOES list it (a
	// symlink is a blob to git); skipping it is the caller's job,
	// so assert it surfaces here.
	if err := os.Symlink("/tmp", filepath.Join(dir, "external")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.ListBundleFiles("")
	if err != nil {
		t.Fatalf("ListBundleFiles must not crash on a dir symlink / huge ignored tree: %v", err)
	}
	in := map[string]bool{}
	for _, p := range got {
		in[p] = true
	}
	for _, want := range []string{"enju.yaml", ".gitignore", "scripts/run.sh", "external"} {
		if !in[want] {
			t.Errorf("expected %q in scope; got %v", want, got)
		}
	}
	for _, banned := range []string{"resources/big.db", "resources/raw/reads.fastq", "resources/current"} {
		if in[banned] {
			t.Errorf("gitignored path %q must be excluded; got %v", banned, got)
		}
	}
}

// TestListBundleFiles_Pathspec pins the scoped-enumeration path
// (the common case: bundleDir="enju/templates/foo"): only that
// subtree is returned, sibling dirs and the repo root are not.
func TestListBundleFiles_Pathspec(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	for _, rel := range []string{
		"enju.yaml",
		"workflows/a/enju.yaml",
		"workflows/a/scripts/x.sh",
		"workflows/b/enju.yaml",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.ListBundleFiles("workflows/a")
	if err != nil {
		t.Fatalf("ListBundleFiles: %v", err)
	}
	sort.Strings(got)
	want := []string{"workflows/a/enju.yaml", "workflows/a/scripts/x.sh"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pathspec scoping: got %v, want %v (root + sibling must be excluded)", got, want)
	}
}

// --- ReadFile ---

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := OpenClone(dir, "", nullLogger())
	out, err := c.ReadFile("a.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("got %q, want hello", out)
	}
}

func TestReadFileMissing(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	c, _ := OpenClone(dir, "", nullLogger())
	if _, err := c.ReadFile("nope.txt"); err == nil {
		t.Error("expected error for missing file")
	}
}

// --- ReadFileAtCommit ---

func TestReadFileAtCommit(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "hello-at-commit")

	c, _ := OpenClone(dir, "", nullLogger())
	out, ok, err := c.ReadFileAtCommit(sha, "a.txt")
	if err != nil {
		t.Fatalf("ReadFileAtCommit: %v", err)
	}
	if !ok {
		t.Fatal("ok=false on present file")
	}
	if string(out) != "hello-at-commit" {
		t.Errorf("got %q, want hello-at-commit", out)
	}
}

func TestReadFileAtCommitMissingPath(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	_, ok, err := c.ReadFileAtCommit(sha, "nope.txt")
	if err != nil {
		t.Fatalf("ReadFileAtCommit: %v", err)
	}
	if ok {
		t.Error("ok=true on missing path")
	}
}

func TestReadFileAtCommitUnknownSHA(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	_, _, err := c.ReadFileAtCommit(bogus, "a.txt")
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

// --- ReadTreeEntriesAtCommit ---

func TestReadTreeEntriesAtCommitRoot(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), []byte("i"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "seed")
	sha := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))

	c, _ := OpenClone(dir, "", nullLogger())
	entries, ok, err := c.ReadTreeEntriesAtCommit(sha, "")
	if err != nil {
		t.Fatalf("ReadTreeEntriesAtCommit: %v", err)
	}
	if !ok {
		t.Fatal("ok=false on root tree")
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = e.IsDir
	}
	if got["sub"] != true {
		t.Errorf("sub entry: got %v, want IsDir=true", got)
	}
	if _, exists := got["top.txt"]; !exists {
		t.Errorf("top.txt missing")
	}
	if got["top.txt"] != false {
		t.Errorf("top.txt: got IsDir=true, want false")
	}
}

func TestReadTreeEntriesAtCommitSubdir(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), []byte("i"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "seed")
	sha := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))

	c, _ := OpenClone(dir, "", nullLogger())
	entries, ok, err := c.ReadTreeEntriesAtCommit(sha, "sub")
	if err != nil {
		t.Fatalf("ReadTreeEntriesAtCommit: %v", err)
	}
	if !ok {
		t.Fatal("ok=false on present subdir")
	}
	if len(entries) != 1 || entries[0].Name != "inner.txt" {
		t.Errorf("entries = %+v, want [inner.txt]", entries)
	}
}

func TestReadTreeEntriesAtCommitMissingPathOkFalse(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	_, ok, err := c.ReadTreeEntriesAtCommit(sha, "no-such-dir")
	if err != nil {
		t.Fatalf("ReadTreeEntriesAtCommit: %v", err)
	}
	if ok {
		t.Error("ok=true on missing path")
	}
}

func TestReadTreeEntriesAtCommitPathIsFileOkFalse(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	_, ok, err := c.ReadTreeEntriesAtCommit(sha, "a.txt")
	if err != nil {
		t.Fatalf("ReadTreeEntriesAtCommit: %v", err)
	}
	if ok {
		t.Error("ok=true when path resolves to blob")
	}
}

func TestReadTreeEntriesExecutableModePreserved(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", "run.sh")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "x")
	sha := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))

	c, _ := OpenClone(dir, "", nullLogger())
	entries, ok, err := c.ReadTreeEntriesAtCommit(sha, "")
	if err != nil || !ok {
		t.Fatalf("ReadTreeEntriesAtCommit: ok=%v err=%v", ok, err)
	}
	for _, e := range entries {
		if e.Name == "run.sh" {
			if e.Mode&0o100 == 0 {
				t.Errorf("run.sh mode = %v, expected executable", e.Mode)
			}
			return
		}
	}
	t.Error("run.sh not found in entries")
}

// --- WalkSubtreeBlobsAtCommit ---

func TestWalkSubtreeBlobsAtCommit(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	for _, p := range []string{"a.txt", "sub/b.txt", "sub/nested/c.txt"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content-"+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "seed")
	sha := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))

	c, _ := OpenClone(dir, "", nullLogger())
	got := map[string]string{}
	err := c.WalkSubtreeBlobsAtCommit(sha, "", func(relPath string, mode os.FileMode, content []byte) error {
		got[relPath] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkSubtreeBlobsAtCommit: %v", err)
	}
	want := map[string]string{
		"a.txt":            "content-a.txt",
		"sub/b.txt":        "content-sub/b.txt",
		"sub/nested/c.txt": "content-sub/nested/c.txt",
	}
	if len(got) != len(want) {
		t.Errorf("walked %d, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestWalkSubtreeBlobsWalksDotfiles pins the contract that
// the walker materializes EVERY blob in git's tree, including
// dotfiles. Tracking is the user's decision: if they committed
// `.mcp.json` or `.editorconfig` or anything under `.github/`,
// they did so on purpose and the materializer must honor it.
//
// Pre-fix the walker filtered any path component starting with
// `.` on the rationale of "skip .git, .DS_Store, etc.", but
// git's tree representation has no entry for `.git/` and
// `.DS_Store` only appears here if the user explicitly committed
// it — same answer either way. Showcase TP53 hit this when the
// user authored `.mcp.json` at project root: the snapshot
// silently dropped it, claude couldn't find the biobtree MCP
// config, every section ran without tools.
func TestWalkSubtreeBlobsWalksDotfiles(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	for _, p := range []string{
		"visible.txt",
		".mcp.json",                  // common case: claude MCP config at root
		".gitignore",                 // tracked gitignore
		".github/workflows/ci.yml",   // CI config under dotted parent
		"sub/.env.example",           // dotfile in subdir
		"sub/.dotdir/nested.txt",     // file under dotted subdir
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "seed")
	sha := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))

	c, _ := OpenClone(dir, "", nullLogger())
	var visited []string
	_ = c.WalkSubtreeBlobsAtCommit(sha, "", func(relPath string, _ os.FileMode, _ []byte) error {
		visited = append(visited, relPath)
		return nil
	})
	sort.Strings(visited)
	want := []string{
		".github/workflows/ci.yml",
		".gitignore",
		".mcp.json",
		"sub/.dotdir/nested.txt",
		"sub/.env.example",
		"visible.txt",
	}
	if !equalSlice(visited, want) {
		t.Errorf("visited %v, want %v", visited, want)
	}
}

func TestWalkSubtreeBlobsMissingPathIsNoop(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	called := false
	err := c.WalkSubtreeBlobsAtCommit(sha, "nope", func(_ string, _ os.FileMode, _ []byte) error {
		called = true
		return nil
	})
	if err != nil {
		t.Errorf("missing path should be no-op, got err=%v", err)
	}
	if called {
		t.Error("visitor called on missing path")
	}
}

// --- WalkRecentCommits ---

func TestWalkRecentCommitsBounded(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	// 3 commits.
	commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")
	commitWithMessage(t, dir, "c.txt", "3", "third")

	c, _ := OpenClone(dir, "", nullLogger())
	var msgs []string
	err := c.WalkRecentCommits(2, func(_, msg string) bool {
		msgs = append(msgs, strings.TrimSpace(msg))
		return true
	})
	if err != nil {
		t.Fatalf("WalkRecentCommits: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("walked %d, want 2: %v", len(msgs), msgs)
	}
	if msgs[0] != "third" || msgs[1] != "second" {
		t.Errorf("newest-first order broken: %v", msgs)
	}
}

func TestWalkRecentCommitsEarlyStop(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")
	commitWithMessage(t, dir, "c.txt", "3", "third")

	c, _ := OpenClone(dir, "", nullLogger())
	var msgs []string
	_ = c.WalkRecentCommits(0, func(_, msg string) bool {
		msgs = append(msgs, strings.TrimSpace(msg))
		return len(msgs) < 1 // stop after first
	})
	if len(msgs) != 1 {
		t.Errorf("walked %d, want 1: %v", len(msgs), msgs)
	}
}

func TestWalkRecentCommitsEmptyRepoNoError(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir) // unborn HEAD

	c, _ := OpenClone(dir, "", nullLogger())
	called := false
	err := c.WalkRecentCommits(10, func(_, _ string) bool {
		called = true
		return true
	})
	if err != nil {
		t.Errorf("empty repo should be no-op, got %v", err)
	}
	if called {
		t.Error("visitor called on empty repo")
	}
}

func TestWalkRecentCommitsPreservesMultiLineBody(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "subject line\n\nbody line 1\nbody line 2")

	c, _ := OpenClone(dir, "", nullLogger())
	var captured string
	_ = c.WalkRecentCommits(1, func(_, msg string) bool {
		captured = msg
		return true
	})
	if !strings.Contains(captured, "subject line") {
		t.Errorf("missing subject: %q", captured)
	}
	if !strings.Contains(captured, "body line 1") || !strings.Contains(captured, "body line 2") {
		t.Errorf("body lines lost: %q", captured)
	}
}

// --- WalkCommitsFrom ---

func TestWalkCommitsFromAtSpecificCommit(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")
	commitWithMessage(t, dir, "c.txt", "3", "third")

	c, _ := OpenClone(dir, "", nullLogger())
	var msgs []string
	err := c.WalkCommitsFrom(s1, 10, func(_, msg string) bool {
		msgs = append(msgs, strings.TrimSpace(msg))
		return true
	})
	if err != nil {
		t.Fatalf("WalkCommitsFrom: %v", err)
	}
	// From s1 we should only see s1 (no ancestors except root parent).
	if len(msgs) != 1 || msgs[0] != "first" {
		t.Errorf("got %v, want [first]", msgs)
	}
}

func TestWalkCommitsFromUnknownReturnsErrCommitNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	err := c.WalkCommitsFrom(bogus, 10, func(_, _ string) bool { return true })
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

// --- LogFile ---

func TestLogFileReturnsTouchingCommits(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "v1", "init a")
	commitWithMessage(t, dir, "other.txt", "x", "unrelated")
	commitWithMessage(t, dir, "a.txt", "v2", "update a")

	c, _ := OpenClone(dir, "", nullLogger())
	infos, err := c.LogFile("a.txt", "")
	if err != nil {
		t.Fatalf("LogFile: %v", err)
	}
	if len(infos) != 2 {
		t.Errorf("got %d entries, want 2: %+v", len(infos), infos)
	}
	// Newest first.
	if !strings.Contains(infos[0].Message, "update a") {
		t.Errorf("newest entry: got %q, want update a", infos[0].Message)
	}
	if infos[0].Author != "t" {
		t.Errorf("author = %q, want t", infos[0].Author)
	}
	if infos[0].Time.IsZero() {
		t.Error("time should not be zero")
	}
}

// --- ScanBranchSince ---

func TestScanBranchSinceBaselineReturnsTipNoVisits(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	tip := commitWithMessage(t, dir, "a.txt", "1", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	var visited []string
	newTip, err := c.ScanBranchSince("main", "", func(sha, _ string) {
		visited = append(visited, sha)
	})
	if err != nil {
		t.Fatalf("ScanBranchSince: %v", err)
	}
	if newTip != tip {
		t.Errorf("newTip = %s, want %s", newTip, tip)
	}
	if len(visited) != 0 {
		t.Errorf("baseline should not visit any commit, got %v", visited)
	}
}

func TestScanBranchSinceIncrementalChronological(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	baseline := commitWithMessage(t, dir, "a.txt", "1", "first")
	s2 := commitWithMessage(t, dir, "b.txt", "2", "second")
	s3 := commitWithMessage(t, dir, "c.txt", "3", "third")

	c, _ := OpenClone(dir, "", nullLogger())
	var visited []string
	newTip, err := c.ScanBranchSince("main", baseline, func(sha, _ string) {
		visited = append(visited, sha)
	})
	if err != nil {
		t.Fatalf("ScanBranchSince: %v", err)
	}
	if newTip != s3 {
		t.Errorf("newTip = %s, want %s", newTip, s3)
	}
	if len(visited) != 2 || visited[0] != s2 || visited[1] != s3 {
		t.Errorf("visit order broken: %v, want [%s, %s]", visited, s2, s3)
	}
}

func TestScanBranchSinceNoopWhenAtTip(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	tip := commitWithMessage(t, dir, "a.txt", "1", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	var visited []string
	newTip, err := c.ScanBranchSince("main", tip, func(sha, _ string) {
		visited = append(visited, sha)
	})
	if err != nil {
		t.Fatalf("ScanBranchSince: %v", err)
	}
	if newTip != tip {
		t.Errorf("newTip = %s, want %s", newTip, tip)
	}
	if len(visited) != 0 {
		t.Errorf("noop case should not visit, got %v", visited)
	}
}

func TestScanBranchSinceUnknownBranchReturnsSinceNoError(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	newTip, err := c.ScanBranchSince("never-existed", "abc123", func(_, _ string) {})
	if err != nil {
		t.Errorf("unknown branch should not error: %v", err)
	}
	if newTip != "abc123" {
		t.Errorf("newTip = %s, want abc123 (unchanged)", newTip)
	}
}

func TestScanBranchSinceUnreachableSinceWalksFromRoot(t *testing.T) {
	// Force-push / rebase scenario: caller's cursor is now
	// unreachable. Verb should walk from tip without stopping —
	// caller's reconcile is idempotent so the duplicates are OK.
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")

	c, _ := OpenClone(dir, "", nullLogger())
	var visited []string
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	_, err := c.ScanBranchSince("main", bogus, func(sha, _ string) {
		visited = append(visited, sha)
	})
	if err != nil {
		t.Fatalf("ScanBranchSince: %v", err)
	}
	if len(visited) != 2 {
		t.Errorf("expected to walk all 2 commits from root, got %v", visited)
	}
}
