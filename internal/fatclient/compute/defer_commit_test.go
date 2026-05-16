package compute_test

// Produce-vs-commit split (spec §4). executor: slurm sets
// spec.DeferCommit so the compute node produces the result but
// does NOT touch git; the host-side reaper replays the captured
// commit. These tests prove both halves against a real workflow:
//
//   - DeferCommit=true  → Run returns CommitSHA=="" and a
//     populated Result.DeferredCommit, and makes NO commit.
//   - compute.CommitDeferred(wf, *dc) then produces a real SHA
//     — i.e. the reaper's host-side path commits successfully.
//   - DeferCommit=false → Run commits inline (control), so the
//     two paths differ only in WHERE the commit happens.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

func deferCommitWorkflow(t *testing.T, projID int64) (*enjugit.Workflow, string) {
	t.Helper()
	bare := initBareForComputeTest(t)
	wsRoot := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(t, wsRoot) })

	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	projectPath := filepath.Join(wsRoot, "p")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(projectreg.Entry{ID: projID, LocalPath: projectPath}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), enjugit.WithRegistry(reg))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(projID, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	return wf, wsRoot
}

func deferTrivialScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho deferred-output\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunDeferCommitProducesButDoesNotCommit(t *testing.T) {
	wf, _ := deferCommitWorkflow(t, 301)
	spec := compute.Spec{
		TaskID:          "301:1:dc",
		ProjectID:       301,
		Branch:          "main",
		IterationBranch: "1-test/dc/iter-1",
		ResultDir:       ".enju/runs/1-test/dc",
		ScriptPath:      deferTrivialScript(t),
		ScriptLabel:     "run.sh",
		AuthorName:      "alice",
		AuthorEmail:     "alice@example.com",
		Username:        "alice",
		DeferCommit:     true,
	}
	res := compute.Run(context.Background(), wf, spec,
		os.Environ(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if res.Error != "" || res.ExitCode != 0 {
		t.Fatalf("expected clean run, got error=%q exit=%d stderr=%q", res.Error, res.ExitCode, res.Stderr)
	}
	if res.CommitSHA != "" {
		t.Errorf("DeferCommit: CommitSHA must be empty (node must not commit), got %q", res.CommitSHA)
	}
	if res.GitError != "" {
		t.Errorf("DeferCommit: no git should have run, got GitError=%q", res.GitError)
	}
	if res.DeferredCommit == nil {
		t.Fatal("DeferCommit: Result.DeferredCommit must be populated for the reaper to replay")
	}
	dc := res.DeferredCommit
	if dc.TaskID != spec.TaskID || dc.IterationBranch != spec.IterationBranch || dc.RunBranch != spec.Branch {
		t.Errorf("DeferredCommit identity wrong: %+v", dc)
	}
	if len(dc.Files) == 0 {
		t.Error("DeferredCommit.Files empty — result.md/metadata.json should be captured")
	}

	// The host-side reaper path: replaying the captured commit
	// must produce a real SHA (byte-identical-commit contract).
	sub, err := compute.CommitDeferred(wf, *dc)
	if err != nil {
		t.Fatalf("CommitDeferred (host-side replay): %v", err)
	}
	if sub == nil || sub.CommitSHA == "" {
		t.Fatalf("CommitDeferred produced no commit SHA: %+v", sub)
	}
}

func TestRunInlineCommitsWhenNotDeferred(t *testing.T) {
	wf, _ := deferCommitWorkflow(t, 302)
	spec := compute.Spec{
		TaskID:          "302:1:inl",
		ProjectID:       302,
		Branch:          "main",
		IterationBranch: "1-test/inl/iter-1",
		ResultDir:       ".enju/runs/1-test/inl",
		ScriptPath:      deferTrivialScript(t),
		ScriptLabel:     "run.sh",
		AuthorName:      "alice",
		AuthorEmail:     "alice@example.com",
		Username:        "alice",
		// DeferCommit defaults false — today's local behavior.
	}
	res := compute.Run(context.Background(), wf, spec,
		os.Environ(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if res.Error != "" || res.ExitCode != 0 {
		t.Fatalf("expected clean run, got error=%q exit=%d stderr=%q", res.Error, res.ExitCode, res.Stderr)
	}
	if res.CommitSHA == "" {
		t.Error("non-deferred Run must commit inline (non-empty CommitSHA)")
	}
	if res.DeferredCommit != nil {
		t.Error("non-deferred Run must NOT populate DeferredCommit")
	}
}
