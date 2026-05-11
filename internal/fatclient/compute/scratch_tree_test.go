package compute

// Tests for the snapshot-as-CWD shape: scripts run with the run's
// frozen template-snapshot as their working directory (read sibling
// files like ./scripts/helper.sh and import lib.utils naturally),
// scratch is the writable per-iter sandbox exposed via ENJU_SCRATCH.
//
// Three layers under test:
//   1. ScriptCwdFor priority — snapshot > scratch > workDir.
//   2. Container argv shape with SnapshotDir set — second bind mount
//      (snapshot:/template:ro,z), CWD switches to /template, env
//      values pointing at the host snapshot path get rewritten to
//      /template.
//   3. ChmodSnapshotReadOnly — files → 0444, dirs → 0555, symlinks
//      untouched (containment).

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ─── ScriptCwdFor ──────────────────────────────────────────────

// TestScriptCwdFor_SnapshotWinsWhenSet pins the priority order:
// when SnapshotDir is set, it's the script's CWD regardless of
// whether scratch is also configured. The snapshot is the read
// channel; scratch is the write channel.
func TestScriptCwdFor_SnapshotWinsWhenSet(t *testing.T) {
	spec := Spec{
		SnapshotDir:    "/host/snap",
		TaskScratchDir: "/host/scratch",
	}
	if got := ScriptCwdFor(spec, "/host/work"); got != "/host/snap" {
		t.Errorf("ScriptCwdFor = %q, want %q (snapshot wins over scratch)", got, "/host/snap")
	}
}

// TestScriptCwdFor_ScratchWhenNoSnapshot pins the pre-snapshot
// behavior is preserved for legacy specs that don't carry a
// SnapshotDir. Critical regression guard for inline-YAML runs.
func TestScriptCwdFor_ScratchWhenNoSnapshot(t *testing.T) {
	spec := Spec{TaskScratchDir: "/host/scratch"}
	if got := ScriptCwdFor(spec, "/host/work"); got != "/host/scratch" {
		t.Errorf("ScriptCwdFor with no snapshot = %q, want scratch %q", got, "/host/scratch")
	}
}

// TestScriptCwdFor_WorkDirFallback pins the legacy fallback when
// neither snapshot nor scratch is configured (very old specs).
func TestScriptCwdFor_WorkDirFallback(t *testing.T) {
	spec := Spec{}
	if got := ScriptCwdFor(spec, "/host/work"); got != "/host/work" {
		t.Errorf("ScriptCwdFor with neither = %q, want workDir %q", got, "/host/work")
	}
}

// ─── Container argv with snapshot ──────────────────────────────

// TestBuildContainerArgs_SnapshotBindAndCWD pins the load-bearing
// container-arg behavior under the new shape: when SnapshotDir is
// set, a second bind mount lands the snapshot at /template as
// read-only, the CWD switches to /template, and env values that
// reference the host snapshot path get rewritten to the in-
// container view.
func TestBuildContainerArgs_SnapshotBindAndCWD(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{
		Container:      "alpine:latest",
		ScriptPath:     "/host/work/enju/runs/3/template-snapshot/scripts/run.sh",
		SnapshotDir:    "/host/work/enju/runs/3/template-snapshot",
		TaskScratchDir: "/host/work/scratch/bot1/task-iter-1",
	}
	env := []string{
		"ENJU_TASK_ID=1:3:run",
		"ENJU_TEMPLATE_DIR=/host/work/enju/runs/3/template-snapshot",
		"ENJU_SCRATCH=/host/work/scratch/bot1/task-iter-1",
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/work", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot bind-mount is read-only with SELinux :z label.
	wantSnapBind := "/host/work/enju/runs/3/template-snapshot:/template:ro,z"
	if !hasFlagValue(args, "-v", wantSnapBind) {
		t.Errorf("missing snapshot bind %q in args: %v", wantSnapBind, args)
	}

	// Scratch bind stays read-write — script needs to write logs.
	wantScratchBind := "/host/work/scratch/bot1/task-iter-1:/scratch:z"
	if !hasFlagValue(args, "-v", wantScratchBind) {
		t.Errorf("missing scratch bind %q in args: %v", wantScratchBind, args)
	}

	// Container CWD is /template when snapshot is bound — that's
	// the snapshot-as-CWD shape's load-bearing claim.
	if !hasFlagValue(args, "-w", "/template") {
		t.Errorf("expected -w /template, got: %v", args)
	}

	// Env values pointing at the host snapshot path translate to
	// /template so scripts see consistent paths in env vars.
	if !hasFlagValue(args, "-e", "ENJU_TEMPLATE_DIR=/template") {
		t.Errorf("ENJU_TEMPLATE_DIR should translate to /template: %v", args)
	}
	// Scratch env still translates to /scratch.
	if !hasFlagValue(args, "-e", "ENJU_SCRATCH=/scratch") {
		t.Errorf("ENJU_SCRATCH should translate to /scratch: %v", args)
	}
}

// TestBuildContainerArgs_NoSnapshotKeepsScratchCWD pins the
// cross-runtime no-crosswire property: a Spec with TaskScratchDir
// but no SnapshotDir behaves exactly as it did before the snapshot
// changes — CWD is /scratch, only one bind mount beyond workspace.
// Critical regression guard for inline-YAML runs and any pre-
// existing template that hasn't migrated.
func TestBuildContainerArgs_NoSnapshotKeepsScratchCWD(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{
		Container:      "alpine:latest",
		ScriptPath:     "/host/work/scripts/run.sh",
		TaskScratchDir: "/host/work/scratch/bot1/task-iter-1",
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/work", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	if !hasFlagValue(args, "-w", "/scratch") {
		t.Errorf("expected -w /scratch when no snapshot, got: %v", args)
	}
	// No /template bind should appear.
	for _, a := range args {
		if a != "" && len(a) > 9 && a[len(a)-9:] == "/template" {
			t.Errorf("unexpected /template reference when SnapshotDir empty: %v", args)
		}
	}
}

// TestBuildContainerArgs_NoSnapshotNoScratchKeepsWorkspaceCWD pins
// the deepest fallback: legacy specs (no snapshot, no scratch) keep
// /workspace as CWD. No-regression for very old templates.
func TestBuildContainerArgs_NoSnapshotNoScratchKeepsWorkspaceCWD(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{
		Container:  "alpine:latest",
		ScriptPath: "/host/work/scripts/run.sh",
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/work", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "-w", "/workspace") {
		t.Errorf("expected -w /workspace as deepest fallback, got: %v", args)
	}
}

// ─── ChmodSnapshotReadOnly ────────────────────────────────────

// chmodReadonlyTempDir creates a temp dir AND registers a cleanup
// that re-chmods it writable BEFORE the framework's TempDir cleanup
// runs RemoveAll. Without this, RemoveAll fails on the 0555 dirs
// we'll create. t.Cleanup runs LIFO, so registering this after
// t.TempDir's implicit cleanup gives us first-shot at undoing the
// chmod before deletion.
func chmodReadonlyTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // best-effort cleanup
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			_ = os.Chmod(p, 0o755)
			return nil
		})
	})
	return root
}


// TestChmodSnapshotReadOnly_StripsWriteBitsPreservesExec pins the
// load-bearing chmod behavior: write bits (0222) get stripped on
// every entry, but read and execute bits are preserved. Without
// the exec preservation, entry.sh and other scripts fail fork/exec
// with EACCES before they ever run. With it, the snapshot is
// read-only AND executable scripts still work.
func TestChmodSnapshotReadOnly_StripsWriteBitsPreservesExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes; Windows uses the read-only attribute (see spec portability matrix)")
	}
	root := chmodReadonlyTempDir(t)

	// Build a small representative tree: dirs (0755), a non-
	// executable data file (0644 → 0444), and an executable
	// script (0755 → 0555).
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataFile := filepath.Join(root, "lib", "utils.py")
	if err := os.WriteFile(dataFile, []byte("def greet(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptFile := filepath.Join(root, "scripts", "helper.sh")
	if err := os.WriteFile(scriptFile, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ChmodSnapshotReadOnly(root); err != nil {
		t.Fatalf("ChmodSnapshotReadOnly: %v", err)
	}

	// Non-executable data: 0644 → 0444.
	dataInfo, err := os.Stat(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := dataInfo.Mode().Perm(); got != 0o444 {
		t.Errorf("data file mode = %o, want 0444", got)
	}

	// Executable script: 0755 → 0555. CRITICAL — without
	// preserving exec, fork/exec fails before the wrapper runs.
	scriptInfo, err := os.Stat(scriptFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := scriptInfo.Mode().Perm(); got != 0o555 {
		t.Errorf("script mode = %o, want 0555 (exec bit preserved)", got)
	}

	// Directories: 0755 → 0555 (exec on a dir means traversable).
	libInfo, err := os.Stat(filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if got := libInfo.Mode().Perm(); got != 0o555 {
		t.Errorf("dir mode = %o, want 0555", got)
	}

	// And the negative: no write bits anywhere.
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, ierr := os.Stat(p)
		if ierr != nil {
			return ierr
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s: write bits not stripped (mode %o)", p, info.Mode().Perm())
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

// TestChmodSnapshotReadOnly_PollutionAttemptFails is the most
// important assertion: after chmod, attempting to write to the
// snapshot directly returns EACCES. This is the load-bearing
// safety guarantee for the host-exec path.
func TestChmodSnapshotReadOnly_PollutionAttemptFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission enforcement; Windows protection is weaker (see spec portability matrix)")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses POSIX permissions; this safety net is for unprivileged bots")
	}
	root := chmodReadonlyTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ChmodSnapshotReadOnly(root); err != nil {
		t.Fatalf("ChmodSnapshotReadOnly: %v", err)
	}

	// Direct write attempt (mimics `echo > data.txt`): must fail.
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("v2"), 0o644); err == nil {
		t.Errorf("expected permission denied when writing to chmod-readonly snapshot; got nil")
	}
	// Create-new-file attempt (mimics `> pollution.txt`): must fail.
	if err := os.WriteFile(filepath.Join(root, "pollution.txt"), []byte("oops"), 0o644); err == nil {
		t.Errorf("expected permission denied when creating new file in chmod-readonly snapshot; got nil")
	}
	// Subdir creation (mimics __pycache__): must fail.
	if err := os.Mkdir(filepath.Join(root, "__pycache__"), 0o755); err == nil {
		t.Errorf("expected permission denied when creating new dir in chmod-readonly snapshot; got nil")
	}
}

// TestChmodSnapshotReadOnly_SymlinksUntouched is the containment
// guard. filepath.Walk follows symlinks, which would let chmod
// modify the symlink target — potentially OUTSIDE the snapshot.
// That's a containment break, not just hygiene. We use
// filepath.WalkDir and an explicit symlink skip in the visitor.
// This test makes sure the implementation honors that.
func TestChmodSnapshotReadOnly_SymlinksUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink semantics differ (privilege required); skip for POSIX-specific containment test")
	}
	// Outside-the-snapshot target. Must NOT be chmod'd. This one
	// stays writable so t.TempDir() cleanup succeeds normally.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "external.txt")
	if err := os.WriteFile(outsideFile, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Snapshot tree containing a symlink that points at the
	// outside file.
	snap := chmodReadonlyTempDir(t)
	linkInside := filepath.Join(snap, "link-to-external")
	if err := os.Symlink(outsideFile, linkInside); err != nil {
		t.Fatal(err)
	}

	if err := ChmodSnapshotReadOnly(snap); err != nil {
		t.Fatalf("ChmodSnapshotReadOnly: %v", err)
	}

	// External target's mode should be untouched. If
	// ChmodSnapshotReadOnly followed the symlink, it would be
	// 0444; we expect it to still be 0644 (whatever umask gave
	// us at WriteFile time).
	info, err := os.Stat(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() == 0o444 {
		t.Errorf("external file at %s was chmod'd to 0444 — containment broken; chmod followed the symlink", outsideFile)
	}
}

// TestChmodSnapshotReadOnly_MissingRootIsNoOp pins the soft-fail
// behavior: callers may invoke this before the snapshot exists
// (or for inline-YAML runs where no snapshot exists at all). The
// helper must not error in that case.
func TestChmodSnapshotReadOnly_MissingRootIsNoOp(t *testing.T) {
	if err := ChmodSnapshotReadOnly(""); err != nil {
		t.Errorf("empty root should be no-op, got: %v", err)
	}
	if err := ChmodSnapshotReadOnly(filepath.Join(t.TempDir(), "doesnt-exist")); err != nil {
		t.Errorf("missing root should be no-op, got: %v", err)
	}
}

// TestChmodSnapshotReadOnly_Idempotent pins that re-running the
// chmod against an already-readonly tree succeeds — callers can
// invoke before every execute without worrying about state.
func TestChmodSnapshotReadOnly_Idempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-specific")
	}
	root := chmodReadonlyTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := ChmodSnapshotReadOnly(root); err != nil {
			t.Fatalf("invocation %d: %v", i+1, err)
		}
	}
	info, err := os.Stat(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Errorf("after 3x chmod, mode = %o, want 0444", info.Mode().Perm())
	}
}

// Compile-time guard that fmt is imported (helpful if a future
// refactor removes uses of it in this file's helpers).
var _ = fmt.Sprintf
