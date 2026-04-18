// Package yaml parses Enju run definition files.
//
// The package is split by pipeline stage:
//
//   - types.go     — struct definitions + custom
//                    UnmarshalYAML methods.
//   - parse.go     — public entry points (Parse,
//                    ParseWithParams, ParseFile) and param
//                    substitution helpers.
//   - validate.go  — shape checks, decomposed into narrow
//                    sub-validators.
//   - foreach.go   — for_each parsing / validation /
//                    substitution / expansion.
//   - build.go     — DAG construction.
//   - parser.go (this file) — small shared utilities:
//                    threshold parsing, param-type checks,
//                    value stringification.
package yaml

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)










// validate checks the run definition for errors.
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
