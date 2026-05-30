package yaml

import (
	"testing"
)

// TestForEach_EnvSubstitution is the regression guard for the
// most-expensive-wall feedback: `env: SYMBOL: "{{gene}}"` in a for_each
// task used to silently pass the LITERAL string "{{gene}}" to the
// script (per-instance materialization substituted prompts and writes
// but skipped env). Pin that each instance now gets the substituted
// iteration variable, and that sibling instances don't share an env map.
func TestForEach_EnvSubstitution(t *testing.T) {
	yamlData := []byte(`
name: "env substitution"
version: 1
for_each:
  gene: [TP53, ARID1A, APP]
tasks:
  - id: compute
    action: compute
    script: scripts/run.sh
    prompt: "process {{gene}}"
    env:
      SYMBOL: "{{gene}}"
      STATIC: "fixed"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{"TP53": "TP53", "ARID1A": "ARID1A", "APP": "APP"}
	for gene, wantSym := range want {
		inst := parsed.ExpandedTasks[gene][0]
		if got := inst.Env["SYMBOL"]; got != wantSym {
			t.Errorf("instance %s: env.SYMBOL = %q, want %q (the per-instance for_each var must be substituted in env: values, not passed as the literal template)", gene, got, wantSym)
		}
		if got := inst.Env["STATIC"]; got != "fixed" {
			t.Errorf("instance %s: env.STATIC = %q, want %q (static env values must pass through unchanged)", gene, got, "fixed")
		}
	}
	// (The per-instance SYMBOL check above also catches map-sharing:
	// if instances shared the def's backing map, sibling writes would
	// clobber each other and all three would carry SYMBOL=APP — the
	// last gene processed.)
}
