package enjugit

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/common/oplog"
	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

// ErrProjectNotRegistered is returned when a Workspace operation
// resolves a project ID against the registry and finds no entry
// (or no registry attached at all). Post-Phase-8 every project
// lives at an operator-chosen path tracked in projectreg; there
// is no scan-rootDir fallback. Callers that previously relied on
// the silent fallback now surface this error to the user.
var ErrProjectNotRegistered = errors.New(
	"enjugit: project not registered — run enju_create_project " +
		"path=<abs/dir> to register or adopt the project's directory")

// Workspace is the multi-project entry point. Constructs Workflow
// (mutating) and View (read-only) handles for individual projects,
// using the injected Conventions to decide on-disk paths and
// branch naming.
//
// One Workspace per process. Service constructs it at startup;
// runners receive service.FatClient and never touch Workspace
// directly.
type Workspace struct {
	rootDir  string
	convs    Conventions
	registry *projectreg.Registry
	logger   *slog.Logger

	// logName picks the trace log filename: "<logName>.log".
	// Empty falls back to trace-<pid>.log. Set via WithLogName
	// at construction — typically "operator" for the MCP server
	// and "bot-<name>" for each bot daemon.
	logName string

	mu        sync.Mutex
	workflows map[int64]*Workflow // cached by projectID
	views     map[int64]*View     // cached by projectID
}

// Option configures NewWorkspace via functional options.
type Option func(*Workspace)

// WithRegistry attaches a projectreg.Registry. ForProject and
// related Workspace methods will consult it for project-ID →
// path mappings. Nil-safe (no-op when reg is nil).
func WithRegistry(reg *projectreg.Registry) Option {
	return func(w *Workspace) { w.registry = reg }
}

// WithLogger sets the slog.Logger used for diagnostic output.
// Defaults to slog.Default() when not provided.
func WithLogger(logger *slog.Logger) Option {
	return func(w *Workspace) { w.logger = logger }
}

// WithLogName sets the role label used for the per-project trace
// log filename. The trace log lives at
// <projectRoot>/.enju/logs/<logName>.log. Typical values:
// "operator" for the MCP server, "bot-<botName>" for each bot
// daemon. Empty (default) falls back to trace-<pid>.log.
func WithLogName(name string) Option {
	return func(w *Workspace) { w.logName = name }
}

// NewWorkspace constructs a Workspace rooted at rootDir. Post-
// Phase-8 (NDW.3) every project lives at an operator-chosen path
// tracked via projectreg.Registry; this rootDir is the host-side
// staging directory for derived state (legacy flock files until
// NDW.4 relocates them; cmd/enju helpers that drop temp files).
// It is NEVER where project clones go.
//
// Empty rootDir is a hard error — there is no longer a
// ~/.enju/workspaces fallback. Callers (cmd/enju entry points,
// service.New) supply an explicit path; tests use t.TempDir().
//
// Conventions is passed positionally because it's load-bearing —
// every operation depends on it. Other settings (registry,
// logger) are functional options because they're optional or have
// reasonable defaults.
func NewWorkspace(rootDir string, convs Conventions, opts ...Option) (*Workspace, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("enjugit: NewWorkspace: rootDir is required " +
			"(post-Phase-8 there is no ~/.enju/workspaces default — every " +
			"project lives at its own operator-chosen path registered via " +
			"enju_create_project path=<abs/dir>)")
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("enjugit: create workspace root %s: %w", rootDir, err)
	}
	w := &Workspace{
		rootDir:   rootDir,
		convs:     convs,
		logger:    slog.Default(),
		workflows: make(map[int64]*Workflow),
		views:     make(map[int64]*View),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

// RootDir returns the per-machine root holding all project clones
// (default: ~/.enju/workspaces).
func (w *Workspace) RootDir() string { return w.rootDir }


// ProjectDir returns the on-disk path for a project's clone root.
// Resolves via projectreg.Registry only — every project is
// path-anchored at create_project time, and a missing entry
// surfaces as ErrProjectNotRegistered (no scan-rootDir fallback).
//
// Returned even when no clone exists yet (used for "where would
// the clone go?" queries) as long as the registry has the entry.
func (w *Workspace) ProjectDir(id int64) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.projectDirLocked(id)
}

// AttachRegistry wires the projectreg.Registry post-construction.
// Equivalent to passing WithRegistry to NewWorkspace, but useful
// when the registry is created after the Workspace (e.g. tests
// that build both, then bridge them).
//
// Nil-safe: passing nil clears the registry.
func (w *Workspace) AttachRegistry(reg *projectreg.Registry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.registry = reg
}

// OpenExisting opens the existing clone for id and returns a
// Workflow. Errors with ErrCloneNotFound when no clone exists on
// disk — does NOT clone or init. Used by callers that want to
// fail loudly when the on-disk state is missing rather than
// silently materialize.
//
// Returns ErrProjectNotRegistered when the project ID isn't in
// the registry (no scan-rootDir fallback post-Phase-8).
func (w *Workspace) OpenExisting(id int64) (*Workflow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wf, ok := w.workflows[id]; ok {
		return wf, nil
	}
	dir, err := w.projectDirLocked(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCloneNotFound
		}
		return nil, fmt.Errorf("enjugit: stat %s: %w", dir, err)
	}
	clone, err := git.OpenClone(dir, w.lockPathFor(id, dir), w.logger)
	if err != nil {
		if errors.Is(err, git.ErrCloneNotFound) {
			return nil, ErrCloneNotFound
		}
		return nil, fmt.Errorf("enjugit: open existing %s: %w", dir, err)
	}
	wf := w.newWorkflowFromClone(id, clone)
	w.workflows[id] = wf
	return wf, nil
}

// HasLocalClone reports whether a clone exists on disk for id.
// Returns false when the project isn't registered (no entry =
// no clone the workspace knows about), matching the post-Phase-8
// "registry is the source of truth" semantics.
func (w *Workspace) HasLocalClone(id int64) bool {
	dir, err := w.ProjectDir(id)
	if err != nil || dir == "" {
		return false
	}
	_, statErr := os.Stat(filepath.Join(dir, ".git"))
	return statErr == nil
}

// LeaveProject removes the on-disk clone for id and drops the
// in-memory Workflow/View caches. Used when a citizen explicitly
// detaches from a project. No-op when the project isn't registered
// or no clone exists (idempotent — leave_project must be safely
// re-runnable).
func (w *Workspace) LeaveProject(id int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.workflows, id)
	delete(w.views, id)
	dir, err := w.projectDirLocked(id)
	if err != nil {
		// Not registered — nothing for the workspace to remove.
		// The handler still calls UnregisterProject afterwards
		// to drop the (already absent) row.
		return nil
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("enjugit: remove %s: %w", dir, err)
	}
	return nil
}

// projectDirLocked resolves a project ID to its on-disk path via
// the attached projectreg.Registry. No registry attached, or no
// entry for the ID, returns ErrProjectNotRegistered — there is no
// scan-rootDir fallback post-Phase-8. Caller holds w.mu.
func (w *Workspace) projectDirLocked(id int64) (string, error) {
	if w.registry == nil {
		return "", fmt.Errorf("%w (programming error: Workspace must be opened with a projectreg.Registry attached via WithRegistry/AttachRegistry)", ErrProjectNotRegistered)
	}
	entry, err := w.registry.Get(id)
	if err != nil {
		return "", fmt.Errorf("enjugit: registry lookup for project %d: %w", id, err)
	}
	if entry == nil || entry.LocalPath == "" {
		return "", fmt.Errorf("%w (project id=%d)", ErrProjectNotRegistered, id)
	}
	return entry.LocalPath, nil
}

// slugify turns a free-form name into a filesystem-safe slug:
// lowercase, non-alphanumeric runs replaced with single hyphens,
// trimmed. Empty input → "".
func slugify(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	re := regexp.MustCompile(`[^a-z0-9]+`)
	out := re.ReplaceAllString(name, "-")
	out = strings.Trim(out, "-")
	return out
}

// lockPathFor returns the cross-process flock file for one
// project. Post-NDW.4 it lives at <projectPath>/.enju/locks/project.lock
// — co-located with the project so:
//
//   - cross-machine project clones (rsync'd / shared mount) carry
//     their own lock infrastructure with them, instead of relying
//     on a host-side `~/.enju/workspaces/` directory that may not
//     exist on the receiving machine.
//   - removing the project's dir removes the lock alongside it
//     (no stale ~/.enju/workspaces/project-N.lock files leak
//     after a leave_project).
//   - the operator + async wrapper both compute the SAME inode
//     by feeding the same projectPath (resolved via projectreg)
//     into this helper. The flock contract only holds when the
//     path is byte-identical across processes.
//
// projectID is unused now but kept on the signature for
// diagnostic logging (callers usually have it on hand). The
// MkdirAll-on-demand happens at flock-acquire time inside
// gitcli; this helper is path computation only.
func (w *Workspace) lockPathFor(id int64, projectPath string) string {
	_ = id // diagnostic-only; path is fully derived from projectPath
	return filepath.Join(projectPath, ".enju", "locks", "project.lock")
}

// newWorkflowFromClone wraps a *git.Clone in a Workflow with the
// Workspace's conventions. Used by ForProject and friends.
//
// Opens the per-clone trace log at <workDir>/.enju/logs/trace.log
// so every verb's deferred Emit lands a one-liner there. Failures
// to open (permission denied, disk full) are logged once and the
// Workflow runs without the file — slog stays as the fallback.
func (w *Workspace) newWorkflowFromClone(id int64, clone *git.Clone) *Workflow {
	wf := &Workflow{
		git:    clone,
		convs:  w.convs,
		projID: id,
		logger: w.logger,
	}
	// Trace log lives at <projectRoot>/.enju/logs/trace-<pid>.log
	// — per-process file (PID-suffixed) so operator MCP, each bot
	// daemon, and any autoLocal driver each own their own file
	// without cross-process append coordination. Convention-
	// supplied ProjectRoot reverses the clone-suffix patterns
	// (.clone / bots/<x>/clone) so all writers under one project
	// land their logs in the same directory regardless of which
	// clone they're driving.
	if workDir := clone.WorkDir(); workDir != "" {
		projectRoot := workDir
		if w.convs.DiskLayout.ProjectRoot != nil {
			projectRoot = w.convs.DiskLayout.ProjectRoot(workDir)
		}
		f, err := oplog.OpenProjectLogFile(projectRoot,
			corelayout.LogsDir, oplog.TraceFilename(w.logName))
		if err != nil {
			w.logger.Warn("enjugit: open trace log failed; verb traces go to slog only",
				"project_id", id, "project_root", projectRoot, "error", err)
		} else {
			wf.traceFile = f
		}
	}
	return wf
}

// newViewFromClone wraps a *git.Clone in a read-only View.
func (w *Workspace) newViewFromClone(id int64, clone *git.Clone) *View {
	return &View{
		git:    clone,
		convs:  w.convs,
		projID: id,
		logger: w.logger,
	}
}
