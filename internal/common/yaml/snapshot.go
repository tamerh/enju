package yaml

// TaskDef lookup helpers for callers that hold a frozen template
// snapshot on disk (typically the fatclient at execute time,
// reading enju/runs/{seq}-{slug}/template-snapshot/enju.yaml).
//
// Pulling per-task config (image ref, runtime selector, env)
// from the snapshot rather than the coord DB keeps execution-
// policy fields off the DB-and-wire path and out of TaskMeta —
// coord builds the DAG from the YAML but never persists fields
// it doesn't act on.

import (
	"fmt"
	"path/filepath"
)

// snapshotManifestName mirrors layout.BundleManifestName. Defined
// here as a local constant to avoid an import cycle (layout
// imports yaml). Keep these two in sync.
const snapshotManifestName = "enju.yaml"

// LoadTaskDefFromSnapshot reads templateDir/enju.yaml and
// returns the TaskDef whose ID equals taskDefID. Returns a
// not-found error when no task matches; that's a hard error
// rather than (nil, nil) because callers always have a task in
// hand and a missing match means the snapshot is inconsistent
// with the coord's view.
//
// The returned pointer is into the parsed Run struct — safe to
// read but don't mutate (callers shouldn't be rewriting the
// snapshot in memory; reparse if they need a fresh copy).
func LoadTaskDefFromSnapshot(templateDir, taskDefID string) (*TaskDef, error) {
	if templateDir == "" {
		return nil, fmt.Errorf("templateDir is required")
	}
	if taskDefID == "" {
		return nil, fmt.Errorf("taskDefID is required")
	}
	yamlPath := filepath.Join(templateDir, snapshotManifestName)
	parsed, err := ParseFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("loading template snapshot %q: %w", yamlPath, err)
	}
	for i := range parsed.Run.Tasks {
		if parsed.Run.Tasks[i].ID == taskDefID {
			return &parsed.Run.Tasks[i], nil
		}
	}
	return nil, fmt.Errorf("task def %q not found in template snapshot %q", taskDefID, yamlPath)
}
