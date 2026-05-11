package yaml

// Tests for the `aggregates:` task field — a marker that keeps
// a task singular regardless of expansion mode and gathers all
// instances of a fanned source into one fan-in. Pairs with the
// existing resolve.go fan-in content-aggregation path, which
// joins N source instances into one block when a singleton
// depends on a fanned upstream.
//
// Three layers under test here:
//   1. Parser/validator (Aggregates target exists, is fanned,
//      no self-reference, not combined with for_each).
//   2. Expander — singletons in run-level mode (the user-reported
//      bug: "5 items × 6 stages = 31 expected but 35 got because
//      the aggregator was multiplied too").
//   3. Expander — singletons remain singular in task-level mode
//      (the existing buildTaskLevel path already produces the
//      right topology via parentExpanded && !childExpanded, but
//      the new validator rules must accept that shape).

import (
	"strings"
	"testing"
)

// helperParse runs the full Parse pipeline (decode + validate +
// build) on the supplied YAML so the test exercises the real
// validator + builder, not just the in-memory expand.
func helperParse(t *testing.T, yamlSrc string) (*ParsedRun, error) {
	t.Helper()
	return Parse([]byte(yamlSrc))
}

// TestAggregates_RunLevel_OneSingletonAggregator reproduces the
// user-reported bug. Five items, three stages per item, one
// aggregator. Pre-fix this yielded 16 tasks (5×3 + 5); post-fix
// it should yield 16 tasks → wait no, 5×3 + 1 = 16. Adjust:
// 5 items × 3 stages = 15, + 1 aggregator = 16. The bug shape
// is "aggregator got multiplied to 5 → total 20." We pin the
// fixed count and pin that the aggregator's DependsOn carries
// all N source instances.
func TestAggregates_RunLevel_OneSingletonAggregator(t *testing.T) {
	src := `
name: run-level-aggregator
for_each:
  item: [i01, i02, i03, i04, i05]
tasks:
  - id: step1
    action: answer
    prompt: do stuff for {{item}}
  - id: step2
    action: answer
    depends_on: [step1]
    prompt: step 2 for {{item}}
  - id: step3
    action: answer
    depends_on: [step2]
    prompt: step 3 for {{item}}
  - id: synth
    action: answer
    aggregates: step3
    prompt: |
      synthesize across all items:
      {{step3.content}}
`
	parsed, err := helperParse(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Total task count. Pre-fix bug: 5+5+5+5 = 20. Post-fix: 5+5+5+1 = 16.
	totalInstances := 0
	var synthTI *TaskInstance
	for _, list := range parsed.ExpandedTasks {
		for i := range list {
			totalInstances++
			if list[i].TaskDef.ID == "synth" {
				if synthTI != nil {
					t.Errorf("synth aggregator appeared more than once — should be singular")
				}
				synthTI = &list[i]
			}
		}
	}
	if totalInstances != 16 {
		t.Errorf("total instances = %d, want 16 (5×3 fanned + 1 aggregator). "+
			"Pre-fix bug shape would be 20.", totalInstances)
	}
	if synthTI == nil {
		t.Fatal("synth aggregator instance not found")
	}

	// Aggregator must be singular: empty instance key, bare FullID.
	if synthTI.InstanceKey != "" {
		t.Errorf("synth.InstanceKey = %q, want empty (singular)", synthTI.InstanceKey)
	}
	if synthTI.FullID != "synth" {
		t.Errorf("synth.FullID = %q, want %q (no instance prefix)", synthTI.FullID, "synth")
	}

	// DependsOn must include all 5 source instances, sorted.
	wantDeps := []string{"i01:step3", "i02:step3", "i03:step3", "i04:step3", "i05:step3"}
	if len(synthTI.DependsOn) != len(wantDeps) {
		t.Errorf("synth.DependsOn len = %d (%v), want %d (%v)",
			len(synthTI.DependsOn), synthTI.DependsOn, len(wantDeps), wantDeps)
	} else {
		for i := range wantDeps {
			if synthTI.DependsOn[i] != wantDeps[i] {
				t.Errorf("synth.DependsOn[%d] = %q, want %q", i, synthTI.DependsOn[i], wantDeps[i])
			}
		}
	}
}

// TestAggregates_TaskLevel_StillSingular pins that the existing
// task-level builder also respects the singularity. Today's
// path produces the same topology naturally (singleton with
// parentExpanded fan-in), but the new validator rules must
// accept this shape.
func TestAggregates_TaskLevel_StillSingular(t *testing.T) {
	src := `
name: task-level-aggregator
tasks:
  - id: scan
    action: answer
    for_each:
      item: [alpha, beta, gamma]
    prompt: scan {{item}}
  - id: synth
    action: answer
    aggregates: scan
    prompt: synthesize {{scan.content}}
`
	parsed, err := helperParse(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 3 scans + 1 synth = 4.
	total := 0
	var synthTI *TaskInstance
	for _, list := range parsed.ExpandedTasks {
		for i := range list {
			total++
			if list[i].TaskDef.ID == "synth" {
				synthTI = &list[i]
			}
		}
	}
	if total != 4 {
		t.Errorf("total instances = %d, want 4", total)
	}
	if synthTI == nil {
		t.Fatal("synth not found")
	}
	if synthTI.InstanceKey != "" || synthTI.FullID != "synth" {
		t.Errorf("synth must be singular: key=%q full_id=%q", synthTI.InstanceKey, synthTI.FullID)
	}
	wantDeps := []string{"alpha:scan", "beta:scan", "gamma:scan"}
	if len(synthTI.DependsOn) != len(wantDeps) {
		t.Errorf("synth.DependsOn len = %d (%v), want 3", len(synthTI.DependsOn), synthTI.DependsOn)
	}
}

// TestAggregates_ValidatorRejects_UnknownTarget pins the
// hard-error on `aggregates: <nonexistent>`. Catching this at
// parse time means run-creation fails loudly instead of producing
// a runtime "dependency not satisfied" stall.
func TestAggregates_ValidatorRejects_UnknownTarget(t *testing.T) {
	src := `
name: bad-target
for_each:
  item: [a, b]
tasks:
  - id: source
    action: answer
    prompt: do {{item}}
  - id: synth
    action: answer
    aggregates: nope
    prompt: synthesize
`
	_, err := helperParse(t, src)
	if err == nil {
		t.Fatal("expected error for nonexistent aggregates target")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should mention the missing target, got: %v", err)
	}
}

// TestAggregates_ValidatorRejects_NonFannedTarget guards the
// architectural precondition: aggregates only makes sense over a
// fan-out source. A target with no for_each AND no run-level
// expansion has exactly one instance, so "aggregating" it is a
// category error — say so at parse time.
func TestAggregates_ValidatorRejects_NonFannedTarget(t *testing.T) {
	src := `
name: non-fanned
tasks:
  - id: solo
    action: answer
    prompt: just one
  - id: synth
    action: answer
    aggregates: solo
    prompt: synthesize
`
	_, err := helperParse(t, src)
	if err == nil {
		t.Fatal("expected error for non-fanned aggregates target")
	}
	if !strings.Contains(err.Error(), "not fanned") {
		t.Errorf("error should mention the target isn't fanned, got: %v", err)
	}
}

// TestAggregates_ValidatorRejects_SelfReference protects against
// the typo "aggregates: <self.id>". The cycle would manifest as a
// DAG-validation failure deeper in the pipeline; catching it at
// parse-time gives the author a clear message instead.
func TestAggregates_ValidatorRejects_SelfReference(t *testing.T) {
	src := `
name: self-ref
for_each:
  item: [a, b]
tasks:
  - id: synth
    action: answer
    aggregates: synth
    prompt: synthesize
`
	_, err := helperParse(t, src)
	if err == nil {
		t.Fatal("expected error for self-reference")
	}
	if !strings.Contains(err.Error(), "itself") {
		t.Errorf("error should mention self-reference, got: %v", err)
	}
}

// TestAggregates_ValidatorRejects_WithForEach pins that
// `aggregates:` and a task's own `for_each:` are mutually
// exclusive — the whole point of aggregates is to STAY singular
// while reducing over a fanned source. Combining them is a
// category error.
func TestAggregates_ValidatorRejects_WithForEach(t *testing.T) {
	src := `
name: agg-plus-foreach
tasks:
  - id: scan
    action: answer
    for_each:
      item: [a, b]
    prompt: scan {{item}}
  - id: synth
    action: answer
    aggregates: scan
    for_each:
      item: [a, b]
    prompt: bad
`
	_, err := helperParse(t, src)
	if err == nil {
		t.Fatal("expected error for aggregates + for_each combination")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got: %v", err)
	}
}

// TestAggregates_AutoAddsDependency checks the convenience
// mutation: an `aggregates: X` task gets an auto-injected
// depends_on entry for X if the author didn't write it. Mirrors
// the reviews auto-dep behavior.
func TestAggregates_AutoAddsDependency(t *testing.T) {
	src := `
name: auto-dep
for_each:
  item: [a, b]
tasks:
  - id: source
    action: answer
    prompt: do {{item}}
  - id: synth
    action: answer
    aggregates: source
    prompt: synthesize {{source.content}}
`
	parsed, err := helperParse(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// synth's TaskDef.DependsOn should contain "source" even
	// though the YAML didn't list it explicitly.
	var synthDef *TaskDef
	for i := range parsed.Run.Tasks {
		if parsed.Run.Tasks[i].ID == "synth" {
			synthDef = &parsed.Run.Tasks[i]
			break
		}
	}
	if synthDef == nil {
		t.Fatal("synth def not found")
	}
	found := false
	for _, dep := range synthDef.DependsOn {
		if dep == "source" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("synth.DependsOn = %v, want it to contain auto-injected \"source\"", synthDef.DependsOn)
	}
}

// TestAggregates_CoexistsWithRegularDeps proves an aggregator
// may also depend on non-fanned siblings. The aggregator pulls
// in fan-in for the source AND a regular edge from the singleton
// sibling.
func TestAggregates_CoexistsWithRegularDeps(t *testing.T) {
	src := `
name: agg-plus-regular-dep
tasks:
  - id: scan
    action: answer
    for_each:
      item: [a, b, c]
    prompt: scan {{item}}
  - id: setup
    action: answer
    prompt: setup
  - id: synth
    action: answer
    aggregates: scan
    depends_on: [setup]
    prompt: synthesize {{scan.content}} after {{setup.content}}
`
	parsed, err := helperParse(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var synthTI *TaskInstance
	for _, list := range parsed.ExpandedTasks {
		for i := range list {
			if list[i].TaskDef.ID == "synth" {
				synthTI = &list[i]
			}
		}
	}
	if synthTI == nil {
		t.Fatal("synth not found")
	}
	// Expect: setup (singleton edge) + 3 scan instances = 4 deps total.
	wantContains := []string{"setup", "a:scan", "b:scan", "c:scan"}
	gotSet := make(map[string]bool)
	for _, d := range synthTI.DependsOn {
		gotSet[d] = true
	}
	for _, want := range wantContains {
		if !gotSet[want] {
			t.Errorf("synth.DependsOn missing %q (got %v)", want, synthTI.DependsOn)
		}
	}
	if len(synthTI.DependsOn) != 4 {
		t.Errorf("synth.DependsOn count = %d, want 4", len(synthTI.DependsOn))
	}
}
