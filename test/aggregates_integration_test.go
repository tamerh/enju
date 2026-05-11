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
	"testing"
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

// TestAggregates_RunLevel_FailedSourceCascadesToAggregator pins
// the failure side of the cascade. When one source instance
// FAILs, the aggregator cascades along the same path as any
// downstream of a failed task — it goes SKIPPED via the
// upstream-failed mechanism rather than becoming ready and
// running on incomplete inputs.
func TestAggregates_RunLevel_FailedSourceCascadesToAggregator(t *testing.T) {
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

	// synth must NOT have become ready — exactly one source
	// failing means the aggregator is downstream of a failed
	// upstream, which the existing fail cascade routes to
	// SKIPPED.
	synth := s.get("/api/v1/tasks/" + s.taskID("synth"))
	state, _ := synth["state"].(string)
	if state == "ready" || state == "accepted" {
		t.Errorf("synth state = %q, must NOT be ready/accepted when a source failed; "+
			"want pending or skipped", state)
	}
}
