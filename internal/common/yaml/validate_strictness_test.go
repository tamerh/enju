package yaml

// Group C bug-hunt: validate should CATCH more statically-decidable
// mistakes (M6 for_each non-list, L1 unknown version, L3 undeclared
// output field) rather than letting them surface at run time.

import (
	"strings"
	"testing"
)

// M6: a for_each variable sourced from a scalar param (string) must
// fail validate — for_each fans out over list elements. Pre-fix this
// passed validate and only failed at run materialization.
func TestForEach_NonListParam_Errors(t *testing.T) {
	_, err := Parse([]byte(`
name: "for_each over a scalar param"
version: 1
params:
  - name: thing
    type: string
for_each: { item: "{{thing}}" }
tasks:
  - id: t
    action: answer
    prompt: "do {{item}}"
`))
	if err == nil {
		t.Fatal("expected an error for for_each over a non-list param, got nil")
	}
	if !strings.Contains(err.Error(), "requires a list param") {
		t.Fatalf("error should explain the list requirement, got: %v", err)
	}
}

// M6 positive control: for_each over a list<string> param is fine.
func TestForEach_ListParam_OK(t *testing.T) {
	_, err := Parse([]byte(`
name: "for_each over a list param"
version: 1
params:
  - name: things
    type: list<string>
    default: [a, b]
for_each: { item: "{{things}}" }
tasks:
  - id: t
    action: answer
    prompt: "do {{item}}"
`))
	if err != nil {
		t.Fatalf("for_each over a list param must validate: %v", err)
	}
}

// L1: an unknown schema version validates (forward-tolerant) but
// warns so a future schema roll has a clean signal.
func TestVersion_Unknown_Warns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "future version"
version: 99
tasks:
  - id: t
    action: answer
    prompt: hi
`))
	if err != nil {
		t.Fatalf("unknown version must not be fatal: %v", err)
	}
	if !hasWarnContaining(parsed.Warnings, "unknown schema version 99") {
		t.Fatalf("expected an unknown-version warning, got: %v", parsed.Warnings)
	}
}

// L1: version 1 (and omitted) must NOT warn.
func TestVersion_Known_NoWarn(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "current version"
version: 1
tasks:
  - id: t
    action: answer
    prompt: hi
`))
	if err != nil {
		t.Fatal(err)
	}
	if hasWarnContaining(parsed.Warnings, "unknown schema version") {
		t.Fatalf("version 1 must not warn; got: %v", parsed.Warnings)
	}
}

// L3: a {{task.field}} ref to a non-builtin field on a task that
// declares NO outputs warns (it resolves to empty / leaks the literal
// marker unless the result is JSON with that key).
func TestUndeclaredOutputField_NoOutputs_Warns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "undeclared output field"
version: 1
tasks:
  - id: a
    action: answer
    prompt: "produce something"
  - id: b
    action: answer
    prompt: "use {{a.nonexistent_field}}"
`))
	if err != nil {
		t.Fatalf("undeclared field must be non-fatal (warning): %v", err)
	}
	if !hasWarnContaining(parsed.Warnings, "declares no outputs") {
		t.Fatalf("expected the no-outputs field-ref warning, got: %v", parsed.Warnings)
	}
}

// L3 positive control: {{a.content}} (builtin) on a no-outputs task
// must NOT warn.
func TestUndeclaredOutputField_ContentRef_NoWarn(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "content ref is always fine"
version: 1
tasks:
  - id: a
    action: answer
    prompt: "produce something"
  - id: b
    action: answer
    prompt: "use {{a.content}}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if hasWarnContaining(parsed.Warnings, "declares no outputs") {
		t.Fatalf("{{a.content}} must never warn; got: %v", parsed.Warnings)
	}
}

func hasWarnContaining(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
