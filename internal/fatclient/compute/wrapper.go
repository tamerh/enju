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

	"github.com/enju-ai/enju/internal/common/gitignore"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/project"
)

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

	// Branch the commit MUST land on. Passed through to
	// Project.SubmitTaskResult, which handles checkout + push.
	Branch string `json:"branch"`

	// Repo-relative result directory (e.g. "enju/runs/3/foo").
	// The handler already wrote context.json here before spawn;
	// the wrapper reads it back to include in the final commit.
	ResultDir string `json:"result_dir"`

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
	// container runtime (Docker in v1) instead of exec'ing the
	// script directly on the host. The image reference is
	// passed verbatim to the runtime CLI at invocation time;
	// the wrapper handles workspace bind-mounting, env-var
	// translation from host to container paths, and user
	// mapping so output files land owned by the invoking
	// host user.
	//
	// Empty string = no container (run script on host, as
	// before). Legacy spec files without this field decode
	// cleanly to "", so in-flight async wrappers launched by
	// older binaries keep working.
	Container string `json:"container,omitempty"`

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
// ctx cancellation propagates to the script via exec.CommandContext.
// Wrapper-level errors are returned via Result.Error; script exit
// codes land in Result.ExitCode.
func Run(ctx context.Context, spec Spec, env []string, logger *slog.Logger) Result {
	res := Result{}

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

	// Open the project clone via the fat-client workspace layer
	// so the cross-process flock is honored — the MCP handler and
	// this wrapper run in distinct processes and MUST NOT race on
	// .git/index.lock.
	ws, err := project.NewOpener(spec.WorkspaceRoot, logger)
	if err != nil {
		res.Error = fmt.Sprintf("opening workspace %q: %v", spec.WorkspaceRoot, err)
		return res
	}
	proj, err := ws.ForProject(spec.ProjectID, spec.RemoteURL, spec.ProjectName)
	if err != nil {
		res.Error = fmt.Sprintf("opening project: %v", err)
		return res
	}

	workDir := proj.WorkDir()

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
	cmd, cmdErr := buildExecCommand(ctx, spec, env, workDir)
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
	scriptLogPath := filepath.Join(workDir, spec.ResultDir, "script.log")
	if err := os.MkdirAll(filepath.Dir(scriptLogPath), 0755); err == nil {
		_ = os.WriteFile(scriptLogPath, scriptLog.Bytes(), 0644)
		res.ScriptLogPath = scriptLogPath
	}

	if execErr != nil {
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
	// the commit alone.
	contextPath := filepath.Join(workDir, spec.ResultDir, "context.json")
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
	expanded, missing, expandErr := spec.WritesArtifacts.ExpandAgainstWorkdir(workDir)
	if expandErr != nil {
		res.Error = fmt.Sprintf("expanding writes_artifacts: %v", expandErr)
		return res
	}
	res.MissingArtifacts = append(res.MissingArtifacts, missing...)

	var committedPaths []string
	for _, e := range expanded {
		if e.Track {
			full := filepath.Join(workDir, enjugit.ArtifactPath(e.Path))
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
			continue
		}
		// Untracked: don't stage; just report the path.
		res.ArtifactsWritten = append(res.ArtifactsWritten, e.Path)
	}

	// Maintain the Enju-managed .gitignore block so untracked
	// paths can never slip into a future commit via some
	// unrelated `git add`/`stash` path. We write the ORIGINAL
	// declarations (patterns and all) to gitignore — gitignore
	// understands globs and directory prefixes natively, so the
	// pattern strings work as-is and a `out/*.bam` declaration
	// covers any future bam in that directory automatically. The
	// helper is a no-op when the block already contains every
	// declared pattern, so re-running the same task doesn't
	// churn .gitignore with no-op commits.
	var untrackedDecls []string
	for _, decl := range spec.WritesArtifacts {
		if !decl.Track && decl.Path != "" {
			untrackedDecls = append(untrackedDecls, decl.Path)
		}
	}
	if len(untrackedDecls) > 0 {
		gitignorePath := filepath.Join(workDir, ".gitignore")
		existing, _ := os.ReadFile(gitignorePath) // missing file → nil (fine)
		updated, changed := gitignore.UpdateManagedBlock(existing, untrackedDecls)
		if changed {
			files = append(files, enjugit.FileWrite{
				RepoRelPath: ".gitignore",
				Content:     updated,
			})
		}
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

	// Build a Workflow over project.Clone's *git.Clone (same handle
	// → no #381 dual-handle drift). The compute task commits
	// directly to the run branch, so BranchOverride bypasses
	// topic-branch composition. Compute-specific trailers
	// (Enju-Exit, Enju-Duration-Seconds, Enju-Untracked-Artifacts)
	// flow through CustomTrailers — Workflow doesn't model them as
	// first-class request fields.
	wf := enjugit.WorkflowFromShared(proj.GitClone(), spec.ProjectID, spec.Branch, logger)
	customTrailers := map[string]string{
		enjugit.TrailerExit: "0",
	}
	if durSec := int(elapsed.Round(time.Second) / time.Second); durSec > 0 {
		customTrailers[enjugit.TrailerDurationSeconds] = fmt.Sprintf("%d", durSec)
	}
	if len(untrackedProduced) > 0 {
		customTrailers[enjugit.TrailerUntrackedArtifacts] = strings.Join(untrackedProduced, ", ")
	}

	proj.Lock()
	submitRes, err := wf.SubmitTaskResult(enjugit.SubmitRequest{
		TaskID:         spec.TaskID,
		BranchOverride: spec.Branch,
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
	proj.Unlock()
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
	res := Run(context.Background(), spec, os.Environ(), logger)

	if err := WriteResult(outputPath, res); err != nil {
		fmt.Fprintln(stderr, "wrap-task: writing result:", err)
		return 1
	}
	return 0
}
