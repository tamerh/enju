package test

// End-to-end test for the aggregates fan-in primitive. Drives a
// real coord through the submit pipeline with a run-level for_each
// + an aggregator task, verifies:
//
//   - The expander emits the right shape (N source instances + 1
//     singular aggregator, not N).
//   - Cascade gates the aggregator until ALL N sources are
//     terminal-good. We submit fewer than N sources and assert
//     the aggregator stays PENDING.
//   - Final aggregator becomes READY exactly once all N source
//     instances accept.
//
// The unit-level shape tests live in
// internal/common/yaml/aggregates_test.go; this test exists to
// pin the integration with coord-side cascade evaluation, which
// the unit tests can't reach.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestAggregates_RunLevel_CascadeWaitsForAllSources is the
// load-bearing integration test for the user-reported bug ("5
// items × 6 stages = 31 tasks expected, 35 actual"). We use a
// smaller shape (3 items × 1 stage + 1 aggregator = 4 total) to
// keep the test fast, but the topology is the bug-shape: a
// run-level for_each that previously would have multiplied the
// aggregator to 3 copies. We pin total task count + that the
// aggregator becomes READY only after all 3 sources are
// accepted.
func TestAggregates_RunLevel_CascadeWaitsForAllSources(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitInlineYAML(`
name: aggregates-cascade-test
for_each:
  item: [i01, i02, i03]
tasks:
  - id: source
    action: answer
    prompt: do {{item}}
  - id: synth
    action: answer
    aggregates: source
    prompt: synthesize
`)

	// Total task count: 3 source instances + 1 singular synth = 4.
	// Pre-fix the bug shape would be 3 + 3 = 6.
	results := s.getList(fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks",
		s.lastProjectID, s.lastRunSeq))
	if len(results) != 4 {
		t.Fatalf("expected 4 tasks (3 source + 1 synth), got %d: %+v", len(results), results)
	}

	// Identify the synth task.
	var synthFullID string
	synthCount := 0
	for _, r := range results {
		tr := r.(map[string]interface{})
		if defID, _ := tr["task_def_id"].(string); defID == "synth" {
			synthCount++
			synthFullID, _ = tr["id"].(string)
		}
	}
	if synthCount != 1 {
		t.Fatalf("synth must appear exactly once (singular aggregator), got %d copies", synthCount)
	}

	// Stage 1: submit 2 of 3 source instances. Synth must STAY
	// in pending — its third dep is still incomplete.
	for _, key := range []string{"i01", "i02"} {
		shortID := key + ":source"
		s.claim(shortID, alice)
		if resp := s.submit(shortID, "result for "+key); resp["error"] != nil {
			t.Fatalf("submit %s: %+v", shortID, resp)
		}
	}
	synth := s.get("/api/v1/tasks/" + synthFullID)
	if state, _ := synth["state"].(string); state == "ready" {
		t.Fatalf("synth flipped to ready with only 2/3 sources accepted — cascade gate broken; state=%s", state)
	}

	// Stage 2: submit the third source. Synth must NOW be ready.
	s.claim("i03:source", alice)
	if resp := s.submit("i03:source", "result for i03"); resp["error"] != nil {
		t.Fatalf("submit i03:source: %+v", resp)
	}
	synth = s.get("/api/v1/tasks/" + synthFullID)
	if state, _ := synth["state"].(string); state != "ready" {
		t.Errorf("synth state = %q, want ready after all 3 sources accepted "+
			"(the fan-in cascade gate is the load-bearing property under test)", state)
	}
}

// TestAggregates_RunLevel_InputsDescriptorIncludesAllSources is
// the aggregator-specific content-resolution test. The fat-client
// resolver (enjugit.Workflow.Resolve) joins fan-in content when
// it receives multiple Dependencies with the same task_def_id —
// the general behavior is pinned by
// internal/fatclient/enjugit/resolve_integration_test.go::
// TestResolve_FanInIntegration. What this test pins is the
// upstream half: that coord's claim-time InputsDescriptor for an
// aggregator task lists every source instance as a separate
// Dependency entry with its own commit_sha, so the fat-client
// has everything it needs to drive the join.
//
// Pre-fix, the run-level aggregator wouldn't exist as a
// singleton — it'd be N copies, each with one dep, none of them
// triggering the fan-in branch in Resolve. Post-fix, one
// singular aggregator carries N deps; Resolve's len(deps) > 1
// branch fires and joins the content. This test is the contract
// pin for "yes, all N source instance commits land in the
// descriptor."
func TestAggregates_RunLevel_InputsDescriptorIncludesAllSources(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitInlineYAML(`
name: aggregates-descriptor-test
for_each:
  item: [i01, i02, i03]
tasks:
  - id: source
    action: answer
    prompt: do {{item}}
  - id: synth
    action: answer
    aggregates: source
    prompt: |
      synthesize:
      {{source.content}}
`)

	// Submit all 3 sources so each has a commit_sha to feed
	// into the aggregator's input descriptor.
	contents := map[string]string{
		"i01": "first-result-content",
		"i02": "second-result-content",
		"i03": "third-result-content",
	}
	for _, key := range []string{"i01", "i02", "i03"} {
		shortID := key + ":source"
		s.claim(shortID, alice)
		if resp := s.submit(shortID, contents[key]); resp["error"] != nil {
			t.Fatalf("submit %s: %+v", shortID, resp)
		}
	}

	// Hit /tasks/<synth>/inputs — this is exactly what the
	// fat-client polls at claim time before calling Resolve.
	synthID := s.taskID("synth")
	desc := s.get("/api/v1/tasks/" + synthID + "/inputs")
	deps, ok := desc["dependencies"].([]interface{})
	if !ok {
		t.Fatalf("expected dependencies[], got %+v", desc)
	}
	if len(deps) != 3 {
		t.Fatalf("dependencies count = %d, want 3 (one per source instance); got %+v",
			len(deps), deps)
	}

	// Every dependency must point at the same task_def_id
	// ("source") and carry a non-empty commit_sha. That's the
	// shape enjugit.Resolve's fan-in branch expects: N entries
	// sharing a TaskDefID, each with its own commit to read.
	seenKeys := make(map[string]bool)
	for i, d := range deps {
		dep := d.(map[string]interface{})
		if defID, _ := dep["task_def_id"].(string); defID != "source" {
			t.Errorf("deps[%d].task_def_id = %q, want \"source\"", i, defID)
		}
		if sha, _ := dep["commit_sha"].(string); sha == "" {
			t.Errorf("deps[%d].commit_sha empty — Resolve has nothing to read", i)
		}
		if key, _ := dep["instance_key"].(string); key != "" {
			seenKeys[key] = true
		}
	}
	for _, want := range []string{"i01", "i02", "i03"} {
		if !seenKeys[want] {
			t.Errorf("descriptor missing instance_key %q (got keys: %v)", want, seenKeys)
		}
	}

	// Prompt template must be the unresolved version coord
	// stored — the fat-client does the substitution after
	// fetching this descriptor. We pin that the placeholder
	// is still present (so the substitution downstream has
	// something to operate on).
	tmpl, _ := desc["prompt_template"].(string)
	if !strings.Contains(tmpl, "{{source.content}}") {
		t.Errorf("prompt_template missing {{source.content}} placeholder: %q", tmpl)
	}
}

// TestAggregates_RunLevel_FailedSourceLeavesAggregatorPending
// pins the failure-side behavior, which is more subtle than
// the success cascade.
//
// The fail cascade (engine/cascade.go ComputeInvalidation)
// intentionally filters cross-iteration descendants out of the
// SKIPPED set — see the "for_each fail-isolation contract"
// comment at the iteration-scope filter. The motivating
// battle-test was: one iteration's failure shouldn't transitively
// skip a singleton aggregator that depends on multiple
// iterations; the operator might still want partial results.
//
// Result: an aggregator with one FAILED source dep stays in
// PENDING (NOT skipped, NOT failed). FAILED isn't in the
// cascade gate's "satisfied" set ({accepted, submitted,
// skipped}), so the aggregator can't progress automatically
// either. The operator decides — invalidate the failed source,
// fail the aggregator manually, or terminate the run.
//
// This pre-existing design tradeoff is what fan-in exposes
// cleanly. Pinning the PENDING state positively here so a
// future cascade-gate change that silently flips this — either
// way (skipped or accepted-on-partial-data) — gets caught.
func TestAggregates_RunLevel_FailedSourceLeavesAggregatorPending(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitInlineYAML(`
name: aggregates-failure-cascade
for_each:
  item: [i01, i02]
tasks:
  - id: source
    action: answer
    prompt: do {{item}}
  - id: synth
    action: answer
    aggregates: source
    prompt: synthesize
`)

	// Accept one, fail the other.
	s.claim("i01:source", alice)
	if resp := s.submit("i01:source", "ok"); resp["error"] != nil {
		t.Fatalf("submit i01:source: %+v", resp)
	}
	s.claim("i02:source", alice)
	// Fail i02:source via the explicit fail endpoint.
	failResp := s.post("/api/v1/tasks/"+s.taskID("i02:source")+"/fail",
		map[string]interface{}{"reason": "test-injected"})
	if errMsg, _ := failResp["error"].(string); errMsg != "" {
		t.Fatalf("fail i02:source: %s", errMsg)
	}

	// synth stays PENDING. See the docstring above for the
	// iteration-scope filter that protects cross-iteration
	// aggregators from blanket cascade-to-SKIPPED. A FAILED
	// dep doesn't satisfy the cascade gate either, so PENDING
	// is the natural resting state — the operator decides
	// what to do next.
	synth := s.get("/api/v1/tasks/" + s.taskID("synth"))
	state, _ := synth["state"].(string)
	if state != "pending" {
		t.Errorf("synth state = %q, want pending (cross-iteration aggregators "+
			"are exempt from the same-iter SKIPPED cascade; FAILED dep blocks "+
			"the satisfied-set gate; operator-driven recovery from here)", state)
	}
	// Specifically NOT ready or accepted — a failed source
	// must not let the aggregator run on incomplete data
	// without operator action.
	if state == "ready" || state == "accepted" {
		t.Errorf("synth must not auto-progress past pending when a source failed; got %q", state)
	}
}

// TestAggregates_SkippedSourceCountsAsTerminalGood pins the spec
// invariant: "N-1 accepted + 1 skipped → aggregator transitions
// to READY." Skipped is a non-failure terminal state — typically
// produced by vote-cascade-loss, where a sibling vote killed
// this branch — and the existing cascade evaluator treats it as
// "satisfied for dep purposes" alongside accepted/submitted.
// The aggregator's fan-in dep list must respect that. Without
// this test, a future cascade-gate refactor could narrow the
// satisfied set to {accepted} only and silently break aggregator
// readiness for any run that ever produces skips.
//
// We use the store directly to flip one source to SKIPPED — the
// realistic upstream paths (vote-cascade, manual skip) are
// tested elsewhere and add nothing here. The load-bearing
// assertion is what the aggregator's cascade gate does with a
// SKIPPED dep, not how the dep got there.
func TestAggregates_SkippedSourceCountsAsTerminalGood(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitInlineYAML(`
name: aggregates-skipped-source
for_each:
  item: [i01, i02, i03]
tasks:
  - id: source
    action: answer
    prompt: do {{item}}
  - id: synth
    action: answer
    aggregates: source
    prompt: synthesize
`)

	// Accept 2 of 3 sources via the normal submit path.
	for _, key := range []string{"i01", "i02"} {
		shortID := key + ":source"
		s.claim(shortID, alice)
		if resp := s.submit(shortID, "result "+key); resp["error"] != nil {
			t.Fatalf("submit %s: %+v", shortID, resp)
		}
	}

	// Flip the 3rd source to SKIPPED directly via the store.
	// Production paths that produce SKIPPED (vote-cascade
	// loss, terminate-run, etc.) are covered elsewhere; this
	// test just needs a SKIPPED dep to feed the aggregator's
	// cascade gate.
	skippedID := s.taskID("i03:source")
	if _, err := s.store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetTaskState{
				TaskID:     skippedID,
				NewState:   store.TaskSkipped,
				SkipReason: "test-injected skip",
			},
			store.UpdateReadyTasks{RunID: int64(s.lastRunSeq)},
		},
	}); err != nil {
		t.Fatalf("flipping %s to skipped: %v", skippedID, err)
	}
	// UpdateReadyTasks above wants a run ID, not the seq. Look
	// up the real ID.
	runRecord := s.get(fmt.Sprintf("/api/v1/projects/%d/runs/%d",
		s.lastProjectID, s.lastRunSeq))
	runIDFloat, _ := runRecord["id"].(float64)
	if runIDFloat > 0 {
		_, _ = s.store.ApplyPlan(store.Plan{
			Version: engine.EngineVersion,
			Mutations: []store.Mutation{
				store.UpdateReadyTasks{RunID: int64(runIDFloat)},
			},
		})
	}

	synth := s.get("/api/v1/tasks/" + s.taskID("synth"))
	state, _ := synth["state"].(string)
	if state != "ready" {
		t.Errorf("synth state = %q, want ready (2 accepted + 1 skipped should "+
			"satisfy the aggregator's cascade gate; skipped counts as terminal-good)", state)
	}
}
