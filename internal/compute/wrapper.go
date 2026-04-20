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

	"github.com/enju-ai/enju/internal/mcpgit"
)

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

	// Repo-relative result directory (e.g. ".enju/runs/3/foo").
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

	// Paths the script is expected to write under the
	// project's artifact namespace. Wrapper reads each after
	// the script exits; missing ones land in
	// Result.MissingArtifacts as soft warnings.
	WritesArtifacts []string `json:"writes_artifacts,omitempty"`

	// Commit identity.
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	Username    string `json:"username"`
	Model       string `json:"model,omitempty"`
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

	// Wrapper-level failure (spec parse, project open, commit
	// error). Distinct from ExitCode — a non-empty Error means
	// nothing committed and the handler should bubble up the
	// message to the user as a tool error.
	Error string `json:"error,omitempty"`
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

	// Open the project clone via the fat-client workspace layer
	// so the cross-process flock is honored — the MCP handler and
	// this wrapper run in distinct processes and MUST NOT race on
	// .git/index.lock.
	ws, err := mcpgit.NewWorkspace(spec.WorkspaceRoot, logger)
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

	// Run the script. Three output streams:
	//   - stdout buffer → result.md (contract-defined answer)
	//   - stderr buffer → failure-reason body on non-zero exit
	//   - scriptLog combined → committed with the result on success,
	//                          kept on local disk on failure
	startTime := time.Now()
	cmd := exec.CommandContext(ctx, spec.ScriptPath)
	cmd.Dir = workDir
	cmd.Env = env
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

	files := []mcpgit.FileWrite{
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
		files = append(files, mcpgit.FileWrite{
			RepoRelPath: filepath.Join(spec.ResultDir, "context.json"),
			Content:     ctxBytes,
		})
	}
	// script.log — full stdout+stderr transcript on success.
	files = append(files, mcpgit.FileWrite{
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
	files = append(files, mcpgit.FileWrite{
		RepoRelPath: filepath.Join(spec.ResultDir, "metadata.json"),
		Content:     metaBytes,
	})

	// Pick up declared artifacts. Missing ones are soft warnings,
	// not hard failures — scripts may legitimately skip writing
	// conditionally.
	for _, rel := range spec.WritesArtifacts {
		if rel == "" {
			continue
		}
		full := filepath.Join(workDir, mcpgit.ArtifactPath(rel))
		body, rerr := os.ReadFile(full)
		if rerr != nil {
			res.MissingArtifacts = append(res.MissingArtifacts, rel)
			continue
		}
		files = append(files, mcpgit.FileWrite{
			RepoRelPath: mcpgit.ArtifactPath(rel),
			Content:     body,
		})
		res.ArtifactsWritten = append(res.ArtifactsWritten, rel)
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
	proj.Lock()
	submitRes, err := proj.SubmitTaskResult(mcpgit.SubmitRequest{
		TaskID:        spec.TaskID,
		Username:      spec.Username,
		AuthorName:    spec.AuthorName,
		AuthorEmail:   spec.AuthorEmail,
		ModelName:     spec.Model,
		Files:         files,
		ArtifactPaths: res.ArtifactsWritten,
		Branch:        spec.Branch,
		Trailers: mcpgit.EnjuTrailers{
			TaskID:          spec.TaskID,
			ExitCode:        0,
			ExitSet:         true,
			DurationSeconds: int(elapsed.Round(time.Second) / time.Second),
		},
	})
	proj.Unlock()
	if err != nil {
		res.Error = fmt.Sprintf("git submit failed: %v", err)
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
