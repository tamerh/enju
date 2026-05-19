package yaml

import (
	"strings"
	"testing"
)

// The "validation authoritative" cluster: `enju validate`
// (Parse) used to be a materially weaker pass than the create
// path (ParseWithParams + engine.ValidateRunCreation) despite the
// docs promising they "flatten identically". These tests pin the
// parse-time rejections so the pre-flight can no longer bless YAML
// the create path refuses (C1/C2/C3), plus the C5 collision and
// C7 unknown-field static checks.

// TestParseRejectsReservedArtifactDir — C1. Writing into Enju's
// own state / git internals / the template-bundle root must be
// rejected at parse time (it was enforced nowhere for `enju/`,
// and only on create for `.enju/` `.git/`).
func TestParseRejectsReservedArtifactDir(t *testing.T) {
	for _, p := range []string{"enju/state.txt", ".enju/x", ".git/config", "enju"} {
		y := []byte(`
name: reserved
version: 1
tasks:
  - id: a
    action: answer
    writes: ["` + p + `"]
    prompt: "w"
`)
		if _, err := Parse(y); err == nil {
			t.Errorf("Parse: expected rejection of reserved writes path %q", p)
		}
	}
}

// TestParseRejectsListExpansionShape — C2 + C3. `[*]` on a scalar
// param, two `[*]` in one element, and `[*]` on an undeclared
// param must all fail Parse (they used to pass validate and
// survive create as literal write paths).
func TestParseRejectsListExpansionShape(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "scalar param",
			yaml: `
name: s
version: 1
params:
  - { name: n, type: string, required: true }
tasks:
  - id: t
    action: answer
    writes: ["state/{{n[*]}}.json"]
    prompt: "x"
`,
			want: "requires a list<string> parameter",
		},
		{
			name: "double star",
			yaml: `
name: d
version: 1
params:
  - { name: a, type: list<string>, required: true }
  - { name: b, type: list<string>, required: true }
tasks:
  - id: t
    action: answer
    writes: ["s/{{a[*]}}-{{b[*]}}.json"]
    prompt: "x"
`,
			want: "multiple [*] refs",
		},
		{
			name: "undeclared param",
			yaml: `
name: u
version: 1
tasks:
  - id: t
    action: answer
    writes: ["state/{{ghost[*]}}.json"]
    prompt: "x"
`,
			want: "unknown parameter",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse: expected rejection for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%s): got %v, want substring %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestParseAcceptsValidListExpansion guards against the C2/C3
// fix over-rejecting: a correctly-typed list<string> star-ref
// must still parse.
func TestParseAcceptsValidListExpansion(t *testing.T) {
	y := []byte(`
name: ok
version: 1
params:
  - { name: items, type: list<string>, required: true, description: d }
tasks:
  - id: t
    action: answer
    writes: ["state/{{items[*]}}.json"]
    prompt: "x"
`)
	if _, err := Parse(y); err != nil {
		t.Fatalf("Parse rejected a valid list<string> star-ref: %v", err)
	}
}

// TestParseRejectsParamForEachCollision — C5. A name declared as
// BOTH a param and a run-level for_each var is a parser error
// (was a factually-wrong "never referenced" message before).
func TestParseRejectsParamForEachCollision(t *testing.T) {
	y := []byte(`
name: clash
version: 1
params:
  - { name: disease, type: string, required: true, description: d }
for_each:
  disease: [a, b]
tasks:
  - id: t
    action: answer
    prompt: "Analyze {{disease}}"
`)
	_, err := Parse(y)
	if err == nil {
		t.Fatal("Parse: expected rejection of param/for_each name collision")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BOTH a top-level param and a run-level for_each") {
		t.Fatalf("collision message should name the real cause; got: %v", err)
	}
	// The old message falsely claimed the var was "never
	// referenced" even though the prompt literally used it.
	if strings.Contains(msg, "never referenced") {
		t.Fatalf("collision message must NOT claim the var is unreferenced; got: %v", err)
	}
}

// TestParseRejectsUnknownOutputFieldOnKnownTask — C7. A ref to
// an undeclared output field on a task that DOES declare an
// explicit outputs: block is statically decidable; catch it at
// parse time instead of deferring to claim time.
func TestParseRejectsUnknownOutputFieldOnKnownTask(t *testing.T) {
	y := []byte(`
name: field
version: 1
tasks:
  - id: a
    action: answer
    prompt: "produce"
    outputs:
      summary: { format: md }
  - id: b
    action: answer
    prompt: "use {{a.gene_list}}"
`)
	_, err := Parse(y)
	if err == nil {
		t.Fatal("Parse: expected rejection of {{a.gene_list}} (a declares only 'summary')")
	}
	if !strings.Contains(err.Error(), "declares no output") {
		t.Fatalf("got %v, want an undeclared-output message", err)
	}
}

// TestDeadlineErrorMessageIsGrammatical — C8. The vote-task
// deadline error had an ungrammatical run-on parenthetical
// ("...like 2h, 30m, 1d is NOT supported — use 24h"). The
// reworded message must read cleanly and still name the cause.
func TestDeadlineErrorMessageIsGrammatical(t *testing.T) {
	y := []byte(`
name: dl
version: 1
tasks:
  - id: v
    action: vote
    deadline: "24"
    options:
      - { id: yes }
      - { id: no }
    prompt: "vote"
`)
	_, err := Parse(y)
	if err == nil {
		t.Fatal("Parse: expected rejection of deadline \"24\"")
	}
	msg := err.Error()
	// The old garbled phrasing must be gone.
	if strings.Contains(msg, "like 2h, 30m, 1d is NOT supported") {
		t.Fatalf("deadline message still contains the garbled run-on: %v", err)
	}
	// The reworded message must still be actionable.
	if !strings.Contains(msg, "24h") || !strings.Contains(msg, "Go duration") {
		t.Fatalf("deadline message lost its guidance: %v", err)
	}
}

// TestParseAcceptsBuiltinFieldsOnTaskWithOutputs — C7 guard:
// {{X.content}} / {{X.responses}} are always valid even when X
// declares an explicit outputs: block that doesn't list them
// (they're client-side result projections).
func TestParseAcceptsBuiltinFieldsOnTaskWithOutputs(t *testing.T) {
	y := []byte(`
name: builtin
version: 1
tasks:
  - id: a
    action: answer
    prompt: "produce"
    outputs:
      summary: { format: md }
  - id: b
    action: answer
    prompt: "use {{a.content}} and {{a.summary}}"
`)
	if _, err := Parse(y); err != nil {
		t.Fatalf("Parse rejected valid {{a.content}}/{{a.summary}} refs: %v", err)
	}
}
