package engine

// Engine-side typed wrappers over the pure path layout logic
// in internal/common/layout. Pure primitives + constants live in
// core; engine adds the typed conveniences that need a
// store.TaskRecord. Re-exports of the constants keep all
// existing callsites compiling without forcing an audit in this
// phase.

import (
	"encoding/json"

	"github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// Re-exports of the layout constants. Callers may import
// internal/common/layout directly for new code; these stay so
// in-tree callers don't churn during the boundary refactor.
const (
	ResultDirRoot           = layout.ResultDirRoot
	DefaultTemplatesDir     = layout.DefaultTemplatesDir
	BundleManifestName      = layout.BundleManifestName
	TemplateSnapshotDirName = layout.TemplateSnapshotDirName
)

// Re-exports of the pure helpers. Same compatibility note as the
// constants above.
var (
	RunDir                     = layout.RunDir
	RunTemplateSnapshotDir     = layout.RunTemplateSnapshotDir
	ComputeRunSlug             = layout.ComputeRunSlug
	ComputeResultDirForInstance = layout.ComputeResultDirForInstance
)

// ComputeResultDir is the typed-row variant: takes a
// store.TaskRecord and produces the result_dir, parsing the
// JSON-encoded instance_params on the way through. Lives in
// engine (not core) because it depends on store types.
//
// Falls back to singleton layout on any parse failure of
// `instance_params` — a corrupted row shouldn't take the
// submit flow with it; the worst case is a result written to
// the singleton path (which is still under the correct
// run/task parent).
func ComputeResultDir(t *store.TaskRecord) string {
	var params map[string]string
	if t.InstanceParams != "" {
		_ = json.Unmarshal([]byte(t.InstanceParams), &params)
	}
	return layout.ComputeResultDirForInstance(layout.RunSeqFromTaskID(t.ID), t.RunSlug, t.TaskDefID, params)
}
