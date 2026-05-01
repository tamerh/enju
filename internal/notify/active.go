package notify

// Active-project persistence — the cross-restart hint that lets
// the notify subsystem resume on the project the user was last
// working with, without forcing them to re-supply -notify-project
// every time `enju mcp` boots.
//
// Why a file instead of in-memory: MCP servers are stdio-bound
// per Claude session. When Claude restarts, a brand-new `enju mcp`
// process spawns. Without disk persistence, the user would have
// to "touch" a project (create_project / init) every time just to
// re-arm notifications they already enabled yesterday.
//
// Keying by coordinator URL (or "local" for embedded mode) lets
// users with multiple coordinators (work + personal) keep
// independent active-project records — a model the rest of the
// codebase (credentials.json) already follows.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// activeProjectsFile is the on-disk shape: coordinator-key →
// project-ID. Versioned so future schema bumps can migrate.
type activeProjectsFile struct {
	Version  int             `json:"version"`
	Active   map[string]int64 `json:"active"`
}

const activeProjectsVersion = 1

var activeProjectsMu sync.Mutex // serializes file read-modify-write

// LoadActiveProject returns the saved project ID for the given
// coordinator key, or 0 if none. Missing file / unreadable file /
// malformed JSON all return 0 + nil error — this is a hint, not
// authoritative state, so we err toward "no resume" rather than
// surfacing an error that blocks MCP startup.
func LoadActiveProject(path, coordinatorKey string) int64 {
	if path == "" || coordinatorKey == "" {
		return 0
	}
	activeProjectsMu.Lock()
	defer activeProjectsMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var f activeProjectsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return 0
	}
	return f.Active[coordinatorKey]
}

// SaveActiveProject writes the (coordinator, project) mapping
// atomically (tmp + rename). Reads existing file first so other
// coordinators' entries survive. projectID == 0 clears the entry.
func SaveActiveProject(path, coordinatorKey string, projectID int64) error {
	if path == "" || coordinatorKey == "" {
		return nil
	}
	activeProjectsMu.Lock()
	defer activeProjectsMu.Unlock()

	f := activeProjectsFile{Version: activeProjectsVersion, Active: map[string]int64{}}
	if data, err := os.ReadFile(path); err == nil {
		// Best-effort merge with existing content. If the file
		// is malformed we overwrite — better than refusing to
		// save the new value because of an old typo.
		_ = json.Unmarshal(data, &f)
		if f.Active == nil {
			f.Active = map[string]int64{}
		}
	}

	if projectID == 0 {
		delete(f.Active, coordinatorKey)
	} else {
		f.Active[coordinatorKey] = projectID
	}

	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename active-projects file: %w", err)
	}
	return nil
}
