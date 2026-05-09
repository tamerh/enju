package gitv6

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v6"
)

// TestReadTreeEntriesAtCommit_Root verifies the direct entries
// of a commit's root tree come back classified file vs dir with
// reasonable mode bits.
func TestReadTreeEntriesAtCommit_Root(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Add a tree shape with a file + a subdirectory containing
	// another file, then commit + capture SHA.
	if err := os.MkdirAll(filepath.Join(c.workDir, "templates", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitOneFile(t, c, "templates/demo/enju.yaml", []byte("name: demo\n"))
	sha := commitOneFile(t, c, "top.txt", []byte("top\n"))

	entries, ok, err := c.ReadTreeEntriesAtCommit(sha, "")
	if err != nil || !ok {
		t.Fatalf("ReadTreeEntriesAtCommit: ok=%v err=%v", ok, err)
	}
	names := map[string]TreeEntry{}
	for _, e := range entries {
		names[e.Name] = e
	}
	if _, ok := names["top.txt"]; !ok {
		t.Errorf("expected top.txt in root entries, got %+v", entries)
	}
	if _, ok := names["templates"]; !ok {
		t.Errorf("expected templates subdir in root entries, got %+v", entries)
	}
	if names["templates"].IsDir != true {
		t.Errorf("templates should be marked as dir, got %+v", names["templates"])
	}
	if names["top.txt"].IsDir != false {
		t.Errorf("top.txt should be classified as file, got %+v", names["top.txt"])
	}
}

func TestReadTreeEntriesAtCommit_MissingDir(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	sha := commitOneFile(t, c, "x.txt", []byte("x"))

	_, ok, err := c.ReadTreeEntriesAtCommit(sha, "nonexistent/path")
	if err != nil {
		t.Fatalf("missing dir should not error, got %v", err)
	}
	if ok {
		t.Error("missing dir should return ok=false")
	}
}

func TestReadTreeEntriesAtCommit_BadSHA(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	_, _, err := c.ReadTreeEntriesAtCommit("0000000000000000000000000000000000000000", "")
	if err == nil {
		t.Error("expected error for nonexistent SHA")
	}
}

func TestWalkSubtreeBlobsAtCommit_Recursive(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if err := os.MkdirAll(filepath.Join(c.workDir, "bundle", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitOneFile(t, c, "bundle/enju.yaml", []byte("manifest"))
	commitOneFile(t, c, "bundle/scripts/run.sh", []byte("#!/bin/bash\n"))
	sha := commitOneFile(t, c, "bundle/README.md", []byte("docs"))

	collected := map[string][]byte{}
	err := c.WalkSubtreeBlobsAtCommit(sha, "bundle", func(rel string, mode os.FileMode, content []byte) error {
		collected[rel] = content
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, want := range []string{"enju.yaml", "scripts/run.sh", "README.md"} {
		if _, ok := collected[want]; !ok {
			t.Errorf("expected %q in walked blobs, got keys: %v", want, keysOf(collected))
		}
	}
}

func TestWalkSubtreeBlobsAtCommit_SkipsHiddenSegments(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if err := os.MkdirAll(filepath.Join(c.workDir, "bundle", ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitOneFile(t, c, "bundle/visible.md", []byte("yes"))
	commitOneFile(t, c, "bundle/.hidden/secret.md", []byte("no"))
	commitOneFile(t, c, "bundle/.gitkeep", []byte(""))
	sha := commitOneFile(t, c, "bundle/x.md", []byte("yes"))

	got := map[string]bool{}
	err := c.WalkSubtreeBlobsAtCommit(sha, "bundle", func(rel string, mode os.FileMode, content []byte) error {
		got[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[".hidden/secret.md"] {
		t.Error("hidden subdir blob should be skipped")
	}
	if got[".gitkeep"] {
		t.Error("hidden filename should be skipped")
	}
	if !got["visible.md"] || !got["x.md"] {
		t.Errorf("expected visible files in walk, got %v", got)
	}
}

func TestWalkSubtreeBlobsAtCommit_MissingDirIsNoop(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	sha := commitOneFile(t, c, "x.txt", []byte("x"))

	called := 0
	err := c.WalkSubtreeBlobsAtCommit(sha, "nope", func(string, os.FileMode, []byte) error {
		called++
		return nil
	})
	if err != nil {
		t.Errorf("missing dir should not error, got %v", err)
	}
	if called != 0 {
		t.Errorf("visitor called %d times for missing dir", called)
	}
}

func TestWalkSubtreeBlobsAtCommit_PreservesExecBit(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if err := os.MkdirAll(filepath.Join(c.workDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitOneFile(t, c, "bin/regular.sh", []byte("#!/bin/sh\n"))
	// Make the file executable then re-stage and commit.
	exePath := filepath.Join(c.workDir, "bin/exec.sh")
	if err := os.WriteFile(exePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt, _ := c.repo.Worktree()
	wt.Add("bin/exec.sh")
	sig := testSig()
	h, err := wt.Commit("add exec", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	sha := h.String()

	got := map[string]os.FileMode{}
	if err := c.WalkSubtreeBlobsAtCommit(sha, "bin", func(rel string, mode os.FileMode, content []byte) error {
		got[rel] = mode
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got["exec.sh"]&0o111 == 0 {
		t.Errorf("exec.sh should preserve +x bit, got mode %v", got["exec.sh"])
	}
	if got["regular.sh"]&0o111 != 0 {
		t.Errorf("regular.sh should NOT have +x bit, got mode %v", got["regular.sh"])
	}
}

// keysOf is a tiny helper for stable error messages.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
