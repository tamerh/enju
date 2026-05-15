package enjugit

// Real-git integration test pinning the fast-forward worktree
// sync in Workflow.MergeAcceptedTopic.
//
// Regression context: when a run lands on `main` (HEAD already
// on target), MergeAcceptedTopic took a ref-only fast-forward
// then only synced the INDEX. Files the FF added (e.g. a bot's
// results/published.md) sat in HEAD + index but were never
// written to disk — `git status` reported a phantom delete and
// the file couldn't be opened in the project folder. The
// fake-ops unit tests can't catch this: they assert which Ops
// methods are called, not whether real files land on disk. This
// test drives the real path end-to-end and asserts the worktree.
//
// It also pins the safety property the old (buggy) skip was
// reaching for: an untracked file in the worktree must survive
// the merge — the fix uses read-tree -m -u, which refuses to
// clobber untracked files rather than blowing them away.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

func TestMergeAcceptedTopic_FFMaterializesWorktreeIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 91)
	wf, err := ws.ForProject(91, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Untracked file in the worktree BEFORE the merge — stands in
	// for .enju/ runtime / bigfiles / in-flight scratch. Must
	// survive (the whole reason the FF path historically skipped
	// the checkout).
	scratch := filepath.Join(wf.WorkDir(), ".enju", "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(scratch, "keep.txt")
	if err := os.WriteFile(keep, []byte("untracked-survives"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bot-artifact analogue: a NEW tracked file committed on a
	// topic branch forked from main's seed. main does not advance,
	// so MergeAcceptedTopic takes the clean FF path; HEAD stays on
	// main, hitting the "HEAD already on target" skip-branch — the
	// exact shape that regressed.
	const pubPath = "results/published.md"
	const pubBody = "# Showcase Run Report\n\nPUBLISHED\n"
	if _, err := wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "topic-pub",
		Subject:     "publish result",
		AuthorName:  "Dev Bot",
		AuthorEmail: "dev@enju.local",
		Files: []FileWrite{
			{RepoRelPath: pubPath, Content: []byte(pubBody)},
		},
	}); err != nil {
		t.Fatalf("seed topic-pub: %v", err)
	}

	if _, err := wf.MergeAcceptedTopic("topic-pub", "main",
		MergeAuthor{
			TaskID:       "publish",
			AutoOrManual: "auto",
			Citizen:      Identity{Name: "Dev Bot", Email: "dev@enju.local"},
		}); err != nil {
		t.Fatalf("MergeAcceptedTopic (FF): %v", err)
	}

	// THE FIX: the fast-forwarded file is actually on disk.
	got, err := os.ReadFile(filepath.Join(wf.WorkDir(), pubPath))
	if err != nil {
		t.Fatalf("%s not materialized in worktree (the regression): %v", pubPath, err)
	}
	if string(got) != pubBody {
		t.Errorf("%s content = %q, want %q", pubPath, got, pubBody)
	}

	// No phantom delete: status must not report published.md as
	// deleted/modified (it's clean vs HEAD).
	status := gitStatusPorcelain(t, wf.WorkDir())
	for _, line := range strings.Split(status, "\n") {
		if strings.Contains(line, pubPath) {
			t.Errorf("phantom dirty state for %s: %q", pubPath, line)
		}
	}

	// Safety: the untracked file survived the merge.
	if b, err := os.ReadFile(keep); err != nil || string(b) != "untracked-survives" {
		t.Errorf("untracked scratch file lost across FF merge: %q err=%v", b, err)
	}
}

// gitStatusPorcelain returns `git status --porcelain` for dir.
func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	return gittest.Run(t, dir, "status", "--porcelain")
}
