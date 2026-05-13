// Package compute owns the subprocess-wrappable execution path for
// action:compute tasks. The logic that used to live inline inside
// mcpserver.handleExecuteTask — run script, capture stdout/stderr,
// build result/metadata/artifact files, commit + push — moved here
// so a separate `enju wrap-task` process can invoke it with the
// same contract.
//
// Why a subprocess wrapper at all: async compute tasks (phase 4)
// need the script + commit flow to outlive the MCP session. A
// detached subprocess is the simplest execution environment that
// gives us that, and keeping sync and async mode on the same code
// path (subprocess either way) means we only have one behavior to
// test + reason about. Phase 1 ships the wrapper binary and routes
// sync-mode through it — no user-visible change, just proving the
// contract.
package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// ResolveTaskScratchDir returns the canonical absolute path for a
// compute task's per-iteration scratch dir:
//
//	<projectRoot>/.enju/bots/<botUsername>/scratch/<task_id_safe>-iter-<iterSeq>
//
// task_id_safe replaces ':' (the task ID separator) with '-' so the
// path is filesystem-friendly. projectRoot is the user-facing project
// directory (`wf.ProjectRoot()` in production) — bot scratch lives
// under the project's hidden `.enju/` tree so removing the project
// removes the bot state alongside the rest of the runtime cache.
//
// The botUsername segment keeps two replicas of the same bot on the
// same machine from clobbering each other when their startup-sweeps
// run (replica-A's SweepStaleScratchAtStartup would otherwise nuke
// replica-B's live scratch dir if B was mid-task during A's startup).
//
// Empty projectRoot or empty botUsername returns "" — opts the caller
// out of scratch entirely (legacy/test paths). botUsername is a soft
// requirement (production callers always have one); the early-return
// here keeps tests with empty identity strings working.
func ResolveTaskScratchDir(projectRoot, botUsername, taskID string, iterSeq int) string {
	if projectRoot == "" || botUsername == "" || taskID == "" {
		return ""
	}
	safe := strings.ReplaceAll(taskID, ":", "-")
	if iterSeq < 1 {
		iterSeq = 1
	}
	return filepath.Join(projectRoot, ".enju", "bots", botUsername, "scratch",
		fmt.Sprintf("%s-iter-%d", safe, iterSeq))
}

// GitSubmitFailedPrefix is the leading text of the wrapper's
// legacy Result.Error string when the script ran fine but the
// post-script commit/push failed. New wrappers populate
// Result.GitError directly; old ones (in-flight async wrappers
// launched by an older binary) still write the prefixed
// message into Result.Error. The handler's classification path
// in mcpserver/execute_compute.go uses this prefix to detect
// the legacy shape and route it identically to GitError. Shared
// here so a wording change in wrapper.go can't silently demote
// the classification to compute_errored on the handler side.
const GitSubmitFailedPrefix = "git submit failed:"

// Spec is the handler→wrapper contract. Populated by the MCP
// handler, serialized as JSON into a temp file, and read by the
// wrapper subprocess. Every field the wrapper needs to produce a
// byte-identical commit to what the inline path produced must be
// represented here — nothing read from live state.
type Spec struct {
	TaskID string `json:"task_id"`

	ProjectID     int64  `json:"project_id"`
	RemoteURL     string `json:"remote_url,omitempty"`
	WorkspaceRoot string `json:"workspace_root"`
	ProjectName   string `json:"project_name,omitempty"`

	// Branch is the run branch (the integration target). The
	// commit's per-task topic branch is built FROM this base via
	// IterationBranch — see SubmitComputeTaskResult — and the
	// existing coord-side auto-merge integrates the topic into
	// this run branch on accept.
	Branch string `json:"branch"`

	// IterationBranch is the per-task topic branch the commit
	// lands on (e.g. "1-build/fetch_a/iter-1"). Composed by
	// coord at claim time via generateIterationBranch and surfaced
	// through TaskMeta. Required: empty value means coord didn't
	// populate it, which would only happen on a legacy claim row;
	// SubmitComputeTaskResult fails loudly in that case rather
	// than silently falling back to landing on the run branch
	// directly (which is the parallel-compute race we're fixing).
	IterationBranch string `json:"iteration_branch"`

	// Repo-relative result directory (e.g. "enju/runs/3/foo").
	// The handler already wrote context.json here before spawn;
	// the wrapper reads it back to include in the final commit.
	ResultDir string `json:"result_dir"`

	// BigfilesDir is the absolute path where this task's
	// declared-untracked outputs (writes_artifacts entries with
	// track:false) live. Resolved by the handler via
	// enjugit.ResolveBigfilesDir — either
	// <project>/.enju/bigfiles/<branch>/ for local-only mode, or
	// <ENJU_BIGFILES>/<slug>-<id>/<branch>/ when the citizen has
	// set the shared-mount env var.
	//
	// The wrapper:
	//   - Pre-creates this dir (mkdir -p) before exec'ing the
	//     script, so the script can write into it without
	//     "no such file or directory".
	//   - Exports ENJU_BIGFILES=<this path> in the script's env
	//     so recipes can write directly via $ENJU_BIGFILES/<path>.
	//   - Expands writes_artifacts(track:false) entries against
	//     this dir (instead of workDir), so the file's natural
	//     home is outside the worktree and git literally can't
	//     see it.
	//
	// Required when the task declares any track:false output —
	// the wrapper rejects the spec rather than silently routing
	// untracked writes back into the worktree.
	BigfilesDir string `json:"bigfiles_dir,omitempty"`

	// Absolute path to the script file the wrapper execs.
	// Resolution rules (template snapshot vs. inline YAML) live
	// in the caller; wrapper just runs what it's told.
	ScriptPath string `json:"script_path"`

	// Short label for error messages — the value from
	// `script:` in the YAML, not the absolute resolved path.
	ScriptLabel string `json:"script_label"`

	// WritesArtifacts is the full declaration the task carries —
	// tracked AND untracked, literal AND patterned. The wrapper
	// expands each entry against the working tree AFTER the
	// script exits (the script is what creates the files; we
	// can't enumerate before it runs). The expanded set is then
	// split:
	//
	//   - Tracked literal/glob/dir entries → read into the
	//     commit and reported via Enju-Artifacts trailer.
	//   - Untracked entries → stat-only; not committed (managed
	//     .gitignore block keeps them out); listed in
	//     Result.ArtifactsWritten so the coordinator records a
	//     tracked=false index row.
	//
	// Missing required entries fail the iteration loudly via
	// Result.MissingArtifacts; missing optional entries are
	// silent no-ops.
	//
	// JSON shape: yaml.WriteArtifacts.UnmarshalJSON accepts the
	// object form (`[{"path":"a","track":true,"optional":false}]`)
	// and a bare-string list (`["path/a", "path/b"]`) which
	// decodes as all-tracked — the bare form is what the
	// coordinator's writes_artifacts column held before the
	// track flag landed, so DB rows from that era still parse.
	//
	// Spec carries no separate `untracked_artifacts` field —
	// untracked entries live inside this list with `track:false`.
	// A spec file written by an out-of-tree tool that uses the
	// older split-fields shape would have its untracked entries
	// dropped (encoding/json silently ignores unknown keys). The
	// edge is "wrapper subprocess in flight while binary upgrades
	// from a pre-track-flag version" — recoverable by re-running
	// the task; not worth a back-compat shim.
	WritesArtifacts enjuYaml.WriteArtifacts `json:"writes_artifacts,omitempty"`

	// Commit identity.
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	Username    string `json:"username"`
	Model       string `json:"model,omitempty"`

	// Container, when set, routes script execution through a
	// container runtime instead of exec'ing the script directly
	// on the host. The image reference is passed verbatim to the
	// runtime CLI at invocation time; the wrapper handles
	// workspace bind-mounting, env-var translation from host to
	// container paths, and (where the runtime needs it) user
	// mapping so output files land owned by the invoking host
	// user.
	//
	// Empty string = no container (run script on host, as
	// before). Legacy spec files without this field decode
	// cleanly to "", so in-flight async wrappers launched by
	// older binaries keep working.
	Container string `json:"container,omitempty"`

	// ContainerRuntime picks the runtime CLI the wrapper invokes
	// when Container is set: "docker" (default when empty),
	// "apptainer". Singularity is rewritten to apptainer at
	// parse time, so this field never carries that value.
	// Ignored when Container is empty.
	ContainerRuntime string `json:"container_runtime,omitempty"`

	// ReadsArtifacts lists the declared input paths the task
	// expects to find under TaskScratchDir before its script
	// runs. Each path is read from ReadsSourceSHA via the
	// workflow's ReadFileAtCommit and written under
	// TaskScratchDir/<path>. Phase 2.2 contract.
	//
	// Empty / nil suppresses the materialization step entirely
	// — tasks with no declared reads (e.g. a fetch from an
	// external API) just see an empty scratch dir.
	//
	// Required when ReadsSourceSHA is set, and vice versa: the
	// pair travels together. A read-only spec without a SHA
	// (e.g. test fixtures) gets a sentinel-clean rejection.
	ReadsArtifacts []string `json:"reads_artifacts,omitempty"`

	// ReadsSourceSHA is the commit the wrapper reads each
	// ReadsArtifacts entry from. Production callers populate
	// this with the run-branch tip resolved at claim time —
	// once a task claims, its inputs are pinned to that SHA so
	// concurrent siblings don't race the materializer.
	ReadsSourceSHA string `json:"reads_source_sha,omitempty"`

	// TaskScratchDir is an absolute path the wrapper creates
	// before exec'ing the script and removes when Run returns
	// (success or failure). Empty string suppresses the lifecycle
	// — legacy spec files / tests that haven't opted in keep
	// running with the script's CWD == workDir as before. The
	// legacy path is back-compat only; production callers via
	// service/execute.go always populate this field.
	//
	// Cleanup-failure recovery: os.RemoveAll on exit is
	// best-effort. If it fails (zombie subprocess holding a file
	// open, EBUSY on a fuse mount, permissions changed mid-run),
	// the dir leaks past this Run() call. The next iteration of
	// the SAME task at the SAME iter_seq could collide with the
	// leak — coord prevents that in practice (each iter_seq is
	// claimed once), but the broader safety net is
	// SweepStaleScratchAtStartup at the next bot daemon start,
	// which nukes any survivor under this bot's scratch subtree.
	//
	// Phase 2.1 added the lifecycle. Phase 2.3 flipped the
	// script's CWD to scratch for direct-exec mode. Phase 2.5
	// extended that to container mode (docker bind-mounts the
	// host scratch at ContainerScratchDir).
	//
	// Naming convention is the caller's choice; the wrapper just
	// honors what the spec carries. Production callers compose
	// <project_root>/.enju/bots/<bot>/scratch/<task-slug>-iter-<n>/
	// via compute.ResolveTaskScratchDir — project-scoped, with the
	// bot subsegment keeping replicas of the same bot from
	// colliding on the same machine.
	TaskScratchDir string `json:"task_scratch_dir,omitempty"`

	// SnapshotDir is the absolute path on the host to the run's
	// frozen template-snapshot directory. When set, it becomes the
	// script's working directory (so `./scripts/helper.sh` and
	// sibling-relative paths resolve naturally against the full
	// template tree). The snapshot is treated as read-only: inside
	// a container it's bind-mounted with :ro; on host exec we rely
	// on the chmod 0444/0555 applied at snapshot creation time to
	// block accidental writes. Scratch remains the writable channel
	// for logs and intermediate state, exposed via $ENJU_SCRATCH.
	//
	// Empty for legacy specs that haven't been migrated; in that
	// case ScriptCwdFor falls back to scratch (or workDir).
	SnapshotDir string `json:"snapshot_dir,omitempty"`

	// Env is the task's declared env: block — keys + values
	// the template author opted into as script inputs. Used
	// only in container mode, as the allowlist alongside
	// ENJU_* for which host-env entries are legitimate script
	// inputs vs. accidental leaks (credentials, PATH, HOME,
	// etc.). Direct-exec mode ignores this field — the flat
	// env slice already carries the same values, and the host
	// process inherits its own env naturally.
	Env map[string]string `json:"env,omitempty"`
}

// Result is the wrapper→handler contract. Returned as JSON via the
// --output file. Handler reads it after the subprocess exits and
// decides how to report to the coordinator.
type Result struct {
	// Script exit code. 0 means success (commit was made).
	// Non-zero means the script failed; no commit.
	ExitCode int `json:"exit_code"`

	// Commit SHA produced on the success path. Empty when
	// ExitCode != 0 or a wrapper-level error occurred.
	CommitSHA string `json:"commit_sha,omitempty"`

	// Wall-clock time the script ran, in milliseconds.
	ElapsedMS int64 `json:"elapsed_ms"`

	// Script stdout (becomes result.md body). Empty-string
	// inputs get replaced by a "(script produced no output)"
	// sentinel before commit; the handler's response formatter
	// already counts bytes from this field.
	Content string `json:"content,omitempty"`

	// Script stderr. Surfaced in failure messages. Truncated
	// server-side by the handler if long.
	Stderr string `json:"stderr,omitempty"`

	// Artifacts the script actually produced and the wrapper
	// successfully included in the commit.
	ArtifactsWritten []string `json:"artifacts_written,omitempty"`

	// Declared-but-missing artifacts. Not a failure; the
	// handler surfaces these as a soft warning.
	MissingArtifacts []string `json:"missing_artifacts,omitempty"`

	// Absolute path to the script.log on disk. Always written
	// (success or failure) so a human can debug from local disk
	// even before the coordinator catches up.
	ScriptLogPath string `json:"script_log_path,omitempty"`

	// Wrapper-level failure (spec parse, project open, script
	// not found, container runtime missing). Distinct from
	// ExitCode — a non-empty Error means the wrapper couldn't
	// even attempt the script. The handler bubbles this up as
	// a tool error.
	Error string `json:"error,omitempty"`

	// GitError is set when the script ran successfully (exit 0,
	// produced output) but the post-script commit/push failed —
	// e.g. "object not found" on a freshly-added remote, push
	// rejected and rebase failed, etc. Distinct from Error so
	// callers can route a script-passed-but-git-failed task to
	// a clear "fix the git state, the work product is still on
	// disk" recovery path. Old wrappers didn't have this field;
	// when GitError is empty and Error contains "git submit
	// failed:" the new handler still classifies correctly via
	// fallback heuristic in execute_compute.go.
	GitError string `json:"git_error,omitempty"`
}

// Run executes a compute task's script per the given Spec and, on
// exit 0, commits the result + declared artifacts to the project's
// local clone. Returns a Result in all cases. env is the full
// environment handed to the script (OS env + ENJU_* vars + task
// env), pre-assembled by the caller.
//
// wf is the project's Workflow handle. Sync callers pass the
// already-cached one (`s.enjugit.ForProject(...)`) so concurrent
// compute goroutines share the in-process sync.Mutex instead of
// falling through to the slower cross-process flock. Pass nil
// from the wrap-task subprocess (separate process), and Run will
// construct its own from spec.WorkspaceRoot.
//
// ctx cancellation propagates to the script via exec.CommandContext.
// Wrapper-level errors are returned via Result.Error; script exit
// codes land in Result.ExitCode.
func Run(ctx context.Context, wf *enjugit.Workflow, spec Spec, env []string, logger *slog.Logger) Result {
	res := Result{}

	// Scratch-dir lifecycle (Phase 2.1). Mkdir up front, defer
	// rm. Early-return failure paths still get a clean wipe;
	// only the SUBMIT-failed path (script ran fine, post-script
	// commit/push failed — res.GitError set) preserves scratch
	// so the operator's retry can pick up the script's outputs
	// from disk. That's TP53 Bug 2: pre-fix, the wrapper's
	// outputs survived only inside the failed submit's
	// .wrap-result.done.json on disk; the scratch contents that
	// the next retry needed were already gone.
	//
	// Empty TaskScratchDir is a no-op: legacy spec files predate
	// this field and the JSON omits it; their behavior is
	// unchanged.
	if spec.TaskScratchDir != "" {
		if err := os.MkdirAll(spec.TaskScratchDir, 0o755); err != nil {
			res.Error = fmt.Sprintf("creating task scratch dir %q: %v",
				spec.TaskScratchDir, err)
			return res
		}
		defer func() {
			if res.GitError != "" {
				// Submit-failed path. Outputs are still in
				// scratch; the operator's retry can re-claim
				// the task and re-submit without re-running
				// the script. Logged at Warn so it surfaces in
				// daemon stdout/log.
				if logger != nil {
					logger.Warn("submit failed; preserving scratch for retry",
						"path", spec.TaskScratchDir,
						"task_id", spec.TaskID,
						"git_error", res.GitError)
				}
				return
			}
			if err := os.RemoveAll(spec.TaskScratchDir); err != nil && logger != nil {
				logger.Warn("scratch dir cleanup failed",
					"path", spec.TaskScratchDir, "error", err)
			}
		}()
	}

	if spec.ScriptPath == "" {
		res.Error = "spec.script_path is required"
		return res
	}
	if _, err := os.Stat(spec.ScriptPath); os.IsNotExist(err) {
		res.Error = fmt.Sprintf("script %q not found at %s", spec.ScriptLabel, spec.ScriptPath)
		return res
	}
	// Container runtime presence check. When spec.Container is
	// set this validates docker is on PATH up front so a missing
	// runtime fails fast (before workspace open + shared-root
	// symlink setup) with a user-actionable error rather than the
	// cryptic `exec: "docker": executable file not found in $PATH`
	// that exec.Command would surface a second later.
	if err := checkContainerRuntime(spec); err != nil {
		res.Error = err.Error()
		return res
	}

	// Subprocess fallback: wrap-task is a separate process and
	// cannot inherit the parent fat-client's Workflow. Open one
	// from spec — flock honors cross-process serialization.
	if wf == nil {
		ws, err := enjugit.NewWorkspace(spec.WorkspaceRoot,
			enjugit.NewProductionConventions(),
			enjugit.WithLogger(logger))
		if err != nil {
			res.Error = fmt.Sprintf("opening workspace %q: %v", spec.WorkspaceRoot, err)
			return res
		}
		wf, err = ws.ForProject(spec.ProjectID, spec.RemoteURL, spec.ProjectName)
		if err != nil {
			res.Error = fmt.Sprintf("opening project: %v", err)
			return res
		}
	}

	workDir := wf.WorkDir()

	// Phase 2.3 — pick the script's working directory. With a
	// scratch dir set + direct-exec mode, scripts run isolated
	// in scratch; legacy / container paths stay on workDir.
	scriptCwd := ScriptCwdFor(spec, workDir)

	// Phase 2.2 — materialize declared reads_artifacts into the
	// task's scratch dir BEFORE the script runs. The script's
	// CWD is still workDir at this point (Phase 2.3 flips it),
	// so ENJU_TASK_DIR is the only way for the script to find
	// these files; most scripts won't yet, and that's fine —
	// 2.2 lays the substrate, 2.3 cuts the worktree dependency.
	//
	// Skipped when scratch is unset (legacy spec) or no reads
	// were declared (e.g. external-fetch task). A non-empty
	// reads list with no source SHA is a wiring bug — the pair
	// travels together — so we reject loudly.
	if spec.TaskScratchDir != "" && len(spec.ReadsArtifacts) > 0 {
		if spec.ReadsSourceSHA == "" {
			res.Error = fmt.Sprintf("task %s: spec.reads_artifacts present but reads_source_sha empty (caller bug — service.execute should populate both as a pair)", spec.TaskID)
			return res
		}
		missing, err := MaterializeReads(spec.TaskScratchDir, spec.ReadsSourceSHA,
			spec.ReadsArtifacts, wf.ReadFileAtCommit)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		// Inputs absent at the source commit are a soft warning
		// (matches existing MissingArtifacts semantics for
		// declared-but-not-produced outputs). The handler maps
		// res.MissingArtifacts → "warn" in the response.
		res.MissingArtifacts = append(res.MissingArtifacts, missing...)
	}

	// Pre-exec: materialize shared-root symlinks for every
	// declared untracked LITERAL path. When ENJU_SHARED_ROOT is
	// set, this swaps the workspace path for a symlink pointing
	// at shared storage BEFORE the script runs — the script
	// then writes THROUGH the symlink and the bytes land on the
	// shared mount where downstream citizens with the same
	// mount can see them. When ENJU_SHARED_ROOT is unset, the
	// helper is a noop and untracked writes stay local.
	//
	// Pattern entries (globs, directories) skip this step
	// because we don't yet know what filenames the script will
	// produce. The shared-root layer is purely a literal-path
	// optimization; pattern-declared untracked outputs land
	// locally. Operators who need shared storage for pattern
	// outputs should pre-create the directory as a symlink
	// themselves, or declare each expected literal path.
	for _, decl := range spec.WritesArtifacts {
		if decl.Track {
			continue
		}
		if decl.Path == "" || enjuYaml.IsGlob(decl.Path) || enjuYaml.IsDir(decl.Path) {
			continue
		}
		if err := enjugit.EnsureSharedSymlink(enjugit.ArtifactPath(decl.Path), workDir,
			spec.ProjectID, spec.ProjectName, spec.Branch, decl.Path); err != nil {
			logger.Warn("shared-root symlink setup failed",
				"path", decl.Path, "error", err)
			// Don't fail the whole task — if the mount is
			// unavailable, the script can still write the
			// local path and downstream consumers on the
			// same workspace will read it.
		}
	}

	// Run the script. Three output streams:
	//   - stdout buffer → result.md (contract-defined answer)
	//   - stderr buffer → failure-reason body on non-zero exit
	//   - scriptLog combined → committed with the result on success,
	//                          kept on local disk on failure
	//
	// Two execution modes:
	//   - Direct (spec.Container == ""): exec the script on the
	//     host with the assembled `env`. Legacy path, unchanged.
	//   - Containerized (spec.Container != ""): exec `docker run`
	//     with the script inside. `env` is translated into `-e`
	//     flags on the docker argv (path prefixes rewritten
	//     host → /workspace); the docker CLI itself inherits
	//     os.Environ() so it can find DOCKER_HOST, the user's
	//     auth config, etc.
	startTime := time.Now()
	cmd, cmdErr := buildExecCommand(ctx, spec, env, workDir, scriptCwd)
	if cmdErr != nil {
		res.Error = cmdErr.Error()
		return res
	}
	var stdout, stderr, scriptLog bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, &scriptLog)
	cmd.Stderr = io.MultiWriter(&stderr, &scriptLog)

	execErr := cmd.Run()
	elapsed := time.Since(startTime).Round(time.Millisecond)
	res.ElapsedMS = elapsed.Milliseconds()

	// Always write script.log — makes failure debugging work
	// from local disk even if commit never happens.
	//
	// Phase 2.6 — worktree isolation. Production callers populate
	// TaskScratchDir, so the log lives in scratch (and on success
	// gets committed via the in-memory FileWrite below; no on-disk
	// copy survives the defer-cleanup since the commit is the
	// canonical record). Legacy callers without scratch keep the
	// old workDir/<resultDir>/script.log placement so existing tests
	// and any out-of-tree consumers don't break.
	//
	// Why this matters: writing under workDir/<resultDir> leaves the
	// log as an UNTRACKED file in the worktree (plumbing-commit
	// adds it to the tree but doesn't `git add`). A later non-FF
	// MergeAcceptedTopic does Checkout(target) which then refuses
	// "untracked files would be overwritten" — exactly the
	// parallel-merge stall the load test surfaced.
	//
	// On exec failure (script non-zero or wrapper-level), the log
	// gets copied OUT of scratch to a persistent per-task path
	// before the defer cleanup wipes scratch — see persistFailedLog
	// at the failure branch below.
	scriptLogPath := filepath.Join(workDir, spec.ResultDir, "script.log")
	if spec.TaskScratchDir != "" {
		scriptLogPath = filepath.Join(spec.TaskScratchDir, "script.log")
	}
	if err := os.MkdirAll(filepath.Dir(scriptLogPath), 0755); err == nil {
		_ = os.WriteFile(scriptLogPath, scriptLog.Bytes(), 0644)
		res.ScriptLogPath = scriptLogPath
	}

	if execErr != nil {
		// Phase 2.6 — when the log lives in scratch, persist a copy
		// to <workspaceRoot>/logs/ before the defer wipes scratch.
		// Without this, post-mortem debugging would have nothing
		// on disk: the commit never happens (no git copy) AND
		// scratch goes away. Best-effort; on copy failure the
		// in-memory Stderr surfaced via Result.Stderr is the
		// last-resort signal for the caller.
		if persisted := persistFailedLog(spec, scriptLog.Bytes()); persisted != "" {
			res.ScriptLogPath = persisted
		}
		if exitErr, ok := execErr.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			res.Stderr = stderr.String()
			return res
		}
		res.Error = fmt.Sprintf("failed to run script: %v", execErr)
		return res
	}

	// Exit 0 — build the commit.
	content := stdout.String()
	if content == "" {
		content = "(script produced no output)"
	}
	res.Content = content

	files := []enjugit.FileWrite{
		{
			RepoRelPath: filepath.Join(spec.ResultDir, "result.md"),
			Content:     []byte(content),
		},
	}
	// context.json was written by the handler before spawn; read
	// it back so it lands in the same commit as the result. A
	// reader can then reconstruct "what was this task told?" from
	// the commit alone. Phase 2.6 — handler writes context.json
	// to scratch (production), or workDir/<resultDir> (legacy
	// callers without scratch). Match the placement on read.
	contextPath := filepath.Join(workDir, spec.ResultDir, "context.json")
	if spec.TaskScratchDir != "" {
		contextPath = filepath.Join(spec.TaskScratchDir, "context.json")
	}
	if ctxBytes, cerr := os.ReadFile(contextPath); cerr == nil {
		files = append(files, enjugit.FileWrite{
			RepoRelPath: filepath.Join(spec.ResultDir, "context.json"),
			Content:     ctxBytes,
		})
	}
	// script.log — full stdout+stderr transcript on success.
	files = append(files, enjugit.FileWrite{
		RepoRelPath: filepath.Join(spec.ResultDir, "script.log"),
		Content:     scriptLog.Bytes(),
	})
	metadata := map[string]interface{}{
		"task_id":     spec.TaskID,
		"model":       spec.Model,
		"result_type": "text",
		"action":      "compute",
		"script":      spec.ScriptLabel,
		"exit_code":   0,
		"elapsed_ms":  elapsed.Milliseconds(),
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	metaBytes, _ := json.MarshalIndent(metadata, "", "  ")
	files = append(files, enjugit.FileWrite{
		RepoRelPath: filepath.Join(spec.ResultDir, "metadata.json"),
		Content:     metaBytes,
	})

	// Pick up declared artifacts. The expansion step walks the
	// working tree and resolves every declared pattern (literal,
	// glob, directory) to concrete files; required-but-missing
	// entries land in `missing` so we surface them loudly, while
	// optional missing entries fold silently.
	//
	// Two flavors of expanded entries, handled differently:
	//   - Tracked: read the file, include it in the commit,
	//     report the path to the coordinator.
	//   - Untracked: stat-only (already done by ExpandAgainstWorkdir);
	//     report the path; do NOT stage it. The file stays on disk
	//     (kept out of git by the managed .gitignore block) and
	//     the coordinator upserts an index row with tracked=false +
	//     empty commit_sha, which downstream tasks use to verify
	//     presence at claim time.
	//
	// committedPaths is the tracked subset that actually landed
	// in the commit. ArtifactPaths on the submit request carries
	// this list so the commit message body and the Enju-Artifacts
	// trailer accurately reflect "what's in this commit" —
	// untracked paths are never mentioned at the git layer.
	// Tracked entries are walked against the worktree (where the
	// script wrote them, and where the commit will pick them up).
	// Untracked entries (track:false) are walked against the
	// bigfiles dir — the per-branch sibling of the worktree where
	// big data lives. Splitting the declaration list by Track
	// flag and running ExpandAgainstWorkdir against the right
	// root gives each kind its own home with no .gitignore trick.
	trackedDecls, untrackedDecls := splitArtifactsByTrack(spec.WritesArtifacts)

	// Tracked writes_artifacts expand against scratch when it's
	// set, falling back to the script's CWD otherwise. With the
	// scratch-as-CWD shape, scriptCwd IS the scratch dir, so
	// outputDir lines up naturally — scripts that write
	// `data/foo.txt` (relative) land where pickup looks. The
	// fallback covers legacy specs that didn't populate scratch.
	outputDir := scriptCwd
	if spec.TaskScratchDir != "" {
		outputDir = spec.TaskScratchDir
	}
	tracked, missingT, expandErr := trackedDecls.ExpandAgainstWorkdir(outputDir)
	if expandErr != nil {
		res.Error = fmt.Sprintf("expanding writes_artifacts: %v", expandErr)
		return res
	}
	res.MissingArtifacts = append(res.MissingArtifacts, missingT...)

	// Untracked entries expand against the bigfiles dir — the
	// per-branch sibling of the worktree. BigfilesDir is required
	// for any task with track:false declarations; an empty value
	// means the handler didn't populate it, which is a wiring
	// bug, not a recipe shape we should silently tolerate.
	if len(untrackedDecls) > 0 && spec.BigfilesDir == "" {
		res.Error = "BigfilesDir not set on Spec but task declares track:false outputs"
		return res
	}
	untracked, missingU, expandErr := untrackedDecls.ExpandAgainstWorkdir(spec.BigfilesDir)
	if expandErr != nil {
		res.Error = fmt.Sprintf("expanding writes_artifacts (untracked): %v", expandErr)
		return res
	}
	res.MissingArtifacts = append(res.MissingArtifacts, missingU...)

	// Required (non-optional) writes_artifacts that produced
	// nothing fail the iteration loudly. ExpandAgainstWorkdir
	// already filters Optional entries out of the missing list,
	// so anything that landed there is by definition contractually
	// required — committing without it would be silent acceptance
	// of a broken task whose downstream consumers can't make
	// progress (see the compute-load-test cascade-stall: a
	// non-parametric script wrote raw_a.txt for every sibling, so
	// fetch_data_b/c "succeeded" with empty artifacts and
	// process_b/c sat PENDING forever on a dep that would never
	// land in the artifact index).
	//
	// We emit a single Error covering the union of missing tracked
	// + untracked, then return BEFORE the commit step. ExitCode
	// stays 0 (the script itself ran fine; the contract violation
	// is the wrapper's call) — the handler's classification path
	// already routes Result.Error as a wrapper-level failure
	// distinct from script-non-zero, surfacing this cleanly to
	// the operator.
	if total := len(missingT) + len(missingU); total > 0 {
		all := append(append([]string{}, missingT...), missingU...)
		res.Error = fmt.Sprintf("required writes_artifacts not produced: %v", all)
		return res
	}

	var committedPaths []string
	for _, e := range tracked {
		// Read back from outputDir, which is where the script
		// wrote its outputs and where ExpandAgainstWorkdir
		// successfully resolved each path moments ago. Under
		// the scratch-as-CWD shape, outputDir == scriptCwd
		// == scratch — one path, no fan-out.
		full := filepath.Join(outputDir, enjugit.ArtifactPath(e.Path))
		body, rerr := os.ReadFile(full)
		if rerr != nil {
			// Expansion already stat'd this file moments
			// ago, so a read failure here is a transient
			// IO problem (or a script that deleted the
			// file between expand and read). Surface as
			// missing for consistency with the legacy
			// "soft warning" semantics.
			res.MissingArtifacts = append(res.MissingArtifacts, e.Path)
			continue
		}
		files = append(files, enjugit.FileWrite{
			RepoRelPath: enjugit.ArtifactPath(e.Path),
			Content:     body,
		})
		committedPaths = append(committedPaths, e.Path)
		res.ArtifactsWritten = append(res.ArtifactsWritten, e.Path)
	}
	for _, e := range untracked {
		// Untracked: don't stage; just report the path. File
		// stays at <untrackedRoot>/<path>; coord upserts an
		// index row with tracked=false + empty commit_sha,
		// downstream tasks resolve via the same root.
		res.ArtifactsWritten = append(res.ArtifactsWritten, e.Path)
	}

	// Trailers carry the machine-parseable task-complete
	// signal the reconcile endpoint + fetch-path scanner key
	// on. Exit is always set to 0 here (the failure branch
	// above returned early); duration rounds seconds so the
	// trailer stays compact. Artifacts populate from
	// ArtifactPaths by default inside SubmitTaskResult.
	// Deliberately NOT setting SubmitRequest.StateDir /
	// ProjectID here: compute.Run serves both the sync inline
	// path (where the handler reports via /tasks/:id/result)
	// and the async detached path (where the submitter's
	// later scanner sweep MUST see this trailer to post
	// /tasks/reconcile). Auto-advancing the cursor from here
	// would starve the scanner on the async path. Scanner
	// idempotency handles the sync case — it re-posts, the
	// coordinator no-ops on the already-terminal task.
	// Untracked artifacts that the script actually produced
	// (exist on disk) land in res.ArtifactsWritten alongside
	// tracked ones; the set-subtract gives us just the
	// untracked subset for the trailer. Without the trailer,
	// the async reconcile path would never surface these to
	// the coordinator — sync mode POSTs the union directly
	// via /tasks/:id/result, but async hosts rely entirely
	// on the commit's trailer for the reconcile payload.
	committedSet := make(map[string]struct{}, len(committedPaths))
	for _, p := range committedPaths {
		committedSet[p] = struct{}{}
	}
	var untrackedProduced []string
	for _, p := range res.ArtifactsWritten {
		if _, ok := committedSet[p]; !ok {
			untrackedProduced = append(untrackedProduced, p)
		}
	}

	// The compute task commits directly to the run branch, so
	// BranchOverride bypasses topic-branch composition. Compute-
	// specific trailers (Enju-Exit, Enju-Duration-Seconds,
	// Enju-Untracked-Artifacts) flow through CustomTrailers —
	// Workflow doesn't model them as first-class request fields.
	customTrailers := map[string]string{
		enjugit.TrailerExit: "0",
	}
	if durSec := int(elapsed.Round(time.Second) / time.Second); durSec > 0 {
		customTrailers[enjugit.TrailerDurationSeconds] = fmt.Sprintf("%d", durSec)
	}
	if len(untrackedProduced) > 0 {
		customTrailers[enjugit.TrailerUntrackedArtifacts] = strings.Join(untrackedProduced, ", ")
	}

	// Compute submits via the no-checkout plumbing path: each
	// parallel goroutine builds a commit on its OWN topic branch
	// (spec.IterationBranch) without touching HEAD/.git/index/
	// working-tree, then pushes that topic branch. Coord's
	// existing acceptedMergeForTask + fat-client applyAcceptedMerges
	// handles the integration: on accept, the topic branch is
	// FF-merged into spec.Branch (the run branch), same flow LLM
	// tasks already use.
	//
	// LLM/bot tasks keep using SubmitTaskResult (porcelain) —
	// they run in their own per-bot clone and need the worktree
	// flow for tools that exec git commands inside the script.
	submitRes, err := wf.SubmitComputeTaskResult(enjugit.SubmitRequest{
		TaskID:         spec.TaskID,
		BranchOverride: spec.IterationBranch,
		RunBranch:      spec.Branch,
		Files:          files,
		// ArtifactPaths feeds the commit message + Enju-Artifacts
		// trailer — these describe what's *in* this commit, so only
		// tracked artifacts belong. Untracked paths go via
		// CustomTrailers["Enju-Untracked-Artifacts"] so the async
		// reconcile path can see them too.
		ArtifactPaths:  committedPaths,
		Citizen:        enjugit.Identity{Name: spec.AuthorName, Email: spec.AuthorEmail},
		ModelName:      spec.Model,
		CustomTrailers: customTrailers,
	})
	if err != nil {
		// Script ran fine; the failure is at the git layer
		// (commit retry exhausted, push rejected, rebase
		// failed, "object not found" on a freshly-added
		// remote). Route via GitError so the caller can
		// distinguish this from a wrapper-level failure
		// (script not found, project open failed) and surface
		// "git op failed, work product is on disk" to the
		// user instead of "compute task errored."
		res.GitError = fmt.Sprintf("%s %v", GitSubmitFailedPrefix, err)
		return res
	}
	res.CommitSHA = submitRes.CommitSHA
	return res
}

// splitArtifactsByTrack partitions writes_artifacts declarations
// into the tracked / untracked subsets so each can be expanded
// against its own filesystem root: tracked entries live in the
// worktree (and land in the commit); untracked entries live in
// the bigfiles dir (sibling of the worktree).
//
// Preserves declaration order within each subset so error
// messages and the per-decl Optional flag stay stable.
func splitArtifactsByTrack(decls enjuYaml.WriteArtifacts) (tracked, untracked enjuYaml.WriteArtifacts) {
	for _, d := range decls {
		if d.Track {
			tracked = append(tracked, d)
		} else {
			untracked = append(untracked, d)
		}
	}
	return tracked, untracked
}

// ReadSpec loads a Spec from a JSON file. Used by the
// `enju wrap-task` subcommand.
func ReadSpec(path string) (Spec, error) {
	var s Spec
	data, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("reading spec %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parsing spec %q: %w", path, err)
	}
	return s, nil
}

// WriteResult serializes a Result to a JSON file. Used by the
// `enju wrap-task` subcommand to hand the outcome back to the
// calling MCP handler.
func WriteResult(path string, r Result) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing result %q: %w", path, err)
	}
	return nil
}

// WrapMain is the entry point for the `enju wrap-task` subcommand
// — shared between cmd/enju/main.go and the test binary so both
// can dispatch to the same wrapper logic. Returns the process
// exit code (0 on success, non-zero on wrapper-level failure).
//
// Caller is expected to have already peeled off the "wrap-task"
// token; args is the flag tail ("--spec …" "--output …").
//
// args are the flag tokens ("--spec VAL --output VAL"). stderr
// is used for diagnostic logging; it's passed in so callers can
// route it (tests can swallow, main writes to os.Stderr).
func WrapMain(args []string, stderr io.Writer) int {
	var specPath, outputPath string
	// Hand-parsed to avoid a flag dependency — two flags,
	// simple "--key=val" or "--key val" pairs.
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--spec":
			i++
			if i < len(args) {
				specPath = args[i]
			}
		case strings.HasPrefix(a, "--spec="):
			specPath = strings.TrimPrefix(a, "--spec=")
		case a == "--output":
			i++
			if i < len(args) {
				outputPath = args[i]
			}
		case strings.HasPrefix(a, "--output="):
			outputPath = strings.TrimPrefix(a, "--output=")
		default:
			fmt.Fprintf(stderr, "wrap-task: unknown arg %q\n", a)
			return 2
		}
		i++
	}

	if specPath == "" || outputPath == "" {
		fmt.Fprintln(stderr, "wrap-task: --spec and --output are required")
		return 2
	}

	spec, err := ReadSpec(specPath)
	if err != nil {
		_ = WriteResult(outputPath, Result{Error: err.Error()})
		fmt.Fprintln(stderr, "wrap-task:", err)
		return 1
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// Subprocess: no parent Workflow to share — Run opens its own
	// from spec.WorkspaceRoot. Cross-process flock keeps it safe
	// against the parent's concurrent ops.
	res := Run(context.Background(), nil, spec, os.Environ(), logger)

	if err := WriteResult(outputPath, res); err != nil {
		fmt.Fprintln(stderr, "wrap-task: writing result:", err)
		return 1
	}
	return 0
}
