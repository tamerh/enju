package compute

// sweep_runs.go — per-run on-disk snapshot cleanup.
//
// The per-run-snapshot redesign materializes each run's frozen
// tree to <project>/.enju/runs/<seq>-<slug>/snapshot/ at
// create_run time. That directory survives until the run is
// terminal (completed / failed / terminated) — at which point
// it's pure dead weight on disk.
//
// SweepRunStateDirs is the read-the-floor cleanup: enumerate
// every per-run dir under .enju/runs/ and remove the ones whose
// seq isn't in the caller-provided "alive" set. Used at bot
// startup (catch dirs that outlived a coord crash or a missed
// terminal-state signal) and periodically by the daemon's
// poll loop.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
)

// SweepRunStateDirs removes every per-run dir under
// <projectRoot>/.enju/runs/ whose run-seq is NOT in aliveSeqs.
//
// projectRoot is the user's repo root (the dir holding the
// operator's .git/). aliveSeqs is the set of run seqs the
// coordinator currently considers non-terminal — typically
// derived from a project-scoped GET /runs filtered to
// state != {completed, failed, terminated}.
//
// Returns (removed, firstErr). Errors don't abort the sweep —
// the caller logs and continues. Missing root (no .enju/runs/
// yet) is a no-op.
//
// Directory naming convention: each per-run dir is
// "<seq>-<slug>/". The seq prefix is parsed via strconv;
// entries that don't match the shape are skipped (defensive —
// could be a manual operator file, a future sibling dir like
// scratch/ or bigfiles/, etc.).
func SweepRunStateDirs(projectRoot string, aliveSeqs map[int]bool) (int, error) {
	if projectRoot == "" {
		return 0, nil
	}
	root := filepath.Join(projectRoot, corelayout.RunStateRunsRoot())
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("sweep run state dirs: read %s: %w", root, err)
	}
	count := 0
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		seq, ok := parseRunSeqPrefix(e.Name())
		if !ok {
			continue
		}
		if aliveSeqs[seq] {
			continue
		}
		full := filepath.Join(root, e.Name())
		if rerr := os.RemoveAll(full); rerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("sweep run state dirs: remove %s: %w", full, rerr)
			}
			continue
		}
		count++
	}
	return count, firstErr
}

// parseRunSeqPrefix extracts the integer prefix from a per-run
// dir name of the shape "<seq>-<slug>". Returns (seq, true) on
// match, (0, false) on anything else.
//
// Mirrors RunStateDir's path construction (".enju/runs/<seq>-<slug>/")
// — if the format ever changes, both must update together.
func parseRunSeqPrefix(name string) (int, bool) {
	dash := strings.IndexByte(name, '-')
	if dash <= 0 {
		return 0, false
	}
	seq, err := strconv.Atoi(name[:dash])
	if err != nil || seq <= 0 {
		return 0, false
	}
	return seq, true
}
