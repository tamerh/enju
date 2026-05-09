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

// BotManifestPath is the optional per-project bot roster. When
// present, declares the bots a project uses (name, model, system
// prompt, tool allowlist, credentials path). Read by `enju bot
// setup` to register identities and by `enju bot run` to spawn
// each daemon with the declared configuration. Coordinator never
// touches this file — bot execution is fatclient-local.
const BotManifestPath = "enju/bots.yaml"

// BotsRuntimeDir is the per-project runtime root where each bot
// daemon's git worktree lives. Sibling to enju/runs/, both
// transient runtime state. Conventionally git-ignored — worktrees
// have their own .git pointer files anyway, so they never round-
// trip through `git add` even if the ignore is missing.
const BotsRuntimeDir = "enju/bots"

// BigfilesDir is the per-project root where action:compute tasks
// land their declared-untracked outputs (writes_artifacts entries
// with track:false). Sibling to .clone/ and .bare.git/, so it
// lives INSIDE the project tree but OUTSIDE the worktree — git
// in .clone/ literally can't see files here, which is the whole
// point: scripts produce multi-GB BAM/FASTQ data, the data lives
// next to the run that produced it, and no .gitignore trickery
// or preserve-during-checkout dance is needed.
//
// Per-branch subdirs (BigfilesBranchDir) keep parallel-branch
// runs from clobbering each other's outputs at the same logical
// path.
const BigfilesDir = "enju/bigfiles"

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

// BotPushTargetDir is the per-project bare repo the bot daemon
// pushes its topic branches to. Created by `enju bot setup` (see
// service.EnsureBotPushTarget); operator's working tree's `origin`
// is rewired to point at this path so async submits land here too.
// The dot prefix on the directory name keeps it out of normal
// `ls`-ing while matching the visible/hidden split convention
// elsewhere in enju/. Always gitignored — no scenario where
// a bare belongs in the project's git history.
const BotPushTargetDir = "enju/.bare.git"

// BotCloneDirFor returns the per-bot per-project managed clone
// directory. Each bot citizen running on a given machine gets
// its own clone so multiple daemons can work in parallel
// without stepping on each other's working tree (claude -p
// writes scratch files, branch switches, in-flight commits —
// all isolated per bot).
//
// Layout: <project>/enju/bots/<botUsername>/clone/
//
// Sits under BotsRuntimeDir so the existing `enju/bots/`
// gitignore rule covers every bot's clone in one go and
// `ls enju/bots/` advertises the local fleet to the operator.
//
// botUsername is the coord-assigned username (the canonical
// citizen identity). Coord-side ValidateUsername already
// restricts the character set, but this helper rejects
// path-hostile input as defense in depth — empty strings,
// path separators, and `..` traversals all get refused so a
// future un-validated caller can't escape into the project
// tree.
func BotCloneDirFor(botUsername string) (string, error) {
	if botUsername == "" {
		return "", fmt.Errorf("bot username is required")
	}
	if strings.ContainsAny(botUsername, `/\`) {
		return "", fmt.Errorf("bot username %q contains path separator", botUsername)
	}
	if botUsername == "." || botUsername == ".." || strings.Contains(botUsername, "..") {
		return "", fmt.Errorf("bot username %q contains path traversal", botUsername)
	}
	return filepath.Join(BotsRuntimeDir, botUsername, "clone"), nil
}

// BotPromptsDir is the conventional location bot system prompts
// live in. Convention only — the manifest's `system_prompt:`
// field can point anywhere repo-relative; this constant just
// names the place tooling expects to find them by default.
const BotPromptsDir = "enju/prompts"

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
// up in enju/runs/{seq}-{slug}/ AND in auto-branch names
// (e.g. "variant-calling-2"). Canonical sources in order:
//
//  1. Template bundle dir's basename (e.g. "variant-calling"
//     from enju/templates/variant-calling).
//  2. Run's `name:` field.
//  3. Fallback "run" for inline-YAML runs with no name:.
//
// Every candidate is normalized through slugifyKebab so the
// output is uniform regardless of the source (template author
// conventions, human-typed run names, etc.). Without uniform
// normalization, a template "hello" produced dir `1-hello/`
// while inline `name: "Quick Inline"` produced `2-Quick_Inline/`
// — same system, two styles, user-visible inconsistency.
//
// Branch-auto naming calls this with the same arguments so
// `git checkout quick-inline-1` lines up with
// `cd enju/runs/2-quick-inline/` instead of diverging into
// `run-1` on the git side and `2-Quick_Inline/` on disk.
func ComputeRunSlug(sourcePath, runName string) string {
	if sourcePath != "" {
		if slug := slugifyKebab(filepath.Base(sourcePath)); slug != "" {
			return slug
		}
	}
	if runName != "" {
		if slug := slugifyKebab(runName); slug != "" {
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
