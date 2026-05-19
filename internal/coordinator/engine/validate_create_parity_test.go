package engine

import (
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// TestValidateCreateParity is the C-cluster's whole point: the
// `enju validate` pre-flight (enjuYaml.Parse / ParseWithParams)
// must reject EVERY YAML the create path (ParseWithParams +
// ValidateRunCreation) rejects. Before the fix, validate was a
// materially weaker pass — these exact inputs got a green ✓ from
// validate while create refused them, a false pre-flight.
//
// The assertion is parity, not just "create rejects": for each
// case we confirm the create path fails AND that the parse-time
// path (what `enju validate` runs) also fails. If a future change
// re-weakens validate, this test goes red.
func TestValidateCreateParity(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		params map[string]interface{}
	}{
		{
			name: "[*] on scalar param",
			yaml: `
name: s
version: 1
params:
  - { name: n, type: string, required: true, description: d }
tasks:
  - id: seed
    action: answer
    writes: ["state/{{n[*]}}.json"]
    prompt: "x"
`,
			params: map[string]interface{}{"n": "solo"},
		},
		{
			name: "double [*] in one element",
			yaml: `
name: d
version: 1
params:
  - { name: a, type: list<string>, required: true, description: d }
  - { name: b, type: list<string>, required: true, description: d }
tasks:
  - id: seed
    action: answer
    writes: ["s/{{a[*]}}-{{b[*]}}.json"]
    prompt: "x"
`,
			params: map[string]interface{}{
				"a": []interface{}{"x"}, "b": []interface{}{"y"},
			},
		},
		{
			name: "write escapes with ..",
			yaml: `
name: esc
version: 1
tasks:
  - id: a
    action: answer
    writes: ["../outside.txt"]
    prompt: "x"
`,
		},
		{
			name: "absolute write path",
			yaml: `
name: abs
version: 1
tasks:
  - id: a
    action: answer
    writes: ["/etc/passwd"]
    prompt: "x"
`,
		},
		{
			name: "write into reserved enju/ dir",
			yaml: `
name: res
version: 1
tasks:
  - id: a
    action: answer
    writes: ["enju/state.txt"]
    prompt: "x"
`,
		},
		{
			name: "{{ghost[*]}} undeclared param",
			yaml: `
name: ghost
version: 1
tasks:
  - id: seed
    action: answer
    writes: ["state/{{ghost[*]}}.json"]
    prompt: "x"
`,
		},
	}

	eng := New(&mockStore{}, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The create path: ParseWithParams then (if it
			// parsed) ValidateRunCreation. It must reject.
			createRejected := false
			parsed, perr := enjuYaml.ParseWithParams([]byte(tc.yaml), tc.params)
			if perr != nil {
				createRejected = true
			} else if verr := eng.ValidateRunCreation(parsed); verr != nil {
				createRejected = true
			}
			if !createRejected {
				t.Fatalf("create path unexpectedly ACCEPTED %q — test fixture is stale", tc.name)
			}

			// The `enju validate` path: bare Parse (no params).
			// Parity requires it to reject the same input.
			if _, verr := enjuYaml.Parse([]byte(tc.yaml)); verr == nil {
				t.Fatalf("PARITY VIOLATION: create rejects %q but `enju validate` (Parse) accepts it — pre-flight is weaker than create", tc.name)
			}

			// And the params-aware parse (enju go --dry-run /
			// MCP create entry point) must reject it too.
			if _, verr := enjuYaml.ParseWithParams([]byte(tc.yaml), tc.params); verr == nil {
				t.Fatalf("PARITY VIOLATION: create rejects %q but ParseWithParams accepts it", tc.name)
			}
		})
	}
}
