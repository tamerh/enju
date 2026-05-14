package enjugit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

// ForProject returns a Workflow handle for the given project. The
// project must already be registered via projectreg (typically by
// enju_create_project path=<abs/dir>); ForProject opens the path
// the registry resolves to. If no on-disk clone exists yet, clones
// from remoteURL into that path (or, when remoteURL is empty,
// init-locals a fresh repo there).
//
// projectName is accepted for back-compat with NDW.1 callers but
// ignored — the path is registry-resolved, not slug-derived.
// NDW.5 will drop the parameter.
//
// Returns ErrProjectNotRegistered when the project isn't in the
// registry. Returns the same Workflow on subsequent calls (cached
// by projectID).
//
// workflowMatchesRegistry checks whether a cached Workflow's
// WorkDir is still consistent with the registry's current view
// for the project ID. Returns false when the registry's LocalPath
// has changed under the same ID — that's the wipe-and-recreate
// scenario the cache must not silently honor (coord state wiped,
// new project at a different path got the same ID recycled).
func (w *Workspace) workflowMatchesRegistry(id int64, wf *Workflow) bool {
	entry, err := w.registry.Get(id)
	if err != nil || entry == nil || entry.LocalPath == "" {
		return false
	}
	return wf.ProjectRoot() == entry.LocalPath
}

func (w *Workspace) ForProject(id int64, remoteURL string, projectName ...string) (*Workflow, error) {
	_ = projectName // accepted for back-compat; registry resolves the path
	w.mu.Lock()
	defer w.mu.Unlock()
	if wf, ok := w.workflows[id]; ok {
		// Validate: the cached Workflow's WorkDir must still
		// agree with what the registry says for this project
		// ID. Downstream verbs (execute_run, etc.) resolve
		// script paths against the cached dir; if the registry
		// has rotated under the same ID we drop the cache and
		// re-open.
		if w.workflowMatchesRegistry(id, wf) {
			return wf, nil
		}
		delete(w.workflows, id)
	}
	dir, err := w.projectDirLocked(id)
	if err != nil {
		return nil, err
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
// Errors with ErrProjectNotRegistered when the project isn't
// registered.
func (w *Workspace) OpenView(id int64) (*View, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.views[id]; ok {
		return v, nil
	}
	dir, err := w.projectDirLocked(id)
	if err != nil {
		return nil, err
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

// OpenOrLazyClone returns a read-only View for project id. Post-
// Phase-8 the name is a misnomer — there is no lazy-clone:
//
//   - Unregistered project → ErrProjectNotRegistered.
//   - Registered but no on-disk clone → ErrCloneNotFound. The webui
//     surfaces this as "project not adopted on this machine" so the
//     operator runs enju_create_project path=<abs/dir> to wire up
//     a clone explicitly. We don't silently materialize at a
//     directory the operator hasn't confirmed.
//
// remoteURL is accepted for back-compat with NDW.1 callers but
// unused — adoption goes through enju_create_project, not a
// silent webui-side clone.
func (w *Workspace) OpenOrLazyClone(id int64, remoteURL string) (*View, error) {
	_ = remoteURL // see doc comment
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.views[id]; ok {
		return v, nil
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
	clone, err := git.OpenClone(dir, w.lockPathFor(id), w.logger)
	if err != nil {
		if errors.Is(err, git.ErrCloneNotFound) {
			return nil, ErrCloneNotFound
		}
		return nil, fmt.Errorf("enjugit: open existing %s: %w", dir, err)
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
