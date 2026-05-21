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

// M7 (bughunt): the canonical glob fan-in. The aggregator never
// references {{analyze...}} in a prompt, but it READS the glob path
// data/{{items[*]}}/analysis.txt — which is exactly the per-iteration
// path data/{{item}}/analysis.txt that analyze WRITES. Template-aware
// path matching must recognize this as consumption and NOT warn.
func TestCollects_GlobFanInRead_NoWarn(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Collects glob fan-in"
version: 1
params:
  - name: items
    type: list<string>
    default: [a, b]
for_each: { item: "{{items}}" }
tasks:
  - id: analyze
    action: compute
    script: scripts/analyze.sh
    prompt: "analyze {{item}}"
    writes:
      - data/{{item}}/analysis.txt
  - id: aggregate
    action: compute
    script: scripts/aggregate.sh
    collects: analyze
    prompt: "aggregate"
    reads:
      - data/{{items[*]}}/analysis.txt
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hasC6Warn(parsed.Warnings) {
		t.Fatalf("glob fan-in read of the target's written path must suppress the warning; got: %v", parsed.Warnings)
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

func hasNoDescWarn(ws []string, param string) bool {
	for _, w := range ws {
		if strings.Contains(w, "param "+`"`+param+`"`) && strings.Contains(w, "no description") {
			return true
		}
	}
	return false
}

// L8 (bughunt): a param used only as a for_each fan-out axis (never
// in an LLM/human prompt) must NOT get the "no description" warning —
// it's never turned into a question. The pure-compute pipeline shape.
func TestParam_ForEachOnly_NoDescriptionWarn(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "for_each-only param"
version: 1
params:
  - name: items
    type: list<string>
    default: [a, b]
for_each: { item: "{{items}}" }
tasks:
  - id: work
    action: compute
    script: scripts/work.sh
    prompt: "process {{item}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hasNoDescWarn(parsed.Warnings, "items") {
		t.Fatalf("a for_each-only param must not warn about a missing description; got: %v", parsed.Warnings)
	}
}

// Positive control for L8: a param actually referenced in an answer
// prompt still warns when it has no description (the LLM needs the
// prose to ask the user).
func TestParam_InPrompt_StillWarnsNoDescription(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "prompt param needs description"
version: 1
params:
  - name: disease
    type: string
tasks:
  - id: analyze
    action: answer
    prompt: "Analyze {{disease}} and report findings."
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !hasNoDescWarn(parsed.Warnings, "disease") {
		t.Fatalf("a prompt-referenced param with no description must still warn; got: %v", parsed.Warnings)
	}
}
