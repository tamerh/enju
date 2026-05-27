package format

// Phase 8.6 renderer tests for iter_count surfaces. Two
// places: Mermaid DAG nodes (badge "(N×)" appended after the
// state icon) and the get_task formatter's Iterations block.
// Pin both so a future renderer touch can't drift the visible
// shape silently.

import (
	"strings"
	"testing"
)

func TestRenderMermaidBody_IterCountBadge(t *testing.T) {
	runData := []byte(`{"id":1,"project_id":1,"seq":1,"name":"t","state":"active"}`)
	tasksData := []byte(`[
		{"id":"1:1:bounced","state":"accepted","iter_count":3,"task_def_id":"bounced"},
		{"id":"1:1:fresh","state":"accepted","task_def_id":"fresh"}
	]`)

	body := RenderMermaidBody(runData, tasksData)
	if !strings.Contains(body, "(3×)") {
		t.Errorf("expected '(3×)' badge for iter_count=3 task; got:\n%s", body)
	}
	// Fresh task (iter_count omitted from JSON) must NOT
	// pick up a stray badge — the renderer reads the value
	// via IntFromJSON which returns 0 for a missing key, and
	// the > 1 gate suppresses output.
	if strings.Contains(body, "fresh\\\" (1×)") || strings.Contains(body, "fresh (0×)") {
		t.Errorf("badge wrongly appears on iter_count<=1 task; body:\n%s", body)
	}
}

func TestRenderTaskMeta_IterationsBlock(t *testing.T) {
	// Compose a get_task response with iter_count=2 +
	// iterations[]. Renderer should emit an "Iterations (2×)"
	// block listing each entry with citizen, verdict, and
	// duration.
	taskJSON := `{
		"id":"1:1:write",
		"task_def_id":"write",
		"action":"answer",
		"state":"accepted",
		"iter_count":2,
		"iterations":[
			{"seq":1,"citizen":"alice","outcome":"completed","review_decision":"request_changes","duration_ms":300000},
			{"seq":2,"citizen":"alice","outcome":"completed","review_decision":"approve","duration_ms":900000}
		]
	}`
	out := TaskDetail([]byte(taskJSON), nil, "")
	if !strings.Contains(out, "Iterations (2×)") {
		t.Errorf("expected Iterations block header, got:\n%s", out)
	}
	if !strings.Contains(out, "@alice") {
		t.Errorf("missing citizen in iterations block:\n%s", out)
	}
	if !strings.Contains(out, "request_changes") {
		t.Errorf("missing iter-1 verdict:\n%s", out)
	}
	if !strings.Contains(out, "approve") {
		t.Errorf("missing iter-2 verdict:\n%s", out)
	}
	if !strings.Contains(out, "5m") {
		t.Errorf("missing iter-1 duration (5m for 300000ms):\n%s", out)
	}
	if !strings.Contains(out, "15m") {
		t.Errorf("missing iter-2 duration (15m for 900000ms):\n%s", out)
	}
}

// TestRenderTaskMeta_IterationsBlock_SuppressedOnSingleAttempt
// pins the gate: a task with iter_count=1 (or absent) MUST
// NOT render an Iterations block — the common case stays
// uncluttered.
func TestRenderTaskMeta_IterationsBlock_SuppressedOnSingleAttempt(t *testing.T) {
	taskJSON := `{
		"id":"1:1:write",
		"task_def_id":"write",
		"action":"answer",
		"state":"accepted"
	}`
	out := TaskDetail([]byte(taskJSON), nil, "")
	if strings.Contains(out, "Iterations (") {
		t.Errorf("Iterations block leaked on single-attempt task; got:\n%s", out)
	}
}

// TestSetMermaidDirection pins the orientation option: a matching
// flowchart header flips to the requested (normalized) direction;
// an unknown direction clamps to TD; a non-flowchart body passes
// through untouched.
func TestSetMermaidDirection(t *testing.T) {
	cases := []struct{ body, dir, want string }{
		{"flowchart TD\n  a --> b", "LR", "flowchart LR\n  a --> b"},
		{"flowchart TD\n  a --> b", "lr", "flowchart LR\n  a --> b"},
		{"flowchart LR\n  x", "TD", "flowchart TD\n  x"},
		{"flowchart TD\n  x", "sideways", "flowchart TD\n  x"}, // unknown → TD
		{"flowchart TD\n  x", "", "flowchart TD\n  x"},         // empty → TD
		{"graph TD\n  a --> b", "LR", "graph TD\n  a --> b"},   // not a flowchart header
		{"", "LR", ""},
	}
	for _, c := range cases {
		if got := SetMermaidDirection(c.body, c.dir); got != c.want {
			t.Errorf("SetMermaidDirection(%q, %q) = %q, want %q", c.body, c.dir, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, "0s"},
		{-1, "0s"},
		{500, "500ms"},
		{30000, "30s"},
		{300000, "5m"},
		{7200000, "2h"},
		{3 * 24 * 3600 * 1000, "3d"},
	}
	for _, c := range cases {
		if got := humanDuration(c.ms); got != c.want {
			t.Errorf("humanDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}
