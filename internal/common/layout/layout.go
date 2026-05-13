// Package layout owns the on-disk layout schema for Enju runs:
// where task result files live, what the template snapshot dir
// is called, what the project config file is named.
//
// Pure logic — no I/O, no DB, no internal dependencies on either
// coordinator or fat-client. Both sides use the same conventions
// so paths line up: the coordinator emits `result_dir` strings
// the fat-client reads at, and they have to agree on the rule
// without one having to ask the other.
//
// See docs/storage.md for the rationale and history of the
// current visible-root + task-first + key=value layout.
package layout

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// ResultDirRoot is the top-level directory every task's result
// files live under. Same root as StateDirRoot (.enju/) by design:
// audit and runtime cache share the umbrella so removing `.enju/`
// removes everything enju-managed. Tracked-vs-untracked is enforced
// by the project's .gitignore (caches like `.enju/runs/*/snapshot/`
// are ignored; the per-task audit files `result.md`, `metadata.json`,
// `context.json`, `script.log` are not). Visibility on disk is
// independent from being in git — operators reading history use
// `git log .enju/runs/...`; operators reading the worktree see
// their own content, not enju bookkeeping.
const ResultDirRoot = ".enju"

// StateDirRoot is the hidden top-level directory holding all
// enju-managed state. The umbrella covers BOTH committed audit
// files (under runs/) AND gitignored runtime caches (snapshot/,
// scratch/, bigfiles/, events/, logs/). The project's .gitignore
// distinguishes the two — tracked audit files survive the umbrella
// ignore via explicit subdir rules, caches are caught by it.
//
// Same value as ResultDirRoot — the split between "visible enju/"
// and "hidden .enju/" was an incoherent pre-launch decision that
// conflated visibility with traceability. Git history is the audit
// trail; the worktree should stay free of enju bookkeeping. Both
// constants are kept as named entry points so call sites read
// semantically (a result dir, vs a cache root) even though they
// resolve to the same path.
const StateDirRoot = ".enju"

// EventsDir holds the project-local event log (live.jsonl,
// cursor.json) the notify loop reads/writes. Sibling to runs/
// and scratch/ under StateDirRoot — all runtime caches, all
// gitignored.
const EventsDir = StateDirRoot + "/events"

// LogsDir holds per-clone trace logs (one per long-running
// fatclient role: operator, bot-<name>). Under StateDirRoot so
// the operator's project tree doesn't show trace files as
// untracked content post-Phase-8.
const LogsDir = StateDirRoot + "/logs"

// DefaultTemplatesDir is a soft convention for where template
// bundles tend to live. After Phase 8 LoadTemplate accepts any
// path, so this constant is just a fallback hint for tooling
// that wants "the conventional spot." User can put workflow
// YAMLs anywhere.
const DefaultTemplatesDir = "enju/templates"

// BundleManifestName is the canonical filename at the root of
// every template bundle directory. Scoped by role (not by
// enclosing folder, so bundle dirs can be renamed freely) and
// distinctive enough that a grep for "enju.yaml" in a mixed
// repo won't collide with GitHub Actions / Ansible / etc.
const BundleManifestName = "enju.yaml"

// BigfilesDir is the per-project root where action:compute tasks
// land their declared-untracked outputs (writes_artifacts entries
// with track:false). Lives under StateDirRoot (.enju/) so the
// existing gitignore umbrella covers it — scripts produce multi-
// GB BAM/FASTQ data, the data lives next to the run that produced
// it, git never sees it.
//
// Per-branch subdirs (BigfilesBranchDir) keep parallel-branch
// runs from clobbering each other's outputs at the same logical
// path.
const BigfilesDir = StateDirRoot + "/bigfiles"

// BigfilesBranchDir returns the per-branch root for untracked
// artifacts produced on `branch`. Branch is used verbatim — git's
// own branch-name validity rules (no ".." segments, no leading
// dashes, etc.) double as our filesystem-safety check, and slash-
// separated branch names ("feature/foo") create useful directory
// structure on disk.
//
// Empty branch collapses to "main" — same default the artifact
// index and topic-branch composer use, so callers that haven't
// resolved a branch name yet still produce a well-formed path.
func BigfilesBranchDir(branch string) string {
	if branch == "" {
		branch = "main"
	}
	return filepath.Join(BigfilesDir, branch)
}

// TemplateSnapshotDirName is the per-run subdirectory name that
// holds the run's frozen YAML manifest (plus any scripts/data
// when the run came from a template bundle). Inline-YAML runs
// land here too — just the manifest, no bundle — so the on-disk
// layout is uniform regardless of how the run was created.
//
// Named with the -snapshot suffix to disambiguate from the live
// templates dir — otherwise a failure message "reading from
// enju/runs/3/template-snapshot/..." was indistinguishable from
// "enju/templates/..." at a glance. The "template-" prefix is a
// slight misnomer for inline runs; renaming to "run-snapshot/"
// is deferred to avoid disrupting existing on-disk layouts.
const TemplateSnapshotDirName = "template-snapshot"

// RunTemplateSnapshotDir returns the per-run bundle snapshot
// location. Computed from the run seq + slug so callers don't
// string-format the `enju/runs/%d-%s/template-snapshot/` layout
// themselves; keeps the snapshot naming rule in one place.
// Empty slug falls back to "run" so the path is always
// well-formed even for a run with no name or template.
//
// This is the IN-GIT path — where the bundle is committed under
// the run branch. Distinct from RunSnapshotOnDiskDir, which is
// the on-disk cache directory the materialized snapshot lives
// at while a run is executing.
func RunTemplateSnapshotDir(runSeq int, slug string) string {
	return filepath.Join(RunDir(runSeq, slug), TemplateSnapshotDirName)
}

// RunSnapshotOnDiskDir returns the on-disk cache location where
// a run's frozen tree gets materialized at create_run time.
// Scripts read everything from here: the in-git template
// snapshot, repo files at the run's base SHA, anything in the
// run branch's tree. Lives under StateDirRoot (.enju/) so the
// user-owned enju/ visible dir stays curated for audit trail
// only.
//
// One snapshot per run, shared by every task in that run. The
// run-completion sweep `rm -rf`s this directory; the operator's
// .git/ retains the run branch + outputs forever as the
// authoritative artifact trail.
//
// Empty slug falls back to "run" via RunDir's normalization so
// the path is always well-formed.
func RunSnapshotOnDiskDir(runSeq int, slug string) string {
	return filepath.Join(RunStateDir(runSeq, slug), "snapshot")
}

// RunStateDir returns the per-run state directory under
// StateDirRoot (.enju/runs/<seq>-<slug>/). This is the parent
// of the run's snapshot/ subdir (RunSnapshotOnDiskDir) — and the
// directory the run-completion sweep removes.
//
// Distinct from RunDir, which returns the VISIBLE enju/runs/...
// path where committed artifact trails live. RunStateDir holds
// only runtime caches (snapshot/, scratch/, future per-run
// transient state); RunDir holds the audit trail.
//
// Empty slug falls back to "run" so the path is always
// well-formed.
func RunStateDir(runSeq int, slug string) string {
	s := slug
	if s == "" {
		s = "run"
	}
	return filepath.Join(StateDirRoot, "runs", fmt.Sprintf("%d-%s", runSeq, s))
}

// RunStateRunsRoot returns the parent dir under which every
// run's RunStateDir lives (.enju/runs/). Used by the sweep
// pass to enumerate all per-run state dirs on disk and diff
// against the coordinator's active-run set.
func RunStateRunsRoot() string {
	return filepath.Join(StateDirRoot, "runs")
}

// RunDir renders the per-run root segment
// (enju/runs/{seq}-{slug}/) used by every path the run owns —
// task result dirs, the template snapshot, and sibling
// artifacts like graph/ and events/ exports.
//
// Centralizing this keeps ComputeResultDirForInstance,
// RunTemplateSnapshotDir, and the mcpserver graph/events
// writers producing the same run-dir prefix — no string-format
// drift.
func RunDir(runSeq int, slug string) string {
	s := slug
	if s == "" {
		s = "run"
	}
	return filepath.Join(ResultDirRoot, "runs", fmt.Sprintf("%d-%s", runSeq, s))
}

// ComputeRunSlug derives the filesystem-safe slug that shows
// up in .enju/runs/{seq}-{slug}/ AND in auto-branch names
// (e.g. "variant-calling-2"). Canonical sources in order:
//
//  1. Run's `name:` field — workflow author's intent wins.
//  2. Source path basename — directory basename when sourcePath
//     is a directory, parent-dir basename when sourcePath is a
//     `*.yaml`/`*.yml` file (so `workflows/scan-deps/enju.yaml`
//     yields "scan-deps" rather than the meaningless "enju-yaml").
//  3. Fallback "run" when neither source produces a usable slug.
//
// Every candidate is normalized through slugifyKebab so the
// output is uniform regardless of source (workflow author
// conventions, human-typed run names, etc.).
//
// Branch-auto naming calls this with the same arguments so
// `git checkout scan-deps-1` lines up with
// `cd .enju/runs/2-scan-deps/`.
func ComputeRunSlug(sourcePath, runName string) string {
	if runName != "" {
		if slug := slugifyKebab(runName); slug != "" {
			return slug
		}
	}
	if sourcePath != "" {
		// For *.yaml / *.yml file paths, the filename ("enju.yaml")
		// is rarely meaningful — the parent directory's basename
		// is. For directory paths, use the basename directly.
		base := filepath.Base(sourcePath)
		if strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") {
			parent := filepath.Base(filepath.Dir(sourcePath))
			if parent != "" && parent != "." && parent != "/" {
				base = parent
			} else {
				// Root-level *.yaml file: trim the extension so
				// "my-pipeline.yaml" → "my-pipeline" instead of
				// "my-pipeline-yaml".
				base = strings.TrimSuffix(base, ".yaml")
				base = strings.TrimSuffix(base, ".yml")
			}
		}
		if slug := slugifyKebab(base); slug != "" {
			return slug
		}
	}
	return "run"
}

// slugifyKebab is the kebab-case slug rule used for human-
// readable run identifiers (directory suffixes + branch
// names). Rules:
//   - ASCII letters are lowercased.
//   - ASCII digits pass through.
//   - Everything else (spaces, punctuation, non-ASCII) collapses
//     into a single `-`.
//   - Leading / trailing `-` are trimmed.
//
// Deliberately different from yaml.SlugInstanceKey: for_each
// values preserve case ("BRCA1" must not become "brca1" — that
// would collide with a hypothetical gene "Brca1" and also make
// logs harder to read), whereas run identifiers benefit from
// uniform lowercasing because they're never compared to user
// data, only to other run identifiers.
func slugifyKebab(s string) string {
	var b strings.Builder
	prevDash := true // suppress a leading dash
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevDash = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// ComputeResultDirForInstance returns the repo-relative directory
// for a task instance's result files. Layouts:
//
//	enju/runs/{seq}-{slug}/{taskDefID}/                            (singleton)
//	enju/runs/{seq}-{slug}/{taskDefID}/{var}={value}/              (one for_each var)
//	enju/runs/{seq}-{slug}/{taskDefID}/{var1}={v1}/{var2}={v2}/    (nested, alpha-sorted)
//
// Deliberately task-first rather than iteration-first: the
// filesystem reflects the coordinator's own task-id grouping
// (`ls enju/runs/3-foo/` shows every task in that run, singletons
// and expanded alike, flat), and "why did stage X fail across
// samples?" is a `cd enju/runs/3-foo/align/ && ls` away.
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
// IDs, on-disk paths, and run_status listings.
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

// RunSeqFromTaskID parses `{projID}:{runSeq}:...`. Returns 0
// on malformed input — callers render `enju/runs/0/...` which
// is obviously wrong and flags the bug loudly rather than
// silently routing results to a plausible-but-wrong path.
//
// Lifted out of the typed `engine.ComputeResultDir` so callers
// without a TaskRecord (e.g. the cascade emitting task_ready
// events with parent metadata) can derive the seq from just the
// task id without pulling store types in.
func RunSeqFromTaskID(taskID string) int {
	parts := strings.SplitN(taskID, ":", 3)
	if len(parts) < 2 {
		return 0
	}
	var seq int
	fmt.Sscanf(parts[1], "%d", &seq)
	return seq
}
