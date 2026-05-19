package yaml

// C6 (bughunt): an aggregator that `collects:` a fanned target but
// never references the collected fan-in does pointless fan-in
// waiting and surfaces zero content. The other collects rules are
// hard errors (target missing / not fanned / collects+for_each);
// this one is a non-fatal authoring hint (escalated by -strict),
// and it must not false-positive on a correctly-wired aggregator.

import (
	"strings"
	"testing"
)

func hasC6Warn(ws []string) bool {
	for _, w := range ws {
		if strings.Contains(w, "nothing references the collected fan-in") {
			return true
		}
	}
	return false
}

// The exact finding repro: collects analyze, but synth's prompt
// has no {{analyze...}} ref → warning.
func TestCollects_NoConsumer_Warns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Collects no reads"
version: 1
for_each: { item: [alpha, beta, gamma] }
tasks:
  - id: analyze
    action: answer
    prompt: "Analyze {{item}}"
  - id: synth
    action: answer
    collects: analyze
    prompt: "Synthesize across all items (no template ref to analyze here)."
`))
	if err != nil {
		t.Fatalf("parse (warning must be non-fatal): %v", err)
	}
	if !hasC6Warn(parsed.Warnings) {
		t.Fatalf("expected the collects-no-consumer warning; got: %v", parsed.Warnings)
	}
}

// Correctly wired: synth references {{analyze.content}} → NO warn.
func TestCollects_ContentRef_NoWarn(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Collects with ref"
version: 1
for_each: { item: [a, b] }
tasks:
  - id: analyze
    action: answer
    prompt: "Analyze {{item}}"
  - id: synth
    action: answer
    collects: analyze
    prompt: "Synthesize: {{analyze.content}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hasC6Warn(parsed.Warnings) {
		t.Fatalf("a referenced aggregator must NOT warn; got: %v", parsed.Warnings)
	}
}

// Consumed via a fan/index ref ({{analyze[*]}}) from a third task
// — still consumed, no warn (conservative: any ref suppresses).
func TestCollects_IndexRef_NoWarn(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Collects fan ref"
version: 1
for_each: { item: [a, b] }
tasks:
  - id: analyze
    action: answer
    prompt: "Analyze {{item}}"
  - id: synth
    action: answer
    collects: analyze
    prompt: "Reduce {{analyze[*]}} into one."
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hasC6Warn(parsed.Warnings) {
		t.Fatalf("an index/fan ref must suppress the warning; got: %v", parsed.Warnings)
	}
}
