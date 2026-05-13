package compute

// Tests for the scratch-as-CWD shape: scripts run with their per-task
// scratch directory as the working directory (relative-path writes
// like `data/$ITEM/s1.txt` land where writes_artifacts pickup looks).
// The run's frozen snapshot is mounted read-only and accessed via
// $ENJU_TEMPLATE_DIR / $ENJU_REPO_DIR for sibling files.
//
// Two layers under test:
//   1. ScriptCwdFor priority — scratch > snapshot > workDir.
//   2. Container argv shape with SnapshotDir + scratch set — the
//      snapshot binds at /template:ro,z but CWD goes to /scratch.
//      Env values pointing at the host snapshot path still get
//      rewritten to /template so $ENJU_TEMPLATE_DIR resolves
//      correctly from the script's POV.

import (
	"fmt"
	"os"
	"testing"
)

// ─── ScriptCwdFor ──────────────────────────────────────────────

// TestScriptCwdFor_ScratchWinsWhenSet pins the priority order:
// scratch is the script's CWD whenever it's set, even alongside a
// snapshot. Scripts that write relative paths (`data/$ITEM/s1.txt`)
// land in scratch, where writes_artifacts pickup looks; the snapshot
// is reached via $ENJU_TEMPLATE_DIR / $ENJU_REPO_DIR for reads.
func TestScriptCwdFor_ScratchWinsWhenSet(t *testing.T) {
	spec := Spec{
		SnapshotDir:    "/host/snap",
		TaskScratchDir: "/host/scratch",
	}
	if got := ScriptCwdFor(spec, "/host/work"); got != "/host/scratch" {
		t.Errorf("ScriptCwdFor = %q, want %q (scratch wins over snapshot)", got, "/host/scratch")
	}
}

// TestScriptCwdFor_SnapshotWhenNoScratch pins the legacy fallback for
// the rare callers that populate SnapshotDir but not TaskScratchDir.
// Without scratch the snapshot is the only candidate above workDir.
func TestScriptCwdFor_SnapshotWhenNoScratch(t *testing.T) {
	spec := Spec{SnapshotDir: "/host/snap"}
	if got := ScriptCwdFor(spec, "/host/work"); got != "/host/snap" {
		t.Errorf("ScriptCwdFor with no scratch = %q, want snapshot %q", got, "/host/snap")
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

// TestBuildContainerArgs_SnapshotBindAndCWD pins the container-arg
// behavior: when SnapshotDir is set, a second bind mount lands the
// snapshot at /template:ro,z, but CWD goes to /scratch (scratch wins
// over snapshot). Env values referencing the host snapshot path still
// get rewritten to /template so $ENJU_TEMPLATE_DIR resolves correctly.
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

	// Container CWD is /scratch even with the snapshot bound —
	// scratch wins over snapshot so relative-path writes land
	// where writes_artifacts pickup looks.
	if !hasFlagValue(args, "-w", "/scratch") {
		t.Errorf("expected -w /scratch, got: %v", args)
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

// Compile-time guard that fmt is imported (helpful if a future
// refactor removes uses of it in this file's helpers).
var _ = fmt.Sprintf
var _ = os.Getuid
