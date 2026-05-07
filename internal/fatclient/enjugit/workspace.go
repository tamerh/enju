package enjugit

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

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

	mu         sync.Mutex
	workflows  map[int64]*Workflow // cached by projectID
	views      map[int64]*View     // cached by projectID
	botCloneAt map[int64]string    // bot-specific clone path overrides
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

// NewWorkspace constructs a Workspace rooted at rootDir.
// Empty rootDir defaults to ~/.enju/workspaces.
//
// Conventions is passed positionally because it's load-bearing —
// every operation depends on it. Other settings (registry,
// logger) are functional options because they're optional or have
// reasonable defaults.
func NewWorkspace(rootDir string, convs Conventions, opts ...Option) (*Workspace, error) {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("enjugit: resolve home dir: %w", err)
		}
		rootDir = filepath.Join(home, ".enju", "workspaces")
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("enjugit: create workspace root %s: %w", rootDir, err)
	}
	w := &Workspace{
		rootDir:    rootDir,
		convs:      convs,
		logger:     slog.Default(),
		workflows:  make(map[int64]*Workflow),
		views:      make(map[int64]*View),
		botCloneAt: make(map[int64]string),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

// RootDir returns the per-machine root holding all project clones
// (default: ~/.enju/workspaces).
func (w *Workspace) RootDir() string { return w.rootDir }

// Conventions returns the conventions this Workspace was
// constructed with. Useful for tests + diagnostic surfaces that
// want to render the same names workflow uses.
func (w *Workspace) Conventions() Conventions { return w.convs }

// ProjectDir returns the on-disk path for a project's clone root.
// Resolves via:
//
//  1. Bot-specific override registered via OpenBotCloneAt.
//  2. projectreg.Registry entry's LocalPath.
//  3. Slug+id form: <rootDir>/<slug>-<id>/.
//  4. Numeric fallback: <rootDir>/<id>/.
//
// Returned even when no clone exists yet (used for "where would
// the clone go?" queries).
func (w *Workspace) ProjectDir(id int64) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if path, ok := w.botCloneAt[id]; ok {
		return path
	}
	return w.projectDirLocked(id)
}

// HasLocalClone reports whether a clone exists on disk for id.
// Works the same as OpenView returning ErrCloneNotFound but
// without constructing a View.
func (w *Workspace) HasLocalClone(id int64) bool {
	dir := w.ProjectDir(id)
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// LeaveProject removes the on-disk clone for id and drops the
// in-memory Workflow/View caches. Used when a citizen explicitly
// detaches from a project. No-op when no clone exists.
func (w *Workspace) LeaveProject(id int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.workflows, id)
	delete(w.views, id)
	dir := w.projectDirLocked(id)
	if dir == "" {
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

// projectDirLocked computes the project's on-disk path. Caller
// holds w.mu.
func (w *Workspace) projectDirLocked(id int64) string {
	if w.registry != nil {
		if entry, err := w.registry.Get(id); err == nil && entry != nil && entry.LocalPath != "" {
			return entry.LocalPath
		}
	}
	// Fallback: scan rootDir for a slug-id or numeric directory
	// matching this project. Slug-id wins when both present (we
	// prefer human-readable). Returns numeric form when nothing
	// found — caller will create on first use.
	if entries, err := os.ReadDir(w.rootDir); err == nil {
		idSuffix := fmt.Sprintf("-%d", id)
		numericName := fmt.Sprintf("%d", id)
		var slugMatch, numericMatch string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasSuffix(name, idSuffix) {
				slugMatch = filepath.Join(w.rootDir, name)
			}
			if name == numericName {
				numericMatch = filepath.Join(w.rootDir, name)
			}
		}
		if slugMatch != "" {
			return slugMatch
		}
		if numericMatch != "" {
			return numericMatch
		}
	}
	return filepath.Join(w.rootDir, fmt.Sprintf("%d", id))
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

// projectDirForName returns the canonical slug-id form for a
// project. Used by ForProject when constructing fresh clone paths.
func (w *Workspace) projectDirForName(id int64, projectName string) string {
	slug := slugify(projectName)
	if slug == "" {
		return filepath.Join(w.rootDir, fmt.Sprintf("%d", id))
	}
	return filepath.Join(w.rootDir, fmt.Sprintf("%s-%d", slug, id))
}

// lockPathFor returns the cross-process flock file for one
// project. Lives next to the project dir, suffixed with .lock.
func (w *Workspace) lockPathFor(id int64) string {
	return filepath.Join(w.rootDir, fmt.Sprintf("project-%d.lock", id))
}

// newWorkflowFromClone wraps a *git.Clone in a Workflow with the
// Workspace's conventions. Used by ForProject and friends.
func (w *Workspace) newWorkflowFromClone(id int64, clone *git.Clone) *Workflow {
	return &Workflow{
		git:    clone,
		convs:  w.convs,
		projID: id,
		logger: w.logger,
	}
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
