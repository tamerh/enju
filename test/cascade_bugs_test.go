package test

// Regression tests for two cascade bugs found in production
// (showcase project, 2026-05-11):
//
//  1. Invalidating a FAILED task doesn't re-promote its
//     SKIPPED descendants (descendants that were skipped via
//     the upstream-failed fail cascade stay terminally
//     skipped forever).
//
//  2. A fan-in aggregator becomes READY and runs on empty
//     content when ALL its source instances are SKIPPED.
//     The dep gate treats SKIPPED as "satisfied" — true for
//     a SUBSET of sources (the spec's intended fan-in
//     "skipped == terminal-good" rule), but wrong when EVERY
//     source skipped (nothing to aggregate, silently produces
//     a "successful" but empty result).
//
// Both pin behavior the user explicitly hit and reported.

import (
	"fmt"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestParseYAMLDataWithoutParams_LeavesTasksDeferred is the
// minimal unit-level reproduction of the production bug
// surfaced after a coord restart wiped the in-memory cache.
//
// The hypothesis: dagcache.GetParsedRun on a cache miss calls
// `enjuYaml.Parse([]byte(run.YAMLData))` — no params. The
// YAML it parses has `for_each: {item: "{{items}}"}`. Without
// params substituted, that template ref stays literal, the
// validator classifies the for_each as dynamic, every task
// becomes DEFERRED, and ParsedRun.ExpandedTasks ends up empty
// — only def-level structure survives.
//
// This test pins exactly that: parse the same YAML two ways
// (Parse vs ParseWithParams), and assert the no-params parse
// produces zero materialized instances while the with-params
// parse produces the expected fanned set.
//
// Without this test passing pre-fix, the cache theory is
// unproven. With it passing, the dagcache fix is justified:
// re-parse must use the stored params, not raw YAMLData.
func TestParseYAMLDataWithoutParams_LeavesTasksDeferred(t *testing.T) {
	yamlSrc := `
name: test
params:
  - name: items
    type: list<string>
for_each:
  item: "{{items}}"
tasks:
  - id: stage1
    action: answer
    prompt: stage1 for {{item}}
  - id: stage2
    action: answer
    depends_on: [stage1]
    prompt: stage2 for {{item}}
`

	// Path A: parse WITHOUT params (the cache's current behavior).
	parsedNoParams, err := enjuYaml.Parse([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	totalNoParams := 0
	for _, list := range parsedNoParams.ExpandedTasks {
		totalNoParams += len(list)
	}

	// Path B: parse WITH the run's stored params (the fix).
	parsedWithParams, err := enjuYaml.ParseWithParams([]byte(yamlSrc), map[string]interface{}{
		"items": []interface{}{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("ParseWithParams: %v", err)
	}
	totalWithParams := 0
	for _, list := range parsedWithParams.ExpandedTasks {
		totalWithParams += len(list)
	}

	t.Logf("Parse (no params):      ExpandedTasks count = %d", totalNoParams)
	t.Logf("ParseWithParams (a,b,c): ExpandedTasks count = %d", totalWithParams)
	t.Logf("DeferredTaskDefs no-params:   %d", len(parsedNoParams.DeferredTaskDefs))
	t.Logf("DeferredTaskDefs with-params: %d", len(parsedWithParams.DeferredTaskDefs))

	// Hypothesis: no-params parse produces deferred tasks
	// (no instances) and a populated DeferredTaskDefs list.
	// with-params parse produces 3 items × 2 stages = 6
	// materialized instances and zero deferred.
	if totalWithParams == 0 {
		t.Errorf("ParseWithParams produced zero materialized instances — "+
			"test fixture broken (expected 3 items × 2 stages = 6)")
	}
	if totalNoParams >= totalWithParams {
		t.Errorf("Parse (no params) materialized %d, ParseWithParams "+
			"materialized %d — hypothesis FAILED. Cache re-parse without "+
			"params is NOT producing a degraded DAG. Bug must be elsewhere.",
			totalNoParams, totalWithParams)
	}

	// Specifically: with params we should have per-instance
	// nodes in the DAG; without params, only def-level
	// structure (or none).
	withNode, _ := parsedWithParams.DAG.GetNode("a:stage1")
	if withNode == nil {
		t.Errorf("ParseWithParams DAG missing per-instance node 'a:stage1' — "+
			"test fixture is wrong; can't draw conclusions about the no-params parse")
	}
	noParamsNode, _ := parsedNoParams.DAG.GetNode("a:stage1")
	if noParamsNode != nil {
		t.Errorf("Parse (no params) DAG unexpectedly contains 'a:stage1' — "+
			"if it does, then the cascade walk WOULD find descendants and "+
			"the bug isn't where I think it is")
	}
}

// TestInvalidateUnskipDescendantsAfterFailCascade reproduces
// the user's exact path:
//
//   1. Two-stage chain: source → consumer.
//   2. source fails → consumer cascades to SKIPPED with
//      skip_reason = "upstream failed: <source-id>".
//   3. Operator invalidates source.
//   4. Expectation: source flips FAILED → READY (works today)
//      AND consumer flips SKIPPED → PENDING (BROKEN today).
//
// Without (4), the workflow is permanently stuck even after
// the root cause is fixed — the operator's only escape is to
// terminate the run entirely.
func TestInvalidateUnskipDescendantsAfterFailCascade(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	// Mirror the user's showcase shape: for_each items, multi-
	// stage chain, fail mid-stage. Tests the production path
	// not just a singleton case (which behaves differently per
	// the iteration-scope filter in engine/cascade.go).
	s.submitInlineYAML(`
name: invalidate-unskip-fanout
for_each:
  item: [a, b]
tasks:
  - id: stage1
    action: answer
    prompt: stage1 for {{item}}
  - id: stage2
    action: answer
    depends_on: [stage1]
    prompt: stage2 for {{item}} from {{stage1.content}}
  - id: stage3
    action: answer
    depends_on: [stage2]
    prompt: stage3 for {{item}} from {{stage2.content}}
`)

	// Drive item a's stage1 → accepted. Then fail item a's
	// stage2 mid-chain — this should cascade to skip a:stage3
	// while leaving item b alone (iteration-scoped failure).
	s.claim("a:stage1", alice)
	if r := s.submit("a:stage1", "ok"); r["error"] != nil {
		t.Fatalf("submit a:stage1: %+v", r)
	}
	s.claim("a:stage2", alice)
	failResp := s.post("/api/v1/tasks/"+s.taskID("a:stage2")+"/fail",
		map[string]interface{}{"reason": "intentional-test-fail"})
	if errMsg, _ := failResp["error"].(string); errMsg != "" {
		t.Fatalf("fail a:stage2: %s", errMsg)
	}

	// Pre-condition: a:stage3 is SKIPPED via fail cascade.
	stage3 := s.get("/api/v1/tasks/" + s.taskID("a:stage3"))
	if state, _ := stage3["state"].(string); state != "skipped" {
		t.Fatalf("test setup: a:stage3 should be skipped after a:stage2 fail, got %q", state)
	}

	// Invalidate a:stage2. The bug: descendants stay SKIPPED.
	invResp := s.post("/api/v1/tasks/"+s.taskID("a:stage2")+"/invalidate",
		map[string]interface{}{"reason": "re-run after fix"})
	if errMsg, _ := invResp["error"].(string); errMsg != "" {
		t.Fatalf("invalidate a:stage2: %s", errMsg)
	}

	// a:stage2: FAILED → READY.
	stage2 := s.get("/api/v1/tasks/" + s.taskID("a:stage2"))
	if state, _ := stage2["state"].(string); state != "ready" {
		t.Errorf("a:stage2 state = %q, want ready after invalidate", state)
	}

	// a:stage3: SKIPPED → PENDING. THIS IS THE BUG.
	stage3 = s.get("/api/v1/tasks/" + s.taskID("a:stage3"))
	if state, _ := stage3["state"].(string); state != "pending" {
		t.Errorf("a:stage3 state = %q, want pending (invalidate must un-skip "+
			"descendants whose skip_reason names this task — otherwise the "+
			"cascade is permanently broken even after the root cause is fixed)", state)
	}
	if reason, _ := stage3["skip_reason"].(string); reason != "" {
		t.Errorf("a:stage3 skip_reason = %q, want empty after invalidate-driven un-skip", reason)
	}
}

// TestAggregatorSkipsWhenAllSourcesSkipped pins Issue 2: a
// fan-in aggregator must NOT fire on a set where every source
// is skipped. The aggregator's `{{source.content}}` resolution
// returns empty in that case, producing a silently-wrong
// "success" that pollutes the downstream cascade.
//
// Spec semantics:
//   - 1 skipped + N-1 accepted → aggregator runs on surviving
//     sources (current behavior, correct).
//   - all N skipped → aggregator should also skip, with a
//     reason naming the all-sources-skipped condition. No
//     content to aggregate = no aggregator work to do.
//
// This test uses a tiny fan-in (2 sources, 1 aggregator) and
// flips both sources to SKIPPED directly via the store so the
// dep-gate evaluates with all-skipped inputs.
func TestAggregatorSkipsWhenAllSourcesSkipped(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	_ = alice
	s.submitInlineYAML(`
name: aggregator-all-skipped
for_each:
  item: [i01, i02]
tasks:
  - id: source
    action: answer
    prompt: produce {{item}}
  - id: aggregate
    action: answer
    collects: source
    prompt: synthesize
`)

	// Get the run's internal ID for store.ApplyPlan calls.
	runMeta := s.get(fmt.Sprintf("/api/v1/projects/%d/runs/%d",
		s.lastProjectID, s.lastRunSeq))
	runIDFloat, _ := runMeta["id"].(float64)
	runID := int64(runIDFloat)
	if runID == 0 {
		t.Fatalf("could not resolve run ID from %+v", runMeta)
	}

	// Flip BOTH source instances to SKIPPED. We bypass the
	// normal submit flow because the bug is about the cascade
	// gate's reaction to a fully-skipped source set — not
	// about how the sources got there. (Real-world paths
	// include vote-cascade loss across all branches, or an
	// operator manually skipping all sources; either way the
	// aggregator sees the same input.)
	mutations := []store.Mutation{}
	for _, tid := range []string{s.taskID("i01:source"), s.taskID("i02:source")} {
		mutations = append(mutations, store.SetTaskState{
			TaskID:     tid,
			NewState:   store.TaskSkipped,
			SkipReason: "test-injected: all-sources-skipped scenario",
		})
	}
	mutations = append(mutations, store.UpdateReadyTasks{RunID: runID})
	if _, err := s.store.ApplyPlan(store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	}); err != nil {
		t.Fatalf("flipping sources to skipped + cascade: %v", err)
	}

	// The aggregator must NOT have become ready. The dep gate's
	// "skipped counts as terminal-good" rule was designed for
	// partial-data aggregation; an all-skipped input set means
	// the aggregator has nothing to aggregate.
	agg := s.get("/api/v1/tasks/" + s.taskID("aggregate"))
	state, _ := agg["state"].(string)
	if state == "ready" || state == "accepted" {
		t.Errorf("aggregate state = %q, must NOT be ready/accepted when "+
			"every source is skipped (no content to aggregate). Either: "+
			"(a) aggregate skips with reason='all aggregate sources skipped' "+
			"(preferred), or (b) aggregate stays pending awaiting operator "+
			"action. What it must NOT do: silently succeed on empty inputs.", state)
	}
}
