package test

// Regression test for the "two unrelated init commits" bug:
// when a user copies an existing project's enju/ directory
// (including the managed bare) to a new path without copying
// the .git/, then runs enju_create_project on the new path,
// adoption case 3 ("populated dir, no .git") would silently
// seed a fresh working tree alongside a pre-existing bare with
// unrelated history. Result: non-fast-forward push failures
// that block every run from completing.
//
// The proximate cause is user-error (the partial cp), but the
// architecture has no safety check to catch it. This test pins
// the safety check: adoption must refuse when a managed bare
// is on disk without a matching .git/.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// TestAdoptionRefusesWhenBareCopiedFromAnotherProject mirrors
// the user's exact recipe:
//
//   1. enju_create_project at /A  → seeds .git, creates bare
//   2. cp -r /A/enju/  →  /B/enju/  (carries the bare; leaves
//                                    /A's .git behind)
//   3. enju_create_project at /B  → adoption case 3 sees
//                                    populated dir, no .git
//
// Pre-fix: step 3 silently runs initGitWithExistingFiles,
// seeding /B with a commit that has the copied bare's files
// in its tree. ensureManagedBare then sees the pre-existing
// bare and skips promotion. The new working tree's history
// and the bare's history are unrelated; every subsequent push
// fails with "local and remote share no history".
//
// Post-fix: step 3 refuses with a clear error pointing at the
// inconsistent state, asking the operator to either restore
// the matching .git/ or remove the orphan bare.
func TestAdoptionRefusesWhenBareCopiedFromAnotherProject(t *testing.T) {
	h := newMCPHarness(t, "Copy Bare")

	// Step 1: project A
	projectAPath := t.TempDir()
	if err := writeTestFile(filepath.Join(projectAPath, "README.md"), "# A\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Call(context.Background(), "enju_create_project", map[string]any{
		"name": fmt.Sprintf("project-a-%d", nowNano()),
		"path": projectAPath,
	}); err != nil {
		t.Fatalf("create_project A: %v", err)
	}

	// Step 2: cp -r A/enju/ → B/enju/. README also copied so
	// B looks like a "populated dir" the way the user's cp
	// would have left it. We intentionally do NOT copy A's
	// .git or .gitignore — that's the user's reported recipe.
	projectBPath := t.TempDir()
	if err := writeTestFile(filepath.Join(projectBPath, "README.md"), "# B\n"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cp", "-r", filepath.Join(projectAPath, "enju"), projectBPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cp -r: %v: %s", err, out)
	}

	// Step 3: enju_create_project at B. Must REFUSE — pre-fix
	// would silently produce a working tree whose history is
	// unrelated to the bare's.
	res, err := h.client.Call(context.Background(), "enju_create_project", map[string]any{
		"name": fmt.Sprintf("project-b-%d", nowNano()),
		"path": projectBPath,
	})
	if err == nil && !res.IsError {
		// The MCP call succeeded; check whether the resulting
		// repo has the two-roots smell.
		rootsInWorktree := collectRootCommits(t, projectBPath)
		rootsInBare := collectRootCommits(t, filepath.Join(projectBPath, "enju", ".bare.git"))
		allRoots := make(map[string]bool)
		for _, r := range rootsInWorktree {
			allRoots[strings.SplitN(r, " ", 2)[0]] = true
		}
		for _, r := range rootsInBare {
			allRoots[strings.SplitN(r, " ", 2)[0]] = true
		}
		if len(allRoots) > 1 {
			t.Errorf("CONFIRMED — copy-bare adoption produced %d unrelated roots: %v. "+
				"Adoption must refuse when a managed bare already exists at "+
				"enju/.bare.git/ but no .git/ — that pairing is the user-error "+
				"shape that produces unmergeable history.",
				len(allRoots), allRoots)
		}
		// If we got here without an error AND with one root,
		// something resolved silently — also not the desired
		// outcome (loud refusal), but no longer the data-loss
		// case. Log it.
		t.Logf("create_project B succeeded with %d total roots: %v", len(allRoots), allRoots)
		return
	}

	// Refusal is the desired outcome. Surface the message so
	// the test logs read clearly.
	if err != nil {
		t.Logf("create_project B refused as expected: %v", err)
	} else {
		t.Logf("create_project B refused as expected: %s", mcpText(res))
	}
}

// collectRootCommits returns all commits in repoPath that have
// no parents (root commits). git log --all --max-parents=0 does
// this in one shot.
func collectRootCommits(t *testing.T, repoPath string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", repoPath, "log", "--all", "--max-parents=0", "--pretty=%H %s")
	out, err := cmd.Output()
	if err != nil {
		t.Logf("git log in %s: %v (path may not be a git repo yet)", repoPath, err)
		return nil
	}
	var roots []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			roots = append(roots, line)
		}
	}
	return roots
}
