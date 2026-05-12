package compute

// Tests for the snapshot-as-CWD shape: scripts run with the run's
// frozen template-snapshot as their working directory (read sibling
// files like ./scripts/helper.sh and import lib.utils naturally),
// scratch is the writable per-iter sandbox exposed via ENJU_SCRATCH.
//
// Two layers under test:
//   1. ScriptCwdFor priority — snapshot > scratch > workDir.
//   2. Container argv shape with SnapshotDir set — second bind mount
//      (snapshot:/template:ro,z), CWD switches to /template, env
//      values pointing at the host snapshot path get rewritten to
//      /template.
//
// Host-side chmod enforcement was dropped with the per-run-snapshot
// redesign — read-only is convention now; scripts that write to the
// snapshot are buggy and should target $ENJU_SCRATCH. Container path
// still gets a kernel-side :ro bind for the strong guarantee inside
// the sandbox.

import (
	"fmt"
	"os"
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

// Compile-time guard that fmt is imported (helpful if a future
// refactor removes uses of it in this file's helpers).
var _ = fmt.Sprintf
var _ = os.Getuid
