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

// ScriptCwdFor picks the working directory the script should run
// in. Direct-exec compute tasks with a TaskScratchDir set get
// isolated in their scratch dir (Phase 2.3); legacy specs and
// container-mode tasks keep the historical workDir.
//
// Container mode is excluded because docker bind-mounts workDir
// to /workspace and translates ENJU_PROJECT_DIR through that
// mapping; scratch lives outside the bind mount, so a container
// scriptCwd of scratch would point at a host path the container
// can't see. Container support for scratch isolation is deferred.
func ScriptCwdFor(spec Spec, workDir string) string {
	if spec.TaskScratchDir != "" && spec.Container == "" {
		return spec.TaskScratchDir
	}
	return workDir
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
