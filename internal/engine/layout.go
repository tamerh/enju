package engine

// On-disk layout for task results. This is the canonical place
// the layout schema lives — clients receive the full path as a
// string over the wire and never compute it themselves, so
// future layout changes are a one-function edit.
//
// Rationale: the previous layout used a hidden `.enju/` root
// and iteration-first grouping (`enju/runs/{seq}/{instanceKey}/{defID}/`),
// which obscured the human-readable audit trail and made it
// hard to answer "which samples failed stage X?" at the
// filesystem. The new layout inverts both: visible root and
// task-first with self-documenting key=value iteration
// segments. See docs/storage.md for the full rationale.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/store"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// ResultDirRoot is the visible top-level directory every task's
// result files live under. Renamed from `.enju/` pre-launch so
// `result.md`, `script.log`, and committed metadata don't hide
// behind a dotfile — Enju's audit trail is supposed to be
// easy to read, not a ls -a discovery.
const ResultDirRoot = "enju"

// DefaultTemplatesDir is the built-in location template bundles
// live when no enju/conf.yaml override is present. Unified under
// ResultDirRoot so "everything Enju" lives under one visible
// top-level directory instead of scattering enju/templates/ and
// enju/ as sibling entries.
const DefaultTemplatesDir = "enju/templates"

// BundleManifestName is the canonical filename at the root of
// every template bundle directory. Scoped by role (not by
// enclosing folder, so bundle dirs can be renamed freely) and
// distinctive enough that a grep for "enju.yaml" in a mixed
// repo won't collide with GitHub Actions / Ansible / etc.
const BundleManifestName = "enju.yaml"

// ProjectConfigPath is the optional per-project config file
// read at project-open time. Lives inside enju/ (same parent as
// templates/ and runs/) rather than at the repo root — the file
// is small and optional, and keeping it here advertises "all
// Enju-owned paths live under enju/."
const ProjectConfigPath = "enju/conf.yaml"

// TemplateSnapshotDirName is the per-run subdirectory name that
// holds the frozen bundle copy. Named with the -snapshot suffix
// to disambiguate from the live templates dir — otherwise a
// failure message "reading from enju/runs/3/template-snapshot/..." was
// indistinguishable from "enju/templates/..." at a glance.
const TemplateSnapshotDirName = "template-snapshot"

// RunTemplateSnapshotDir returns the per-run bundle snapshot
// location. Computed from the run seq + slug so callers don't
// string-format the `enju/runs/%d-%s/template-snapshot/` layout
// themselves; keeps the snapshot naming rule in one place.
// Empty slug falls back to "run" so the path is always
// well-formed even for a run with no name or template.
func RunTemplateSnapshotDir(runSeq int, slug string) string {
	return filepath.Join(RunDir(runSeq, slug), TemplateSnapshotDirName)
}

// RunDir renders the per-run root segment
// (enju/runs/{seq}-{slug}/) used by every path the run owns —
// task result dirs, the template snapshot, and sibling
// artifacts like graph/ and events/ exports.
//
// Centralizing this keeps ComputeResultDir,
// ComputeResultDirForInstance, RunTemplateSnapshotDir, and the
// mcpserver graph/events writers producing the same run-dir
// prefix — no string-format drift.
func RunDir(runSeq int, slug string) string {
	s := slug
	if s == "" {
		s = "run"
	}
	return filepath.Join(ResultDirRoot, "runs", fmt.Sprintf("%d-%s", runSeq, s))
}

// ComputeRunSlug derives the filesystem-safe slug that shows
// up in enju/runs/{seq}-{slug}/. Canonical sources in order:
//
//  1. Template bundle dir's basename (e.g. "variant-calling"
//     from enju/templates/variant-calling). Already lowercase
//     + hyphen-friendly by bundle-loader convention, but we
//     still run it through the slug rule for defense against
//     hand-rolled paths.
//  2. Run's `name:` field, slugged.
//  3. Fallback "run" for inline-YAML runs with no name:.
//
// Kept in engine so the server-side run-create path and any
// client-side helper that needs the slug (e.g. for the
// template-snapshot commit target) stay in lock-step. A slug
// mismatch between create-time and later reads would corrupt
// the layout silently.
func ComputeRunSlug(sourcePath, runName string) string {
	if sourcePath != "" {
		base := filepath.Base(sourcePath)
		if slug := enjuYaml.SlugInstanceKey(base); slug != "" {
			return slug
		}
	}
	if runName != "" {
		if slug := enjuYaml.SlugInstanceKey(runName); slug != "" {
			return slug
		}
	}
	return "run"
}

// ComputeResultDir returns the repo-relative directory for a
// task's result files. Layouts:
//
//	enju/runs/{seq}/{taskDefID}/                                  (singleton)
//	enju/runs/{seq}/{taskDefID}/{var}={value}/                    (one for_each var)
//	enju/runs/{seq}/{taskDefID}/{var1}={v1}/{var2}={v2}/          (nested, alpha-sorted)
//
// Deliberately task-first rather than iteration-first: the
// filesystem reflects the coordinator's own task-id grouping
// (`ls enju/runs/3/` shows every task in that run, singletons
// and expanded alike, flat), and "why did stage X fail across
// samples?" is a `cd enju/runs/3/align/ && ls` away.
//
// Values are slugged via yaml.SlugInstanceKey — the same rule
// already applied to the instance-key identifier — so
// filesystem-unsafe characters (`/`, `:`, whitespace, shell
// metachars) never land in path segments. The original value
// survives unslugged in the task's instance_params for
// prompts and context.json.
//
// Ordering: variable names sorted alphabetically. Matches the
// existing instance-key slug rule so a for_each over
// `{gene, tissue}` produces consistent ordering across task
// IDs, on-disk paths, and run_status listings. Declaration-
// order preservation is a separate change (requires making
// ForEachMap carry order through YAML unmarshal — tracked in
// WORKFLOW_GAPS.md as a follow-up).
//
// Falls back to singleton layout on any parse failure of
// `instance_params` — a corrupted row shouldn't take the
// submit flow with it; the worst case is a result written to
// the singleton path (which is still under the correct
// run/task parent).
func ComputeResultDir(t *store.TaskRecord) string {
	base := filepath.Join(RunDir(runSeqFromTask(t), t.RunSlug), t.TaskDefID)
	if t.InstanceParams == "" {
		return base
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(t.InstanceParams), &params); err != nil || len(params) == 0 {
		return base
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		seg := fmt.Sprintf("%s=%s", k, enjuYaml.SlugInstanceKey(params[k]))
		base = filepath.Join(base, seg)
	}
	return base
}

// ComputeResultDirForInstance is the pre-persistence variant:
// same layout rule, but consumes the YAML parser's output
// directly (TaskInstance) rather than a DB row. Used by
// create_run and materialize to stamp the path onto a
// TaskRecord before it's persisted. Kept separate from
// ComputeResultDir so the DB-row path doesn't need to
// re-parse JSON at every call site.
func ComputeResultDirForInstance(runSeq int, runSlug, taskDefID string, rawParams map[string]string) string {
	base := filepath.Join(RunDir(runSeq, runSlug), taskDefID)
	if len(rawParams) == 0 {
		return base
	}
	keys := make([]string, 0, len(rawParams))
	for k := range rawParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		seg := fmt.Sprintf("%s=%s", k, enjuYaml.SlugInstanceKey(rawParams[k]))
		base = filepath.Join(base, seg)
	}
	return base
}

// runSeqFromTask parses the task ID `{projID}:{runSeq}:...`
// to extract the run seq. The task record doesn't carry the
// run seq directly (only run_id), but every task ID is
// constructed with it upfront so the parse is reliable.
// Returns 0 on malformed ID — callers render "enju/runs/0/..."
// which is obviously wrong and flags the bug loudly rather
// than silently routing results to a plausible-looking path.
func runSeqFromTask(t *store.TaskRecord) int {
	parts := strings.SplitN(t.ID, ":", 3)
	if len(parts) < 2 {
		return 0
	}
	var seq int
	fmt.Sscanf(parts[1], "%d", &seq)
	return seq
}
