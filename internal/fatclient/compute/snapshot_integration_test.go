package compute_test

// End-to-end integration tests for the snapshot-as-CWD shape.
//
// These complement the pure-unit tests in scratch_tree_test.go by
// running a real compute.Run against a real workflow + bare with
// a realistic snapshot tree on disk. The point: prove that scripts
// can reach sibling files via relative paths when SnapshotDir is
// set, and that the chmod-readonly defense actually blocks
// pollution attempts when invoked from production-shaped code
// paths.
//
// We reuse initBareForComputeTest from the worktree-isolation
// integration test (same package, defined there).

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// seedSnapshotTree drops a representative template snapshot onto
// disk inside the workspace at the given path. The tree has:
//   - entry.sh that calls scripts/helper.sh
//   - scripts/helper.sh that writes a line + reads $ENJU_SCRATCH
//   - lib/utils.sh sourced by helper (proves multi-dir reachability)
//
// Returns the snapshot's absolute path. Caller is responsible for
// chmod-readonly via compute.ChmodSnapshotReadOnly (this helper
// stays write-mode so test setup can keep editing).
func seedSnapshotTree(t *testing.T, base string) string {
	t.Helper()
	snap := filepath.Join(base, "snapshot")
	if err := os.MkdirAll(filepath.Join(snap, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(snap, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}

	// entry.sh sources lib/utils.sh and runs scripts/helper.sh —
	// both via relative paths against the snapshot CWD. Output
	// goes to stdout, which the wrapper captures as Result.Content
	// (the canonical result channel for compute tasks). Writing
	// to $ENJU_SCRATCH would also work as an output channel but
	// the wrapper wipes scratch on teardown, so we use stdout
	// for the test's verification path.
	entry := `#!/bin/sh
set -e
. ./lib/utils.sh
./scripts/helper.sh
greet
echo "siblings-ok"
`
	if err := os.WriteFile(filepath.Join(snap, "entry.sh"), []byte(entry), 0o755); err != nil {
		t.Fatal(err)
	}

	helper := `#!/bin/sh
echo "helper ran at $(date +%s%N)"
`
	if err := os.WriteFile(filepath.Join(snap, "scripts", "helper.sh"), []byte(helper), 0o755); err != nil {
		t.Fatal(err)
	}

	lib := `# Library sourced by entry.sh — proves cross-dir reachability.
greet() {
	echo "hello from lib/utils.sh"
}
`
	if err := os.WriteFile(filepath.Join(snap, "lib", "utils.sh"), []byte(lib), 0o644); err != nil {
		t.Fatal(err)
	}
	return snap
}

// chmodTreeWritable walks a tree and re-chmods everything to 0755
// so test cleanup (t.TempDir's RemoveAll) doesn't fail on
// readonly dirs. Mirrors the helper in scratch_tree_test.go but
// scoped to the integration tests' workflow trees.
func chmodTreeWritable(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		_ = os.Chmod(p, 0o755)
		return nil
	})
}

// TestSnapshotAsCWD_SiblingsReachable is the load-bearing
// end-to-end assertion for the spec: with SnapshotDir set, a
// script's `./scripts/...` and `./lib/...` relative references
// resolve naturally against the full template tree. The script
// itself runs in the snapshot dir, sourcing lib/utils.sh and
// invoking scripts/helper.sh both as relative paths, then writes
// its output to $ENJU_SCRATCH.
//
// Pre-fix shape: scratch had only entry.sh + context.json, so
// any `./scripts/<x>.sh` reference would have failed with "No
// such file or directory."
func TestSnapshotAsCWD_SiblingsReachable(t *testing.T) {
	bare := initBareForComputeTest(t)
	wsRoot := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(t, wsRoot) })

	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(201, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Snapshot lives outside the workspace clone for this test —
	// the path doesn't need to be inside workDir; compute.Run
	// honors SnapshotDir verbatim via ScriptCwdFor. Production
	// callers would point at the in-workspace snapshot path; here
	// we use a sibling temp dir for simplicity.
	snapBase := t.TempDir()
	snap := seedSnapshotTree(t, snapBase)

	// Read-only is convention now (per-run-snapshot redesign);
	// no chmod step here. The snapshot stays writable on disk —
	// scripts that target it are buggy and should write to
	// $ENJU_SCRATCH instead. Container path retains the kernel-
	// side :ro bind for the strong guarantee inside the sandbox.

	scratch := compute.ResolveTaskScratchDir(wsRoot, "alice", "1:1:siblings_check", 1)

	scriptPath := filepath.Join(snap, "entry.sh")
	spec := compute.Spec{
		TaskID:          "1:1:siblings_check",
		ProjectID:       201,
		RemoteURL:       bare,
		WorkspaceRoot:   wsRoot,
		Branch:          "main",
		IterationBranch: "1-test/siblings_check/iter-1",
		ResultDir:       "enju/runs/1-test/siblings_check",
		ScriptPath:      scriptPath,
		ScriptLabel:     "entry.sh",
		AuthorName:      "alice",
		AuthorEmail:     "alice@example.com",
		Username:        "alice",
		TaskScratchDir:  scratch,
		SnapshotDir:     snap,
	}

	// ENJU_SCRATCH is set by buildComputeEnv in service/execute.go
	// in production. Since we're calling compute.Run directly,
	// add it explicitly so the script's `${ENJU_SCRATCH}` resolves.
	env := append(os.Environ(), "ENJU_SCRATCH="+scratch)

	res := compute.Run(context.Background(), wf, spec,
		env, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if res.Error != "" {
		t.Fatalf("compute.Run wrapper error: %q (stderr=%q)", res.Error, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("script exit %d, stderr=%q", res.ExitCode, res.Stderr)
	}

	// Script stdout becomes res.Content. It must contain output
	// from both reached siblings: helper.sh's "helper ran" line
	// AND the greet() function from the sourced lib/utils.sh.
	// Both are load-bearing — they prove `./scripts/...` and
	// `. ./lib/...` resolve naturally with snapshot-as-CWD.
	if !strings.Contains(res.Content, "helper ran") {
		t.Errorf("res.Content missing helper.sh output (./scripts/ sibling unreachable?): %q",
			res.Content)
	}
	if !strings.Contains(res.Content, "hello from lib/utils.sh") {
		t.Errorf("res.Content missing lib/utils.sh greet output (./lib/ sibling unreachable?): %q",
			res.Content)
	}
	if !strings.Contains(res.Content, "siblings-ok") {
		t.Errorf("res.Content missing trailing sentinel — script exited early? %q", res.Content)
	}
}

// Pollution-attempt-fails test was removed alongside the
// ChmodSnapshotReadOnly helper in the per-run-snapshot redesign.
// Read-only is convention now (scripts that write to the snapshot
// are buggy and should target $ENJU_SCRATCH); the kernel-side :ro
// bind inside the container still gives the strong sandbox guarantee.

// TestWrapper_PreservesScratchOnSubmitFail pins TP53 Bug 2's
// fix: when the script exits 0 and produces outputs in scratch
// but the post-script SubmitComputeTaskResult fails (commit
// retry exhausted, push rejected, branch missing — anything at
// the git layer), the wrapper must LEAVE scratch on disk so the
// operator's retry can read the script's outputs from there.
//
// Pre-fix, the unconditional defer-rm in compute.Run wiped
// scratch on every exit path, including the submit-failed one.
// The tester's TP53 run lost 12 sections' worth of output to
// this — they survived only inside the failed submit's
// .wrap-result.done.json envelope on disk and were unrecoverable
// once the daemon cycled.
//
// We force submit failure by pointing the spec at a non-existent
// RunBranch: SubmitComputeTaskResult's resolve-base step fails
// loudly, the wrapper sets res.GitError, and the defer must
// see GitError set and skip cleanup.
func TestWrapper_PreservesScratchOnSubmitFail(t *testing.T) {
	bare := initBareForComputeTest(t)
	wsRoot := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(t, wsRoot) })

	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(303, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	snapBase := t.TempDir()
	snap := filepath.Join(snapBase, "snapshot")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}

	// Script writes a real output to scratch and exits 0. With
	// the pre-fix unconditional cleanup, this output would be
	// lost even though the submit will fail below.
	script := `#!/bin/sh
set -e
echo "result content" > "$ENJU_SCRATCH/result.md"
echo "stdout content"
`
	if err := os.WriteFile(filepath.Join(snap, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	scratch := compute.ResolveTaskScratchDir(wsRoot, "alice", "1:1:submit_fail_check", 1)

	spec := compute.Spec{
		TaskID:        "1:1:submit_fail_check",
		ProjectID:     303,
		RemoteURL:     bare,
		WorkspaceRoot: wsRoot,
		// Branch (→ SubmitRequest.RunBranch) points at a ref
		// that doesn't exist anywhere. SubmitComputeTaskResult
		// fails at the resolve-base step with a clear error,
		// which the wrapper wraps into res.GitError. That's
		// the exact submit-failed shape the spec preserves
		// scratch for.
		Branch:          "this-branch-does-not-exist-anywhere",
		IterationBranch: "this-branch-does-not-exist-anywhere/iter-1",
		ResultDir:       "enju/runs/1-fail/submit_fail_check",
		ScriptPath:      filepath.Join(snap, "run.sh"),
		ScriptLabel:     "run.sh",
		AuthorName:      "alice",
		AuthorEmail:     "alice@example.com",
		Username:        "alice",
		TaskScratchDir:  scratch,
		SnapshotDir:     snap,
	}

	env := append(os.Environ(), "ENJU_SCRATCH="+scratch)
	res := compute.Run(context.Background(), wf, spec,
		env, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Script ran — exit 0, no wrapper-level error.
	if res.Error != "" {
		t.Fatalf("unexpected wrapper error (script-pre-submit failure?): %q", res.Error)
	}
	if res.ExitCode != 0 {
		t.Fatalf("script should have exited 0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
	// Submit failed → GitError set.
	if res.GitError == "" {
		t.Fatalf("expected GitError on submit failure, got empty (CommitSHA=%q)", res.CommitSHA)
	}
	// Scratch survives — this is the load-bearing assertion.
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch dir should have been preserved on submit failure, got: %v", err)
	}
	// And the script's outputs in scratch are readable.
	got, err := os.ReadFile(filepath.Join(scratch, "result.md"))
	if err != nil {
		t.Fatalf("script output result.md should survive: %v", err)
	}
	if string(got) != "result content\n" {
		t.Errorf("output content mismatch: %q", got)
	}
}

// TestWrapper_CleansScratchOnSubmitSuccess complements the above:
// when the submit succeeds, scratch goes away as before. Without
// this, a regression that made the conditional cleanup never fire
// would silently turn every task into a leaked scratch dir.
func TestWrapper_CleansScratchOnSubmitSuccess(t *testing.T) {
	bare := initBareForComputeTest(t)
	wsRoot := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(t, wsRoot) })

	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(304, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	snapBase := t.TempDir()
	snap := filepath.Join(snapBase, "snapshot")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "run.sh"),
		[]byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	scratch := compute.ResolveTaskScratchDir(wsRoot, "alice", "1:1:submit_ok", 1)
	spec := compute.Spec{
		TaskID:          "1:1:submit_ok",
		ProjectID:       304,
		RemoteURL:       bare,
		WorkspaceRoot:   wsRoot,
		Branch:          "main",
		IterationBranch: "1-test/submit_ok/iter-1",
		ResultDir:       "enju/runs/1-test/submit_ok",
		ScriptPath:      filepath.Join(snap, "run.sh"),
		ScriptLabel:     "run.sh",
		AuthorName:      "alice",
		AuthorEmail:     "alice@example.com",
		Username:        "alice",
		TaskScratchDir:  scratch,
		SnapshotDir:     snap,
	}
	env := append(os.Environ(), "ENJU_SCRATCH="+scratch)
	res := compute.Run(context.Background(), wf, spec,
		env, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if res.Error != "" {
		t.Fatalf("wrapper error: %q", res.Error)
	}
	if res.GitError != "" {
		t.Fatalf("unexpected GitError: %q", res.GitError)
	}
	if res.ExitCode != 0 {
		t.Fatalf("script exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// Scratch must be gone — the success path still cleans up.
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch should be cleaned on submit success, stat=%v", err)
	}
}
