package enjugit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// cursors.go — per-project, per-branch scan cursor state.
//
// On-disk form: `~/.enju/state/project-<id>-cursors.json`.
//
// Lifecycle: after a fat client fetches a branch, it walks
// `cursor..tip` for Enju-* trailers (see ScanBranchSince). On
// success the cursor advances to tip; the next scan is
// incremental. Cursors are idempotent enough that a crash mid-
// scan re-emits the same trailers and the coordinator no-ops the
// duplicates, so we don't need transactional cursor advance —
// the load-modify-save pattern with a per-project mutex is
// sufficient.

// cursorsFormatVersion tracks the on-disk schema for cursor
// files. Bumping this invalidates existing state; readers drop
// the file rather than interpret it under new rules.
const cursorsFormatVersion = 1

// cursorMutexes is the process-wide registry of per-(stateDir,
// projectID) mutexes serializing cursor load-modify-save.
// Submits, scanners, and any future trailer-writing surface all
// acquire the same mutex for the same project, so concurrent
// load-modify-save can't race-overwrite each other. Keyed by
// stateDir so a test with its own temp state dir doesn't
// serialize against an unrelated in-process project.
var cursorMutexes sync.Map

// CursorMutexFor returns the process-wide mutex guarding cursor
// load-modify-save for the given (stateDir, projectID). Callers
// must Lock() before LoadCursors+Save and Unlock() after.
func CursorMutexFor(stateDir string, projectID int64) *sync.Mutex {
	key := fmt.Sprintf("%s|%d", stateDir, projectID)
	if existing, ok := cursorMutexes.Load(key); ok {
		return existing.(*sync.Mutex)
	}
	fresh := &sync.Mutex{}
	actual, _ := cursorMutexes.LoadOrStore(key, fresh)
	return actual.(*sync.Mutex)
}

// AdvanceScanCursor marks a just-landed commit as "already
// processed" so the next trailer scan doesn't replay it. No-op
// when (stateDir, projectID, branch, sha) is incomplete (the
// "caller doesn't maintain a scanner cursor" case).
func AdvanceScanCursor(projectID int64, stateDir, branch, sha string) {
	if projectID == 0 || stateDir == "" || branch == "" || sha == "" {
		return
	}
	mu := CursorMutexFor(stateDir, projectID)
	mu.Lock()
	defer mu.Unlock()
	cursors, err := LoadCursors(stateDir, projectID)
	if err != nil {
		return
	}
	cursors.Set(branch, sha)
	_ = cursors.Save()
}

// RescanSentinelSHA is the cursor value that forces
// ScanBranchSince to walk a branch's full history from tip on
// the next scan. The 40-zero string is non-empty (so it doesn't
// trigger the first-scan baseline early return) and guaranteed
// not to resolve to any real commit (so the walk goes back to
// the root). The coordinator's reconcile is idempotent, so
// re-emitting already-seen trailers is safe — the cursor
// advances to tip after the full walk and subsequent scans
// return to incremental behavior.
//
// Used by enju_set_project_remote to recover the late-remote-add
// case: a project that ran async compute with no origin has
// commits stranded on local refs/heads/* that the scanner never
// saw. Setting a remote + pushing makes refs/remotes/origin/*
// exist for the first time, but cursor entries (if any)
// baselined empty. Setting each branch's cursor to this sentinel
// forces a one-shot retroactive scan.
const RescanSentinelSHA = "0000000000000000000000000000000000000000"

// Cursors tracks per-branch scan position for one project.
// Readers load, scan, and atomically save via Cursors.Save().
type Cursors struct {
	Version  int               `json:"version"`
	Branches map[string]string `json:"branches"`

	path string     `json:"-"`
	mu   sync.Mutex `json:"-"`
}

// NewCursors returns an empty Cursors bound to the on-disk file
// for the given project. Does NOT load — call LoadCursors if
// you want existing state. Useful when you're about to overwrite
// whatever's there (tests, reset).
func NewCursors(stateDir string, projectID int64) *Cursors {
	return &Cursors{
		Version:  cursorsFormatVersion,
		Branches: map[string]string{},
		path:     cursorsPath(stateDir, projectID),
	}
}

// LoadCursors reads the cursors file for the given project from
// stateDir. Returns an empty-but-valid Cursors when the file
// doesn't exist yet. Corrupted files are treated as empty —
// never an error that would wedge the scanner.
func LoadCursors(stateDir string, projectID int64) (*Cursors, error) {
	c := NewCursors(stateDir, projectID)
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("reading cursors %q: %w", c.path, err)
	}
	var raw Cursors
	if err := json.Unmarshal(data, &raw); err != nil {
		return c, nil
	}
	if raw.Version != cursorsFormatVersion {
		return c, nil
	}
	if raw.Branches != nil {
		c.Branches = raw.Branches
	}
	return c, nil
}

// Get returns the last-scanned SHA for branch, or "" when no
// cursor yet.
func (c *Cursors) Get(branch string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Branches[branch]
}

// Set updates the in-memory cursor for a branch. Call Save to
// persist. The Set/Save split lets callers batch updates from a
// multi-branch scan and commit them atomically.
func (c *Cursors) Set(branch, sha string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Branches == nil {
		c.Branches = map[string]string{}
	}
	c.Branches[branch] = sha
}

// Save writes cursors atomically: temp file + rename. A crash
// mid-scan leaves either previous or new state — never partial.
func (c *Cursors) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		return fmt.Errorf("cursors path not set")
	}
	if c.Version == 0 {
		c.Version = cursorsFormatVersion
	}
	serial := struct {
		Version  int               `json:"version"`
		Branches map[string]string `json:"branches"`
	}{
		Version:  c.Version,
		Branches: sortedCopy(c.Branches),
	}
	data, err := json.MarshalIndent(serial, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding cursors: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".cursors-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := io.WriteString(tmp, string(data)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp cursors: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming cursors: %w", err)
	}
	return nil
}

// cursorsPath returns the canonical state-file path for a
// project. Kept separate so callers that just need the path
// (tests, migration helpers) don't have to construct a Cursors.
func cursorsPath(stateDir string, projectID int64) string {
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".enju", "state")
	}
	return filepath.Join(stateDir, fmt.Sprintf("project-%d-cursors.json", projectID))
}

// writeFileImpl is exported for the test file's writeFile helper
// — kept lowercase here so it's not part of the public API but
// reachable from cursors_test.go in the same package.
func writeFileImpl(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

// sortedCopy returns m's contents in deterministic key order
// — `diff` between cursor-file versions stays sane across Go
// versions and platforms.
func sortedCopy(m map[string]string) map[string]string {
	if len(m) == 0 {
		return map[string]string{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}
