package main

import (
	"fmt"
	"strconv"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// coerceCLIParams converts the string→string map that
// parseParamsArg produces into the typed map that
// enjuYaml.ParseWithParams expects. Looks each supplied key up
// in the workflow's declared `params:` block and parses the
// string according to the declared type.
//
// Why this exists: the CLI's `--params k=v,k=v` syntax has no
// type tagging — every value arrives as a string. The MCP
// path doesn't hit this because the MCP transport decodes the
// `params` object via encoding/json, which produces float64
// for numeric literals and bool for true/false. To behave
// identically to the MCP path, the CLI has to convert at the
// boundary, using the declared types as the source of truth.
//
// Unknown keys (supplied but not declared) are passed through
// as strings — the validator's "unknown param name" error
// fires there with a clearer message than the type-coercer
// could produce. The substituteParams stage rejects them
// downstream.
//
// list<string> coercion accepts comma-separated values inside
// the string (`--params "genes=TP53|BRCA1"` is awkward; for
// list-typed params the CLI form is rarely used and the
// recommended path is the MCP tool, but we handle the simple
// case for completeness).
func coerceCLIParams(raw map[string]interface{}, declared []enjuYaml.ParamDef) (map[string]interface{}, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	byName := make(map[string]*enjuYaml.ParamDef, len(declared))
	for i := range declared {
		byName[declared[i].Name] = &declared[i]
	}
	out := make(map[string]interface{}, len(raw))
	for k, v := range raw {
		s, isStr := v.(string)
		if !isStr {
			// Already a typed value (test path, future use).
			out[k] = v
			continue
		}
		def := byName[k]
		if def == nil {
			// Pass through — the validator will reject unknown
			// keys with a better message than we can produce.
			out[k] = s
			continue
		}
		coerced, err := coerceParam(def, s)
		if err != nil {
			return nil, err
		}
		out[k] = coerced
	}
	return out, nil
}

// coerceParam converts a raw string from the CLI to the type
// declared in the workflow's params: block. The four supported
// types match enjuYaml's checkParamValueType: string, int,
// bool, list<string>.
func coerceParam(def *enjuYaml.ParamDef, s string) (interface{}, error) {
	switch def.Type {
	case "", "string":
		return s, nil
	case "int":
		// strconv.ParseInt is strict ("90" → 90, "90.0" → error,
		// "ninety" → error). Matches checkParamValueType's
		// int-only semantics — no surprise truncation.
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("--params %s=%q: %s declares type int but value is not an integer", def.Name, s, def.Name)
		}
		return n, nil
	case "bool":
		t := strings.TrimSpace(strings.ToLower(s))
		switch t {
		case "true", "yes", "1":
			return true, nil
		case "false", "no", "0":
			return false, nil
		}
		return nil, fmt.Errorf("--params %s=%q: %s declares type bool but value is not true/false (also accepted: yes/no, 1/0)", def.Name, s, def.Name)
	case "list<string>":
		// Comma-separated splitting can't coexist with the outer
		// `,` separator parseParamsArg uses, so the CLI shape
		// for list params is "k=a|b|c" (pipe-separated).
		// Documented in `enju go --help` description for --params.
		if strings.TrimSpace(s) == "" {
			return []interface{}{}, nil
		}
		parts := strings.Split(s, "|")
		out := make([]interface{}, 0, len(parts))
		for _, p := range parts {
			out = append(out, strings.TrimSpace(p))
		}
		return out, nil
	}
	// Unknown declared type — pass through and let the
	// validator complain about the type field itself.
	return s, nil
}
