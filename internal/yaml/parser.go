// Package yaml parses Cedar run definition files.
package yaml

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/dag"
	"github.com/enju-ai/enju/internal/template"
	yamlv3 "gopkg.in/yaml.v3"
)

// Run is the top-level structure of a run.yaml file.
type Run struct {
	Name         string                 `yaml:"name"`
	Description  string                 `yaml:"description,omitempty"` // human-readable — surfaced to LLM for template discovery
	Version      int                    `yaml:"version"`
	Ref          string                 `yaml:"ref,omitempty"`
	Params       []ParamDef             `yaml:"params,omitempty"` // top-level run params, substituted into {{param}} refs at submission
	ForEach      map[string][]string    `yaml:"for_each,omitempty"`
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
	ResultType string            `yaml:"result_type,omitempty"`
	Timeout    string            `yaml:"timeout,omitempty"`
	Gather     bool              `yaml:"gather,omitempty"`
	Outputs      map[string]OutputSpec  `yaml:"outputs,omitempty"`
	Requirements map[string]interface{} `yaml:"requirements,omitempty"` // task-level requirements (replaces project-level)
	Config       map[string]interface{} `yaml:"config,omitempty"`

	// ForEach expands a single task into N parallel instances. Mutually
	// exclusive with the run-level for_each — a run either uses the
	// run-level matrix form (every task expanded together) or declares
	// for_each on individual tasks with singletons aggregating results.
	// All tasks in the same run that declare for_each must share the
	// same variables and values; differing task-level for_each blocks
	// in the same run are rejected at parse time.
	ForEach map[string][]string `yaml:"for_each,omitempty"`

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
	// Warnings are non-fatal advisories raised during
	// validation. Authors can still create the run; the
	// coordinator logs and surfaces them in the creation
	// response. Examples: a review task whose target has no
	// downstream consumers (the review runs but gates
	// nothing), a for_each variable with only one value
	// (equivalent to a singleton), etc.
	Warnings []string
}

// TaskInstance is a concrete task instance after for_each expansion.
type TaskInstance struct {
	TaskDef
	InstanceKey string            // e.g., "endometriosis"
	Params      map[string]string // resolved for_each parameters
	FullID      string            // e.g., "endometriosis:foundation" or just "foundation"
}

// ParseFile reads and parses a run YAML file.
func ParseFile(path string) (*ParsedRun, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	return Parse(data)
}

// Parse parses run YAML bytes without substituting any top-level
// `params:` values. Any {{param}} references in task prompts are
// left as literal placeholders — this is the right entry point
// for linting a template file before it's instantiated.
//
// To instantiate a template with actual values, use
// ParseWithParams.
func Parse(data []byte) (*ParsedRun, error) {
	return parseInternal(data, nil, false)
}

// ParseWithParams parses run YAML bytes and substitutes the
// supplied parameter values into every `{{param}}` reference in
// task prompts. Required params with no supplied value and no
// declared default produce a natural-language error; unknown
// param names produce an error; type mismatches produce an
// error. Optional params fall back to their declared default.
//
// This is the entry point for the "run a template" path
// (enju_run_from_template) and the direct-submission path when
// the caller passes values inline.
func ParseWithParams(data []byte, paramValues map[string]interface{}) (*ParsedRun, error) {
	return parseInternal(data, paramValues, true)
}

func parseInternal(data []byte, paramValues map[string]interface{}, substitute bool) (*ParsedRun, error) {
	var prob Run
	dec := yamlv3.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&prob); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	warnings, err := validate(&prob)
	if err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}

	if substitute {
		if err := applyParamValues(&prob, paramValues); err != nil {
			return nil, err
		}
	}

	parsed, err := build(&prob)
	if err != nil {
		return nil, fmt.Errorf("building DAG: %w", err)
	}
	parsed.Warnings = warnings

	return parsed, nil
}

// applyParamValues merges supplied parameter values with declared
// defaults, validates required params are present and types
// match, and substitutes `{{param}}` references in every task
// prompt. Errors are phrased in natural language so the LLM can
// forward them to the user as conversational follow-ups.
func applyParamValues(p *Run, supplied map[string]interface{}) error {
	// If the run declares no params, the caller should not be
	// passing any either — that usually means a template path
	// mixup.
	if len(p.Params) == 0 {
		if len(supplied) > 0 {
			var keys []string
			for k := range supplied {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return fmt.Errorf("this run does not declare any parameters, but values were supplied for: %s", strings.Join(keys, ", "))
		}
		return nil
	}

	declared := make(map[string]*ParamDef, len(p.Params))
	for i := range p.Params {
		declared[p.Params[i].Name] = &p.Params[i]
	}

	// Reject unknown param names first — a typo in a param name
	// should surface as "unknown parameter 'diesase'" rather than
	// "missing required parameter 'disease'" (which hides the
	// typo).
	for name := range supplied {
		if _, ok := declared[name]; !ok {
			var known []string
			for k := range declared {
				known = append(known, k)
			}
			sort.Strings(known)
			return fmt.Errorf("unknown parameter %q — this run declares: %s", name, strings.Join(known, ", "))
		}
	}

	// Build the merged value map: defaults first, then supplied
	// (so supplied values win). Type-check supplied values as we
	// go so the error mentions the param name naturally.
	merged := make(map[string]interface{}, len(p.Params))
	for _, pp := range p.Params {
		if pp.Default != nil {
			merged[pp.Name] = pp.Default
		}
	}
	for name, v := range supplied {
		pp := declared[name]
		if err := checkParamValueType(name, pp.Type, v); err != nil {
			return err
		}
		merged[name] = v
	}

	// Required-but-missing check. Phrase the error as a bullet
	// list the LLM can turn into a follow-up question per param.
	var missing []string
	for _, pp := range p.Params {
		if !pp.Required {
			continue
		}
		if _, ok := merged[pp.Name]; !ok {
			line := fmt.Sprintf("%s (%s)", pp.Name, pp.Type)
			if pp.Description != "" {
				line += " — " + pp.Description
			}
			missing = append(missing, line)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required parameter(s):\n  - %s", strings.Join(missing, "\n  - "))
	}

	// Substitute into task prompts. We stringify each value with
	// a type-aware formatter so an int param lands as "42", a
	// bool as "true", and a list<string> as one value per line
	// (readable in an LLM prompt).
	strMap := make(map[string]string, len(merged))
	for k, v := range merged {
		strMap[k] = stringifyParamValue(v)
	}
	for i := range p.Tasks {
		t := &p.Tasks[i]
		if t.Prompt != "" {
			t.Prompt = template.ResolveParams(t.Prompt, strMap)
		}
		if t.UserPrompt != "" {
			t.UserPrompt = template.ResolveParams(t.UserPrompt, strMap)
		}
	}
	return nil
}

// stringifyParamValue renders a YAML-decoded parameter value as
// a string suitable for in-prompt substitution. The goal is a
// readable result in the final LLM prompt, not a round-trippable
// encoding.
func stringifyParamValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []interface{}:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, stringifyParamValue(item))
		}
		return strings.Join(parts, "\n")
	case []string:
		return strings.Join(x, "\n")
	}
	return fmt.Sprintf("%v", v)
}

// resolveAction sets default action if not specified.
func resolveAction(t *TaskDef) {
	if t.Action == "" {
		t.Action = "answer"
	}
}

// validate checks the run definition for errors.
func validate(p *Run) ([]string, error) {
	var warnings []string
	if p.Name == "" {
		return nil, fmt.Errorf("run name is required")
	}
	if len(p.Tasks) == 0 {
		return nil, fmt.Errorf("at least one task is required")
	}

	validActions := map[string]bool{
		"answer": true, "contribute": true, "compute": true,
		"review": true, "vote": true,
	}

	// Validate run-level for_each shape (strict: no empty lists).
	if err := validateForEachMap("run", p.ForEach); err != nil {
		return nil, err
	}

	// Validate top-level params: names unique, types recognized,
	// required + default are mutually exclusive (a required param
	// with a default makes no sense — the default would mask the
	// "missing value" error). Descriptions aren't required by the
	// parser, but a warning fires later if one is missing — the
	// LLM needs the prose to turn the param into a question.
	paramNames := make(map[string]bool, len(p.Params))
	for i := range p.Params {
		pp := &p.Params[i]
		if pp.Name == "" {
			return nil, fmt.Errorf("params[%d]: name is required", i)
		}
		if paramNames[pp.Name] {
			return nil, fmt.Errorf("params[%d]: duplicate name %q", i, pp.Name)
		}
		paramNames[pp.Name] = true
		if !isValidParamType(pp.Type) {
			return nil, fmt.Errorf("param %q: invalid type %q (must be string, int, bool, or list<string>)", pp.Name, pp.Type)
		}
		if pp.Required && pp.Default != nil {
			return nil, fmt.Errorf("param %q: required and default are mutually exclusive", pp.Name)
		}
		if pp.Default != nil {
			if err := checkParamValueType(pp.Name, pp.Type, pp.Default); err != nil {
				return nil, err
			}
		}
		if pp.Description == "" {
			warnings = append(warnings, fmt.Sprintf("param %q has no description — the LLM needs prose to turn this into a question for the user", pp.Name))
		}
	}

	ids := make(map[string]bool)
	hasTaskLevelForEach := false
	for i := range p.Tasks {
		t := &p.Tasks[i]

		if t.ID == "" {
			return nil, fmt.Errorf("task ID is required")
		}
		if ids[t.ID] {
			return nil, fmt.Errorf("duplicate task ID %q", t.ID)
		}
		ids[t.ID] = true

		// Set default action
		resolveAction(t)

		// Validate action
		if !validActions[t.Action] {
			return nil, fmt.Errorf("task %q: invalid action %q (must be answer, contribute, compute, review, or vote)", t.ID, t.Action)
		}

		// Validate required fields based on action
		switch t.Action {
		case "answer", "contribute", "review":
			if t.Prompt == "" {
				return nil, fmt.Errorf("task %q: prompt is required for %s action", t.ID, t.Action)
			}
		case "compute":
			if t.Script == "" {
				return nil, fmt.Errorf("task %q: script is required for compute action", t.ID)
			}
		}

		// Review tasks need an explicit target.
		if t.Action == "review" {
			if t.Reviews == "" {
				return nil, fmt.Errorf("task %q: reviews: <target_task_id> is required on review-action tasks", t.ID)
			}
			if t.Reviews == t.ID {
				return nil, fmt.Errorf("task %q: reviews cannot reference the review task itself", t.ID)
			}
		} else if t.Reviews != "" {
			return nil, fmt.Errorf("task %q: reviews is only valid on action: review tasks", t.ID)
		}

		// Vote-action validation. Session 1 ships single-voter
		// only; citizens > 1 is explicitly rejected so authors
		// get a loud error instead of a silent downgrade when
		// they expect multi-voter behavior.
		if t.Action == "vote" {
			if len(t.Options) < 2 {
				return nil, fmt.Errorf("task %q: action: vote requires at least 2 options", t.ID)
			}
			seenOptIDs := make(map[string]bool, len(t.Options))
			for i, opt := range t.Options {
				if opt.ID == "" {
					return nil, fmt.Errorf("task %q: option #%d is missing an id", t.ID, i+1)
				}
				if seenOptIDs[opt.ID] {
					return nil, fmt.Errorf("task %q: duplicate option id %q", t.ID, opt.ID)
				}
				seenOptIDs[opt.ID] = true
			}
			if t.Threshold != "" {
				if err := validateThreshold(t.Threshold); err != nil {
					return nil, fmt.Errorf("task %q: %w", t.ID, err)
				}
			}
			if t.Deadline != "" {
				if _, err := time.ParseDuration(t.Deadline); err != nil {
					return nil, fmt.Errorf("task %q: invalid deadline %q (expected a Go duration like 2h, 30m, 1d is NOT supported — use 24h): %w", t.ID, t.Deadline, err)
				}
			}
			// Phase E.2 session 2a — citizens: N is now supported.
			// The COLLECTING state + multi-claim + per-citizen
			// submission dirs handle the substrate.
			citizens := t.Citizens
			if citizens == 0 {
				citizens = 1
			}
			if t.MinQuorum > 0 && t.MinQuorum > citizens {
				return nil, fmt.Errorf("task %q: min_quorum %d exceeds citizens %d", t.ID, t.MinQuorum, citizens)
			}
			if t.Visibility != "" && t.Visibility != "open" && t.Visibility != "blind" {
				return nil, fmt.Errorf("task %q: invalid visibility %q (must be 'open' or 'blind')", t.ID, t.Visibility)
			}
		} else {
			if len(t.Options) > 0 {
				return nil, fmt.Errorf("task %q: options is only valid on action: vote tasks", t.ID)
			}
			if t.Threshold != "" && t.Action != "review" {
				return nil, fmt.Errorf("task %q: threshold is only valid on action: vote or action: review tasks", t.ID)
			}
			if t.Deadline != "" && t.Action != "review" {
				return nil, fmt.Errorf("task %q: deadline is only valid on action: vote or action: review tasks", t.ID)
			}
			if t.MinQuorum > 0 && t.Action != "review" {
				return nil, fmt.Errorf("task %q: min_quorum is only valid on action: vote or action: review tasks", t.ID)
			}
			// Review-specific field validation. Review tasks
			// may carry their own threshold (dissent policy),
			// deadline, and min_quorum.
			if t.Action == "review" {
				rc := t.Citizens
				if rc == 0 {
					rc = 1
				}
				if t.MinQuorum > 0 && t.MinQuorum > rc {
					return nil, fmt.Errorf("task %q: min_quorum %d exceeds citizens %d", t.ID, t.MinQuorum, rc)
				}
				if t.Deadline != "" {
					if _, err := time.ParseDuration(t.Deadline); err != nil {
						return nil, fmt.Errorf("task %q: invalid deadline %q: %w", t.ID, t.Deadline, err)
					}
				}
				if t.Threshold != "" {
					if err := validateReviewThreshold(t.Threshold); err != nil {
						return nil, fmt.Errorf("task %q: %w", t.ID, err)
					}
				}
				if t.Visibility != "" && t.Visibility != "open" && t.Visibility != "blind" {
					return nil, fmt.Errorf("task %q: invalid visibility %q (must be 'open' or 'blind')", t.ID, t.Visibility)
				}
			} else {
				if t.Anonymize {
					return nil, fmt.Errorf("task %q: anonymize is only valid on action: vote or action: review tasks", t.ID)
				}
				if t.Visibility != "" {
					return nil, fmt.Errorf("task %q: visibility is only valid on action: vote or action: review tasks", t.ID)
				}
			}
		}

		if t.ResultType != "" && t.ResultType != "text" && t.ResultType != "json" && t.ResultType != "file" {
			return nil, fmt.Errorf("task %q: invalid result_type %q", t.ID, t.ResultType)
		}

		if len(t.ForEach) > 0 {
			hasTaskLevelForEach = true
			if err := validateForEachMap("task "+t.ID, t.ForEach); err != nil {
				return nil, err
			}
		}
	}

	// Run-level and task-level for_each are mutually exclusive. Keeping
	// them separate makes the two shapes unambiguous: run-level matrix
	// runs stay exactly as before, task-level runs opt in by declaring
	// for_each only on the tasks that need it.
	if len(p.ForEach) > 0 && hasTaskLevelForEach {
		return nil, fmt.Errorf("run declares a run-level for_each AND task-level for_each on at least one task — these are mutually exclusive; move the for_each block to either the run level or the individual tasks but not both")
	}

	// If multiple tasks declare for_each, they must all agree on the
	// same variable space. A single run only supports one iteration
	// dimension at a time (the common "analyze each X, then summarize"
	// pattern). Differing task-level for_each groups in one run are
	// rejected so users get a clear signal instead of a silently
	// weird DAG.
	if hasTaskLevelForEach {
		var firstID string
		var firstFE map[string][]string
		for i := range p.Tasks {
			t := &p.Tasks[i]
			if len(t.ForEach) == 0 {
				continue
			}
			if firstFE == nil {
				firstID = t.ID
				firstFE = t.ForEach
				continue
			}
			if !forEachEqual(firstFE, t.ForEach) {
				return nil, fmt.Errorf("task %q declares a for_each that differs from task %q — all tasks in a run that use task-level for_each must declare the same variables and values", t.ID, firstID)
			}
		}
	}

	// Second pass: verify all depends_on references exist.
	// Also auto-insert a dependency on the reviews: target so
	// review tasks always run after whatever they review — authors
	// don't have to declare the same relationship twice.
	for i := range p.Tasks {
		t := &p.Tasks[i]
		for _, dep := range t.DependsOn {
			if !ids[dep] {
				return nil, fmt.Errorf("task %q depends on %q which does not exist", t.ID, dep)
			}
		}
		if t.Reviews != "" {
			if !ids[t.Reviews] {
				return nil, fmt.Errorf("task %q: reviews target %q does not exist", t.ID, t.Reviews)
			}
			hasDep := false
			for _, dep := range t.DependsOn {
				if dep == t.Reviews {
					hasDep = true
					break
				}
			}
			if !hasDep {
				t.DependsOn = append(t.DependsOn, t.Reviews)
			}
		}
		// Vote-action: validate `activates:` targets and
		// auto-insert a dep edge from each activated task back to
		// the vote task. This ensures activated branches can't
		// start running until the vote resolves — authors don't
		// have to hand-declare `depends_on: [vote_task]` on every
		// branch root.
		if t.Action == "vote" && len(t.Options) > 0 {
			for i, opt := range t.Options {
				for _, target := range opt.Activates {
					if !ids[target] {
						return nil, fmt.Errorf("task %q: option %q (#%d) activates unknown task %q", t.ID, opt.ID, i+1, target)
					}
					if target == t.ID {
						return nil, fmt.Errorf("task %q: option %q cannot activate the vote task itself", t.ID, opt.ID)
					}
				}
			}
		}
	}
	// Review gating: for every review task R with reviews: T,
	// inject an implicit dep from every other task that
	// consumes T (explicitly via depends_on, or implicitly
	// via {{T.content}} / {{T.field}} template references) to
	// R. This enforces "if you added a review, downstream
	// waits for the verdict" automatically — authors don't
	// have to hand-write `depends_on: [check]` on every task
	// that uses the reviewed draft.
	//
	// Without this pass, draft → {check, publish} runs in
	// parallel and publish can ship an unreviewed draft
	// before the review finishes.
	//
	// Along the way, track which review tasks have zero
	// downstream consumers so we can emit a non-fatal warning
	// (review runs but gates nothing — probably an authoring
	// mistake).
	for i := range p.Tasks {
		reviewTask := &p.Tasks[i]
		if reviewTask.Action != "review" || reviewTask.Reviews == "" {
			continue
		}
		target := reviewTask.Reviews
		consumersCount := 0
		for j := range p.Tasks {
			if i == j {
				continue
			}
			consumer := &p.Tasks[j]
			// Skip other review tasks reviewing the same
			// target — each review is independent and
			// shouldn't wait on another reviewer's verdict.
			if consumer.Action == "review" && consumer.Reviews == target {
				continue
			}
			// Does this task consume the reviewed target?
			// Check explicit deps first, then template
			// references (inferred deps).
			consumes := false
			for _, dep := range consumer.DependsOn {
				if dep == target {
					consumes = true
					break
				}
			}
			if !consumes {
				for _, inferred := range template.InferDependencies(consumer.Prompt) {
					if inferred == target {
						consumes = true
						break
					}
				}
			}
			if !consumes {
				continue
			}
			consumersCount++
			// Inject the review-gating edge.
			hasDep := false
			for _, dep := range consumer.DependsOn {
				if dep == reviewTask.ID {
					hasDep = true
					break
				}
			}
			if !hasDep {
				consumer.DependsOn = append(consumer.DependsOn, reviewTask.ID)
			}
		}
		if consumersCount == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"task %q reviews %q but %q has no downstream consumers — the review runs but gates nothing (possibly an authoring mistake)",
				reviewTask.ID, target, target,
			))
		}
	}

	// Second-and-a-half pass: now that we've walked every task,
	// inject the reverse "activated → vote" dep edges. We couldn't
	// do it inside the loop because the activated tasks might be
	// defined after the vote in source order.
	for i := range p.Tasks {
		voteTask := &p.Tasks[i]
		if voteTask.Action != "vote" {
			continue
		}
		for _, opt := range voteTask.Options {
			for _, target := range opt.Activates {
				for j := range p.Tasks {
					activated := &p.Tasks[j]
					if activated.ID != target {
						continue
					}
					hasDep := false
					for _, dep := range activated.DependsOn {
						if dep == voteTask.ID {
							hasDep = true
							break
						}
					}
					if !hasDep {
						activated.DependsOn = append(activated.DependsOn, voteTask.ID)
					}
					break
				}
			}
		}
	}

	// Third pass — strict template checks (bugs 2/3/4 from feedback):
	//   - every bare {{var}} must resolve against a declared for_each
	//     variable (run-level OR task-level on that specific task)
	//   - every declared for_each variable must be referenced by at
	//     least one task prompt in its visible scope, otherwise it
	//     silently multiplies task count
	//   - every {{task.field}} reference must target a known task id
	//
	// Under run-level for_each, the visible variable scope for every
	// task is the run-level map. Under task-level for_each, the scope
	// for an expanded task is its own for_each map; for a singleton,
	// no variables are visible (bare {{var}} is always an error).
	if err := validateTemplateReferences(p, ids); err != nil {
		return nil, err
	}

	return warnings, nil
}

// validateForEachMap rejects empty for_each variable lists. Each list
// must have at least one value — an empty list silently produces zero
// iterations and therefore zero tasks, which is almost always a bug in
// the upstream process that generated the YAML.
func validateForEachMap(scope string, fe map[string][]string) error {
	for name, values := range fe {
		if len(values) == 0 {
			return fmt.Errorf("%s for_each: variable %q has an empty list — declare at least one value or remove the variable", scope, name)
		}
		for i, v := range values {
			if v == "" {
				return fmt.Errorf("%s for_each: variable %q has an empty string at index %d", scope, name, i)
			}
		}
	}
	return nil
}

// forEachEqual returns true if two for_each maps declare the same
// variables with the same ordered value lists. Used to enforce that all
// task-level for_each blocks in one run share the same iteration space.
func forEachEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if len(va) != len(vb) {
			return false
		}
		for i := range va {
			if va[i] != vb[i] {
				return false
			}
		}
	}
	return true
}

// builtinTemplateVar returns true for bare {{name}} placeholders that
// are reserved runtime substitutions rather than for_each variables.
func builtinTemplateVar(name string) bool {
	switch name {
	case "user_input":
		return true
	}
	return false
}

// validateTemplateReferences walks every task's prompt and user_prompt
// and enforces:
//   - bare {{var}} must match a for_each variable in scope
//   - {{task.field}} must name a real task id
//   - every declared for_each variable must be used by at least one
//     prompt in its scope
//
// Together these catch typos and unused iteration dimensions before
// they leak into a silent-miscount runtime state.
func validateTemplateReferences(p *Run, taskIDs map[string]bool) error {
	runScope := p.ForEach

	// Run-level params (Phase H.1) are visible to every task's
	// prompt, alongside any for_each variables in scope. They're
	// resolved at submission time, not at parse time, so the
	// parser only needs to know *which* names are legal.
	paramInScope := make(map[string]bool, len(p.Params))
	for _, pp := range p.Params {
		paramInScope[pp.Name] = true
	}
	paramReferenced := make(map[string]bool, len(p.Params))

	// Track which for_each variables we've actually seen referenced.
	// Key: scope label ("run" or "task:id"), value: set of referenced
	// variable names. A scope with declared variables but no matching
	// references means some variable is unused — flagged below.
	runScopeReferenced := make(map[string]bool)
	taskScopeReferenced := make(map[string]map[string]bool)

	for i := range p.Tasks {
		t := &p.Tasks[i]
		// Variables visible to this task: run-level OR its own task-level.
		var visible map[string][]string
		var scopeLabel string
		if len(runScope) > 0 {
			visible = runScope
			scopeLabel = "run"
		} else if len(t.ForEach) > 0 {
			visible = t.ForEach
			scopeLabel = "task:" + t.ID
			if taskScopeReferenced[scopeLabel] == nil {
				taskScopeReferenced[scopeLabel] = make(map[string]bool)
			}
		}

		for _, field := range []string{t.Prompt, t.UserPrompt} {
			if field == "" {
				continue
			}
			for _, name := range template.ListParams(field) {
				// Built-in runtime placeholders are always allowed —
				// they're substituted by the client at submission
				// time, not the parser. See docs/task-lifecycle.md.
				if builtinTemplateVar(name) {
					continue
				}
				// Run-level params (Phase H.1) are always in scope
				// for every task. They're substituted at submission
				// time from the caller-supplied values (or defaults).
				if paramInScope[name] {
					paramReferenced[name] = true
					continue
				}
				if _, ok := visible[name]; !ok {
					// Build a friendly list of what WOULD have matched.
					var declared []string
					for k := range visible {
						declared = append(declared, k)
					}
					sort.Strings(declared)
					var upstreamIDs []string
					for id := range taskIDs {
						upstreamIDs = append(upstreamIDs, id)
					}
					sort.Strings(upstreamIDs)
					var runParams []string
					for k := range paramInScope {
						runParams = append(runParams, k)
					}
					sort.Strings(runParams)
					return fmt.Errorf(
						"task %q: prompt references undefined variable {{%s}}; declared for_each variables in scope: %v; run-level params in scope: %v; known task ids: %v",
						t.ID, name, declared, runParams, upstreamIDs,
					)
				}
				if scopeLabel == "run" {
					runScopeReferenced[name] = true
				} else {
					taskScopeReferenced[scopeLabel][name] = true
				}
			}
			// {{task.field}} references must target a known task id.
			for _, ref := range template.ExtractReferences(field) {
				if !taskIDs[ref.TaskID] {
					return fmt.Errorf(
						"task %q: prompt references unknown task id {{%s.%s}}",
						t.ID, ref.TaskID, ref.Field,
					)
				}
			}
		}
	}

	// Unused variable check — any declared for_each variable that
	// never appears in a prompt in its scope is a silent cost
	// multiplier (Cartesian product with no effect on content).
	if len(runScope) > 0 {
		for name := range runScope {
			if !runScopeReferenced[name] {
				return fmt.Errorf(
					"run-level for_each variable %q is declared but never referenced in any task prompt — remove it or reference it via {{%s}}",
					name, name,
				)
			}
		}
	}
	for i := range p.Tasks {
		t := &p.Tasks[i]
		if len(t.ForEach) == 0 {
			continue
		}
		label := "task:" + t.ID
		seen := taskScopeReferenced[label]
		for name := range t.ForEach {
			if !seen[name] {
				return fmt.Errorf(
					"task %q: for_each variable %q is declared but never referenced in its prompt",
					t.ID, name,
				)
			}
		}
	}

	return nil
}

// build constructs the DAG and expands for_each parameters. There are
// two mutually-exclusive expansion modes, selected purely by where
// for_each is declared:
//
//  1. Run-level (p.ForEach set): every task is expanded N times — one
//     per iteration — and dependencies within an iteration stay scoped
//     to that iteration (per-iteration binding). This is the original
//     matrix-style model, untouched here so existing runs keep working
//     exactly as before.
//
//  2. Task-level (any task.ForEach set): only tasks that declare their
//     own for_each are expanded; others remain singletons. A singleton
//     depending on an expanded task receives all iterations as a
//     fan-in (resolved to an aggregated block at claim time). An
//     expanded task depending on a singleton sees the same singleton
//     across every iteration.
func build(p *Run) (*ParsedRun, error) {
	if hasAnyTaskForEach(p.Tasks) {
		return buildTaskLevel(p)
	}
	return buildRunLevel(p)
}

// hasAnyTaskForEach returns true if any task declares its own for_each.
func hasAnyTaskForEach(tasks []TaskDef) bool {
	for i := range tasks {
		if len(tasks[i].ForEach) > 0 {
			return true
		}
	}
	return false
}

// buildRunLevel is the original expansion: every task gets one instance
// per run-level iteration. Preserved as-is so existing run-level
// for_each users get identical behavior after this change.
func buildRunLevel(p *Run) (*ParsedRun, error) {
	instances := expandForEach(p.ForEach)

	result := &ParsedRun{
		Run:           p,
		DAG:           dag.New(),
		ExpandedTasks: make(map[string][]TaskInstance),
	}

	taskIDs := make(map[string]bool)
	for _, t := range p.Tasks {
		taskIDs[t.ID] = true
	}

	for _, inst := range instances {
		var taskInstances []TaskInstance

		for _, taskDef := range p.Tasks {
			fullID := MakeFullID(inst.key, taskDef.ID)

			resolvedPrompt := template.ResolveParams(taskDef.Prompt, inst.params)
			resolvedUserPrompt := template.ResolveParams(taskDef.UserPrompt, inst.params)

			allDeps := template.MergeDependencies(taskDef.DependsOn, taskDef.Prompt)
			for _, dep := range allDeps {
				if !taskIDs[dep] {
					return nil, fmt.Errorf("task %q references %q which does not exist", taskDef.ID, dep)
				}
			}

			// Dependencies in run-level mode resolve within the current
			// iteration: foundation depends on prior-same-iteration tasks.
			resolvedDeps := make([]string, 0, len(allDeps))
			for _, dep := range allDeps {
				resolvedDeps = append(resolvedDeps, MakeFullID(inst.key, dep))
			}

			ti := TaskInstance{
				TaskDef:     taskDef,
				InstanceKey: inst.key,
				Params:      inst.params,
				FullID:      fullID,
			}
			ti.Prompt = resolvedPrompt
			ti.UserPrompt = resolvedUserPrompt
			ti.DependsOn = resolvedDeps
			if ti.Requirements == nil {
				ti.Requirements = p.Requirements
			}
			ti.ReadsArtifacts = template.MergeArtifactReads(taskDef.ReadsArtifacts, taskDef.Prompt)

			taskInstances = append(taskInstances, ti)

			data := map[string]interface{}{
				"instance_key": inst.key,
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(fullID, taskDef.Action, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", fullID, err)
			}
		}

		for _, ti := range taskInstances {
			for _, parentID := range ti.DependsOn {
				if err := result.DAG.AddEdge(parentID, ti.FullID); err != nil {
					return nil, fmt.Errorf("adding edge %s -> %s: %w", parentID, ti.FullID, err)
				}
			}
		}

		result.ExpandedTasks[inst.key] = taskInstances
	}

	if err := result.DAG.Validate(); err != nil {
		return nil, fmt.Errorf("DAG validation: %w", err)
	}

	return result, nil
}

// buildTaskLevel handles the task-level for_each mode. Tasks with
// for_each become N instances sharing an iteration dimension; tasks
// without for_each become singletons. Dependency wiring depends on
// both sides being expanded (per-iteration binding) or either side
// being a singleton (fan-in / fan-out).
func buildTaskLevel(p *Run) (*ParsedRun, error) {
	// Find the shared iteration space (already validated to be
	// uniform across all tasks that declare for_each).
	var shared map[string][]string
	for i := range p.Tasks {
		if len(p.Tasks[i].ForEach) > 0 {
			shared = p.Tasks[i].ForEach
			break
		}
	}
	iterations := expandForEach(shared)

	result := &ParsedRun{
		Run:           p,
		DAG:           dag.New(),
		ExpandedTasks: make(map[string][]TaskInstance),
	}

	taskIDs := make(map[string]bool)
	expandedTaskDef := make(map[string]bool) // which task_def_ids are fan-out
	for i := range p.Tasks {
		taskIDs[p.Tasks[i].ID] = true
		if len(p.Tasks[i].ForEach) > 0 {
			expandedTaskDef[p.Tasks[i].ID] = true
		}
	}

	// Step 1 — create every TaskInstance (without dep wiring yet), add
	// DAG nodes, and group by instanceKey so the downstream loop in
	// handleCreateRun sees the same [instanceKey]->[]TaskInstance shape
	// as the run-level path. Singletons live under key "".
	singletons := make([]TaskInstance, 0)
	// expanded[taskDefID][iterationKey] = short fullID
	expanded := make(map[string]map[string]string)

	createInstance := func(taskDef TaskDef, iter forEachInstance) TaskInstance {
		fullID := MakeFullID(iter.key, taskDef.ID)
		resolvedPrompt := template.ResolveParams(taskDef.Prompt, iter.params)
		resolvedUserPrompt := template.ResolveParams(taskDef.UserPrompt, iter.params)
		ti := TaskInstance{
			TaskDef:     taskDef,
			InstanceKey: iter.key,
			Params:      iter.params,
			FullID:      fullID,
		}
		ti.Prompt = resolvedPrompt
		ti.UserPrompt = resolvedUserPrompt
		if ti.Requirements == nil {
			ti.Requirements = p.Requirements
		}
		ti.ReadsArtifacts = template.MergeArtifactReads(taskDef.ReadsArtifacts, taskDef.Prompt)
		return ti
	}

	for _, taskDef := range p.Tasks {
		if len(taskDef.ForEach) == 0 {
			// Singleton.
			singleton := forEachInstance{key: "", params: map[string]string{}}
			ti := createInstance(taskDef, singleton)
			singletons = append(singletons, ti)
			data := map[string]interface{}{
				"instance_key": "",
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(ti.FullID, taskDef.Action, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", ti.FullID, err)
			}
			continue
		}
		// Expanded.
		expanded[taskDef.ID] = make(map[string]string, len(iterations))
		for _, iter := range iterations {
			ti := createInstance(taskDef, iter)
			result.ExpandedTasks[iter.key] = append(result.ExpandedTasks[iter.key], ti)
			expanded[taskDef.ID][iter.key] = ti.FullID
			data := map[string]interface{}{
				"instance_key": iter.key,
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(ti.FullID, taskDef.Action, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", ti.FullID, err)
			}
		}
	}
	if len(singletons) > 0 {
		result.ExpandedTasks[""] = singletons
	}

	// Step 2 — resolve dependencies into concrete short IDs and wire
	// DAG edges. For a given TaskInstance (child) and each declared
	// dep task_def_id (parent):
	//
	//   parent expanded, child expanded → per-iteration binding:
	//     parent at child.InstanceKey → child
	//   parent expanded, child singleton → fan-in:
	//     every instance of parent → child
	//   parent singleton, child expanded → fan-out:
	//     parent → every instance of child (handled per child)
	//   parent singleton, child singleton → straight edge
	wireDeps := func(ti *TaskInstance) error {
		allDeps := template.MergeDependencies(ti.TaskDef.DependsOn, ti.TaskDef.Prompt)
		for _, dep := range allDeps {
			if !taskIDs[dep] {
				return fmt.Errorf("task %q references %q which does not exist", ti.TaskDef.ID, dep)
			}
		}

		var resolved []string
		for _, dep := range allDeps {
			parentExpanded := expandedTaskDef[dep]
			childExpanded := ti.InstanceKey != ""

			switch {
			case parentExpanded && childExpanded:
				// Per-iteration binding.
				parentID := expanded[dep][ti.InstanceKey]
				if parentID == "" {
					return fmt.Errorf("task %q iteration %q: missing expected upstream %q", ti.TaskDef.ID, ti.InstanceKey, dep)
				}
				resolved = append(resolved, parentID)
			case parentExpanded && !childExpanded:
				// Fan-in into the singleton. Deterministic iteration
				// order is critical so the aggregated result at claim
				// time is reproducible run-over-run.
				keys := make([]string, 0, len(expanded[dep]))
				for k := range expanded[dep] {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					resolved = append(resolved, expanded[dep][k])
				}
			default:
				// parent is a singleton — one edge regardless of child side.
				resolved = append(resolved, dep)
			}
		}
		ti.DependsOn = resolved
		for _, parentID := range resolved {
			if err := result.DAG.AddEdge(parentID, ti.FullID); err != nil {
				return fmt.Errorf("adding edge %s -> %s: %w", parentID, ti.FullID, err)
			}
		}
		return nil
	}

	// Walk instances in the same order as step 1 so errors mention a
	// predictable "first bad" task if any.
	for key, list := range result.ExpandedTasks {
		for i := range list {
			ti := &list[i]
			if err := wireDeps(ti); err != nil {
				return nil, err
			}
		}
		result.ExpandedTasks[key] = list
	}

	if err := result.DAG.Validate(); err != nil {
		return nil, fmt.Errorf("DAG validation: %w", err)
	}

	return result, nil
}

type forEachInstance struct {
	key    string
	params map[string]string
}

// expandForEach generates all combinations of for_each parameters.
// For now, supports single for_each variable (most common case).
// Multi-variable cartesian product can be added later.
func expandForEach(forEach map[string][]string) []forEachInstance {
	if len(forEach) == 0 {
		// No expansion — single instance with empty key
		return []forEachInstance{{key: "", params: map[string]string{}}}
	}

	// Single variable expansion (most common: for_each: disease: [...])
	if len(forEach) == 1 {
		for varName, values := range forEach {
			instances := make([]forEachInstance, 0, len(values))
			for _, val := range values {
				instances = append(instances, forEachInstance{
					key:    val,
					params: map[string]string{varName: val},
				})
			}
			return instances
		}
	}

	// Multi-variable: cartesian product. Sort variable names so the
	// order of dimensions within the generated slug — and therefore
	// the task IDs, iteration labels, and sort order of run_status —
	// is deterministic across runs. Without this, Go's randomized map
	// iteration leaks through: a run with gene+tissue might produce
	// `BRCA1_breast` one time and `breast_BRCA1` the next.
	keys := make([]string, 0, len(forEach))
	for k := range forEach {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([][]string, len(keys))
	for i, k := range keys {
		vals[i] = forEach[k]
	}

	return cartesianProduct(keys, vals)
}

func cartesianProduct(keys []string, vals [][]string) []forEachInstance {
	if len(keys) == 0 {
		return []forEachInstance{{key: "", params: map[string]string{}}}
	}

	var result []forEachInstance
	var generate func(depth int, current map[string]string)
	generate = func(depth int, current map[string]string) {
		if depth == len(keys) {
			// Build instance key from all param values
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, current[k])
			}
			key := strings.Join(parts, "_")
			params := make(map[string]string)
			for k, v := range current {
				params[k] = v
			}
			result = append(result, forEachInstance{key: key, params: params})
			return
		}

		for _, v := range vals[depth] {
			current[keys[depth]] = v
			generate(depth+1, current)
		}
	}
	generate(0, make(map[string]string))
	return result
}

// MakeFullID constructs a full task ID from instance key and task ID.
func MakeFullID(instanceKey, taskID string) string {
	if instanceKey == "" {
		return taskID
	}
	return instanceKey + ":" + taskID
}

// validateThreshold accepts the recognized vote-threshold forms.
// Supported values: "plurality", "majority", "unanimous", and
// "percent:N" where N is an integer 1..100. Case-insensitive.
func validateThreshold(s string) error {
	lower := strings.ToLower(s)
	switch lower {
	case "plurality", "majority", "unanimous":
		return nil
	}
	if strings.HasPrefix(lower, "percent:") {
		numStr := strings.TrimPrefix(lower, "percent:")
		n, err := strconv.Atoi(numStr)
		if err != nil {
			return fmt.Errorf("invalid threshold %q: percent value must be an integer", s)
		}
		if n < 1 || n > 100 {
			return fmt.Errorf("invalid threshold %q: percent must be between 1 and 100", s)
		}
		return nil
	}
	return fmt.Errorf("invalid threshold %q (must be 'plurality', 'majority', 'unanimous', or 'percent:N')", s)
}

// validateReviewThreshold accepts the dissent-policy shapes for
// action:review tasks. Supported values: "any-reject-kills"
// (default, first reject resolves as reject), "majority-approve"
// (more than half must approve), "unanimous-approve" (all must
// approve), and "percent:N" where N is 1..100 (at least N% must
// approve). Case-insensitive. Left as a separate function from
// validateThreshold because vote and review threshold vocabularies
// are disjoint — "plurality" doesn't mean anything for a
// binary-verdict review.
func validateReviewThreshold(s string) error {
	lower := strings.ToLower(s)
	switch lower {
	case "any-reject-kills", "majority-approve", "unanimous-approve":
		return nil
	}
	if strings.HasPrefix(lower, "percent:") {
		numStr := strings.TrimPrefix(lower, "percent:")
		n, err := strconv.Atoi(numStr)
		if err != nil {
			return fmt.Errorf("invalid review threshold %q: percent value must be an integer", s)
		}
		if n < 1 || n > 100 {
			return fmt.Errorf("invalid review threshold %q: percent must be between 1 and 100", s)
		}
		return nil
	}
	return fmt.Errorf("invalid review threshold %q (must be 'any-reject-kills', 'majority-approve', 'unanimous-approve', or 'percent:N')", s)
}

// ensureDurationParses is a thin wrapper documenting intent at the
// call site. Parsing a duration is cheap; the point of the helper
// is that tests and callers can refer to it by name.
var _ = time.ParseDuration

// isValidParamType reports whether s is a recognized top-level
// run parameter type. We deliberately keep the type vocabulary
// small — richer types (enums, JSON schema, cross-param
// constraints) are future work. What's here is enough for the
// LLM to build a natural-language interview out of a param
// block.
func isValidParamType(s string) bool {
	switch s {
	case "string", "int", "bool", "list<string>":
		return true
	}
	return false
}

// checkParamValueType reports whether v is assignable to a param
// declared as paramType. Used both for default-value validation
// at parse time and for supplied-value validation at submission
// time. YAML decoding yields int for `1`, bool for `true`,
// string for `"pcos"`, []interface{} for `[a, b]` — we accept
// those shapes.
//
// Error messages use natural-English type names ("a number",
// "true/false") and quote the offending value so the LLM can
// forward them to the user as conversation turns. Go-level
// type names like `float64` never appear in user-facing output.
func checkParamValueType(name, paramType string, v interface{}) error {
	switch paramType {
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("param %q: expected a string, got %s", name, describeValue(v))
		}
	case "int":
		switch v.(type) {
		case int, int64:
			return nil
		case float64:
			// YAML/JSON decode whole numbers as float64. Accept
			// them if they're integral; reject if they're not.
			f := v.(float64)
			if f == float64(int64(f)) {
				return nil
			}
			return fmt.Errorf("param %q: expected a whole number, got a decimal (%v)", name, v)
		}
		return fmt.Errorf("param %q: expected a whole number, got %s", name, describeValue(v))
	case "bool":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("param %q: expected true or false, got %s", name, describeValue(v))
		}
	case "list<string>":
		xs, ok := v.([]interface{})
		if !ok {
			if _, isStringSlice := v.([]string); isStringSlice {
				return nil
			}
			return fmt.Errorf("param %q: expected a list of strings, got %s", name, describeValue(v))
		}
		for i, item := range xs {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("param %q: list item #%d is not a string — %s", name, i+1, describeValue(item))
			}
		}
	}
	return nil
}

// describeValue renders a value the way a user would describe
// it in English ("a string ('foo')", "a number (123)",
// "true/false (false)"), not the way Go's %T reflects on it.
// Used in param type-mismatch errors so the LLM can forward
// them verbatim without exposing Go-internal type names like
// `float64` or `[]interface {}`.
func describeValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("a string (%q)", x)
	case bool:
		return fmt.Sprintf("a boolean (%v)", x)
	case int:
		return fmt.Sprintf("a number (%d)", x)
	case int64:
		return fmt.Sprintf("a number (%d)", x)
	case float64:
		return fmt.Sprintf("a number (%v)", x)
	case []interface{}, []string:
		return "a list"
	case map[string]interface{}:
		return "an object"
	case nil:
		return "null"
	}
	return "an unrecognized value"
}
