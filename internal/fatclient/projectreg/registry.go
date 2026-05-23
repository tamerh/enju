// Package projectreg is the per-machine project registry —
// `~/.enju/projects.json`. It records the projects this
// fat-client is involved in, with the *durable* mapping from
// project ID to local path. Post-NDW.2 every project is
// path-anchored via this registry — no scan-rootDir fallback
// exists, so the registry IS the resolution mechanism.
//
// Scope is deliberately tight: project-machine bindings only.
// User preferences (last-active project, theme) belong in
// `~/.enju/webui-state.json`. Credentials stay in
// `~/.enju/credentials.json` (different perms, different
// lifecycle). Three small files, three clean purposes.
package projectreg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one project the fat-client knows about. ID +
// LocalPath are load-bearing; the cached fields (Name,
// RemoteURL, DefaultBranch) speed up cross-project landing
// pages so a render doesn't fan out into N coord calls. They
// can drift — the source of truth is the coordinator — and
// callers should refresh on next coord touch.
type Entry struct {
	ID            int64     `json:"id"`
	LocalPath     string    `json:"local_path"`
	Name          string    `json:"name,omitempty"`
	RemoteURL     string    `json:"remote_url,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	LastTouched   time.Time `json:"last_touched"`
}

// Index is the on-disk shape. Version is for forward-compat:
// a future schema change bumps the version and either migrates
// or refuses to load.
type Index struct {
	Version  int     `json:"version"`
	Projects []Entry `json:"projects"`
}

// CurrentVersion is the schema version this code writes.
const CurrentVersion = 1

// Registry is a handle to the on-disk file. Safe for concurrent
// use within a process; cross-process safety relies on the
// atomic write-temp-then-rename pattern (POSIX rename is
// atomic). Last writer wins on concurrent rename.
//
// Cross-process semantics caveat: last-writer-wins is fine
// for accumulative ops (Upsert/Upsert against different IDs,
// Touch following Upsert). It is NOT correct for an Upsert
// racing against a Remove on the same ID — the loser's view
// of the file may not reflect the winner's mutation. In
// practice that race requires two fat-client processes
// touching the same project ID concurrently, which the v1
// usage pattern (one `enju mcp` plus an occasional CLI) does
// not exercise. If a future deployment runs multiple
// long-lived fat-client processes concurrently, add a
// per-file flock around saveLocked.
type Registry struct {
	path string
	mu   sync.Mutex
}

// DefaultPath returns `~/.enju/projects.json`. Returns an
// empty string if the home directory can't be resolved
// (rare; caller should fall back to in-memory operation).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".enju", "projects.json")
}

// Open returns a Registry rooted at the given path. The file
// itself isn't read until Load / List / Upsert / Remove fire,
// so this is cheap and never fails on a missing file.
func Open(path string) *Registry {
	return &Registry{path: path}
}

// Path returns the file path this Registry is rooted at. Exposed
// for callers that need to log it, surface it in --registry
// echoes, or open a parallel handle from another process.
func (r *Registry) Path() string { return r.path }

// Load reads the index from disk. Missing file returns an
// empty Index with the current version (a freshly initialized
// registry is indistinguishable from a missing file). Malformed
// JSON returns an error — we don't silently overwrite a
// possibly-corrupt user file.
func (r *Registry) Load() (Index, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

func (r *Registry) loadLocked() (Index, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Index{Version: CurrentVersion}, nil
		}
		return Index{}, fmt.Errorf("read registry: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return Index{}, fmt.Errorf("parse registry: %w", err)
	}
	if idx.Version == 0 {
		idx.Version = CurrentVersion
	}
	return idx, nil
}

// List returns the registry entries whose LocalPath still
// exists on disk. Stale entries (manually deleted clones) are
// filtered at read time but NOT removed from the file —
// removals only happen via explicit Remove. This way a
// transient mount issue doesn't permanently lose the entry.
func (r *Registry) List() ([]Entry, error) {
	idx, err := r.Load()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(idx.Projects))
	for _, e := range idx.Projects {
		if _, err := os.Stat(e.LocalPath); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Upsert adds or updates an entry. Matched by ID. If the entry
// is new, LastTouched is set to now. If it's an update, fields
// with zero values in `e` are preserved from the existing entry
// — Upsert is for "I have new info about this project," not
// "wipe the row." Use Remove + Upsert for full replacement.
func (r *Registry) Upsert(e Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, err := r.loadLocked()
	if err != nil {
		return err
	}
	idx = applyUpsert(idx, e, time.Now().UTC())
	return r.saveLocked(idx)
}

// applyUpsert inserts or merges e into idx by ID and returns the
// updated index. Field-preserving: zero-valued fields in e keep the
// existing entry's values. Shared by Upsert and Register; the caller
// holds the lock.
func applyUpsert(idx Index, e Entry, now time.Time) Index {
	for i, existing := range idx.Projects {
		if existing.ID != e.ID {
			continue
		}
		merged := existing
		if e.LocalPath != "" {
			merged.LocalPath = e.LocalPath
		}
		if e.Name != "" {
			merged.Name = e.Name
		}
		if e.RemoteURL != "" {
			merged.RemoteURL = e.RemoteURL
		}
		if e.DefaultBranch != "" {
			merged.DefaultBranch = e.DefaultBranch
		}
		if !e.LastTouched.IsZero() {
			merged.LastTouched = e.LastTouched
		} else {
			merged.LastTouched = now
		}
		idx.Projects[i] = merged
		return idx
	}
	if e.LastTouched.IsZero() {
		e.LastTouched = now
	}
	idx.Projects = append(idx.Projects, e)
	return idx
}

// Register records a path-anchored project, keeping the registry to
// one entry per LocalPath. If other entries already point at
// e.LocalPath under a *different* ID, they are dropped: a directory
// maps to exactly one project, so a same-path/different-id entry is
// a stale prior generation — typically a re-adoption after the
// coordinator assigned a new id (e.g. its DB was wiped). Without
// this, re-adoption appended a second row and path resolution could
// land on the dead id ("No runs yet") instead of the live project.
//
// Falls back to a plain upsert when e.LocalPath is empty (a partial,
// field-only update with no path to dedupe on, e.g. a name backfill).
func (r *Registry) Register(e Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, err := r.loadLocked()
	if err != nil {
		return err
	}
	if e.LocalPath != "" {
		kept := idx.Projects[:0]
		for _, ex := range idx.Projects {
			if ex.LocalPath == e.LocalPath && ex.ID != e.ID {
				continue // stale entry for this path under a dead id
			}
			kept = append(kept, ex)
		}
		idx.Projects = kept
	}
	idx = applyUpsert(idx, e, time.Now().UTC())
	return r.saveLocked(idx)
}

// Touch updates only LastTouched on an existing entry. No-op if
// the entry doesn't exist — Touch is a freshness signal, not a
// creation path. Returns nil regardless so callers don't have
// to special-case the "first call before Upsert" race.
func (r *Registry) Touch(id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, err := r.loadLocked()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i, e := range idx.Projects {
		if e.ID == id {
			idx.Projects[i].LastTouched = now
			return r.saveLocked(idx)
		}
	}
	return nil
}

// Remove drops the entry by ID. No-op if the entry doesn't
// exist (idempotent — leave_project should be safely re-runnable).
// Get returns the entry for projectID, or (nil, nil) if absent.
// Stale entries (LocalPath no longer on disk) are filtered the
// same way List does. Used by callers that need a single
// entry's path without scanning the full list.
func (r *Registry) Get(id int64) (*Entry, error) {
	idx, err := r.Load()
	if err != nil {
		return nil, err
	}
	for _, e := range idx.Projects {
		if e.ID != id {
			continue
		}
		if _, err := os.Stat(e.LocalPath); err != nil {
			return nil, nil
		}
		return &e, nil
	}
	return nil, nil
}

func (r *Registry) Remove(id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, err := r.loadLocked()
	if err != nil {
		return err
	}
	out := idx.Projects[:0]
	for _, e := range idx.Projects {
		if e.ID != id {
			out = append(out, e)
		}
	}
	idx.Projects = out
	return r.saveLocked(idx)
}

// saveLocked writes the index to disk via the standard
// write-temp-then-rename pattern (atomic on POSIX). The
// containing directory is created if missing — registry init
// happens on first write, not at construction.
func (r *Registry) saveLocked(idx Index) error {
	if r.path == "" {
		return fmt.Errorf("registry path not set")
	}
	if idx.Version == 0 {
		idx.Version = CurrentVersion
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("mkdir registry parent: %w", err)
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp registry: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tmp registry: %w", err)
	}
	return nil
}

// FindContaining returns the registry entry whose LocalPath is absPath
// or an ancestor directory. Prefer the deepest match so nested
// project layouts resolve to the closest project. Returns nil
// when no entry contains the path.
func (r *Registry) FindContaining(absPath string) (*Entry, error) {
	idx, err := r.Load()
	if err != nil {
		return nil, err
	}
	var best *Entry
	for i := range idx.Projects {
		root := idx.Projects[i].LocalPath
		if root == "" {
			continue
		}
		// os.Stat filter matches Registry.List() behavior — we
		// don't resolve to paths that were manually deleted.
		if _, err := os.Stat(root); err != nil {
			continue
		}
		rel, err := filepath.Rel(root, absPath)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if best == nil {
			best = &idx.Projects[i]
			continue
		}
		// Prefer the deepest match so nested layouts resolve to the
		// closest project. On a tie — two entries with the SAME path,
		// which happens when a re-adoption left a stale duplicate
		// under a dead id — prefer the most-recently-touched entry,
		// since the live re-adoption carries the newer timestamp.
		// Without this tiebreak, resolution kept whichever entry came
		// first in the file (the older, dead one).
		cur := &idx.Projects[i]
		switch {
		case len(cur.LocalPath) > len(best.LocalPath):
			best = cur
		case len(cur.LocalPath) == len(best.LocalPath) && cur.LastTouched.After(best.LastTouched):
			best = cur
		}
	}
	return best, nil
}
