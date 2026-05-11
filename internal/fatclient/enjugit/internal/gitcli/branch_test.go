package gitcli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// --- CreateBranchAt ---

func TestCreateBranchAt(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CreateBranchAt("feature-x", sha); err != nil {
		t.Fatalf("CreateBranchAt: %v", err)
	}
	got := strings.TrimSpace(gitRun(t, dir, "rev-parse", "refs/heads/feature-x"))
	if got != sha {
		t.Errorf("branch tip = %s, want %s", got, sha)
	}
}

func TestCreateBranchAtErrBranchExists(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CreateBranchAt("feature-x", sha); err != nil {
		t.Fatal(err)
	}
	err := c.CreateBranchAt("feature-x", sha)
	if !errors.Is(err, ErrBranchExists) {
		t.Errorf("expected ErrBranchExists, got %v", err)
	}
}

func TestCreateBranchAtErrCommitNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	err := c.CreateBranchAt("feature-x", bogus)
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

// --- DeleteBranch ---

func TestDeleteBranch(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")
	gitRun(t, dir, "update-ref", "refs/heads/topic-x", sha)

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.DeleteBranch("topic-x"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	// Verify gone.
	if _, err := c.LocalBranchHash("topic-x"); err != nil {
		t.Fatal(err)
	}
	hash, _ := c.LocalBranchHash("topic-x")
	if hash != "" {
		t.Errorf("topic-x still resolves to %s", hash)
	}
}

func TestDeleteBranchIdempotent(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.DeleteBranch("never-existed"); err != nil {
		t.Errorf("DeleteBranch on missing should be no-op, got %v", err)
	}
}

// --- SetBranchTo ---

func TestSetBranchTo(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	s2 := commitWithMessage(t, dir, "b.txt", "2", "second")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.SetBranchTo("topic-x", s1); err != nil {
		t.Fatalf("SetBranchTo (create): %v", err)
	}
	got, _ := c.LocalBranchHash("topic-x")
	if got != s1 {
		t.Errorf("got %s, want %s", got, s1)
	}
	// Overwrite to s2.
	if err := c.SetBranchTo("topic-x", s2); err != nil {
		t.Fatalf("SetBranchTo (overwrite): %v", err)
	}
	got, _ = c.LocalBranchHash("topic-x")
	if got != s2 {
		t.Errorf("got %s, want %s", got, s2)
	}
}

func TestSetBranchToErrCommitNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	err := c.SetBranchTo("topic-x", bogus)
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

// --- UpdateRef ---

func TestUpdateRefNoCAS(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	s2 := commitWithMessage(t, dir, "b.txt", "2", "second")
	gitRun(t, dir, "update-ref", "refs/heads/topic-x", s1)

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.UpdateRef("topic-x", s2, ""); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
	got, _ := c.LocalBranchHash("topic-x")
	if got != s2 {
		t.Errorf("got %s, want %s", got, s2)
	}
}

func TestUpdateRefCASSuccess(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	s2 := commitWithMessage(t, dir, "b.txt", "2", "second")
	gitRun(t, dir, "update-ref", "refs/heads/topic-x", s1)

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.UpdateRef("topic-x", s2, s1); err != nil {
		t.Fatalf("UpdateRef CAS: %v", err)
	}
	got, _ := c.LocalBranchHash("topic-x")
	if got != s2 {
		t.Errorf("got %s, want %s", got, s2)
	}
}

func TestUpdateRefCASFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	s2 := commitWithMessage(t, dir, "b.txt", "2", "second")
	s3 := commitWithMessage(t, dir, "c.txt", "3", "third")
	gitRun(t, dir, "update-ref", "refs/heads/topic-x", s2) // actually s2

	c, _ := OpenClone(dir, "", nullLogger())
	// Tell CAS we expect s1 (wrong) — should fail.
	err := c.UpdateRef("topic-x", s3, s1)
	if err == nil {
		t.Fatal("expected CAS to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "CAS") {
		t.Errorf("error should mention CAS, got %v", err)
	}
	// Ref must be unchanged.
	got, _ := c.LocalBranchHash("topic-x")
	if got != s2 {
		t.Errorf("ref changed despite CAS failure: %s, want %s", got, s2)
	}
}

func TestUpdateRefCreatesIfMissing(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	// Empty oldSHA + non-existent ref → create.
	if err := c.UpdateRef("brand-new", sha, ""); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
	got, _ := c.LocalBranchHash("brand-new")
	if got != sha {
		t.Errorf("got %s, want %s", got, sha)
	}
}

// --- EnsureOrigin / RemoveOrigin ---

func TestEnsureOriginCreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	c, _ := OpenClone(dir, "", nullLogger())
	if c.RemoteURL() != "" {
		t.Fatalf("precondition: RemoteURL should be empty, got %q", c.RemoteURL())
	}
	if err := c.EnsureOrigin("/path/to/remote"); err != nil {
		t.Fatalf("EnsureOrigin: %v", err)
	}
	if c.RemoteURL() != "/path/to/remote" {
		t.Errorf("RemoteURL = %q, want /path/to/remote", c.RemoteURL())
	}
	// Verify on disk.
	got := strings.TrimSpace(gitRun(t, dir, "remote", "get-url", "origin"))
	if got != "/path/to/remote" {
		t.Errorf("origin URL on disk = %q, want /path/to/remote", got)
	}
}

func TestEnsureOriginOverwritesWhenDifferent(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", "/old/url")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.EnsureOrigin("/new/url"); err != nil {
		t.Fatalf("EnsureOrigin: %v", err)
	}
	if c.RemoteURL() != "/new/url" {
		t.Errorf("RemoteURL = %q, want /new/url", c.RemoteURL())
	}
}

func TestEnsureOriginIdempotentWhenSame(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", "/same/url")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.EnsureOrigin("/same/url"); err != nil {
		t.Errorf("idempotent EnsureOrigin failed: %v", err)
	}
	if c.RemoteURL() != "/same/url" {
		t.Errorf("RemoteURL = %q, want /same/url", c.RemoteURL())
	}
}

func TestEnsureOriginEmptyURLNoop(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", "/existing/url")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.EnsureOrigin(""); err != nil {
		t.Errorf("EnsureOrigin('') should be no-op, got %v", err)
	}
	// On-disk origin must be unchanged.
	got := strings.TrimSpace(gitRun(t, dir, "remote", "get-url", "origin"))
	if got != "/existing/url" {
		t.Errorf("origin changed despite empty url: %q", got)
	}
}

func TestRemoveOrigin(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", "/some/url")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.RemoveOrigin(); err != nil {
		t.Fatalf("RemoveOrigin: %v", err)
	}
	if c.RemoteURL() != "" {
		t.Errorf("RemoteURL = %q, want empty", c.RemoteURL())
	}
}

func TestRemoveOriginIdempotent(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.RemoveOrigin(); err != nil {
		t.Errorf("RemoveOrigin on missing should be no-op, got %v", err)
	}
}

// --- Cross-check: EnsureOrigin sets the default fetch refspec ---

func TestEnsureOriginSetsDefaultFetchRefspec(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	clone := filepath.Join(tmp, "clone")
	gitInit(t, clone)

	c, _ := OpenClone(clone, "", nullLogger())
	if err := c.EnsureOrigin(bare); err != nil {
		t.Fatalf("EnsureOrigin: %v", err)
	}
	// Verify fetch refspec was set so a subsequent `git fetch`
	// populates refs/remotes/origin/*.
	out := strings.TrimSpace(gitRun(t, clone, "config", "--get-all", "remote.origin.fetch"))
	if !strings.Contains(out, "+refs/heads/*:refs/remotes/origin/*") {
		t.Errorf("missing default fetch refspec, got: %q", out)
	}
}
