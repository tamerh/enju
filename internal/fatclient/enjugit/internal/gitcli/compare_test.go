package gitcli

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --- CompareToRemote ---

func TestCompareToRemoteAllStates(t *testing.T) {
	// Build a fixture covering all states we can:
	//   - in-sync: local == origin
	//   - ahead: local has commits past origin
	//   - behind: origin has commits past local
	//   - diverged: both have unique commits
	//   - local-only: refs/heads/<X> exists, no origin/<X>
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	commitWithMessage(t, seed, "a.txt", "base", "base")
	gitRun(t, seed, "push", "origin", "main")

	// in-sync branch.
	gitRun(t, seed, "branch", "in-sync-branch")
	gitRun(t, seed, "push", "origin", "in-sync-branch")

	// ahead branch: local has 1 extra commit.
	gitRun(t, seed, "branch", "ahead-branch")
	gitRun(t, seed, "push", "origin", "ahead-branch")
	gitRun(t, seed, "checkout", "ahead-branch")
	commitWithMessage(t, seed, "ahead.txt", "x", "ahead-only")
	gitRun(t, seed, "checkout", "main")

	// behind branch: origin has 1 extra commit, then we rewind local.
	gitRun(t, seed, "branch", "behind-branch")
	gitRun(t, seed, "checkout", "behind-branch")
	commitWithMessage(t, seed, "behind.txt", "y", "behind-future")
	gitRun(t, seed, "push", "origin", "behind-branch")
	gitRun(t, seed, "reset", "--hard", "HEAD~1") // rewind local
	gitRun(t, seed, "checkout", "main")

	// local-only: branch exists locally, never pushed.
	gitRun(t, seed, "branch", "local-only")

	c, _ := OpenClone(seed, "", nullLogger())
	cmp, err := c.CompareToRemote([]string{"in-sync-branch", "ahead-branch", "behind-branch", "local-only"})
	if err != nil {
		t.Fatalf("CompareToRemote: %v", err)
	}
	got := map[string]RemoteState{}
	for _, bs := range cmp.Branches {
		got[bs.Name] = bs.State
	}
	want := map[string]RemoteState{
		"in-sync-branch": RemoteInSync,
		"ahead-branch":   RemoteAhead,
		"behind-branch":  RemoteBehind,
		"local-only":     RemoteAhead, // local-only = ahead (push would create)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s state = %s, want %s", name, got[name], w)
		}
	}
}

func TestCompareToRemoteDiverged(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	// Two clones diverge on the same branch.
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	gitInit(t, a)
	gitRun(t, a, "remote", "add", "origin", bare)
	commitWithMessage(t, a, "common.txt", "c", "common")
	gitRun(t, a, "push", "origin", "main")
	gitRun(t, ".", "clone", bare, b)

	// a commits + pushes.
	commitWithMessage(t, a, "from-a.txt", "x", "from-a")
	gitRun(t, a, "push", "origin", "main")

	// b commits divergent (different content) and fetches origin
	// so origin/main is now ahead of b's local main, but b's
	// local has a commit not in origin → diverged.
	commitWithMessage(t, b, "from-b.txt", "y", "from-b")
	gitRun(t, b, "fetch", "origin")

	c, _ := OpenClone(b, "", nullLogger())
	cmp, err := c.CompareToRemote([]string{"main"})
	if err != nil {
		t.Fatalf("CompareToRemote: %v", err)
	}
	if len(cmp.Branches) != 1 {
		t.Fatalf("expected 1 branch, got %d", len(cmp.Branches))
	}
	if cmp.Branches[0].State != RemoteDiverged {
		t.Errorf("state = %s, want diverged", cmp.Branches[0].State)
	}
}

func TestCompareToRemoteDefaultsToAllBranches(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	commitWithMessage(t, seed, "a.txt", "x", "first")
	gitRun(t, seed, "push", "origin", "main")
	gitRun(t, seed, "branch", "topic-a")
	gitRun(t, seed, "branch", "topic-b")

	c, _ := OpenClone(seed, "", nullLogger())
	cmp, err := c.CompareToRemote(nil) // nil = all
	if err != nil {
		t.Fatalf("CompareToRemote: %v", err)
	}
	names := []string{}
	for _, bs := range cmp.Branches {
		names = append(names, bs.Name)
	}
	sort.Strings(names)
	want := []string{"main", "topic-a", "topic-b"}
	if !equalSlice(names, want) {
		t.Errorf("got %v, want %v", names, want)
	}
}

// --- CompareCommits ---

func TestCompareCommitsInSync(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := commitWithMessage(t, dir, "a.txt", "x", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	res, err := c.CompareCommits(sha, sha)
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if res.State != RemoteInSync {
		t.Errorf("state = %s, want in-sync", res.State)
	}
}

func TestCompareCommitsAheadWithCount(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	base := commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")
	tip := commitWithMessage(t, dir, "c.txt", "3", "third")

	c, _ := OpenClone(dir, "", nullLogger())
	res, err := c.CompareCommits(tip, base)
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if res.State != RemoteAhead {
		t.Errorf("state = %s, want ahead", res.State)
	}
	if res.AheadBy != 2 {
		t.Errorf("AheadBy = %d, want 2", res.AheadBy)
	}
}

func TestCompareCommitsBehindWithCount(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	base := commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")
	tip := commitWithMessage(t, dir, "c.txt", "3", "third")

	c, _ := OpenClone(dir, "", nullLogger())
	res, err := c.CompareCommits(base, tip)
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if res.State != RemoteBehind {
		t.Errorf("state = %s, want behind", res.State)
	}
	if res.BehindBy != 2 {
		t.Errorf("BehindBy = %d, want 2", res.BehindBy)
	}
}

func TestCompareCommitsDivergedWithCounts(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "common")
	gitRun(t, dir, "branch", "other")
	// main: +1
	mainTip := commitWithMessage(t, dir, "main.txt", "m", "main-only")
	// other: +2 from the branch base
	gitRun(t, dir, "checkout", "other")
	commitWithMessage(t, dir, "o1.txt", "1", "other-1")
	otherTip := commitWithMessage(t, dir, "o2.txt", "2", "other-2")

	c, _ := OpenClone(dir, "", nullLogger())
	res, err := c.CompareCommits(mainTip, otherTip)
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if res.State != RemoteDiverged {
		t.Errorf("state = %s, want diverged", res.State)
	}
	if res.AheadBy != 1 {
		t.Errorf("AheadBy = %d, want 1", res.AheadBy)
	}
	if res.BehindBy != 2 {
		t.Errorf("BehindBy = %d, want 2", res.BehindBy)
	}
}

func TestCompareCommitsUnrelatedNoMergeBase(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	mainSHA := commitWithMessage(t, dir, "a.txt", "x", "first")
	// Create an orphan branch with no shared history.
	gitRun(t, dir, "checkout", "--orphan", "alien")
	gitRun(t, dir, "rm", "-rf", "--", ".")
	orphanSHA := commitWithMessage(t, dir, "alien.txt", "z", "alien root")

	c, _ := OpenClone(dir, "", nullLogger())
	res, err := c.CompareCommits(mainSHA, orphanSHA)
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if res.State != RemoteUnrelated {
		t.Errorf("state = %s, want unrelated", res.State)
	}
}

func TestCompareCommitsLocalMissingErrors(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := commitWithMessage(t, dir, "a.txt", "x", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	_, err := c.CompareCommits(bogus, sha)
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

func TestCompareCommitsRemoteMissingFallsBackToDiverged(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := commitWithMessage(t, dir, "a.txt", "x", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	// No origin remote → no fetch retry → diverged with zero
	// counts (best effort).
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	res, err := c.CompareCommits(sha, bogus)
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if res.State != RemoteDiverged {
		t.Errorf("state = %s, want diverged (best-effort fallback)", res.State)
	}
	if res.AheadBy != 0 || res.BehindBy != 0 {
		t.Errorf("counts should be zero when remote unresolved: ahead=%d behind=%d",
			res.AheadBy, res.BehindBy)
	}
}

func TestCompareCommitsEmptyArgsErrors(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "x", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	if _, err := c.CompareCommits("", "abc"); err == nil {
		t.Error("expected error on empty localSHA")
	}
	if _, err := c.CompareCommits("abc", ""); err == nil {
		t.Error("expected error on empty remoteSHA")
	}
}

// suppress unused import.
var _ = strings.TrimSpace
