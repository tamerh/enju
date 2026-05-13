package enjugit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
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
// workflowMatchesRegistry checks whether a cached Workflow's
// WorkDir is still consistent with the registry's current view
// for the project ID. Returns true when no registry is attached
// (legacy / test setups without persistent path tracking) or
// when the registry has no entry (lookup will fall through
// naturally). Returns false when the registry's LocalPath has
// changed under the same ID — that's the wipe-and-recreate
// scenario the cache must not silently honor.
func (w *Workspace) workflowMatchesRegistry(id int64, wf *Workflow) bool {
	if w.registry == nil {
		return true
	}
	entry, err := w.registry.Get(id)
	if err != nil || entry == nil || entry.LocalPath == "" {
		return true
	}
	// Compare against ProjectRoot (canonical project dir) rather
	// than WorkDir (clone dir under it). The registry stores the
	// project root.
	return wf.ProjectRoot() == entry.LocalPath
}

func (w *Workspace) ForProject(id int64, remoteURL string, projectName ...string) (*Workflow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wf, ok := w.workflows[id]; ok {
		// Validate: the cached Workflow's WorkDir must still
		// agree with what the registry says for this project
		// ID. They can diverge when coord state was wiped and
		// a new project at a different path got the same ID
		// recycled — the registry upserts to the new path on
		// the create_project call, but a previously-cached
		// Workflow keeps pointing at the old path. Without
		// this check, downstream verbs (execute_run, etc.)
		// resolve script paths against the stale dir and fail.
		if w.workflowMatchesRegistry(id, wf) {
			return wf, nil
		}
		// Drop the stale entry; fall through to re-open at
		// the registry's current LocalPath.
		delete(w.workflows, id)
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
// remoteURL if missing. When remoteURL is empty (path-mode
// project with no real remote), bootstraps via InitLocal so the
// repo has a HEAD; origin stays unset until the operator wires
// one via enju_set_project_remote. Caller holds w.mu.
func (w *Workspace) openOrClone(id int64, dir, remoteURL string) (*git.Clone, error) {
	lockPath := w.lockPathFor(id)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return git.OpenClone(dir, lockPath, w.logger)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("enjugit: mkdir parent %s: %w", dir, err)
	}
	if remoteURL == "" {
		// Path-mode bootstrap: no remote, init a fresh local repo.
		return git.InitLocal(dir, lockPath, w.logger)
	}
	return git.CloneOrInit(dir, remoteURL, lockPath, w.logger)
}
