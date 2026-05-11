package gitv6

// Stale-temp-file recovery for the pack store.
//
// Git's fetch protocol writes incoming packs as tmp_pack_<n>
// and tmp_idx_<n> in .git/objects/pack/, then renames them to
// pack-<sha>.{pack,idx} on successful receive. If the fetch is
// killed mid-stream (signal, OOM, panic), the rename never
// happens and the temp file persists.
//
// On the next fetch attempt, git scans the pack dir and tries
// to read every file there as a pack. The truncated temp file
// fails the magic-bytes check and the operation errors with
// "malformed pack file: bad signature". The clone is then
// effectively read-broken until someone manually deletes the
// temp file or re-clones.
//
// The fix is preventative: sweep any tmp_pack_* / tmp_idx_*
// at OpenClone time. By the time we're opening a clone, no
// fetch can be in progress on it (the caller hasn't started
// any git ops yet), so any temp file is guaranteed orphaned.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// sweepStaleTempPackFiles removes tmp_pack_* and tmp_idx_*
// entries from <workDir>/.git/objects/pack/. Best-effort:
// errors are logged but do not block the caller — a failed
// sweep just leaves the clone in its current state for the
// fetch path to deal with.
func sweepStaleTempPackFiles(workDir string, logger *slog.Logger) {
	packDir := filepath.Join(workDir, ".git", "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		// Missing pack dir is fine — a fresh clone may not
		// have one yet. Other errors (permission, IO) get a
		// debug-level note; not load-bearing.
		if !os.IsNotExist(err) && logger != nil {
			logger.Debug("sweep stale temp pack: readdir failed",
				"pack_dir", packDir, "error", err)
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Git uses the exact prefixes "tmp_pack_" and
		// "tmp_idx_" for in-flight fetch temp files. A
		// stricter check than "tmp_*" — guards against
		// accidentally removing some unrelated file an
		// operator might have left in the pack dir.
		if !strings.HasPrefix(name, "tmp_pack_") && !strings.HasPrefix(name, "tmp_idx_") {
			continue
		}
		full := filepath.Join(packDir, name)
		if err := os.Remove(full); err != nil {
			if logger != nil {
				logger.Warn("sweep stale temp pack: remove failed",
					"path", full, "error", err)
			}
			continue
		}
		if logger != nil {
			logger.Info("swept stale temp pack file",
				"path", full,
				"reason", "leftover from interrupted fetch — would have caused malformed-pack errors")
		}
	}
}
