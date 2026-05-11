package compute

// Container-runtime argument builder. A pure function that
// turns (Spec, env, workDir) into the argv for a container CLI
// invocation. Lives in its own file so it's straightforward to
// unit-test exhaustively (path translation, user mapping, env
// forwarding, shared-root compose) without a docker binary
// present.
//
// Why "buildContainerArgs" not "buildDockerArgs": keeps the
// door open for apptainer / podman as a second runtime
// post-launch. The signature stays stable; adding a new
// runtime is a switch case, not a caller rewrite. See
// docs/containers.md § Why not Apptainer yet for the defer
// rationale.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// ContainerWorkDir is where the host workspace clone is
// bind-mounted inside the container. Fixed-constant: there's
// no benefit to making it configurable, and callers that
// translate host paths → container paths rely on a stable
// target for the prefix substitution.
const ContainerWorkDir = "/workspace"

// ContainerScratchDir is where the host's TaskScratchDir is
// bind-mounted inside the container when Phase 2.5 isolation
// is in effect (spec.TaskScratchDir non-empty). Same fixed-
// constant rationale as ContainerWorkDir.
//
// Scripts inside the container see their declared inputs +
// outputs at this path; ENJU_PROJECT_DIR + ENJU_TASK_DIR are
// rewritten to /scratch in the env-forwarding loop, so a
// script that does "cat $ENJU_PROJECT_DIR/data/raw_a.txt"
// reads from the bind-mounted scratch transparently.
const ContainerScratchDir = "/scratch"

// Container-runtime selectors. Singularity is collapsed to
// apptainer at YAML parse time (validateTaskContainerRuntime),
// so internal code only deals with two values.
const (
	RuntimeDocker    = "docker"
	RuntimeApptainer = "apptainer"
)

// resolveRuntime returns the effective runtime for a spec.
// Empty spec.ContainerRuntime defaults to docker so the
// pre-apptainer behavior (every container task ran under
// docker) holds for templates that don't set the field.
func resolveRuntime(spec Spec) string {
	if spec.ContainerRuntime == "" {
		return RuntimeDocker
	}
	return spec.ContainerRuntime
}

// BuildContainerArgs returns the argv (without the leading
// command) for running spec.ScriptPath inside spec.Container
// with the workspace at workDir bind-mounted at
// ContainerWorkDir. hostUID/hostGID get mapped via --user so
// output files land owned by the host user, not root.
//
// Every KEY=VALUE in env gets passed through as -e KEY=VALUE
// after path translation: any prefix matching workDir is
// rewritten to ContainerWorkDir, so ENJU_RUN_DIR and related
// variables that refer to host paths under the workspace end
// up pointing at the right container-side paths. Other values
// pass through unchanged (the translator is a prefix check,
// not a guess).
//
// When ENJU_SHARED_ROOT is configured (via the env that
// enjugit.SharedRoot() reads), its host path is also
// bind-mounted at the same path inside the container —
// read-write, because untracked artifact symlinks in the
// workspace point at it and the script needs to write through.
// Without this bind, those symlinks would dangle inside the
// container.
//
// Returns an error if spec.ScriptPath doesn't live under
// workDir (it would have no container-side path). That's
// shouldn't happen with the current handler (scripts resolve
// inside the project clone), but catching it cleanly here
// avoids a mystifying docker error downstream.
func BuildContainerArgs(runtime string, spec Spec, env []string, workDir string, hostUID, hostGID int) ([]string, error) {
	switch runtime {
	case RuntimeDocker:
		return buildDockerArgs(spec, env, workDir, hostUID, hostGID)
	case RuntimeApptainer:
		return buildApptainerArgs(spec, env, workDir)
	default:
		return nil, fmt.Errorf("unsupported container runtime %q", runtime)
	}
}

func buildDockerArgs(spec Spec, env []string, workDir string, hostUID, hostGID int) ([]string, error) {
	if spec.Container == "" {
		return nil, fmt.Errorf("spec.container is required for container execution")
	}
	if workDir == "" {
		return nil, fmt.Errorf("workDir is required for container execution")
	}
	scriptInContainer, ok := translatePath(spec.ScriptPath, workDir, ContainerWorkDir)
	if !ok {
		return nil, fmt.Errorf("script path %q is outside workspace %q — cannot translate to container path", spec.ScriptPath, workDir)
	}

	// Phase 2.5: when scratch isolation is active, the script's
	// effective CWD inside the container is /scratch (where its
	// inputs are materialized + outputs land). Without scratch
	// (legacy specs), keep the pre-2.5 default of /workspace as
	// CWD. Either way the workspace is bind-mounted so the
	// script file itself (which lives under workDir) is reachable.
	containerCWD := ContainerWorkDir
	if spec.TaskScratchDir != "" {
		containerCWD = ContainerScratchDir
	}

	args := []string{
		"run", "--rm",
		// `:z` is a shared SELinux label. On RHEL/Fedora/
		// CentOS with enforcing SELinux, a bind-mount without
		// a label gets AVC-denied on reads/writes — every
		// task would fail with a permission-denied that looks
		// like a uid mismatch but isn't. The kernel ignores
		// the label on non-SELinux systems (Ubuntu, macOS,
		// etc.), so we append it universally rather than
		// detecting the host's LSM.
		"-v", workDir + ":" + ContainerWorkDir + ":z",
	}
	// Phase 2.5: scratch bind. Read-write because the script
	// writes its outputs here (writes_artifacts expansion will
	// pick them up from the host side after the container exits).
	if spec.TaskScratchDir != "" {
		args = append(args, "-v", spec.TaskScratchDir+":"+ContainerScratchDir+":z")
	}
	args = append(args,
		"-w", containerCWD,
		"--user", fmt.Sprintf("%d:%d", hostUID, hostGID),
	)

	// Shared-root bind-mount, when configured. Read-write so
	// untracked artifacts written through workspace-side
	// symlinks reach shared storage. Same SELinux-label
	// rationale as the workspace mount above.
	if shared := enjugit.SharedRoot(); shared != "" {
		args = append(args, "-v", shared+":"+shared+":z")
	}

	// Env-var forwarding with a strict allowlist. Two classes
	// survive the filter:
	//
	//   1. ENJU_* — the wrapper + handler set these (TASK_ID,
	//      PROJECT_DIR, RUN_DIR, TEMPLATE_DIR, PARAM_*, etc.).
	//      Load-bearing for scripts; always forwarded.
	//
	//   2. Keys in spec.Env — the task's declared env: block.
	//      Template author opted into each one explicitly.
	//
	// Everything else is dropped, including host credentials
	// (AWS_ACCESS_KEY_ID, ANTHROPIC_API_KEY, GITHUB_TOKEN,
	// SSH_AUTH_SOCK, GIT_ASKPASS), host PATH/HOME/USER/TMPDIR/
	// LANG/PWD, and any other shell-inherited variables. A
	// third-party community image has no business seeing the
	// citizen's API keys; nor should a non-standard host PATH
	// silently override the image's own PATH and break basic
	// commands.
	//
	// Translation still applies to allowed values: any prefix
	// matching workDir gets rewritten to /workspace so
	// ENJU_RUN_DIR, ENJU_TEMPLATE_DIR, etc. point at the
	// right container-side paths.
	for _, kv := range env {
		k, v, split := splitEnvEntry(kv)
		if !split {
			continue
		}
		if !envKeyAllowed(k, spec.Env) {
			continue
		}
		translated, ok := translatePath(v, workDir, ContainerWorkDir)
		// Phase 2.5: a value that didn't match the workspace
		// prefix may match the scratch prefix (e.g.
		// ENJU_PROJECT_DIR / ENJU_TASK_DIR which the service
		// layer points at host-side scratch). Translate to
		// the in-container scratch path so the script sees
		// the bind-mounted view.
		if !ok && spec.TaskScratchDir != "" {
			translated, _ = translatePath(v, spec.TaskScratchDir, ContainerScratchDir)
		}
		args = append(args, "-e", k+"="+translated)
	}

	args = append(args, spec.Container)
	args = append(args, "/bin/sh", "-c", scriptInContainer)
	return args, nil
}

// buildApptainerArgs is the apptainer mirror of buildDockerArgs.
// Same logical shape — workspace bind, optional scratch bind,
// CWD selection, env allowlist with host→container path
// translation, image + /bin/sh -c — but with the CLI grammar
// apptainer accepts:
//
//   - `exec` instead of `run --rm` (apptainer has no persistent
//     containers to remove).
//   - `--bind host:container` instead of `-v host:container:z`.
//     SELinux relabeling on bind mounts is a docker-only concept;
//     apptainer's user-namespace mode doesn't need it.
//   - `--pwd /work` instead of `-w /work`.
//   - `--env KEY=VAL` instead of `-e KEY=VAL`.
//   - No `--user`. Apptainer runs as the calling host user by
//     default, so files written from inside the container land
//     owned by that user — no uid mapping gymnastics.
//   - `--cleanenv` so the container starts with an empty env
//     and only the explicit `--env KEY=VAL` flags carry forward.
//     Without this, apptainer leaks host PATH/HOME/etc. into
//     the script's view, defeating the allowlist that docker
//     gets implicitly from its own env-isolation default.
func buildApptainerArgs(spec Spec, env []string, workDir string) ([]string, error) {
	if spec.Container == "" {
		return nil, fmt.Errorf("spec.container is required for container execution")
	}
	if workDir == "" {
		return nil, fmt.Errorf("workDir is required for container execution")
	}
	scriptInContainer, ok := translatePath(spec.ScriptPath, workDir, ContainerWorkDir)
	if !ok {
		return nil, fmt.Errorf("script path %q is outside workspace %q — cannot translate to container path", spec.ScriptPath, workDir)
	}

	containerCWD := ContainerWorkDir
	if spec.TaskScratchDir != "" {
		containerCWD = ContainerScratchDir
	}

	args := []string{
		"exec",
		"--cleanenv",
		"--bind", workDir + ":" + ContainerWorkDir,
	}
	if spec.TaskScratchDir != "" {
		args = append(args, "--bind", spec.TaskScratchDir+":"+ContainerScratchDir)
	}
	args = append(args, "--pwd", containerCWD)

	if shared := enjugit.SharedRoot(); shared != "" {
		args = append(args, "--bind", shared+":"+shared)
	}

	for _, kv := range env {
		k, v, split := splitEnvEntry(kv)
		if !split {
			continue
		}
		if !envKeyAllowed(k, spec.Env) {
			continue
		}
		translated, ok := translatePath(v, workDir, ContainerWorkDir)
		if !ok && spec.TaskScratchDir != "" {
			translated, _ = translatePath(v, spec.TaskScratchDir, ContainerScratchDir)
		}
		args = append(args, "--env", k+"="+translated)
	}

	args = append(args, spec.Container)
	args = append(args, "/bin/sh", "-c", scriptInContainer)
	return args, nil
}

// envKeyAllowed reports whether a given env key is safe to
// forward into a container. Allowlist rules:
//
//   - Any `ENJU_*` key — these are Enju's own vocabulary
//     (ENJU_TASK_ID, ENJU_PROJECT_DIR, ENJU_RUN_DIR,
//     ENJU_TEMPLATE_DIR, ENJU_PARAM_*). Scripts depend on
//     them; the wrapper manufactures them.
//   - Any key present in taskEnv — the template author's
//     declared `env:` block. Opted-in by definition.
//
// Everything else is dropped. See buildDockerArgs for the
// rationale.
func envKeyAllowed(key string, taskEnv map[string]string) bool {
	if strings.HasPrefix(key, "ENJU_") {
		return true
	}
	if taskEnv == nil {
		return false
	}
	_, ok := taskEnv[key]
	return ok
}

// translatePath rewrites host path → container path when
// hostPath is under workDir. Returns (containerPath, true) on
// match, (hostPath, false) otherwise. Callers that MUST
// translate (the script path) check the bool; callers that
// TRY to translate (env values, where non-paths pass
// through) ignore it.
//
// Uses filepath.Clean on both sides so trailing slashes and
// "./" noise don't defeat the prefix match. Deliberately does
// NOT call filepath.EvalSymlinks — the wrapper's job is to
// respect what the handler gave it, not second-guess symlinks
// the handler intended the container to traverse.
func translatePath(hostPath, workDir, containerWorkDir string) (string, bool) {
	if hostPath == "" || workDir == "" {
		return hostPath, false
	}
	cleanWork := filepath.Clean(workDir)
	cleanHost := filepath.Clean(hostPath)
	if cleanHost == cleanWork {
		return containerWorkDir, true
	}
	prefix := cleanWork + string(filepath.Separator)
	if !strings.HasPrefix(cleanHost, prefix) {
		return hostPath, false
	}
	rel := cleanHost[len(prefix):]
	return containerWorkDir + "/" + filepath.ToSlash(rel), true
}

// checkContainerRuntime verifies the resolved container runtime's
// CLI is on PATH. Returns nil when spec.Container is empty (no
// runtime needed) or when the CLI is found. Otherwise returns a
// user-facing error pointing at the install URL for the specific
// runtime — friendlier than the generic exec.LookPath failure
// the wrapper would surface a second later.
//
// Runs before workspace open + shared-root symlink setup so the
// error arrives fast and no side effects happen on the way to
// rejection. The MCP handler bubbles Result.Error straight up
// as a tool-level error, which the LLM can surface to the human.
func checkContainerRuntime(spec Spec) error {
	if spec.Container == "" {
		return nil
	}
	runtime := resolveRuntime(spec)
	if _, err := exec.LookPath(runtime); err != nil {
		return fmt.Errorf(
			"task %q declares container %q but %q is not on PATH — %s, or remove the container: field to run the script on the host directly",
			spec.TaskID, spec.Container, runtime, installHintFor(runtime),
		)
	}
	return nil
}

// installHintFor returns the per-runtime install pointer used
// in the friendly "runtime not on PATH" error. Doesn't fail open
// on unknown runtimes — those wouldn't reach here in practice
// (validate.go rejects them at parse time) but the generic
// fallback keeps the message useful if a new runtime constant
// slips in without a hint update.
func installHintFor(runtime string) string {
	switch runtime {
	case RuntimeDocker:
		return "install Docker (https://docs.docker.com/get-docker/)"
	case RuntimeApptainer:
		return "install Apptainer (https://apptainer.org/docs/admin/main/installation.html)"
	default:
		return fmt.Sprintf("install %s", runtime)
	}
}

// buildExecCommand returns the *exec.Cmd the wrapper will Run()
// for a task. When spec.Container is empty, it's a direct
// host-side exec of spec.ScriptPath with the assembled `env`
// (legacy path). When spec.Container is set, it's the resolved
// runtime CLI (docker or apptainer) built via BuildContainerArgs
// — `env` gets funneled into the runtime's per-key env flags;
// the runtime CLI itself inherits os.Environ() so it can find
// DOCKER_HOST + ~/.docker/config (docker) or APPTAINER_CACHEDIR
// + auth files (apptainer).
//
// Extracted from Run() so the container branch has one
// obvious entry point that tests can cover without booting
// the whole git + commit pipeline.
func buildExecCommand(ctx context.Context, spec Spec, env []string, workDir, scriptCwd string) (*exec.Cmd, error) {
	if spec.Container == "" {
		cmd := exec.CommandContext(ctx, spec.ScriptPath)
		cmd.Dir = scriptCwd
		cmd.Env = env
		return cmd, nil
	}
	runtime := resolveRuntime(spec)
	args, err := BuildContainerArgs(runtime, spec, env, workDir, os.Getuid(), os.Getgid())
	if err != nil {
		return nil, fmt.Errorf("building %s args: %w", runtime, err)
	}
	cmd := exec.CommandContext(ctx, runtime, args...)
	// The runtime CLI reads config + credentials from the
	// invoking user's HOME. Inherit the wrapper's environment so
	// remote / auth setups work. The CONTAINER's environment
	// comes from per-key flags already baked into args — keeping
	// these two namespaces separate avoids leaking host paths
	// (PATH, HOME, ...) into the script's view unintentionally.
	cmd.Env = os.Environ()
	cmd.Dir = workDir
	return cmd, nil
}

// splitEnvEntry parses "KEY=VALUE" into its two parts. Returns
// (key, value, true) on success, ("", "", false) on a malformed
// entry (no `=` in the string). Equivalent to strings.Cut but
// avoids importing strings just for that single call at the
// caller level.
func splitEnvEntry(kv string) (string, string, bool) {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return "", "", false
	}
	return kv[:i], kv[i+1:], true
}
