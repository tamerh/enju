package compute

import (
	"context"
	"os"
	"strings"
	"testing"
)

// writeExecutableStub creates a minimal executable at `path`.
// Content is irrelevant — LookPath only checks the executable
// bit. Used to simulate "docker is installed" without pulling
// in a real Docker client on CI runners that don't have one.
func writeExecutableStub(path string) error {
	return os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755)
}

// hasFlagValue returns true when `args` contains `flag`
// followed immediately by `value`. Docker flags are
// position-significant (two-argument form) so an equality
// check against a joined form wouldn't reject a reordered
// argv that happens to contain both tokens in wrong spots.
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// argIndex returns the position of `arg` in args, or -1.
// Used to assert ordering (image name must precede shell
// invocation, -v flags must precede the image, etc).
func argIndex(args []string, arg string) int {
	for i, a := range args {
		if a == arg {
			return i
		}
	}
	return -1
}

func TestBuildContainerArgsHappyPath(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{
		Container:  "biocontainers/samtools:1.18",
		ScriptPath: "/host/workspaces/demo-1/enju/runs/3/template-snapshot/scripts/align.sh",
	}
	env := []string{
		"ENJU_TASK_ID=1:3:align",
		"ENJU_PROJECT_DIR=/host/workspaces/demo-1",
		"ENJU_RUN_DIR=/host/workspaces/demo-1/enju/runs/3/align",
		"ENJU_PARAM_sample=alpha",
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/workspaces/demo-1", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Leading command shape.
	if args[0] != "run" || args[1] != "--rm" {
		t.Fatalf("expected args to start with 'run --rm', got %v", args[:2])
	}

	// Workspace bind-mount (with :z SELinux label — see the
	// dedicated SELinux test for the rationale).
	if !hasFlagValue(args, "-v", "/host/workspaces/demo-1:/workspace:z") {
		t.Errorf("missing workspace bind-mount: %v", args)
	}

	// Working directory.
	if !hasFlagValue(args, "-w", "/workspace") {
		t.Errorf("missing -w /workspace: %v", args)
	}

	// User mapping.
	if !hasFlagValue(args, "--user", "1000:1000") {
		t.Errorf("missing --user 1000:1000: %v", args)
	}

	// Env var with host-path value → translated.
	if !hasFlagValue(args, "-e", "ENJU_PROJECT_DIR=/workspace") {
		t.Errorf("ENJU_PROJECT_DIR not translated: %v", args)
	}
	if !hasFlagValue(args, "-e", "ENJU_RUN_DIR=/workspace/enju/runs/3/align") {
		t.Errorf("ENJU_RUN_DIR not translated: %v", args)
	}

	// Env var with non-path value → pass-through.
	if !hasFlagValue(args, "-e", "ENJU_TASK_ID=1:3:align") {
		t.Errorf("ENJU_TASK_ID not passed through: %v", args)
	}
	if !hasFlagValue(args, "-e", "ENJU_PARAM_sample=alpha") {
		t.Errorf("ENJU_PARAM_sample not passed through: %v", args)
	}

	// Image + script invocation (ordering matters: image
	// before the /bin/sh).
	imgIdx := argIndex(args, "biocontainers/samtools:1.18")
	shIdx := argIndex(args, "/bin/sh")
	scriptIdx := argIndex(args, "/workspace/enju/runs/3/template-snapshot/scripts/align.sh")
	if imgIdx < 0 || shIdx < 0 || scriptIdx < 0 {
		t.Fatalf("image/shell/script not all present: img=%d sh=%d script=%d\n%v", imgIdx, shIdx, scriptIdx, args)
	}
	if !(imgIdx < shIdx && shIdx < scriptIdx) {
		t.Errorf("expected order image < /bin/sh < script, got %d/%d/%d", imgIdx, shIdx, scriptIdx)
	}
}

// TestBuildContainerArgsScriptOutsideWorkspaceRejected — the
// translator returns (path, false) when the script lives
// outside workDir, which the builder promotes to a hard
// error. Otherwise the container would exec a host path that
// doesn't exist inside its namespace.
func TestBuildContainerArgsScriptOutsideWorkspaceRejected(t *testing.T) {
	spec := Spec{
		Container:  "alpine",
		ScriptPath: "/opt/elsewhere/run.sh",
	}
	_, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err == nil {
		t.Fatal("expected error for script outside workspace, got nil")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("error should mention 'outside workspace', got %q", err)
	}
}

// TestBuildContainerArgsEmptyContainerRejected — calling the
// builder on a spec with no container field is a programmer
// error, not a user-facing case. Caller's job to guard first.
func TestBuildContainerArgsEmptyContainerRejected(t *testing.T) {
	spec := Spec{ScriptPath: "/host/ws/run.sh"}
	_, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err == nil {
		t.Fatal("expected error for empty container, got nil")
	}
}

// TestBuildContainerArgsEmptyWorkDirRejected — similarly a
// programmer error. Bind-mounting `:/workspace` would be
// disastrous (mounting the current working directory
// unexpectedly).
func TestBuildContainerArgsEmptyWorkDirRejected(t *testing.T) {
	spec := Spec{Container: "alpine", ScriptPath: "/x"}
	_, err := BuildContainerArgs(RuntimeDocker, spec, nil, "", 1000, 1000)
	if err == nil {
		t.Fatal("expected error for empty workDir, got nil")
	}
}

// TestBuildContainerArgsUnknownRuntimeRejected pins the
// forward-compat seam: the switch case is the only place
// runtime strings are accepted, so future apptainer support
// slots in cleanly.
func TestBuildContainerArgsUnknownRuntimeRejected(t *testing.T) {
	spec := Spec{Container: "alpine", ScriptPath: "/host/ws/run.sh"}
	_, err := BuildContainerArgs("apptainer", spec, nil, "/host/ws", 1000, 1000)
	if err == nil {
		t.Fatal("expected error for unknown runtime, got nil")
	}
	if !strings.Contains(err.Error(), "apptainer") {
		t.Errorf("error should name the offending runtime, got %q", err)
	}
}

func TestBuildContainerArgsSharedRootBindMount(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "/mnt/nfs/enju")

	spec := Spec{
		Container:  "alpine",
		ScriptPath: "/host/ws/run.sh",
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// Read-write bind-mount at the same host path with :z
	// SELinux label — symlink targets must resolve
	// identically inside and outside, and the mount needs
	// to work on RHEL/Fedora hosts.
	if !hasFlagValue(args, "-v", "/mnt/nfs/enju:/mnt/nfs/enju:z") {
		t.Errorf("missing shared-root bind-mount: %v", args)
	}
}

func TestBuildContainerArgsSharedRootUnsetNoMount(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{Container: "alpine", ScriptPath: "/host/ws/run.sh"}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" && strings.Contains(args[i+1], "/mnt") {
			t.Errorf("unexpected shared-root mount when unset: %v", args)
		}
	}
}

// TestTranslatePathVariants covers the path-translator in
// isolation: prefix match, exact match, outside-workspace
// rejection, trailing-slash normalization.
func TestTranslatePathVariants(t *testing.T) {
	cases := []struct {
		name         string
		hostPath     string
		workDir      string
		wantPath     string
		wantTranslat bool
	}{
		{"exact match", "/host/ws", "/host/ws", "/workspace", true},
		{"exact with trailing slash", "/host/ws/", "/host/ws", "/workspace", true},
		{"subdir", "/host/ws/out/report.md", "/host/ws", "/workspace/out/report.md", true},
		{"deep subdir", "/host/ws/enju/runs/3/align", "/host/ws", "/workspace/enju/runs/3/align", true},
		{"workdir with trailing slash", "/host/ws/out", "/host/ws/", "/workspace/out", true},
		{"outside workspace", "/opt/tools/bwa", "/host/ws", "/opt/tools/bwa", false},
		{"adjacent prefix not a match", "/host/ws-other/x", "/host/ws", "/host/ws-other/x", false},
		{"empty host", "", "/host/ws", "", false},
		{"empty workdir", "/host/ws/x", "", "/host/ws/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := translatePath(tc.hostPath, tc.workDir, ContainerWorkDir)
			if got != tc.wantPath || ok != tc.wantTranslat {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.wantPath, tc.wantTranslat)
			}
		})
	}
}

func TestSplitEnvEntry(t *testing.T) {
	cases := []struct {
		in           string
		wantKey      string
		wantValue    string
		wantOK       bool
	}{
		{"KEY=value", "KEY", "value", true},
		{"KEY=", "KEY", "", true},
		{"KEY=with=equals=signs", "KEY", "with=equals=signs", true},
		{"=only_value", "", "only_value", true},
		{"no_equals", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		k, v, ok := splitEnvEntry(tc.in)
		if k != tc.wantKey || v != tc.wantValue || ok != tc.wantOK {
			t.Errorf("splitEnvEntry(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, k, v, ok, tc.wantKey, tc.wantValue, tc.wantOK)
		}
	}
}

// TestBuildContainerArgsMalformedEnvSkipped — entries without
// `=` are dropped silently, independent of the allowlist
// filter. Malformed env is rare (Go's os.Environ guarantees
// KEY=VALUE) but defense-in-depth is cheap.
//
// Uses ENJU_-prefixed keys to bypass the host-env allowlist
// (TestBuildContainerArgsFiltersHostEnvLeaks covers that
// layer); the focus here is the `=`-split behavior.
func TestBuildContainerArgsMalformedEnvSkipped(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{Container: "alpine", ScriptPath: "/host/ws/run.sh"}
	env := []string{"ENJU_A=yes", "bad_no_equals", "ENJU_B=1"}
	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "-e", "ENJU_A=yes") {
		t.Error("ENJU_A env var missing")
	}
	if !hasFlagValue(args, "-e", "ENJU_B=1") {
		t.Error("ENJU_B env var missing")
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" && args[i+1] == "bad_no_equals" {
			t.Error("malformed env entry made it through")
		}
	}
}

// TestCheckContainerRuntimeNoContainer — no container declared
// means no runtime is needed; check is a clean noop regardless
// of PATH contents.
func TestCheckContainerRuntimeNoContainer(t *testing.T) {
	t.Setenv("PATH", "") // even an empty PATH is fine when no container.
	spec := Spec{TaskID: "1:1:t", ScriptPath: "/host/ws/run.sh"}
	if err := checkContainerRuntime(spec); err != nil {
		t.Fatalf("unexpected error on non-container task: %v", err)
	}
}

// TestCheckContainerRuntimeMissingDockerUserError — container
// declared but PATH doesn't resolve `docker`: surface an error
// that names the task id, the declared image, and points at
// the fix.
func TestCheckContainerRuntimeMissingDockerUserError(t *testing.T) {
	// Isolate PATH to a directory that definitely lacks docker.
	t.Setenv("PATH", t.TempDir())

	spec := Spec{
		TaskID:    "proj:1:align",
		Container: "biocontainers/samtools:1.18",
		ScriptPath: "/host/ws/run.sh",
	}
	err := checkContainerRuntime(spec)
	if err == nil {
		t.Fatal("expected error when docker missing, got nil")
	}
	msg := err.Error()
	// Error must be user-actionable, naming both the task and
	// the declared image so a human sees why the task is
	// blocked at a glance.
	for _, want := range []string{
		"proj:1:align",
		"biocontainers/samtools:1.18",
		"docker",
		"install",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
}

// TestCheckContainerRuntimePresentNoError — shadow docker with
// a fake executable on PATH and verify the check passes.
// Doesn't actually run the fake; LookPath just checks the
// bit and that it's executable.
func TestCheckContainerRuntimePresentNoError(t *testing.T) {
	fakeDir := t.TempDir()
	fakeDocker := fakeDir + "/docker"
	// Minimal shell stub — content doesn't matter, only the
	// exec bit. LookPath checks x permission.
	if err := writeExecutableStub(fakeDocker); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)

	spec := Spec{TaskID: "1:1:t", Container: "alpine", ScriptPath: "/x/y"}
	if err := checkContainerRuntime(spec); err != nil {
		t.Errorf("unexpected error when docker stub is on PATH: %v", err)
	}
}

// TestBuildExecCommandDirectMode — no container set: exec the
// script directly, pass the wrapper-assembled env through
// verbatim. This is the legacy pre-container behavior and
// must stay byte-identical.
func TestBuildExecCommandDirectMode(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{
		ScriptPath: "/host/ws/scripts/run.sh",
		// Container intentionally empty.
	}
	env := []string{"ENJU_TASK_ID=1:2:3", "FOO=bar"}

	cmd, err := buildExecCommand(context.Background(), spec, env, "/host/ws", "/host/ws")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/host/ws/scripts/run.sh" && cmd.Args[0] != "/host/ws/scripts/run.sh" {
		t.Errorf("expected direct script exec, got Path=%q Args=%v", cmd.Path, cmd.Args)
	}
	if cmd.Dir != "/host/ws" {
		t.Errorf("Dir = %q, want /host/ws", cmd.Dir)
	}
	if len(cmd.Env) != 2 || cmd.Env[0] != "ENJU_TASK_ID=1:2:3" {
		t.Errorf("direct-mode env should be exactly the wrapper-provided slice, got %v", cmd.Env)
	}
}

// TestBuildExecCommandContainerMode — container set: we build
// `docker run ...` with the pure-function arg builder. The
// docker CLI inherits the host env (so it can find
// DOCKER_HOST / config); the SCRIPT's env lives as -e flags
// baked into the argv.
func TestBuildExecCommandContainerMode(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{
		Container:  "alpine:3.19",
		ScriptPath: "/host/ws/scripts/run.sh",
	}
	env := []string{"ENJU_TASK_ID=1:2:3", "ENJU_PROJECT_DIR=/host/ws"}

	cmd, err := buildExecCommand(context.Background(), spec, env, "/host/ws", "/host/ws")
	if err != nil {
		t.Fatal(err)
	}
	// First arg is `docker` (the binary), then the argv from
	// BuildContainerArgs (run --rm ...).
	if !strings.HasSuffix(cmd.Args[0], "docker") && cmd.Args[0] != "docker" {
		t.Errorf("expected docker invocation, got Args[0]=%q", cmd.Args[0])
	}
	if cmd.Args[1] != "run" || cmd.Args[2] != "--rm" {
		t.Errorf("expected 'run --rm' after docker, got Args[1..3]=%v", cmd.Args[1:3])
	}
	// The translated env var must appear as an -e flag.
	found := false
	for i := 0; i < len(cmd.Args)-1; i++ {
		if cmd.Args[i] == "-e" && cmd.Args[i+1] == "ENJU_PROJECT_DIR=/workspace" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("translated ENJU_PROJECT_DIR not in docker args: %v", cmd.Args)
	}
	// cmd.Env should be the wrapper's host env (os.Environ),
	// NOT the env slice we passed — that one went into -e
	// flags. Lack of os.Environ would break `docker` on hosts
	// that rely on DOCKER_HOST / PATH.
	if len(cmd.Env) < 1 {
		t.Error("container-mode cmd.Env should inherit os.Environ, got empty")
	}
	for _, e := range cmd.Env {
		if e == "ENJU_PROJECT_DIR=/host/ws" {
			t.Errorf("the per-script env slice leaked into docker CLI env: %q (should only be in -e flags)", e)
		}
	}
}

// TestBuildExecCommandContainerWorkDir — the working dir on
// the host-side Cmd is still the workspace root so any
// relative-path error messages from the docker CLI make
// sense. Inside the container `-w /workspace` kicks in.
func TestBuildExecCommandContainerWorkDir(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{Container: "alpine", ScriptPath: "/host/ws/run.sh"}
	cmd, err := buildExecCommand(context.Background(), spec, nil, "/host/ws", "/host/ws")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != "/host/ws" {
		t.Errorf("Dir = %q, want /host/ws", cmd.Dir)
	}
}

// TestBuildExecCommandContainerBadSpecReturnsError — an
// invalid spec surfaces via the builder error rather than
// panicking or silently falling through to a direct exec.
func TestBuildExecCommandContainerBadSpecReturnsError(t *testing.T) {
	spec := Spec{Container: "alpine", ScriptPath: "/outside/ws/run.sh"}
	_, err := buildExecCommand(context.Background(), spec, nil, "/host/ws", "/host/ws")
	if err == nil {
		t.Fatal("expected builder error for out-of-workspace script")
	}
	if !strings.Contains(err.Error(), "docker args") {
		t.Errorf("error should cite docker-args step, got %q", err)
	}
}

// TestBuildContainerArgsFiltersHostEnvLeaks is the security
// regression guard. The env slice handed to the wrapper
// carries os.Environ() (for the direct-exec host path), which
// includes whatever the user has in their shell — credentials
// (AWS_ACCESS_KEY_ID, ANTHROPIC_API_KEY, GITHUB_TOKEN,
// SSH_AUTH_SOCK), host PATH, HOME, USER, TMPDIR, etc.
// Forwarding those into a third-party container image is an
// unintended exposure: a community biocontainer would see
// the citizen's API keys, and a non-standard host PATH would
// override the image's PATH (breaking basic commands).
//
// The fix: in container mode, only forward ENJU_* variables
// and the task's declared env: block (passed via spec.Env).
// Everything else gets dropped. Host values never reach a
// container the user didn't put them there.
func TestBuildContainerArgsFiltersHostEnvLeaks(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{
		Container:  "alpine",
		ScriptPath: "/host/ws/run.sh",
		Env: map[string]string{
			"SAMPLE":      "alpha",
			"MY_BIO_TOOL": "/opt/tool",
		},
	}
	env := []string{
		// Allowlisted — ENJU_*:
		"ENJU_TASK_ID=1:2:3",
		"ENJU_PROJECT_DIR=/host/ws",
		"ENJU_PARAM_source=sample_data",
		// Allowlisted — task env block:
		"SAMPLE=alpha",
		"MY_BIO_TOOL=/opt/tool",
		// Must NOT reach the container:
		"AWS_ACCESS_KEY_ID=AKIA...",
		"ANTHROPIC_API_KEY=sk-ant-...",
		"GITHUB_TOKEN=ghp_...",
		"SSH_AUTH_SOCK=/tmp/ssh-agent",
		"GIT_ASKPASS=/usr/libexec/askpass",
		"PATH=/home/user/nix/bin:/usr/local/bin",
		"HOME=/home/user",
		"USER=tamer",
		"TMPDIR=/tmp",
		"LANG=en_US.UTF-8",
		"PWD=/wherever",
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// The allowlisted entries must survive.
	for _, want := range []string{
		"ENJU_TASK_ID=1:2:3",
		"ENJU_PROJECT_DIR=/workspace", // translated too
		"ENJU_PARAM_source=sample_data",
		"SAMPLE=alpha",
		"MY_BIO_TOOL=/opt/tool",
	} {
		if !hasFlagValue(args, "-e", want) {
			t.Errorf("allowlisted env %q missing from argv", want)
		}
	}

	// Every forbidden host-env key must be absent — no -e
	// flag should mention it, regardless of the value.
	forbidden := []string{
		"AWS_ACCESS_KEY_ID",
		"ANTHROPIC_API_KEY",
		"GITHUB_TOKEN",
		"SSH_AUTH_SOCK",
		"GIT_ASKPASS",
		"PATH",
		"HOME",
		"USER",
		"TMPDIR",
		"LANG",
		"PWD",
	}
	for _, key := range forbidden {
		prefix := key + "="
		for i := 0; i < len(args)-1; i++ {
			if args[i] != "-e" {
				continue
			}
			if strings.HasPrefix(args[i+1], prefix) {
				t.Errorf("host env %q leaked into container: %q", key, args[i+1])
			}
		}
	}
}

// TestBuildContainerArgsFiltersWithNilSpecEnv — nil
// spec.Env should still forward ENJU_* variables (the common
// case: no task env: block declared). Protects against a
// regression where the filter collapses the allowlist to
// just the spec.Env keys.
func TestBuildContainerArgsFiltersWithNilSpecEnv(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{
		Container:  "alpine",
		ScriptPath: "/host/ws/run.sh",
		// Env nil intentionally.
	}
	env := []string{
		"ENJU_TASK_ID=1:2:3",
		"ENJU_PARAM_x=1",
		"AWS_ACCESS_KEY_ID=leak", // host leak — must drop
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "-e", "ENJU_TASK_ID=1:2:3") {
		t.Error("ENJU_TASK_ID dropped when spec.Env is nil")
	}
	if !hasFlagValue(args, "-e", "ENJU_PARAM_x=1") {
		t.Error("ENJU_PARAM_x dropped when spec.Env is nil")
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" && strings.HasPrefix(args[i+1], "AWS_ACCESS_KEY_ID=") {
			t.Errorf("host env leaked with nil spec.Env: %q", args[i+1])
		}
	}
}

// TestBuildContainerArgsBindMountHasSELinuxLabel — on
// SELinux-enforcing distros (RHEL/Fedora/CentOS), a bind-mount
// without a :z (shared) or :Z (private) label gets AVC-denied
// on reads/writes. `:z` is safe on non-SELinux systems (kernel
// ignores it) and correct on SELinux systems, so we append it
// universally rather than trying to detect the host's LSM.
//
// Without this label, every task on a RHEL workstation would
// fail mysteriously with permission denied that looks like a
// uid mismatch.
func TestBuildContainerArgsBindMountHasSELinuxLabel(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{Container: "alpine", ScriptPath: "/host/ws/run.sh"}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "-v", "/host/ws:/workspace:z") {
		t.Errorf("workspace bind-mount missing :z SELinux label: %v", args)
	}
}

// TestBuildContainerArgsSharedRootBindMountHasSELinuxLabel —
// same rule for the shared-root mount. Without it, a
// RHEL-hosted task would write to the workspace path
// (labelled correctly) but read-through-symlink hits the
// shared root and gets AVC-denied.
func TestBuildContainerArgsSharedRootBindMountHasSELinuxLabel(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "/mnt/nfs/enju")
	spec := Spec{Container: "alpine", ScriptPath: "/host/ws/run.sh"}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "-v", "/mnt/nfs/enju:/mnt/nfs/enju:z") {
		t.Errorf("shared-root bind-mount missing :z SELinux label: %v", args)
	}
}

// TestBuildContainerArgsFlagsBeforeImage pins the Docker arg
// ordering rule: every docker flag must appear before the
// image name, and the image name before the command. Breaking
// this shows up as 'unknown flag' or similar at runtime.
func TestBuildContainerArgsFlagsBeforeImage(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "/mnt/nfs")
	spec := Spec{Container: "myimg:1", ScriptPath: "/host/ws/run.sh"}
	env := []string{"X=1"}
	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	imgIdx := argIndex(args, "myimg:1")
	if imgIdx < 0 {
		t.Fatalf("image not in args: %v", args)
	}
	for i := 0; i < imgIdx; i++ {
		if args[i] == "/bin/sh" {
			t.Errorf("/bin/sh appears before image (position %d < %d)", i, imgIdx)
		}
	}
	// Every -v / -e / -w / --user / --rm must come before
	// the image name. Scan backwards past the image.
	for i := imgIdx + 1; i < len(args); i++ {
		switch args[i] {
		case "-v", "-e", "-w", "--user", "--rm":
			t.Errorf("flag %q appears after image (position %d > %d)", args[i], i, imgIdx)
		}
	}
}

// TestBuildContainerArgsScratchBindMount pins the Phase 2.5
// behaviour: when spec.TaskScratchDir is set in container mode,
// buildDockerArgs adds a second bind mount for the scratch dir,
// sets -w to the in-container scratch path, and translates
// host scratch paths in env values to the in-container path.
func TestBuildContainerArgsScratchBindMount(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	scratch := "/host/scratch/task-1-1-fetch-iter-1"
	spec := Spec{
		Container:      "bioalpine:latest",
		ScriptPath:     "/host/ws/scripts/run.sh",
		TaskScratchDir: scratch,
	}
	env := []string{
		"ENJU_TASK_ID=1:1:fetch",
		"ENJU_PROJECT_DIR=" + scratch,
		"ENJU_TASK_DIR=" + scratch,
		"ENJU_RUN_DIR=/host/ws/enju/runs/3/fetch",
	}

	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatalf("BuildContainerArgs: %v", err)
	}

	// Workspace bind still present.
	if !hasFlagValue(args, "-v", "/host/ws:/workspace:z") {
		t.Errorf("workspace bind missing: %v", args)
	}
	// New scratch bind.
	wantScratchBind := scratch + ":/scratch:z"
	if !hasFlagValue(args, "-v", wantScratchBind) {
		t.Errorf("scratch bind missing %q: %v", wantScratchBind, args)
	}
	// In-container CWD flipped to /scratch.
	if !hasFlagValue(args, "-w", "/scratch") {
		t.Errorf("expected -w /scratch, got: %v", args)
	}
	// ENJU_PROJECT_DIR + ENJU_TASK_DIR translated via scratch
	// prefix; ENJU_RUN_DIR via workspace prefix; ENJU_TASK_ID
	// passes through.
	checks := map[string]string{
		"ENJU_TASK_ID":     "1:1:fetch",
		"ENJU_PROJECT_DIR": "/scratch",
		"ENJU_TASK_DIR":    "/scratch",
		"ENJU_RUN_DIR":     "/workspace/enju/runs/3/fetch",
	}
	for k, want := range checks {
		if !hasFlagValue(args, "-e", k+"="+want) {
			t.Errorf("env %s: want %q in args, got: %v", k, want, args)
		}
	}
}

// TestBuildContainerArgsNoScratchKeepsLegacy pins that container
// tasks WITHOUT a TaskScratchDir still get the pre-Phase-2.5
// behaviour: only the workspace is bound, -w is /workspace,
// no /scratch in argv. Guards against regressions for legacy
// specs (older spec files in flight, tests that don't set
// TaskScratchDir).
func TestBuildContainerArgsNoScratchKeepsLegacy(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")
	spec := Spec{
		Container:  "alpine:3.19",
		ScriptPath: "/host/ws/scripts/run.sh",
		// TaskScratchDir intentionally empty.
	}
	env := []string{"ENJU_TASK_ID=1:1:t"}

	args, err := BuildContainerArgs(RuntimeDocker, spec, env, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatalf("BuildContainerArgs: %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "/scratch") {
			t.Errorf("legacy spec should not produce /scratch refs, got: %v", args)
			break
		}
	}
	if !hasFlagValue(args, "-w", "/workspace") {
		t.Errorf("legacy: expected -w /workspace, got: %v", args)
	}
}
