// Package yaml — type definitions.
//
// Every struct that represents a piece of the run.yaml shape
// lives here: the top-level Run, per-task TaskDef, ParamDef /
// OutputSpec / VoteOption, and the custom YAML unmarshallers
// for scalar-or-list fields. Keeping these in one file makes
// the surface visible at a glance — adding or renaming a
// schema field means editing types.go, not hunting through
// parse / validate / build for the right struct.
//
// The ParsedRun / TaskInstance / ForEachRef / DeferredTaskDef
// "output" types also live here — they're the shapes the rest
// of the engine consumes, and anchoring them with the input
// types keeps the pipeline's producer/consumer contract
// visible in one place.
package yaml

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/common/dag"
	yamlv3 "gopkg.in/yaml.v3"
)

// Run is the top-level structure of a run.yaml file.
type Run struct {
	Name         string                 `yaml:"name"`
	Description  string                 `yaml:"description,omitempty"` // human-readable — surfaced to LLM for template discovery
	Version      int                    `yaml:"version"`
	Ref          string                 `yaml:"ref,omitempty"`
	// BaseBranch names the project branch the run is cut from.
	// When empty the run branches off the project's default
	// branch (typically main). Carried through to coord at
	// create_run so the run branch is created from the right
	// commit. Optional — templates that don't care use the
	// project default.
	BaseBranch   string                 `yaml:"base_branch,omitempty"`
	// Bots is the per-run-snapshot-redesign Phase 7 inline bot
	// roster. Same schema as the stand-alone enju/bots.yaml
	// manifest, embedded directly in the workflow YAML so a
	// single file can carry the whole pipeline + its bot fleet.
	//
	// Stored as a raw yamlv3.Node here to avoid an import cycle:
	// the internal/bots package depends on common/yaml, so the
	// concrete Manifest type can't live here. Callers that need
	// the parsed Manifest invoke bots.FromInlineNode(run.Bots)
	// after the yaml.Run is loaded. An empty / absent block
	// leaves Bots.Kind == 0 (zero-value yamlv3.Node), which
	// FromInlineNode treats as "no inline bots, fall back to
	// the legacy enju/bots.yaml file".
	//
	// Wiring status: Phase 7 lands the parser + conversion path
	// only. The bot-start CLI (`enju agent run`) still reads from
	// enju/bots.yaml directly today; a follow-up will teach it
	// to accept a workflow YAML and prefer its inline bots.
	// Until then, inline declarations parse cleanly but don't
	// drive the daemon — keep both forms in sync if you author
	// inline bots before the CLI catches up.
	Bots         yamlv3.Node            `yaml:"agents,omitempty"`
	Params       []ParamDef             `yaml:"params,omitempty"` // top-level run params, substituted into {{param}} refs at submission
	ForEach      ForEachMap             `yaml:"for_each,omitempty"`
	Defaults     TaskDefaults           `yaml:"defaults,omitempty"`
	Requirements map[string]interface{} `yaml:"requirements,omitempty"` // project-level requirements, inherited by tasks
	Tasks        []TaskDef              `yaml:"tasks"`

	// Living-workflow phase 4c — auto-triage rule. When set, the
	// engine watches for the run to land on `idle` while open
	// issues exist, and spawns a fix task using this template
	// for the oldest open issue. Single-trigger v1: there's only
	// one strategy ("oldest open issue"), so the field carries
	// just the fix-task template, not a trigger discriminator.
	// {{issue.title}}, {{issue.body}}, {{issue.severity}},
	// {{issue.id}} are substituted at spawn time.
	AutoTriage *RemediationTemplate `yaml:"auto_triage,omitempty"`

	// Publish controls whether the run's declared outputs land on the
	// base (deliverable) branch when the run completes. See
	// PublishConfig for the three modes. Optional; omitting the block
	// is equivalent to `publish: mode: local`.
	Publish *PublishConfig `yaml:"publish,omitempty"`
}

// PublishConfig is the per-workflow publish policy declared under the
// optional `publish:` block. It governs ONLY whether the run's
// declared outputs are written to the base branch at run completion
// (and whether that is pushed). It does NOT govern the per-task topic
// and run-branch pushes that happen during the run — those push
// naturally for multi-citizen review and the git audit trail,
// regardless of this setting.
type PublishConfig struct {
	// Mode is one of "none", "local", or "push".
	//   none  — don't publish; base branch unchanged (the run branch
	//           still holds the outputs + full audit trail).
	//   local — write the run's declared outputs onto base_branch as a
	//           curated commit, locally; push nothing (default when
	//           omitted). NB: this is a selective copy of the declared
	//           outputs, NOT a whole-branch merge of the run branch —
	//           base stays a clean deliverable, free of .enju/ trail.
	//   push  — same publish, then push { base_branch, run branch } to Remote.
	Mode string `yaml:"mode,omitempty"`
	// Remote is the git remote to push to when mode=push.
	// Defaults to "origin" when empty.
	Remote string `yaml:"remote,omitempty"`
}

// RecordFields is an ordered map from field name to scalar type
// ("string", "int", or "bool"), used to declare the shape of a
// list<record> param. The insertion order from the YAML document
// is preserved via a custom unmarshaller that walks yaml.v3
// Node.Content pairs instead of the unordered Go map decoder.
//
// Ordered keys matter in two places:
//  1. ParamDef.Key default: the first declared field is the key
//     when no explicit key: is supplied.
//  2. Deterministic error messages during validation.
type RecordFields struct {
	names []string
	types map[string]string
}

// UnmarshalYAML walks the mapping node in YAML declaration order so
// field insertion order is preserved in rf.names.
func (rf *RecordFields) UnmarshalYAML(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("fields: must be a map of field-name: type pairs (e.g. name: string)")
	}
	rf.types = make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		typ := node.Content[i+1].Value
		if _, dup := rf.types[name]; dup {
			return fmt.Errorf("fields: duplicate field name %q", name)
		}
		rf.names = append(rf.names, name)
		rf.types[name] = typ
	}
	return nil
}

// Names returns the field names in YAML declaration order.
func (rf RecordFields) Names() []string { return rf.names }

// TypeOf returns the declared scalar type for the named field and
// whether the field was declared.
func (rf RecordFields) TypeOf(name string) (string, bool) {
	t, ok := rf.types[name]
	return t, ok
}

// Len returns the number of declared fields.
func (rf RecordFields) Len() int { return len(rf.names) }

// FirstName returns the first declared field name (insertion order).
// Returns "" when no fields are declared.
func (rf RecordFields) FirstName() string {
	if len(rf.names) == 0 {
		return ""
	}
	return rf.names[0]
}

// ParamDef declares a single top-level run parameter. A run with
// `params:` declared is a reusable recipe: the same YAML file can
// be submitted directly (with values provided at submission time)
// or committed anywhere in the repo for the LLM to instantiate on
// behalf of a user via enju_list_workflows + enju_create_run.
//
// The prose Description is the field the LLM reads when turning
// a template into a conversation with the user — keep it
// user-facing, not implementation-facing.
type ParamDef struct {
	Name        string       `yaml:"name"`
	Type        string       `yaml:"type"`                  // string | int | bool | list<string> | list<record>
	Required    bool         `yaml:"required,omitempty"`
	Default     interface{}  `yaml:"default,omitempty"`
	Description string       `yaml:"description,omitempty"` // natural-language description for the LLM
	// Key names the field whose value stamps the instance key (task ID
	// slug, result-dir name, branch segment). Required to be one of the
	// declared fields when set. When absent, defaults to the first
	// declared field at parse time. Only valid on type: list<record>.
	Key         string       `yaml:"key,omitempty"`
	// Fields declares the flat record shape: field name → scalar type
	// ("string", "int", or "bool"). Required on type: list<record>;
	// rejected on every other type. Insertion order is preserved so
	// the default Key (first field) is deterministic.
	Fields      RecordFields `yaml:"fields,omitempty"`
}

// TaskDefaults holds default values for all tasks. This is the
// single home for workflow-level defaults — resolveDefaults seeds
// each task's zero-valued field from here before validate +
// expansion, so a per-task explicit value always wins. Future
// defaults (executor:, container:, mode:) land here too rather
// than as N separate default_* keys.
type TaskDefaults struct {
	Timeout string `yaml:"timeout,omitempty"` // e.g., "30m", "2h"

	// VerifyRetryCap seeds every task's verify_retry_cap when the
	// per-task field is 0 (unset). Resolution mirrors Timeout
	// exactly: per-task wins over defaults, defaults wins over the
	// coordinator's built-in const (defaultVerifyFailCap). 0 means
	// "let the coordinator use its built-in default." It caps how
	// many CONSECUTIVE layer-① non-delivery iterations a citizen
	// task may accrue before the coordinator parks it
	// failed_retryable.
	VerifyRetryCap int `yaml:"verify_retry_cap,omitempty"`

	// AssignTo seeds every task whose assign_to is unset. Scalar
	// or list (yamlStringList), same as TaskDef.AssignTo. Defaulted
	// in resolveDefaults before the emit-time assign_to event
	// enrichment, so inbox/review/assigned-ready all see the
	// effective value with no downstream change.
	AssignTo yamlStringList `yaml:"assign_to,omitempty"`
}

// yamlStringList accepts either a scalar or a list in YAML and exposes
// the result as a []string. Used for fields like assign_to where
// `assign_to: tamer` and `assign_to: [tamer, alice]` should both work.
type yamlStringList []string

func (s *yamlStringList) UnmarshalYAML(value *yamlv3.Node) error {
	if value.Kind == yamlv3.ScalarNode {
		*s = yamlStringList{value.Value}
		return nil
	}
	var xs []string
	if err := value.Decode(&xs); err != nil {
		return err
	}
	*s = yamlStringList(xs)
	return nil
}

// ForEachSource is the source of values for one for_each
// variable. Exactly one of Values/RecordValues or Ref is populated
// after parse-time substitution.
//
// - Values:       a literal list from YAML (`gene: [BRCA1, TP53]`)
//                 OR the key-field values after a list<record>
//                 param ref is resolved.
// - RecordValues: populated alongside Values when the source is a
//                 list<record> param. RecordValues[i] is the full
//                 record map for Values[i]. Nil for list<string>
//                 sources.
// - Ref:          a template reference resolved at submit time
//                 (`gene: "{{discover.gene_symbols}}"`)
//
// Phase J.1 adds Ref to support dynamic fan-out — a task
// whose for_each list comes from an upstream task's named
// output. The author-facing YAML syntax is the same keyword
// (`for_each:`) whether the values are static or dynamic;
// only the per-variable shape (list vs scalar) changes.
type ForEachSource struct {
	Values       []string
	Ref          string
	// RecordValues holds the full record maps when Values was populated
	// from a list<record> param. len(RecordValues) == len(Values) when
	// non-nil. Nil for list<string> and literal-list sources.
	RecordValues []map[string]interface{}
}

// ForEachMap is a map from for_each variable name to its
// source. Supports a custom YAML unmarshaller so each
// variable's value can be either a scalar template reference
// or a literal list of strings.
type ForEachMap map[string]ForEachSource

// UnmarshalYAML splits each key's value into either a literal
// list or a template reference scalar. Anything else is
// rejected with a clear error.
func (f *ForEachMap) UnmarshalYAML(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("for_each: must be a map of variable names to values")
	}
	out := make(ForEachMap, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		val := node.Content[i+1]
		var src ForEachSource
		switch val.Kind {
		case yamlv3.ScalarNode:
			src.Ref = strings.TrimSpace(val.Value)
			if src.Ref == "" {
				return fmt.Errorf("for_each %q: value is empty", key)
			}
		case yamlv3.SequenceNode:
			if err := val.Decode(&src.Values); err != nil {
				return fmt.Errorf("for_each %q: %w", key, err)
			}
		default:
			return fmt.Errorf("for_each %q: must be a list of values or a template reference string", key)
		}
		out[key] = src
	}
	*f = out
	return nil
}

// IsDynamic reports whether any for_each variable in the map
// references an upstream task's output (deferred expansion).
// A map where every source is a literal list is static and
// can be expanded at parse time.
func (f ForEachMap) IsDynamic() bool {
	for _, src := range f {
		if src.Ref != "" {
			return true
		}
	}
	return false
}

// StaticValues returns a map[string][]string view of the
// literal sources only, for callers that only need the
// static data (e.g. the existing expansion code path).
// Dynamic entries are omitted.
func (f ForEachMap) StaticValues() map[string][]string {
	out := make(map[string][]string, len(f))
	for k, src := range f {
		if src.Ref == "" {
			out[k] = src.Values
		}
	}
	return out
}

// StaticSources returns all resolved (non-dynamic) sources,
// including both list<string> sources (Values) and list<record>
// sources (RecordValues). Used by build paths that need the full
// ForEachSource to detect and expand record-typed variables.
// Dynamic entries (still-unresolved Refs) are omitted.
func (f ForEachMap) StaticSources() map[string]ForEachSource {
	out := make(map[string]ForEachSource, len(f))
	for k, src := range f {
		if src.Ref == "" {
			out[k] = src
		}
	}
	return out
}

// OutputSpec describes a single named output.
// Supports two YAML formats:
//   outputs:
//     name: "Description"                              # simple string
//   outputs:
//     name:                                            # object form
//       description: "Description"
//       file: "result.csv"                             # optional file
//       format: csv                                    # optional format
type OutputSpec struct {
	Description string `yaml:"description,omitempty"`
	File        string `yaml:"file,omitempty"`
	Format      string `yaml:"format,omitempty"`
}

// UnmarshalYAML supports both string and object forms.
func (o *OutputSpec) UnmarshalYAML(value *yamlv3.Node) error {
	// Try string form first
	if value.Kind == yamlv3.ScalarNode {
		o.Description = value.Value
		return nil
	}
	// Object form
	type alias OutputSpec
	var a alias
	if err := value.Decode(&a); err != nil {
		return err
	}
	*o = OutputSpec(a)
	return nil
}

// WriteArtifact declares one artifact a compute task produces.
// The `track` flag controls whether the artifact lands in git
// history:
//
//   - track: true  (default) — artifact is committed by the
//     fat-client on submit and recorded in the artifact index
//     with its commit SHA. Downstream tasks read it through
//     git like any other tracked file.
//
//   - track: false — artifact stays on disk but is NOT committed
//     (a managed block in .gitignore keeps it out). The artifact
//     index records its existence with an empty commit SHA so
//     downstream readiness still works; the content itself only
//     exists on whatever workspace produced it (or on shared
//     storage if $ENJU_SHARED_ROOT is configured).
//
// The path field accepts three forms, distinguished by syntax —
// no separate kind field, no parser ambiguity. Form is detected
// by IsGlob / IsDir from this package:
//
//   - Literal — `src/server.go`. The exact file must exist after
//     the task runs (unless Optional is true). A literal path
//     must NOT contain `*`, `?`, or `[` — those characters mark
//     the entry as a glob. Filenames that legitimately include
//     those characters can't be declared as literals; declare
//     the parent directory instead, or rename the file.
//   - Glob — `src/api/*.go`, `cmd/*/main.go`. Any path containing
//     `*`, `?`, or `[` is a glob; matched files at submit time
//     are committed. Zero matches is an error unless Optional.
//     Globs match files only — matched directories are silently
//     skipped (use the directory form for recursive coverage).
//     Globs are non-recursive; `**` is not supported.
//   - Directory — `src/api/` (trailing `/`). Recursively walks
//     the directory at submit time and commits every regular
//     file inside. Zero files is an error unless Optional.
//
// The shorthand form — a bare path string — always expands to
// `{Path: <string>, Track: true, Optional: false}`. The object
// form is only needed when overriding Track or Optional.
//
// Optional flag — when true, missing/empty expansions are silent
// no-ops instead of iteration-killing errors. Used for
// auto-generated files that may or may not exist (`go.sum`
// only when external deps are pulled, `requirements.lock`
// only when pip locks were regenerated).
type WriteArtifact struct {
	Path     string `yaml:"path" json:"path"`
	Track    bool   `yaml:"track" json:"track"`
	Optional bool   `yaml:"optional,omitempty" json:"optional,omitempty"`
}

// UnmarshalJSON mirrors UnmarshalYAML for the single-struct
// decode case. Any code path that does
// `json.Unmarshal(data, &writeArtifact)` on `{"path":"x"}`
// would otherwise leave Track at Go's bool zero value (false),
// silently turning a tracked declaration into an untracked
// one. The slice unmarshaler (WriteArtifacts.UnmarshalJSON)
// handles its own pre-set, so this method only matters for
// direct struct decodes — but adding it removes the asymmetry
// and keeps "object-form without `track:` means tracked"
// truthful at every entry point.
//
// Bare-string JSON (`"path/x"`) is also accepted here for
// symmetry with the YAML scalar form.
func (w *WriteArtifact) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		w.Path = s
		w.Track = true
		return nil
	}
	type alias WriteArtifact
	a := alias{Track: true}
	if err := json.Unmarshal(trimmed, &a); err != nil {
		return err
	}
	*w = WriteArtifact(a)
	return nil
}

// UnmarshalYAML accepts either a scalar (path string, track
// defaults to true, optional false) or a mapping (path + any
// of track/optional). Any other shape is a schema error —
// caught by the generic yaml.v3 decoder when we fall through
// to struct decode.
func (w *WriteArtifact) UnmarshalYAML(value *yamlv3.Node) error {
	if value.Kind == yamlv3.ScalarNode {
		w.Path = value.Value
		w.Track = true
		return nil
	}
	if value.Kind != yamlv3.MappingNode {
		return fmt.Errorf("writes entry: expected string or mapping, got %s", nodeKindName(value.Kind))
	}
	// Decode via an alias to avoid recursing into our custom
	// unmarshaller. Pre-set Track=true so an omitted `track:`
	// key stays true — yaml.v3 doesn't overwrite fields
	// absent from the YAML. Optional is false by default which
	// matches the "must produce" contract — Go's zero value
	// happens to be the right default here.
	type alias WriteArtifact
	a := alias{Track: true}
	if err := value.Decode(&a); err != nil {
		return err
	}
	*w = WriteArtifact(a)
	return nil
}

// WriteArtifacts is a typed slice of WriteArtifact entries. The
// named type carries behavior helpers (.Paths(), .TrackedPaths(),
// .UntrackedPaths()) so call sites that only care about one slice
// don't have to loop inline.
type WriteArtifacts []WriteArtifact

// Paths returns every declared path in declaration order,
// regardless of track flag. Used by call sites that treat the
// list as a pure output-set — artifact-dep wiring, LLM prompts,
// error messages.
func (w WriteArtifacts) Paths() []string {
	if len(w) == 0 {
		return nil
	}
	out := make([]string, len(w))
	for i, e := range w {
		out[i] = e.Path
	}
	return out
}

// TrackedPaths returns only the paths with Track=true. Used by
// the fat-client commit step — these are the only files staged
// into git.
func (w WriteArtifacts) TrackedPaths() []string {
	var out []string
	for _, e := range w {
		if e.Track {
			out = append(out, e.Path)
		}
	}
	return out
}

// UntrackedPaths returns only the paths with Track=false. Used
// by the .gitignore management step and by the wrapper when
// skipping files during the commit-staging phase.
func (w WriteArtifacts) UntrackedPaths() []string {
	var out []string
	for _, e := range w {
		if !e.Track {
			out = append(out, e.Path)
		}
	}
	return out
}

// UnmarshalJSON accepts two shapes for back-compat:
//
//   - Legacy:  ["path/a", "path/b"]        — every entry tracked.
//   - Current: [{"path":"a","track":true},
//              {"path":"b","track":false}]
//
// Pre-untracked-artifacts DB rows were written as the legacy
// form; this parser lets them round-trip without a migration.
// Writers always emit the object form.
func (w *WriteArtifacts) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*w = nil
		return nil
	}
	// Peek at the first non-whitespace byte inside the array to
	// decide which shape we're looking at. This avoids a
	// try-decode/fallback dance that would swallow real errors.
	var raw []json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return fmt.Errorf("writes_artifacts: %w", err)
	}
	out := make(WriteArtifacts, 0, len(raw))
	for i, elem := range raw {
		e := bytes.TrimSpace(elem)
		if len(e) == 0 {
			continue
		}
		switch e[0] {
		case '"':
			// Legacy bare string → {Path, Track: true}.
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return fmt.Errorf("writes_artifacts[%d]: %w", i, err)
			}
			out = append(out, WriteArtifact{Path: s, Track: true})
		case '{':
			// Current object form. Pre-set Track=true so an
			// omitted "track" key defaults correctly.
			type alias WriteArtifact
			a := alias{Track: true}
			if err := json.Unmarshal(e, &a); err != nil {
				return fmt.Errorf("writes_artifacts[%d]: %w", i, err)
			}
			out = append(out, WriteArtifact(a))
		default:
			return fmt.Errorf("writes_artifacts[%d]: expected string or object, got %s", i, string(e))
		}
	}
	*w = out
	return nil
}

// nodeKindName renders a yamlv3.Kind as a human label for error
// messages. yaml.v3 doesn't ship one, so we keep a local mapping.
func nodeKindName(k yamlv3.Kind) string {
	switch k {
	case yamlv3.DocumentNode:
		return "document"
	case yamlv3.SequenceNode:
		return "sequence"
	case yamlv3.MappingNode:
		return "mapping"
	case yamlv3.ScalarNode:
		return "scalar"
	case yamlv3.AliasNode:
		return "alias"
	}
	return "unknown"
}

// TaskDef is a single task definition from the YAML.
type TaskDef struct {
	ID         string            `yaml:"id"`
	Action     string            `yaml:"action"`                  // "answer", "contribute", "compute", "review", "vote"
	Ref        string            `yaml:"ref,omitempty"`
	DependsOn  []string          `yaml:"depends_on,omitempty"`
	Prompt     string            `yaml:"prompt,omitempty"`
	UserPrompt string            `yaml:"user_prompt,omitempty"`
	Script     string            `yaml:"script,omitempty"`
	ScriptSource string          `yaml:"script_source,omitempty"`

	// Env is the compute-task-level environment variable block.
	// Keys become env var names, values become env var values,
	// both injected into the process the compute script runs in.
	// Values accept {{param}} / {{forEachVar}} substitution so
	// templates stay runtime-configurable.
	//
	// Intentionally a separate namespace from the system's
	// ENJU_* variables and from ENJU_PARAM_<name> (run-level
	// params). Keys starting with ENJU_ are rejected by the
	// validator so template authors can't accidentally (or
	// intentionally) clobber either reservation. There's no
	// "precedence" question — the three namespaces don't
	// overlap by construction.
	//
	// Only valid on action:compute. Rejected on every other
	// action to keep the field anchored to its purpose.
	Env map[string]string `yaml:"env,omitempty"`

	// Mode controls whether a compute task blocks the
	// enju_execute_task tool call until completion ("sync") or
	// kicks off a detached wrapper process and returns
	// immediately ("async"). Empty → "sync" (backward-compatible
	// default). Only valid on action: compute; rejected on every
	// other action by the validator.
	//
	// Async is the escape hatch for compute jobs that outlast
	// the MCP session — SLURM submissions, multi-hour pipelines,
	// anything where closing the laptop mid-run would lose
	// progress. In async mode the task transitions to running
	// at kickoff; the wrapper commits + pushes on its own
	// schedule; the fetch-path scanner reconciles completion
	// the next time any fat client touches the project.
	// See docs/async-compute.md.
	Mode string `yaml:"mode,omitempty"`

	// Container declares a Docker image that the compute task's
	// script runs inside. When set, the wrapper invokes
	// `docker run <image> /bin/sh -c <script>` with the
	// workspace bind-mounted at /workspace, ENJU_* env vars
	// translated from host paths to container paths, and
	// --user set to the host uid:gid so output files land
	// owned by the host user (not root). Only valid on
	// action: compute; rejected on every other action.
	//
	// Image reference is passed verbatim to docker — any
	// registry/tag/digest form that docker accepts works
	// (biocontainers/samtools:1.18, ghcr.io/org/tool@sha256:...).
	// No pull-policy / network / resource-limit flags in v1.
	// See docs/containers.md.
	Container string `yaml:"container,omitempty"`

	// ContainerRuntime selects which container CLI the wrapper
	// invokes for a compute task. Accepts "docker" (default
	// when empty), "apptainer", or "singularity" (alias for
	// apptainer — rewritten in place at parse time so internal
	// storage and logs always say "apptainer").
	//
	// Setting this without a container: image is a parse error;
	// the runtime needs something to execute. Only valid on
	// action: compute. See docs/containers.md.
	ContainerRuntime string `yaml:"container_runtime,omitempty"`

	// Volumes declares extra host paths bind-mounted into the
	// container, on top of the implicit workspace / scratch /
	// snapshot / shared-root binds the wrapper always sets up.
	// Bioinformatics pipelines separate small git-tracked
	// workflow code from large shared inputs and reference
	// databases that live OUTSIDE the project directory; without
	// this, a containerized task can't reach any of that data.
	//
	// Each entry is a docker-style spec (Linux-style host
	// paths), with param refs allowed (resolved at run-create
	// time alongside every other {{param}} field):
	//
	//   - "/data/refs"                       → /data/refs:/data/refs (same path in)
	//   - "{{raw_data_root}}:/inputs"        → host:container
	//   - "{{checkv_db}}:{{checkv_db}}:ro"   → host:container:options
	//   - "/data/db:/db:ro,z"                → opt into an SELinux relabel
	//
	// The bare form maps the host path to the identical path
	// inside the container — the common case for tools that
	// embed absolute reference-DB paths. The optional third
	// segment is the runtime's mount-options field, passed
	// through verbatim ("ro", "rw", "z", "Z", "ro,z", …); Enju
	// does not interpret it and the runtime CLI arbitrates.
	//
	// Note: declared volumes are NOT auto-relabeled for SELinux.
	// They are arbitrary author-pointed paths (often large
	// shared reference DBs); a forced shared-label relabel would
	// be slow and host-mutating on SELinux-enforcing clusters.
	// Append :z/:Z yourself if you need it.
	//
	// Setting this without a container: image is a parse error
	// (a bare-host script already sees the host filesystem, so
	// the declaration is meaningless). Only valid on
	// action: compute. See docs/containers.md.
	Volumes []string `yaml:"volumes,omitempty"`

	// Executor selects WHERE a compute task's wrapper process
	// runs. "" / "local" → a detached process on this host
	// (today's behavior, unchanged). "slurm" → an sbatch job on
	// the cluster this fat-client submits to. K8s / AWS Batch /
	// GCP Batch remain roadmap and are still rejected at parse
	// time (validateTaskExecutor).
	//
	// executor: slurm implies async semantics — a queued job is
	// never synchronous (see ResolvedModeFields). The executor
	// only changes the launch; snapshot, env, scratch, the
	// wrapper, the result/reaper machinery are byte-identical.
	//
	// Only valid on action: compute. Rides the run snapshot
	// like every other task field (reproducible; later edits
	// don't touch in-flight runs).
	Executor string `yaml:"executor,omitempty"`

	// Resources declares the SLURM ask for an executor: slurm
	// task. Ignored for local/inline execution (a host fork
	// takes whatever the host has). Five modeled knobs cover
	// the common case; SbatchExtra is the raw passthrough
	// escape hatch for everything else — we model the common
	// case, not all of SLURM. Param refs are resolved at
	// run-create time like every other field.
	//
	// Only meaningful with executor: slurm; validateTaskResources
	// checks its shape when that executor is selected.
	Resources Resources `yaml:"resources,omitempty"`

	ResultType string            `yaml:"result_type,omitempty"`
	Timeout    string            `yaml:"timeout,omitempty"`

	// VerifyRetryCap caps how many CONSECUTIVE layer-① non-delivery
	// iterations the coordinator tolerates for this task before
	// parking it failed_retryable (recoverable via enju_retry_task).
	// 0 means "use defaults: verify_retry_cap, or the coordinator's
	// built-in const if that is also 0." Per-task wins over
	// defaults — same precedence as timeout, for the same reason:
	// a hard extraction is flakier than a one-line confirm.
	VerifyRetryCap int `yaml:"verify_retry_cap,omitempty"`

	Outputs      map[string]OutputSpec  `yaml:"outputs,omitempty"`
	Requirements map[string]interface{} `yaml:"requirements,omitempty"` // task-level requirements (replaces project-level)
	Config       map[string]interface{} `yaml:"config,omitempty"`

	// ForEach expands a single task into N parallel instances.
	// Mutually exclusive with the run-level for_each — a run
	// either uses the run-level matrix form or declares
	// for_each on individual tasks with singletons aggregating.
	// All tasks in the same run that declare task-level
	// for_each must share the same variables and sources so
	// downstream per-instance chaining works.
	//
	// Each variable's value can be a literal list (static) or
	// a template reference like "{{upstream.field}}" (dynamic).
	// Dynamic tasks produce zero instances at parse time and
	// materialize on upstream acceptance.
	ForEach ForEachMap `yaml:"for_each,omitempty"`

	// Aggregates names a task def whose fan-out this task
	// reduces over. When set, this task stays singular (one
	// instance) regardless of expansion mode and waits on
	// every instance of the named source before becoming
	// ready. The fan-in resolution at claim time joins all
	// source instances' content into one block so
	// `{{<source>.content}}` reads naturally inside the
	// aggregator's prompt.
	//
	// Target validation: the named task def must exist in the
	// same run AND must itself be fanned (declare for_each, or
	// be carried by a run-level for_each). The parser rejects
	// both forms of misuse at parse time.
	//
	// Coexists with depends_on — an aggregator may also depend
	// on non-fanned siblings; only the relationship to the
	// named source is treated as a fan-in.
	//
	// Failure behavior: when a source instance FAILS, the
	// aggregator stays at PENDING. The fail cascade does not
	// transitively skip aggregators because partial results
	// across the remaining accepted sources may still be
	// valuable. Recover by: (a) invalidating the failed
	// source so it re-runs, (b) manually failing the
	// aggregator with enju_fail_task if partial results
	// aren't useful, or (c) terminating the run. The
	// docs/run-definition.md "collects" section carries the
	// same note for template authors.
	//
	// YAML surface key is `collects:`; the Go field stays
	// Aggregates and all consumers are unchanged.
	Aggregates string `yaml:"collects,omitempty"`

	// Artifact access (Phase C). Repo-relative paths under artifacts/.
	// ReadsArtifacts can be inferred from {{artifact:path}} prompt
	// references — the parser will merge inferred reads with any
	// explicitly declared paths. WritesArtifacts is always explicit.
	ReadsArtifacts  []string       `yaml:"reads,omitempty"`
	WritesArtifacts WriteArtifacts `yaml:"writes,omitempty"`

	// Reviews names the task this one reviews. Required on
	// `action: review` tasks, ignored elsewhere. The reviewer reads
	// the target's output, makes an approve/reject decision, and
	// submits — a reject triggers the existing invalidation cascade
	// on the target. The review task must depend on its target
	// (parser auto-inserts the edge so authors don't have to write
	// it twice). See docs/task-actions.md for the full flow.
	Reviews string `yaml:"reviews,omitempty"`

	// Phase E.2 vote-action fields. Session 1 ships single-voter
	// vote only (citizens: 1 de facto); multi-voter wiring is a
	// follow-up. See docs/task-actions.md for the full semantics.
	//
	// Options is the list of choices on an `action: vote` task.
	// Required and non-empty on vote tasks, forbidden elsewhere.
	// Each option has a stable id (used in submit payloads and in
	// winning-option accessors), a human label for display, and
	// optionally an `activates:` list that routes the DAG — tasks
	// in the winning option's activates set stay alive, tasks in
	// losing options' activates sets flip to SKIPPED.
	Options []VoteOption `yaml:"options,omitempty"`
	// Citizens is how many people are invited to vote (or
	// contribute / review). Defaults to 1. Values > 1 require the
	// multi-voter substrate which ships in session 2; session 1's
	// validator rejects them up front so authors don't get a
	// silent single-voter downgrade.
	Citizens int `yaml:"citizens,omitempty"`
	// MinQuorum is the minimum number of submitted votes required
	// before the tally can resolve. Unset means "any count
	// resolves." Meaningful only for citizens > 1.
	MinQuorum int `yaml:"min_quorum,omitempty"`
	// Threshold is the agreement rule applied to submitted votes:
	// "plurality" (default), "majority", "unanimous", or
	// "percent:N" where N is an integer 1..100.
	Threshold string `yaml:"threshold,omitempty"`
	// Deadline is a relative duration (e.g. "2h", "24h") measured
	// from when the vote task is created. Votes submitted after
	// the deadline are rejected and the tally evaluates whatever
	// landed. Unset means "no time limit."
	Deadline string `yaml:"deadline,omitempty"`

	// Anonymize hides citizen usernames in {{task.responses}}
	// and in the task-detail Voting/Review block. When true,
	// voters render as "citizen-1", "citizen-2", etc. Used
	// for blind-review and blind-voting flows. Optional,
	// defaults to false. Valid on action:vote and action:review.
	Anonymize bool `yaml:"anonymize,omitempty"`
	// Visibility controls whether citizens can see sibling
	// ballots while the task is still COLLECTING. "open"
	// (default) surfaces every submission to every claimer
	// in real time; "blind" hides sibling ballots until the
	// task resolves to ACCEPTED, at which point everyone
	// sees the full tally. Valid on action:vote and
	// action:review.
	Visibility string `yaml:"visibility,omitempty"`

	// Assignment and access control (iteration 1 of build-out plan).
	// Both are optional — the default is open: any registered citizen
	// can claim any task. When set, they narrow who can claim.
	//
	// AssignTo is a list of citizen IDs (not names). The YAML accepts
	// either a scalar (`assign_to: 5bc8c414`) or a list
	// (`assign_to: [5bc8c414, c2f1f36d]`) via yamlStringList.
	//
	// RequireRole checks the claimer's global citizens.role value
	// ("citizen", "author", "reviewer"). Per-project roles are a
	// Phase 2 feature that depends on project membership.
	AssignTo    yamlStringList `yaml:"assign_to,omitempty"`
	RequireRole string         `yaml:"require_role,omitempty"`

	// Living-workflow phase 4b — review-failure spawn rule.
	// Declared on the task that gets reviewed (the dev task,
	// not the review task). When the reviewing task rejects,
	// the engine looks at this field on the target.
	//
	// Recognized values (anything else is treated as empty
	// today; phase 4b.2 will add a parser-level enum check):
	//
	//   - ""               default behavior, no rule.
	//                      OnReviewReject empty = cascade-fail
	//                      target → FAILED, descendants → SKIPPED.
	//                      OnReviewRequestChanges empty = invalidate
	//                      cascade target → PENDING, re-claim same task.
	//   - "spawn_remediation"
	//                      Engine spawns a remediation task using
	//                      RemediationTemplate with the reviewer's
	//                      feedback substituted into the prompt
	//                      ({{review.feedback}}, {{review.decision}}).
	//                      The default cascade is suppressed — the
	//                      author opted in to "review failure forks
	//                      a remediation, doesn't kill the target."
	//
	// "continue_iteration" is the design's prose name for the
	// empty-string default behavior on OnReviewRequestChanges;
	// it is NOT recognized as an explicit value today. Authors
	// who write `on_review_request_changes: continue_iteration`
	// get the default cascade (which IS continue-iteration
	// semantics), so the result is correct, but the field
	// content is treated as empty.
	OnReviewReject         string                  `yaml:"on_review_reject,omitempty"`
	OnReviewRequestChanges string                  `yaml:"on_review_request_changes,omitempty"`
	RemediationTemplate    *RemediationTemplate    `yaml:"remediation_template,omitempty"`

	// HandlerArgs are per-task overrides for the LLM subprocess
	// handler's CLI flags (per-run-snapshot redesign Phase 4b).
	// Merged on top of the bot's HandlerArgs with task-wins
	// semantics — so a task can bump `effort: high` even when the
	// bot's default is `effort: medium`. See bots.Bot.HandlerArgs
	// for the value→argv translation rule.
	//
	// Only meaningful on tasks that route to the subprocess
	// handler (action: claude / gemini / any LLM binary name).
	// Compute / answer / vote tasks ignore the field. Validator
	// could refuse it on non-LLM actions but doesn't yet — pass-
	// through is harmless.
	HandlerArgs map[string]string `yaml:"handler_args,omitempty"`
}

// RemediationTemplate is the inline task spec spawned when a
// review fails and the reviewed task declares
// `on_review_reject: spawn_remediation` (or the same for
// request_changes). Fields mirror the task vocabulary; the
// engine fills in id, depends_on, and reviewer-feedback
// substitution at spawn time.
//
// Resources is the SLURM ask for an executor: slurm compute
// task. Deliberately five modeled knobs + a raw escape hatch —
// model the common case, not all of SLURM. Empty/zero fields
// are omitted from the generated #SBATCH header (SLURM applies
// its partition/site defaults). SbatchExtra lines are spliced
// verbatim, so anything the five knobs don't cover
// (--account, --constraint, --qos, …) still works.
//
// Carried on TaskDef.Resources, JSON-marshaled through the
// tasks.resources column like Env, surfaced on TaskMeta, and
// handed to executor.Executor.Submit. Ignored unless the task's
// effective executor is slurm.
type Resources struct {
	Partition   string   `yaml:"partition,omitempty" json:"partition,omitempty"`
	Time        string   `yaml:"time,omitempty" json:"time,omitempty"` // HH:MM:SS
	CPUs        int      `yaml:"cpus,omitempty" json:"cpus,omitempty"`
	Mem         string   `yaml:"mem,omitempty" json:"mem,omitempty"` // e.g. "32G"
	GPUs        int      `yaml:"gpus,omitempty" json:"gpus,omitempty"`
	SbatchExtra []string `yaml:"sbatch_extra,omitempty" json:"sbatch_extra,omitempty"`
}

// IsZero reports whether no resource knob was set. Used by the
// validator (slurm wants at least a partition or time in
// practice, but we don't force it — SLURM has site defaults)
// and by the SBATCH-header generator to stay terse.
func (r Resources) IsZero() bool {
	return r.Partition == "" && r.Time == "" && r.CPUs == 0 &&
		r.Mem == "" && r.GPUs == 0 && len(r.SbatchExtra) == 0
}

// Stored as JSON in tasks.remediation_template — kept inline
// (not as a reference to a separate template file) so the rule
// is self-contained and the run YAML stays readable.
type RemediationTemplate struct {
	// Action is the new task's action — typically "answer" or
	// "compute". Required.
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
	// Prompt is the new task's prompt. Two template variables
	// are substituted at spawn time:
	//   - {{review.feedback}} = the reviewer's content (rejection rationale)
	//   - {{review.decision}} = "request_changes" or "reject"
	// Other template references (e.g. {{original.content}})
	// resolve at claim time as usual.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	// AssignTo / RequireRole optionally restrict who can claim.
	AssignTo    yamlStringList `yaml:"assign_to,omitempty" json:"assign_to,omitempty"`
	RequireRole string         `yaml:"require_role,omitempty" json:"require_role,omitempty"`
}

// VoteOption is one choice on an action:vote task.
type VoteOption struct {
	// ID is the stable identifier used in submit payloads and in
	// winning-option references. Must be unique within one vote
	// task's options list.
	//
	// json: tags are load-bearing, not cosmetic. tasks.vote_options
	// is json.Marshal(VoteOption) on the coord side and is decoded
	// by the bot daemon (parseVoteOptions), the submit handler, and
	// tally — all keyed on lowercase "id". Without these tags
	// json.Marshal falls back to the exported Go field names
	// ("ID"), which the decoders don't recognize → empty options →
	// vote responses ship unparsed. One struct, one wire shape.
	ID string `yaml:"id" json:"id"`
	// Label is the human-readable description shown to voters
	// when they claim the task. Optional — defaults to ID.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	// Activates is the list of task def ids to keep alive when
	// this option wins. Tasks in other options' Activates (and
	// not in this one's) flip to SKIPPED. Optional — a vote
	// without any activates is a pure decision record with no
	// DAG routing, and downstream tasks can still read the
	// winning option via `{{task.winning_option}}`
	// (session 2 accessor).
	Activates []string `yaml:"activates,omitempty" json:"activates,omitempty"`
}

// ParsedRun is the result of parsing and validating a run file.
// It contains the original definition plus the constructed DAG.
type ParsedRun struct {
	Run  *Run
	DAG      *dag.DAG
	// ExpandedTasks maps instance_key -> []TaskInstance for for_each expansion.
	// If no for_each, there's a single instance with key "".
	ExpandedTasks map[string][]TaskInstance
	// DeferredTaskDefs lists task defs whose instances are
	// not materialized at parse time because their for_each
	// list (or a transitive upstream's) comes from a runtime
	// value. The coordinator materializes these at submit
	// time when the upstream providing the list accepts.
	// Empty for runs with no dynamic for_each.
	DeferredTaskDefs []DeferredTaskDef
	// Warnings are non-fatal advisories raised during
	// validation. Authors can still create the run; the
	// coordinator logs and surfaces them in the creation
	// response. Examples: a review task whose target has no
	// downstream consumers (the review runs but gates
	// nothing), a for_each variable with only one value
	// (equivalent to a singleton), etc.
	Warnings []string
	// MergedParams is the fully-resolved parameter map used
	// for this run: declared defaults merged with caller-
	// supplied values (supplied wins on collision).
	// Populated by ParseWithParams — nil when parsed via the
	// plain Parse path (describe-only usage). This is the
	// authoritative "what params was this run instantiated
	// with" record and is what the coordinator persists to
	// runs.params so compute scripts see defaults via
	// ENJU_PARAM_<name>, not just what the caller typed.
	MergedParams map[string]interface{}
}

// DeferredTaskDef records a task def whose expansion (or
// materialization) waits for an upstream task to accept.
// Populated by buildTaskLevel when the run uses dynamic
// for_each. Consumed at submit time to materialize instances.
type DeferredTaskDef struct {
	// TaskDefID is the task def awaiting materialization.
	TaskDefID string
	// ForEachRefs maps each dynamic for_each variable name
	// to the upstream task ID and field it reads. Empty when
	// this task is deferred transitively (downstream of a
	// deferred task without its own dynamic for_each).
	ForEachRefs map[string]ForEachRef
	// TransitivelyDeferred is true when this task is
	// deferred because one of its ancestors is deferred, not
	// because it has its own dynamic for_each.
	TransitivelyDeferred bool
}

// ForEachRef identifies an upstream task and one of its
// named output fields. Populated from a parsed
// "{{task.field}}" reference in a for_each variable source.
type ForEachRef struct {
	TaskID string
	Field  string
}

// TaskInstance is a concrete task instance after for_each expansion.
type TaskInstance struct {
	TaskDef
	InstanceKey string            // e.g., "endometriosis"
	Params      map[string]string // resolved for_each parameters
	FullID      string            // e.g., "endometriosis:foundation" or just "foundation"
}
