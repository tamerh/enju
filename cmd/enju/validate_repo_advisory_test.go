package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRA(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestRepoAdvisory(t *testing.T) {
	// 1. Not a git repo → no notes, never errors.
	d := t.TempDir()
	yml := filepath.Join(d, "enju.yaml")
	if err := os.WriteFile(yml, []byte("name: x\nversion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := repoAdvisory(yml); n != nil {
		t.Errorf("non-repo should yield no advisory, got: %v", n)
	}

	// 2. Repo on a normal branch, file committed clean → no notes.
	gitRA(t, d, "init", "-q", "-b", "main")
	gitRA(t, d, "add", "enju.yaml")
	gitRA(t, d, "commit", "-q", "-m", "init")
	if n := repoAdvisory(yml); n != nil {
		t.Errorf("clean repo on main should yield no advisory, got: %v", n)
	}

	// 3. Parked on an enju run-shaped branch → branch note.
	gitRA(t, d, "checkout", "-q", "-b", "1-some-run/task/iter-1")
	notes := repoAdvisory(yml)
	if len(notes) != 1 || !strings.Contains(notes[0], "run/iter branch") {
		t.Errorf("run-branch should warn, got: %v", notes)
	}

	// 4. Back on main, uncommitted edit to the workflow → uncommitted note.
	gitRA(t, d, "checkout", "-q", "main")
	if err := os.WriteFile(yml, []byte("name: x\nversion: 1\n# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notes = repoAdvisory(yml)
	if len(notes) != 1 || !strings.Contains(notes[0], "uncommitted changes") {
		t.Errorf("uncommitted workflow should warn, got: %v", notes)
	}

	// 5. Both conditions → two notes.
	gitRA(t, d, "checkout", "-q", "-b", "load-test-pipeline-2")
	notes = repoAdvisory(yml)
	if len(notes) != 2 {
		t.Errorf("run-branch + uncommitted should give 2 notes, got: %v", notes)
	}
}
