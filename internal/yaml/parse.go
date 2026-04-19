package yaml

// Parse-path entry points + per-stage helpers. This file hosts
// the public Parse / ParseWithParams / ParseFile surface, the
// internal pipeline (parseInternal) that orchestrates decode →
// validate → substitute → build, and the helpers involved in
// the substitute stage (substituteParamsInPlace, stringifyParamValue,
// resolveAction, validateTemplateReferences).
//
// The pipeline shape is deliberately linear: each stage takes
// the Run, does its thing (sometimes mutating in place), and
// hands off. Mutation is explicit in the helper names where it
// happens (substituteForEachParamRefs lives in foreach.go).
// validate() lives in validate.go; build() in build.go.

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/template"
	yamlv3 "gopkg.in/yaml.v3"
)

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
// parseInternal is the linear pipeline every Parse entry point
// funnels through:
//
//   1. decode:           YAML bytes → raw Run struct.
//   2. resolveDefaults:  fill in missing defaults (e.g. action).
//      Pure mutation — no errors, no checks.
//   3. validate:         shape checks + implicit-edge derivation.
//      Returns fatal errors + non-fatal author warnings.
//   4. substituteParams: resolve {{paramname}} refs against the
//      supplied values (only when called via ParseWithParams).
//   5. build:            assemble the ParsedRun (DAG +
//      TaskInstances + DeferredTaskDefs).
//
// Each stage is a top-level function in its own file; this
// function is just the orchestrator. Readers tracing behavior
// should start here and follow each call.
func parseInternal(data []byte, paramValues map[string]interface{}, substitute bool) (*ParsedRun, error) {
	var prob Run
	dec := yamlv3.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&prob); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	resolveDefaults(&prob)

	warnings, err := validate(&prob)
	if err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}

	var merged map[string]interface{}
	if substitute {
		var err error
		merged, err = substituteParamsInPlace(&prob, paramValues)
		if err != nil {
			return nil, err
		}
	}

	parsed, err := build(&prob)
	if err != nil {
		return nil, fmt.Errorf("building DAG: %w", err)
	}
	parsed.Warnings = warnings
	parsed.MergedParams = merged

	return parsed, nil
}

// resolveDefaults fills in fields the author may have omitted.
// This is explicit defaulting — runs as a separate stage so
// validate() doesn't have to do it as a side effect. Extend
// this function when new fields gain defaults; don't scatter
// defaulting across validators.
func resolveDefaults(p *Run) {
	for i := range p.Tasks {
		resolveAction(&p.Tasks[i])
	}
}
// substituteParamsInPlace merges supplied parameter values with declared
// defaults, validates required params are present and types
// match, and substitutes `{{param}}` references in every task
// prompt. Errors are phrased in natural language so the LLM can
// forward them to the user as conversational follow-ups.
func substituteParamsInPlace(p *Run, supplied map[string]interface{}) (map[string]interface{}, error) {
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
			return nil, fmt.Errorf("this run does not declare any parameters, but values were supplied for: %s", strings.Join(keys, ", "))
		}
		return nil, nil
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
			return nil, fmt.Errorf("unknown parameter %q — this run declares: %s", name, strings.Join(known, ", "))
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
			return nil, err
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
		return nil, fmt.Errorf("missing required parameter(s):\n  - %s", strings.Join(missing, "\n  - "))
	}

	// Substitute into task prompts. We stringify each value with
	// a type-aware formatter so an int param lands as "42", a
	// bool as "true", and a list<string> as one value per line
	// (readable in an LLM prompt).
	strMap := make(map[string]string, len(merged))
	for k, v := range merged {
		strMap[k] = stringifyParamValue(v)
	}
	// Substitute {{paramname}} refs in run-level for_each.
	// Same pattern as task-level (below) but resolved once at
	// the run scope so buildRunLevel sees a static map.
	if err := substituteForEachParamRefs(p.ForEach, merged, "run"); err != nil {
		return nil, err
	}

	for i := range p.Tasks {
		t := &p.Tasks[i]
		// Substitute in every string / list-of-strings field
		// that downstream validators compare against a strict
		// pattern (username, role, artifact path, script path).
		// If we only substituted prompts, a template author who
		// parameterizes `assign_to: "{{reviewer}}"` would hit a
		// "malformed username {{reviewer}}" error from the
		// engine's ValidateRunCreation because the raw
		// placeholder reaches validation unchanged. The full
		// list below is "every author-facing field that the
		// per-task validators enforce a format on."
		t.Prompt = template.ResolveParams(t.Prompt, strMap)
		t.UserPrompt = template.ResolveParams(t.UserPrompt, strMap)
		t.Script = template.ResolveParams(t.Script, strMap)
		t.RequireRole = template.ResolveParams(t.RequireRole, strMap)
		t.Reviews = template.ResolveParams(t.Reviews, strMap)
		substituteStringSliceInPlace(t.AssignTo, strMap)
		substituteStringSliceInPlace(t.ReadsArtifacts, strMap)
		substituteStringSliceInPlace(t.WritesArtifacts, strMap)
		for k, v := range t.Env {
			t.Env[k] = template.ResolveParams(v, strMap)
		}

		// Task-level for_each param-ref substitution. Shared
		// helper also handles the run-level scope above.
		if err := substituteForEachParamRefs(t.ForEach, merged, fmt.Sprintf("task %q", t.ID)); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// substituteStringSliceInPlace walks a []string (or the
// yamlStringList alias) and replaces every {{paramname}} ref
// with its resolved string value. No-op for empty slices.
// Used by substituteParamsInPlace for list-shaped per-field
// slots like AssignTo / WritesArtifacts / ReadsArtifacts.
func substituteStringSliceInPlace[S ~[]string](s S, strMap map[string]string) {
	for i := range s {
		s[i] = template.ResolveParams(s[i], strMap)
	}
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
		// Variables visible to this task: run-level OR its own
		// task-level. For validation we only need the NAMES —
		// the source (static list vs dynamic ref) doesn't
		// change visibility; a dynamic for_each variable is
		// still a valid placeholder the prompt can reference.
		visible := make(map[string]bool)
		var scopeLabel string
		if len(runScope) > 0 {
			for name := range runScope {
				visible[name] = true
			}
			scopeLabel = "run"
		} else if len(t.ForEach) > 0 {
			for name := range t.ForEach {
				visible[name] = true
			}
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
