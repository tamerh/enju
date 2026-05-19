package format

// B7: a failed_retryable task is NON-terminal (recoverable via
// enju_retry_task), so it never enters the "all terminal" rollup
// branch. Before the fix it had no bucket in the in-progress
// rollup and no mermaid class/glyph — it contributed to total but
// rendered blank ("?" node), hiding a run blocked on a recoverable
// failure. These pin both surfaces.

import (
	"strings"
	"testing"
)

func TestRenderTemplateSummary_SurfacesFailedRetryable(t *testing.T) {
	tasks := []map[string]interface{}{
		{"id": "9:1:t", "task_def_id": "t", "state": "failed_retryable"},
		{"id": "9:1:d", "task_def_id": "d", "state": "pending"},
	}
	out := RenderTemplateSummary(tasks)

	// The recoverable failure must be visible AND name the recovery
	// path — not silently absent from the row.
	if !strings.Contains(out, "failed_retryable") {
		t.Fatalf("by-task rollup must surface failed_retryable; got:\n%s", out)
	}
	if !strings.Contains(out, "↻") {
		t.Errorf("expected the ↻ needs-attention glyph; got:\n%s", out)
	}
	if !strings.Contains(out, "enju_retry_task") {
		t.Errorf("rollup should point the operator at the recovery command; got:\n%s", out)
	}
	// Must NOT be miscounted as terminal (✅/done) — it's blocked,
	// not complete.
	if strings.Contains(out, "t              1/1 ✅") {
		t.Errorf("failed_retryable must not render as a completed template; got:\n%s", out)
	}
}

func TestRenderMermaidBody_StylesFailedRetryable(t *testing.T) {
	runData := []byte(`{"id":1,"project_id":1,"seq":1,"name":"t","state":"active"}`)
	tasksData := []byte(`[
		{"id":"1:1:t","state":"failed_retryable","task_def_id":"t"},
		{"id":"1:1:d","state":"pending","task_def_id":"d","depends_on":"1:1:t"}
	]`)

	body := RenderMermaidBody(runData, tasksData)

	if !strings.Contains(body, "↻") {
		t.Errorf("failed_retryable node should carry the ↻ glyph (not the '?' fallback); got:\n%s", body)
	}
	if !strings.Contains(body, ":::retryable") {
		t.Errorf("failed_retryable node must get the :::retryable class; got:\n%s", body)
	}
	if !strings.Contains(body, "classDef retryable ") {
		t.Errorf("the retryable classDef must be emitted (was: defined-but-never-applied 'failed'); got:\n%s", body)
	}
	// Distinct from terminal failed — the recoverable state must
	// not reuse the red `failed` class.
	if strings.Contains(body, "1:1:t") && strings.Contains(body, ":::failed\n") {
		t.Errorf("failed_retryable must NOT reuse the terminal :::failed class; got:\n%s", body)
	}
}

// StateIconFor / MermaidStateClass direct truth-table pins so a
// future switch edit can't silently drop the arm again.
func TestFailedRetryable_IconAndClass(t *testing.T) {
	if got := StateIconFor("failed_retryable", ""); got != "↻" {
		t.Errorf("StateIconFor(failed_retryable) = %q, want ↻", got)
	}
	if got := MermaidStateClass("failed_retryable"); got != "retryable" {
		t.Errorf("MermaidStateClass(failed_retryable) = %q, want retryable", got)
	}
	// Terminal failed stays distinct.
	if got := MermaidStateClass("failed"); got != "failed" {
		t.Errorf("MermaidStateClass(failed) = %q, want failed (must stay distinct)", got)
	}
}
