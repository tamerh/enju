package yaml

// For_each machinery: parsing references, validating shapes,
// expanding literal lists into iteration instances, and
// substituting param refs at instantiation time.
//
// The file covers the whole for_each lifecycle:
//
//   - parseForEachRef / parseForEachParamRef: extract
//     {{task.field}} or {{paramname}} from a source string.
//   - validateForEachLiteralMap / validateForEachMap /
//     validateDynamicForEach: enforce shape + dereferenced
//     correctness at parse time.
//   - substituteForEachParamRefs: resolve {{paramname}} refs
//     against supplied param values, converting them to
//     literal Values (mutates in place).
//   - expandForEach / cartesianProduct / forEachInstance:
//     turn a literal map into the list of iteration instances
//     build.go consumes.
//   - forEachEqual / builtinTemplateVar: small utilities used
//     by the template-reference validator in parse.go.

import (
	"fmt"
	"sort"
	"strings"
)

// substituteForEachParamRefs walks a ForEachMap and replaces
// every `{{paramname}}` reference with the param's list<string>
// value from the merged param map. Task references are left
// alone — dynamic materialization handles those at run time.
// Errors are phrased for LLM forwarding (the scope label lands
// in the prefix so "task \"foo\"" or "run" shows up first).
func substituteForEachParamRefs(fe ForEachMap, merged map[string]interface{}, scope string) error {
	if len(fe) == 0 {
		return nil
	}
	for name, src := range fe {
		if src.Ref == "" {
			continue
		}
		paramName, ok := parseForEachParamRef(src.Ref)
		if !ok {
			continue // task ref — leave for dynamic materialization
		}
		v, haveValue := merged[paramName]
		if !haveValue {
			return fmt.Errorf("%s for_each variable %q: parameter %q is required to have a value (supply it when creating the run, or give the param a default list)", scope, name, paramName)
		}
		list, ok := v.([]string)
		if !ok {
			// JSON-decoded params arrive as []interface{};
			// YAML-decoded arrive as []string. Coerce the
			// former.
			if raw, ok := v.([]interface{}); ok {
				coerced := make([]string, 0, len(raw))
				for idx, item := range raw {
					s, ok := item.(string)
					if !ok {
						return fmt.Errorf("%s for_each variable %q: parameter %q element %d is not a string", scope, name, paramName, idx)
					}
					coerced = append(coerced, s)
				}
				list = coerced
			} else {
				return fmt.Errorf("%s for_each variable %q: parameter %q must be a list of strings (got %T)", scope, name, paramName, v)
			}
		}
		if err := validateForEachLiteralMap(scope, map[string][]string{name: list}); err != nil {
			return err
		}
		fe[name] = ForEachSource{Values: list}
	}
	return nil
}
// validateDynamicForEach checks every dynamic for_each
// variable's reference. Two valid shapes:
//
//   1. {{upstream_task.field_name}} — dynamic fan-out from
//      another task's list<string> output. The upstream must
//      exist and declare the field with format: list<string>.
//   2. {{paramname}} — parameterized fan-out from a top-level
//      param. The param must be declared and typed as
//      list<string>. Substitution happens in substituteParamsInPlace
//      when the run is actually instantiated; at parse-time
//      we only check the reference shape and declaration.
//
// Errors are phrased so the LLM can forward them as
// natural-language feedback.
func validateDynamicForEach(p *Run, taskIDs map[string]bool) error {
	// Build a quick lookup of outputs per task def.
	taskByID := make(map[string]*TaskDef, len(p.Tasks))
	for i := range p.Tasks {
		taskByID[p.Tasks[i].ID] = &p.Tasks[i]
	}
	// Params declared at the top level. Used to resolve
	// param-ref shapes in for_each (form 2 above).
	paramByName := make(map[string]*ParamDef, len(p.Params))
	for i := range p.Params {
		paramByName[p.Params[i].Name] = &p.Params[i]
	}

	for i := range p.Tasks {
		t := &p.Tasks[i]
		if len(t.ForEach) == 0 || !t.ForEach.IsDynamic() {
			continue
		}
		for name, src := range t.ForEach {
			if src.Ref == "" {
				continue
			}
			// Param-ref shape: {{paramname}}, no dot. Resolve
			// against the top-level params block. The ref
			// stays in place through parse; substituteParamsInPlace
			// substitutes it when the run is instantiated.
			if paramName, ok := parseForEachParamRef(src.Ref); ok {
				pd, declared := paramByName[paramName]
				if !declared {
					return fmt.Errorf("task %q: for_each variable %q: references unknown parameter %q — declare it under top-level params: or use {{upstream_task.field_name}} for a dynamic task reference", t.ID, name, paramName)
				}
				if pd.Type != "list<string>" {
					return fmt.Errorf("task %q: for_each variable %q: parameter %q must be declared with type: list<string> to serve as a for_each source (got %q)", t.ID, name, paramName, pd.Type)
				}
				continue
			}
			upstreamID, field, ok := parseForEachRef(src.Ref)
			if !ok {
				return fmt.Errorf("task %q: for_each variable %q: value %q is not a valid template reference (expected \"{{upstream_task.field_name}}\" or \"{{paramname}}\")", t.ID, name, src.Ref)
			}
			upstream, found := taskByID[upstreamID]
			if !found {
				return fmt.Errorf("task %q: for_each variable %q references unknown upstream task %q", t.ID, name, upstreamID)
			}
			if upstreamID == t.ID {
				return fmt.Errorf("task %q: for_each variable %q references itself — a task cannot fan out over its own output", t.ID, name)
			}
			outSpec, hasOutput := upstream.Outputs[field]
			if !hasOutput {
				return fmt.Errorf("task %q: for_each variable %q references {{%s.%s}}, but task %q does not declare an output field %q", t.ID, name, upstreamID, field, upstreamID, field)
			}
			// OutputSpec format can be either a plain string
			// description (simple form) or the full object
			// with a Format field. Phase J.1 requires the full
			// object form with format: list<string> so we can
			// tell the coordinator it's iterable.
			format := strings.ToLower(strings.TrimSpace(outSpec.Format))
			if format != "list<string>" {
				return fmt.Errorf("task %q: for_each variable %q references {{%s.%s}}, but that output must be declared with format: list<string> (got %q) — dynamic for_each needs a typed list source", t.ID, name, upstreamID, field, outSpec.Format)
			}
		}
	}
	return nil
}
// validateForEachLiteralMap rejects empty for_each variable
// lists. Used for run-level for_each (which is always static)
// and for literal task-level variables after unpacking.
func validateForEachLiteralMap(scope string, fe map[string][]string) error {
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
// validateForEachMap validates a task-level ForEachMap. Each
// variable's source is either a literal list (must be
// non-empty) or a template reference scalar. Accepted ref
// shapes: `{{upstream_task.field}}` (dynamic fan-out from a
// sibling task's list output) and `{{paramname}}` (fan-out
// from a top-level param). The existence checks happen later
// in validateTemplateReferences / validateDynamicForEach once
// the task ID + param tables are built.
func validateForEachMap(scope string, fe ForEachMap) error {
	for name, src := range fe {
		switch {
		case src.Ref != "":
			// Accept either task-ref shape ({{x.y}}) or
			// param-ref shape ({{paramname}}). Deeper
			// validation (existence, type) happens later.
			if _, _, ok := parseForEachRef(src.Ref); ok {
				continue
			}
			if _, ok := parseForEachParamRef(src.Ref); ok {
				continue
			}
			return fmt.Errorf("%s for_each: variable %q: %q is not a template reference — expected \"{{task.field}}\" (dynamic upstream) or \"{{paramname}}\" (top-level param)", scope, name, src.Ref)
		case len(src.Values) == 0:
			return fmt.Errorf("%s for_each: variable %q has an empty list — declare at least one value, a \"{{upstream.field}}\" reference, or remove the variable", scope, name)
		default:
			for i, v := range src.Values {
				if v == "" {
					return fmt.Errorf("%s for_each: variable %q has an empty string at index %d", scope, name, i)
				}
			}
		}
	}
	return nil
}
// parseForEachRef extracts (taskID, field) from a template
// reference string like "{{discover.gene_symbols}}". Returns
// the parts plus an ok flag. A non-reference scalar (or a
// bare "{{param}}" with no dot) returns ok=false — param refs
// are handled separately by parseForEachParamRef.
func parseForEachRef(s string) (taskID, field string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{{") || !strings.HasSuffix(s, "}}") {
		return "", "", false
	}
	inner := strings.TrimSpace(s[2 : len(s)-2])
	dot := strings.Index(inner, ".")
	if dot <= 0 || dot == len(inner)-1 {
		return "", "", false
	}
	return inner[:dot], inner[dot+1:], true
}
// parseForEachParamRef extracts a top-level param name from a
// reference string like "{{genes}}" (no dot). Returns the
// param name + ok=true when the string is a bare-identifier
// template ref; returns ok=false for anything else (including
// task refs with dots, which parseForEachRef handles).
func parseForEachParamRef(s string) (paramName string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{{") || !strings.HasSuffix(s, "}}") {
		return "", false
	}
	inner := strings.TrimSpace(s[2 : len(s)-2])
	if inner == "" || strings.ContainsAny(inner, ". :") {
		return "", false
	}
	// Only plain identifiers are treated as param refs. Anything
	// weirder (colons, spaces, etc.) falls through so the
	// top-level task-ref validator surfaces a clearer error.
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_'
		if !ok {
			return "", false
		}
	}
	return inner, true
}
// forEachEqual returns true if two ForEachMaps declare the
// same variables with matching sources. For literal sources,
// the value lists must match in order. For reference sources,
// the refs must match exactly.
func forEachEqual(a, b ForEachMap) bool {
	if len(a) != len(b) {
		return false
	}
	for k, sa := range a {
		sb, ok := b[k]
		if !ok {
			return false
		}
		if sa.Ref != sb.Ref {
			return false
		}
		if len(sa.Values) != len(sb.Values) {
			return false
		}
		for i := range sa.Values {
			if sa.Values[i] != sb.Values[i] {
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
