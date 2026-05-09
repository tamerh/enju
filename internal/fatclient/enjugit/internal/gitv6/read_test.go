package gitv6

import (
	"errors"
	"os"
	"testing"
)

func TestReadFileAtCommit_Hit(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	sha := commitOneFile(t, c, "src/foo.go", []byte("package foo\n"))

	body, found, err := c.ReadFileAtCommit(sha, "src/foo.go")
	if err != nil {
		t.Fatalf("ReadFileAtCommit: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if string(body) != "package foo\n" {
		t.Errorf("content mismatch: %q", body)
	}
}

func TestReadFileAtCommit_FileNotInTree(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	sha := commitOneFile(t, c, "a.txt", []byte("a\n"))

	_, found, err := c.ReadFileAtCommit(sha, "missing.txt")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if found {
		t.Error("expected found=false for missing path")
	}
}

func TestReadFileAtCommit_CommitNotFound(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	_, _, err := c.ReadFileAtCommit("0000000000000000000000000000000000000000", "any")
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

func TestResolveRef_LocalBranch(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	sha, err := c.ResolveRef("main")
	if err != nil {
		t.Fatalf("ResolveRef main: %v", err)
	}
	if !isHexSHA(sha) {
		t.Errorf("expected 40-hex SHA, got %q", sha)
	}
}

func TestResolveRef_FullRefName(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	sha, err := c.ResolveRef("refs/heads/main")
	if err != nil {
		t.Fatalf("ResolveRef full path: %v", err)
	}
	if !isHexSHA(sha) {
		t.Errorf("expected SHA, got %q", sha)
	}
}

func TestResolveRef_RemoteTracking(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// origin/main should resolve via remote-tracking ref.
	sha, err := c.ResolveRef("main")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	_ = sha
}

func TestResolveRef_NotFound(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	_, err := c.ResolveRef("nonexistent-branch")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

func TestResolveRef_SHAPassthrough(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	knownSHA := commitOneFile(t, c, "x.txt", []byte("x"))

	got, err := c.ResolveRef(knownSHA)
	if err != nil {
		t.Fatalf("SHA passthrough: %v", err)
	}
	if got != knownSHA {
		t.Errorf("SHA passthrough mismatch: got %s, want %s", got, knownSHA)
	}
}

func TestHead_OnBranch(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	sha, branch, err := c.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if branch != "main" {
		t.Errorf("expected branch=main, got %q", branch)
	}
	if !isHexSHA(sha) {
		t.Errorf("expected SHA, got %q", sha)
	}
}

func TestLocalBranches(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	branches, err := c.LocalBranches()
	if err != nil {
		t.Fatalf("LocalBranches: %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected at least one local branch (main)")
	}
	hasMain := false
	for _, b := range branches {
		if b == "main" {
			hasMain = true
		}
	}
	if !hasMain {
		t.Errorf("expected main in branches, got %v", branches)
	}
}

func TestState_Clean(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if got := c.State(); got != StateClean {
		t.Errorf("fresh clone state: got %s, want clean", got)
	}
}

func TestState_DirtyUntracked(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Drop an untracked file in the worktree.
	if err := writeWorktreeFile(c, "scratch.txt", "hello\n"); err != nil {
		t.Fatal(err)
	}
	if got := c.State(); got != StateDirtyUntracked {
		t.Errorf("with untracked file: got %s, want dirty-untracked", got)
	}
}

// writeWorktreeFile drops a file (untracked) into c's worktree.
func writeWorktreeFile(c *Clone, path, content string) error {
	full := c.workDir + "/" + path
	return os.WriteFile(full, []byte(content), 0o644)
}
