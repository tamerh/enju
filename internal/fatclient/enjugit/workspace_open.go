package enjugit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// ForProject returns a Workflow handle for the given project. If
// no on-disk clone exists yet, clones from remoteURL into the
// canonical slug-id directory under rootDir. If a clone already
// exists (or registry points at an adopted dir), opens it.
//
// remoteURL may be empty for path-only projects only when an
// adopted dir is registered. Otherwise returns ErrNoCloneSource.
//
// Returns the same Workflow on subsequent calls (cached by
// projectID).
func (w *Workspace) ForProject(id int64, remoteURL string, projectName ...string) (*Workflow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wf, ok := w.workflows[id]; ok {
		return wf, nil
	}
	name := ""
	if len(projectName) > 0 {
		name = projectName[0]
	}
	dir := w.projectDirLocked(id)
	// If no existing dir was found and we know the project name,
	// prefer the slug-id form. Mirrors project.Opener.projectDir's
	// choice so a wf-first ForProject creates the same path
	// project pkg would have. Without this, wf creates `<root>/<id>/`
	// while a later project.ForProject(...,name) creates
	// `<root>/<slug>-<id>/` — divergent dirs for the same project,
	// the failure mode behind the dual-handle test breakage.
	if name != "" {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			dir = w.projectDirForName(id, name)
		}
	}
	clone, err := w.openOrClone(id, dir, remoteURL)
	if err != nil {
		return nil, err
	}
	wf := w.newWorkflowFromClone(id, clone)
	w.workflows[id] = wf
	return wf, nil
}

// OpenBotCloneAt opens a bot-specific clone at an explicit path
// (typically <project>/enju/bots/<bot>/clone/). Pre-warms the
// per-bot clone path override so ProjectDir(id) returns this
// path for subsequent calls.
//
// Different bots on the same project on the same machine each
// get their own clone via this entry point. Service constructs
// a Workspace per bot identity at startup.
func (w *Workspace) OpenBotCloneAt(id int64, path, sourceURL string) (*Workflow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.botCloneAt[id] = path
	clone, err := w.openOrClone(id, path, sourceURL)
	if err != nil {
		return nil, err
	}
	wf := w.newWorkflowFromClone(id, clone)
	w.workflows[id] = wf
	return wf, nil
}

// OpenView returns a read-only View for project id. Errors with
// ErrCloneNotFound when no clone exists on disk — does NOT
// silently lazy-clone (use OpenOrLazyClone for that semantics).
func (w *Workspace) OpenView(id int64) (*View, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.views[id]; ok {
		return v, nil
	}
	dir := w.projectDirLocked(id)
	if dir == "" {
		return nil, ErrCloneNotFound
	}
	clone, err := git.OpenClone(dir, w.lockPathFor(id), w.logger)
	if err != nil {
		if errors.Is(err, git.ErrCloneNotFound) {
			return nil, ErrCloneNotFound
		}
		return nil, fmt.Errorf("enjugit: open view %s: %w", dir, err)
	}
	v := w.newViewFromClone(id, clone)
	w.views[id] = v
	return v, nil
}

// OpenOrLazyClone returns a read-only View for project id. When
// no clone exists on disk, clones from remoteURL first. Used by
// webui's read path so projects the user has never explicitly
// attached to still render content.
//
// Returns ErrNoCloneSource when no clone exists AND remoteURL is
// empty (path-only project the webui process has never seen).
func (w *Workspace) OpenOrLazyClone(id int64, remoteURL string) (*View, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.views[id]; ok {
		return v, nil
	}
	dir := w.projectDirLocked(id)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		// Clone exists; use it.
		clone, err := git.OpenClone(dir, w.lockPathFor(id), w.logger)
		if err != nil {
			return nil, fmt.Errorf("enjugit: open existing %s: %w", dir, err)
		}
		v := w.newViewFromClone(id, clone)
		w.views[id] = v
		return v, nil
	}
	// No clone; lazy-clone if we have a source.
	if remoteURL == "" {
		return nil, ErrNoCloneSource
	}
	clone, err := git.CloneOrInit(dir, remoteURL, w.lockPathFor(id), w.logger)
	if err != nil {
		return nil, fmt.Errorf("enjugit: lazy-clone for view %s: %w", dir, err)
	}
	v := w.newViewFromClone(id, clone)
	w.views[id] = v
	return v, nil
}

// openOrClone opens an existing clone at dir, or clones from
// remoteURL if missing, or initializes an empty local-only repo
// when remoteURL is empty (no-remote / solo project mode).
// Caller holds w.mu.
func (w *Workspace) openOrClone(id int64, dir, remoteURL string) (*git.Clone, error) {
	lockPath := w.lockPathFor(id)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return git.OpenClone(dir, lockPath, w.logger)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("enjugit: mkdir parent %s: %w", dir, err)
	}
	if remoteURL == "" {
		// Local-only init: no remote configured. Mirrors the
		// project package's openOrClone fallback so no-remote
		// projects (enju_init solo mode, fresh-create) get a
		// usable workspace dir without requiring a bare/clone
		// source. Subsequent operations work against the local
		// branches; SetRemote can wire origin later.
		return git.InitLocal(dir, lockPath, w.logger)
	}
	return git.CloneOrInit(dir, remoteURL, lockPath, w.logger)
}
