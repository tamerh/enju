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
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/common/template"
	yamlv3 "gopkg.in/yaml.v3"
)

// ParseFile reads and parses a run YAML file, resolving any
// top-level `include:` directive first (see FlattenIncludes). A
// file with no `include:` is read through byte-identical.
func ParseFile(path string) (*ParsedRun, error) {
	data, err := FlattenFile(path)
	if err != nil {
		return nil, err
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
		if err := checkParamValueType(pp, v); err != nil {
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
	if err := substituteForEachParamRefs(p.ForEach, merged, "run", p.Params); err != nil {
		return nil, err
	}

	for i := range p.Tasks {
		t := &p.Tasks[i]
		// List-expansion refs (`{{param[*]}}`) run BEFORE
		// scalar substitution: expand each element into N
		// entries based on a list<string> param, then let the
		// scalar pass fill in any remaining `{{scalar}}` refs.
		// Runs on every list-valued field that a template
		// author would reasonably want to scale from one
		// element to many without having to enumerate: the
		// writer side of N per-item files, the reader side,
		// a dynamic reviewer pool, and a list of prereq
		// tasks. for_each: values deliberately aren't covered
		// — they already accept a list-valued param directly.
		// options[].activates stays structural.
		scope := fmt.Sprintf("task %q", t.ID)
		expandedWrites, err := expandStarRefsInWrites(t.WritesArtifacts, merged, declared, scope+".writes_artifacts")
		if err != nil {
			return nil, err
		}
		t.WritesArtifacts = expandedWrites
		expandedReads, err := expandStarRefsInSlice([]string(t.ReadsArtifacts), merged, declared, scope+".reads_artifacts")
		if err != nil {
			return nil, err
		}
		t.ReadsArtifacts = expandedReads
		expandedAssign, err := expandStarRefsInSlice([]string(t.AssignTo), merged, declared, scope+".assign_to")
		if err != nil {
			return nil, err
		}
		t.AssignTo = expandedAssign
		expandedDeps, err := expandStarRefsInSlice([]string(t.DependsOn), merged, declared, scope+".depends_on")
		if err != nil {
			return nil, err
		}
		t.DependsOn = expandedDeps

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
		substituteStringSliceInPlace(t.Volumes, strMap)
		substituteWriteArtifactsInPlace(t.WritesArtifacts, strMap)
		for k, v := range t.Env {
			t.Env[k] = template.ResolveParams(v, strMap)
		}

		// Task-level for_each param-ref substitution. Shared
		// helper also handles the run-level scope above.
		if err := substituteForEachParamRefs(t.ForEach, merged, fmt.Sprintf("task %q", t.ID), p.Params); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// starRefPattern matches the list-expansion syntax used in
// list-valued fields (writes_artifacts, reads_artifacts,
// assign_to, depends_on):
//
//   {{name[*]}}          list<string>: one entry per string.
//                        list<record>: one entry per record,
//                        value = the record's key: field.
//   {{name[*].field}}    list<record>: one entry per record,
//                        value = record[field] (field must be
//                        a declared fields: entry).
//
// Each list element containing a `[*]` ref expands into N
// entries. Group 1 = param name; group 2 = optional record
// field (empty for the bare form).
//
// Deliberately a separate pattern from the general `{{name}}`
// ref — the bracket suffix makes the list-expansion intent
// explicit (opt-in, not magical) and keeps the scalar path
// untouched.
var starRefPattern = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_]*)\[\*\](?:\.([A-Za-z_][A-Za-z0-9_]*))?\}\}`)

// expandStarRefsInSlice duplicates list elements containing
// `{{param[*]}}` into N entries, one per value in the
// referenced list<string> param. Elements without a `[*]`
// ref pass through unchanged.
//
// Multiple `[*]` refs in one element would imply a cartesian
// product — rejected up front to keep the semantics
// predictable and avoid accidental blowup (`[{{a[*]}}-{{b[*]}}]`
// with two 10-element lists silently becomes a 100-element
// list). If a real cartesian use case appears, add an
// explicit `[*,*]` cross-product syntax then.
//
// Wrong-type or unknown param refs fail the parse rather
// than passing the literal placeholder through to
// validators — a typo on `items` surfaces as "unknown
// parameter" instead of "artifact path {{items[*]}}
// invalid."
//
// `scope` is free-form context for error messages
// ("writes_artifacts on task X", "assign_to on task Y").
func expandStarRefsInSlice(items []string, merged map[string]interface{}, declared map[string]*ParamDef, scope string) ([]string, error) {
	if len(items) == 0 {
		return items, nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		expanded, err := expandOneStarElement(item, merged, declared, scope)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
	}
	return out, nil
}

// expandOneStarElement returns the 1-or-N entries a single
// list element produces after `{{param[*]}}` /
// `{{param[*].field}}` expansion.
func expandOneStarElement(item string, merged map[string]interface{}, declared map[string]*ParamDef, scope string) ([]string, error) {
	matches := starRefPattern.FindAllStringSubmatch(item, -1)
	if len(matches) == 0 {
		return []string{item}, nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%s: element %q contains multiple [*] refs; only one is supported per element", scope, item)
	}
	full := matches[0][0]  // the exact `{{name[*]}}` or `{{name[*].field}}` text
	name := matches[0][1]
	field := matches[0][2] // "" for the bare form
	raw, ok := merged[name]
	if !ok {
		return nil, fmt.Errorf("%s: %s references unknown parameter %q", scope, full, name)
	}
	values, err := starExpansionValues(name, field, raw, declared[name], scope)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, strings.ReplaceAll(item, full, v))
	}
	return out, nil
}

// starExpansionValues resolves a `[*]` ref to its expansion
// values, dispatching on the declared param type:
//
//   - list<record>: `{{p[*].f}}` → record[f] for each record;
//     bare `{{p[*]}}` → the record's key: field (same
//     bare-record==key rule as for_each {{entry}}). The field
//     must be a declared fields: entry — an unknown field is a
//     hard parse error naming the declared set, so a typo
//     surfaces here, not as a silently-broken artifact path.
//   - list<string> (or no declared ParamDef): `{{p[*]}}` →
//     each string. A `.field` suffix on a string list is an
//     error — strings have no fields.
func starExpansionValues(name, field string, raw interface{}, pd *ParamDef, scope string) ([]string, error) {
	if pd != nil && pd.Type == "list<record>" {
		eff := field
		if eff == "" {
			eff = pd.Key // defaulted to the first declared field at validate time
		}
		if eff == "" {
			return nil, fmt.Errorf("%s: list<record> parameter %q has no key field to expand {{%s[*]}}", scope, name, name)
		}
		if _, declaredField := pd.Fields.TypeOf(eff); !declaredField {
			return nil, fmt.Errorf("%s: {{%s[*].%s}} references unknown field %q on list<record> %q; declared fields: %s",
				scope, name, field, eff, name, strings.Join(pd.Fields.Names(), ", "))
		}
		recs, ok := starRecordList(raw)
		if !ok {
			// Curative: when it's a list but an element isn't a
			// record, name that element rather than the list type.
			if lst, isList := raw.([]interface{}); isList {
				for i, e := range lst {
					if _, isMap := e.(map[string]interface{}); !isMap {
						return nil, fmt.Errorf("%s: {{%s[*]}} expects list<record> values, but element #%d is %T, not a record", scope, name, i+1, e)
					}
				}
			}
			return nil, fmt.Errorf("%s: {{%s[*]}} expects a list<record> parameter; got %T", scope, name, raw)
		}
		out := make([]string, 0, len(recs))
		for _, rec := range recs {
			out = append(out, stringifyParamValue(rec[eff]))
		}
		return out, nil
	}
	// list<string> path (or param with no declared ParamDef).
	if field != "" {
		return nil, fmt.Errorf("%s: {{%s[*].%s}} uses .field, which requires a list<record> parameter; %q is not list<record>", scope, name, field, name)
	}
	values, ok := starListValues(raw)
	if !ok {
		return nil, fmt.Errorf("%s: {{%s[*]}} requires a list<string> parameter; got %T", scope, name, raw)
	}
	return values, nil
}

// starListValues normalizes a YAML-decoded list param to
// []string. Accepts []string directly and []interface{} with
// string-compatible elements (the common YAML decode shape).
// Returns false if the value isn't list-shaped or contains
// non-stringable elements — the caller emits a typed error
// with the param name.
func starListValues(raw interface{}) ([]string, bool) {
	switch v := raw.(type) {
	case []string:
		return v, true
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// starRecordList normalizes a list<record> param value to
// []map[string]interface{}. The param validator enforces the
// []interface{}-of-map[string]interface{} shape for list<record>
// — that's what BOTH YAML decode and a --params-file
// (json.Unmarshal into interface{}) produce, so it's the only
// shape that reaches here in practice. The []map[string]
// interface{} arm is defensive for a directly-constructed value
// (e.g. a future in-process caller) and is not a pipeline path.
// Returns false on any other shape.
func starRecordList(raw interface{}) ([]map[string]interface{}, bool) {
	switch v := raw.(type) {
	case []map[string]interface{}:
		return v, true
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(v))
		for _, e := range v {
			m, ok := e.(map[string]interface{})
			if !ok {
				return nil, false
			}
			out = append(out, m)
		}
		return out, true
	}
	return nil, false
}

// expandStarRefsInWrites is the WriteArtifacts analogue of
// expandStarRefsInSlice — one input entry with `{{param[*]}}`
// in its Path becomes N entries, each carrying the same
// Track and Optional flags. Track and Optional are literal
// bools — never param refs — so the per-expansion entry
// inherits them verbatim from the source declaration.
func expandStarRefsInWrites(ws WriteArtifacts, merged map[string]interface{}, declared map[string]*ParamDef, scope string) (WriteArtifacts, error) {
	if len(ws) == 0 {
		return ws, nil
	}
	out := make(WriteArtifacts, 0, len(ws))
	for _, w := range ws {
		paths, err := expandOneStarElement(w.Path, merged, declared, scope)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			out = append(out, WriteArtifact{
				Path:     p,
				Track:    w.Track,
				Optional: w.Optional,
			})
		}
	}
	return out, nil
}

// substituteStringSliceInPlace walks a []string (or the
// yamlStringList alias) and replaces every {{paramname}} ref
// with its resolved string value. No-op for empty slices.
// Used by substituteParamsInPlace for list-shaped per-field
// slots like AssignTo / ReadsArtifacts.
func substituteStringSliceInPlace[S ~[]string](s S, strMap map[string]string) {
	for i := range s {
		s[i] = template.ResolveParams(s[i], strMap)
	}
}

// substituteWriteArtifactsInPlace runs {{param}} substitution
// across every entry's Path. Track is unaffected — it's a
// literal YAML bool, never a param ref. Mirrors
// substituteStringSliceInPlace, split out because WriteArtifacts
// is a []struct not a []string.
func substituteWriteArtifactsInPlace(ws WriteArtifacts, strMap map[string]string) {
	for i := range ws {
		ws[i].Path = template.ResolveParams(ws[i].Path, strMap)
	}
}

// ResolveWriteArtifacts returns a new WriteArtifacts slice with
// every Path resolved through strMap. Used by build.go when
// materializing per-instance task copies — the source slice is
// shared across instances, so a fresh allocation is required.
// Exported so `engine/materialize.go` can reuse the same rule
// when dynamic for_each materializes deferred instances.
func ResolveWriteArtifacts(ws WriteArtifacts, strMap map[string]string) WriteArtifacts {
	if len(ws) == 0 {
		return nil
	}
	out := make(WriteArtifacts, len(ws))
	for i, e := range ws {
		out[i] = WriteArtifact{
			Path:     template.ResolveParams(e.Path, strMap),
			Track:    e.Track,
			Optional: e.Optional,
		}
	}
	return out
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
	paramByName := make(map[string]*ParamDef, len(p.Params))
	for i := range p.Params {
		paramInScope[p.Params[i].Name] = true
		paramByName[p.Params[i].Name] = &p.Params[i]
	}
	paramReferenced := make(map[string]bool, len(p.Params))

	// forEachVarParam maps a for_each variable name to the list<record>
	// ParamDef it iterates over (nil if the source is not a record param).
	// Used to validate {{var.field}} field names at parse time.
	forEachVarParam := make(map[string]*ParamDef, len(runScope))
	for varName, src := range runScope {
		if paramName, ok := parseForEachParamRef(src.Ref); ok {
			if pd, found := paramByName[paramName]; found && pd.Type == "list<record>" {
				forEachVarParam[varName] = pd
			}
		}
	}

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
			// Exception: {{forEachVar.field}} where forEachVar is in
			// scope is a record field ref, not a task ref — treat it
			// as a use of that for_each variable and validate the field
			// name against the declared fields: of the record param.
			for _, ref := range template.ExtractReferences(field) {
				if visible[ref.TaskID] {
					// for_each record field ref — validate field name
					// when the source is a known list<record> param.
					if pd, isRecord := forEachVarParam[ref.TaskID]; isRecord {
						if _, declared := pd.Fields.TypeOf(ref.Field); !declared {
							return fmt.Errorf(
								"task %q: prompt references field {{%s.%s}} but %q is not declared in param %q's fields: (known fields: %v)",
								t.ID, ref.TaskID, ref.Field, ref.Field, pd.Name, pd.Fields.Names(),
							)
						}
					}
					// Count as variable use.
					if scopeLabel == "run" {
						runScopeReferenced[ref.TaskID] = true
					} else if scopeLabel != "" {
						taskScopeReferenced[scopeLabel][ref.TaskID] = true
					}
					continue
				}
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
