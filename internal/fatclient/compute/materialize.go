package compute

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReadFileFunc reads a file at a given commit SHA. Mirrors the
// signature of enjugit.Workflow.ReadFileAtCommit:
//   - (content, true, nil)  → blob found, body returned
//   - ("",      false, nil) → path not in that commit's tree
//   - (_, _, err)           → unrecoverable read failure
//
// Defined here as a function type (not an interface) so callers
// can pass either a Workflow method value or a fake closure
// without an adapter — the materializer only needs the one
// operation.
type ReadFileFunc func(sha, path string) ([]byte, bool, error)

// ScriptCwdFor picks the host-side directory the script's outputs
// land in. With TaskScratchDir set (Phase 2.3 / 2.5) → scratch
// for both direct-exec and container modes; legacy specs without
// scratch keep workDir.
//
// For container mode, the host scratch dir is bind-mounted into
// the container at ContainerScratchDir (see container_args.go),
// so the in-container CWD is /scratch. The host-side path
// returned here is what the wrapper uses to:
//   - read writes_artifacts after the container exits
//   - materialize reads_artifacts before the container starts
//
// In other words: returns where outputs land on disk from the
// HOST'S perspective, regardless of execution mode.
func ScriptCwdFor(spec Spec, workDir string) string {
	if spec.TaskScratchDir != "" {
		return spec.TaskScratchDir
	}
	return workDir
}

// SweepStaleScratchAtStartup removes the calling bot's scratch
// subtree under the given workspace root. Intended for crash-
// recovery on bot startup: any scratch dirs surviving a previous
// daemon's exit (crash, kill, OOM, container shutdown) are stale
// by definition since their owning task is no longer running, and
// the wrapper's defer-rm only handles the orderly-exit paths.
//
// Returns (entries_removed, first_error_or_nil). Empty /
// nonexistent tree is a successful no-op.
//
// Safety invariant — read this before adding a non-startup caller:
// scratch by design holds only uncommitted work-in-progress (script
// inputs that were materialized from git, plus outputs the wrapper
// reads back into a commit on success). A surviving scratch dir is
// always loss-tolerant: if work product mattered, it would be
// committed; if it isn't, there's nothing to recover. So nuking
// the directory at startup is correct AS LONG AS no concurrent
// wrapper from THIS bot is using it. The function name carries
// "AtStartup" precisely because that's the only call site that
// holds the invariant — at startup the daemon's poll loop hasn't
// begun yet, so no wrapper of ours is live. A future caller that
// reaches for this from elsewhere would race a running wrapper.
//
// botUsername scopes the sweep to <workspaceRoot>/scratch/<bot>/
// so replica configurations (two daemons of the same project
// sharing one workspace root) don't clobber each other's live
// scratch. Replica A's sweep stays inside replica A's subdir;
// replica B's stays inside B's. Empty botUsername is a no-op
// (test fixtures without a coord identity).
func SweepStaleScratchAtStartup(workspaceRoot, botUsername string) (int, error) {
	if workspaceRoot == "" || botUsername == "" {
		return 0, nil
	}
	root := filepath.Join(workspaceRoot, "scratch", botUsername)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("sweep scratch: read %s: %w", root, err)
	}
	count := 0
	var firstErr error
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		if rerr := os.RemoveAll(full); rerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("sweep scratch: remove %s: %w", full, rerr)
			}
			continue
		}
		count++
	}
	return count, firstErr
}

// MaterializeReads writes each declared input path under
// scratchDir, populated by reading from sourceSHA via read.
// Used by the compute wrapper (Phase 2.2) to seed a task's
// scratch dir with its declared reads_artifacts before the
// script runs, so the script never has to see the rest of the
// project — only its declared inputs.
//
// scratchDir must already exist (the wrapper's mkdir step creates
// it; we only mkdir for INTERMEDIATE directories under each
// input path).
//
// Behaviour:
//   - For each path: read from sourceSHA. If found, mkdir its
//     parent under scratchDir and write the body at
//     filepath.Join(scratchDir, path).
//   - If a read returns (_, false, nil) — path absent from the
//     commit — append it to the returned `missing` slice and
//     continue with the next path. Caller decides whether to
//     soft-warn or hard-fail; we don't presume.
//   - If a read returns an error, abort and propagate. An IO
//     failure during read is structurally different from an
//     absent file; treating them the same would silently turn
//     transient failures into "your input doesn't exist," which
//     is the wrong way to fail in a system where every input is
//     reachable in principle.
//
// Empty paths slice is a successful no-op (returns nil, nil).
func MaterializeReads(scratchDir, sourceSHA string, paths []string, read ReadFileFunc) ([]string, error) {
	var missing []string
	for _, p := range paths {
		body, found, err := read(sourceSHA, p)
		if err != nil {
			return nil, fmt.Errorf("materialize: reading %s at %s: %w", p, sourceSHA, err)
		}
		if !found {
			missing = append(missing, p)
			continue
		}
		dst := filepath.Join(scratchDir, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, fmt.Errorf("materialize: mkdir for %s: %w", p, err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return nil, fmt.Errorf("materialize: write %s: %w", p, err)
		}
	}
	return missing, nil
}
