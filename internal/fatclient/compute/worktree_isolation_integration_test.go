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
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// initBareForComputeTest seeds a bare repo with one initial commit
// on main + the run-branch ref (also pointing at the seed). Mirrors
// enjugit's internal helper but lives here so compute_test stays
// external. Returns the bare path.
func initBareForComputeTest(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	if _, err := gogit.PlainInitWithOptions(bare, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
		Bare:        true,
	}); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	seed := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(seed, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin", URLs: []string{bare},
	}); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	_ = os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644)
	wt.Add("README.md")
	sig := &object.Signature{Name: "T", Email: "t@x", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}
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
	scratch := compute.ResolveTaskScratchDir(wsRoot, "alice", "1:1:isolation_check", 1)
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
