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
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/dag"
	yamlv3 "gopkg.in/yaml.v3"
)

// Run is the top-level structure of a run.yaml file.
type Run struct {
	Name         string                 `yaml:"name"`
	Description  string                 `yaml:"description,omitempty"` // human-readable — surfaced to LLM for template discovery
	Version      int                    `yaml:"version"`
	Ref          string                 `yaml:"ref,omitempty"`
	Params       []ParamDef             `yaml:"params,omitempty"` // top-level run params, substituted into {{param}} refs at submission
	ForEach      ForEachMap             `yaml:"for_each,omitempty"`
	Defaults     TaskDefaults           `yaml:"defaults,omitempty"`
	Requirements map[string]interface{} `yaml:"requirements,omitempty"` // project-level requirements, inherited by tasks
	Tasks        []TaskDef              `yaml:"tasks"`
}

// ParamDef declares a single top-level run parameter. A run with
// `params:` declared is a reusable recipe: the same YAML file can
// be submitted directly (with values provided at submission time)
// or dropped under `templates/` for the LLM to instantiate on
// behalf of a user via enju_list_templates + enju_submit_run.
//
// The prose Description is the field the LLM reads when turning
// a template into a conversation with the user — keep it
// user-facing, not implementation-facing.
type ParamDef struct {
	Name        string      `yaml:"name"`
	Type        string      `yaml:"type"`                  // string | int | bool | list<string>
	Required    bool        `yaml:"required,omitempty"`
	Default     interface{} `yaml:"default,omitempty"`
	Description string      `yaml:"description,omitempty"` // natural-language description for the LLM
}

// TaskDefaults holds default values for all tasks.
type TaskDefaults struct {
	Timeout string `yaml:"timeout,omitempty"` // e.g., "30m", "2h"
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
// variable. Exactly one of Values or Ref is populated.
//
// - Values:  a literal list from YAML (`gene: [BRCA1, TP53]`)
// - Ref:     a template reference resolved at submit time
//            (`gene: "{{discover.gene_symbols}}"`)
//
// Phase J.1 adds Ref to support dynamic fan-out — a task
// whose for_each list comes from an upstream task's named
// output. The author-facing YAML syntax is the same keyword
// (`for_each:`) whether the values are static or dynamic;
// only the per-variable shape (list vs scalar) changes.
type ForEachSource struct {
	Values []string
	Ref    string
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
	ResultType string            `yaml:"result_type,omitempty"`
	Timeout    string            `yaml:"timeout,omitempty"`
	Gather     bool              `yaml:"gather,omitempty"`
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
	// a template reference like "{{upstream.field}}" (dynamic,
	// Phase J.1). Dynamic tasks produce zero instances at
	// parse time and materialize on upstream acceptance.
	ForEach ForEachMap `yaml:"for_each,omitempty"`

	// Artifact access (Phase C). Repo-relative paths under artifacts/.
	// ReadsArtifacts can be inferred from {{artifact:path}} prompt
	// references — the parser will merge inferred reads with any
	// explicitly declared paths. WritesArtifacts is always explicit.
	ReadsArtifacts  []string `yaml:"reads_artifacts,omitempty"`
	WritesArtifacts []string `yaml:"writes_artifacts,omitempty"`

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
}

// VoteOption is one choice on an action:vote task.
type VoteOption struct {
	// ID is the stable identifier used in submit payloads and in
	// winning-option references. Must be unique within one vote
	// task's options list.
	ID string `yaml:"id"`
	// Label is the human-readable description shown to voters
	// when they claim the task. Optional — defaults to ID.
	Label string `yaml:"label,omitempty"`
	// Activates is the list of task def ids to keep alive when
	// this option wins. Tasks in other options' Activates (and
	// not in this one's) flip to SKIPPED. Optional — a vote
	// without any activates is a pure decision record with no
	// DAG routing, and downstream tasks can still read the
	// winning option via `{{task.winning_option}}`
	// (session 2 accessor).
	Activates []string `yaml:"activates,omitempty"`
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
