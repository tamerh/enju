package compute_test

// Phase 2.6 — worktree isolation contract.
//
// Pins the invariant that compute.Run leaves NOTHING on disk under
// workDir/<resultDir> when scratch is set. The plumbing-commit path
// builds the commit's tree from in-memory FileWrites + scratch, never
// touching the worktree at the result-dir paths. Without this, the
// wrapper writes script.log (and the handler writes context.json)
// to the worktree as untracked files, then a later non-FF
// MergeAcceptedTopic's post-merge Checkout(target) refuses to
// overwrite them — surfaced in production as parallel-merge fan-out
// stalling at the second sibling. The smoking gun was
// `enju/runs/.../<task>/{context.json,script.log}` left untracked
// after the wrapper finished.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// initBareForComputeTest seeds a bare repo with one initial commit
// on main. Returns the bare path.
func initBareForComputeTest(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	gittest.InitBareWithSeed(t, bare)
	return bare
}

// TestComputeRun_NoWorktreePollutionUnderResultDir runs compute.Run
// end-to-end through a real Workflow + bare and asserts the
// post-condition: nothing on disk under workDir/<resultDir> after
// success. Exposes the load-test smoke (parallel non-FF merges
// blocked on untracked context.json/script.log) by failing while
// any file is left there.
func TestComputeRun_NoWorktreePollutionUnderResultDir(t *testing.T) {
	bare := initBareForComputeTest(t)
	wsRoot := t.TempDir()
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(101, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	workDir := wf.WorkDir()

	// Scratch dir per Phase 2.1 — production callers always
	// resolve via compute.ResolveTaskScratchDir.
	scratch := compute.ResolveTaskScratchDir(wf.ProjectRoot(), "alice", "1:1:isolation_check", 1)
	if scratch == "" {
		t.Fatal("ResolveTaskScratchDir unexpectedly returned empty")
	}

	resultDir := "enju/runs/1-test/isolation_check"

	// Build a small shell script that runs in scratch and produces
	// a tracked artifact + some stdout. The script's CWD is scratch
	// (Phase 2.3); writes_artifacts paths are picked up from there.
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "isolation_check.sh")
	scriptBody := "#!/bin/sh\nset -e\nmkdir -p out\necho payload > out/value.txt\necho 'hello from script'\n"
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	spec := compute.Spec{
		TaskID:          "1:1:isolation_check",
		ProjectID:       101,
		RemoteURL:       bare,
		WorkspaceRoot:   wsRoot,
		Branch:          "main",
		IterationBranch: "1-test/isolation_check/iter-1",
		ResultDir:       resultDir,
		ScriptPath:      scriptPath,
		ScriptLabel:     "isolation_check.sh",
		WritesArtifacts: enjuYaml.WriteArtifacts{
			{Path: "out/value.txt", Track: true},
		},
		AuthorName:     "alice",
		AuthorEmail:    "alice@example.com",
		Username:       "alice",
		TaskScratchDir: scratch,
	}

	res := compute.Run(context.Background(), wf, spec,
		os.Environ(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if res.Error != "" {
		t.Fatalf("compute.Run returned wrapper error: %q", res.Error)
	}
	if res.GitError != "" {
		t.Fatalf("compute.Run returned git error: %q", res.GitError)
	}
	if res.ExitCode != 0 {
		t.Fatalf("script exit %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.CommitSHA == "" {
		t.Fatal("expected a commit SHA on the success path")
	}

	// THE LOAD-TEST INVARIANT:
	// After a successful compute.Run, nothing should live on disk
	// at workDir/<resultDir>. The commit has the result/metadata/
	// context/log files in its tree; the worktree must NOT have
	// them as untracked, because a later non-FF MergeAcceptedTopic
	// does Checkout(target) which refuses to overwrite untracked.
	resultOnDisk := filepath.Join(workDir, resultDir)
	if entries, err := os.ReadDir(resultOnDisk); err == nil {
		if len(entries) > 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("workDir/%s should be empty after compute.Run, has: %v",
				resultDir, names)
		}
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v", resultOnDisk, err)
	}
}

// TestComputeRun_FailsWhenRequiredArtifactMissing pins the
// fail-loud-on-missing-required contract. A task whose script
// exits 0 but doesn't produce its declared (non-optional)
// writes_artifacts entries must NOT commit — we surface a clear
// error naming the missing path(s) instead.
//
// Surfaced from the compute-load-test where a non-parametric
// fetch.sh wrote `data/raw_a.txt` for all three siblings, so
// fetch_data_b's declared `data/raw_b.txt` was silently absent.
// Pre-fix the task committed an empty artifact set, coord
// recorded ACCEPTED, downstream stalled forever waiting for an
// artifact that never existed. Post-fix the iteration fails at
// fetch_data_b, the operator sees "didn't produce data/raw_b.txt"
// and fixes the template — instead of debugging a phantom
// cascade-stop one task removed.
func TestComputeRun_FailsWhenRequiredArtifactMissing(t *testing.T) {
	bare := initBareForComputeTest(t)
	wsRoot := t.TempDir()
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(102, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	scratch := compute.ResolveTaskScratchDir(wf.ProjectRoot(), "alice", "1:1:wrong_file", 1)
	resultDir := "enju/runs/1-test/wrong_file"

	// Script exits 0 but writes a path the declaration doesn't
	// cover. Mirrors the load-test's fetch.sh which always wrote
	// `data/raw_a.txt` regardless of which sibling ran it.
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "wrong_file.sh")
	scriptBody := "#!/bin/sh\nset -e\nmkdir -p data\necho payload > data/wrong_file.txt\necho 'declared the wrong output'\n"
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	spec := compute.Spec{
		TaskID:          "1:1:wrong_file",
		ProjectID:       102,
		RemoteURL:       bare,
		WorkspaceRoot:   wsRoot,
		Branch:          "main",
		IterationBranch: "1-test/wrong_file/iter-1",
		ResultDir:       resultDir,
		ScriptPath:      scriptPath,
		ScriptLabel:     "wrong_file.sh",
		WritesArtifacts: enjuYaml.WriteArtifacts{
			// Required (Optional defaults to false): script
			// must produce this exact path.
			{Path: "data/raw_b.txt", Track: true},
		},
		AuthorName:     "alice",
		AuthorEmail:    "alice@example.com",
		Username:       "alice",
		TaskScratchDir: scratch,
	}

	res := compute.Run(context.Background(), wf, spec,
		os.Environ(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if res.Error == "" {
		t.Fatalf("expected wrapper Error naming the missing path, got success: CommitSHA=%q", res.CommitSHA)
	}
	if res.CommitSHA != "" {
		t.Errorf("expected NO commit when required artifact missing, got %q", res.CommitSHA)
	}
	if !strings.Contains(res.Error, "data/raw_b.txt") {
		t.Errorf("error should name the missing artifact, got: %q", res.Error)
	}
	// MissingArtifacts is populated for the operator-facing report
	// even though Error is the gate; downstream coord paths still
	// surface the list as before.
	foundInMissing := false
	for _, p := range res.MissingArtifacts {
		if p == "data/raw_b.txt" {
			foundInMissing = true
			break
		}
	}
	if !foundInMissing {
		t.Errorf("MissingArtifacts should include data/raw_b.txt, got: %v", res.MissingArtifacts)
	}
}

// TestComputeRun_OptionalMissingStaysSoft confirms the inverse:
// a `optional: true` entry that produced nothing is fine. The
// task commits, ArtifactsWritten reflects only what was produced,
// MissingArtifacts is empty (the expander filters optional entries
// out of the missing list at expand time, so the wrapper's gate
// never sees them).
func TestComputeRun_OptionalMissingStaysSoft(t *testing.T) {
	bare := initBareForComputeTest(t)
	wsRoot := t.TempDir()
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(103, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	scratch := compute.ResolveTaskScratchDir(wf.ProjectRoot(), "alice", "1:1:opt_missing", 1)
	resultDir := "enju/runs/1-test/opt_missing"

	// Script writes the required output but skips the optional one.
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "opt_missing.sh")
	scriptBody := "#!/bin/sh\nset -e\nmkdir -p data\necho ok > data/required.txt\n"
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	spec := compute.Spec{
		TaskID:          "1:1:opt_missing",
		ProjectID:       103,
		RemoteURL:       bare,
		WorkspaceRoot:   wsRoot,
		Branch:          "main",
		IterationBranch: "1-test/opt_missing/iter-1",
		ResultDir:       resultDir,
		ScriptPath:      scriptPath,
		ScriptLabel:     "opt_missing.sh",
		WritesArtifacts: enjuYaml.WriteArtifacts{
			{Path: "data/required.txt", Track: true},
			{Path: "data/optional.txt", Track: true, Optional: true},
		},
		AuthorName:     "alice",
		AuthorEmail:    "alice@example.com",
		Username:       "alice",
		TaskScratchDir: scratch,
	}

	res := compute.Run(context.Background(), wf, spec,
		os.Environ(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if res.Error != "" {
		t.Fatalf("expected success when only optional missing, got Error=%q", res.Error)
	}
	if res.CommitSHA == "" {
		t.Fatal("expected a commit for partial-but-required-satisfied submit")
	}
}
