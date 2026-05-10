package mcphandlers

// Render-side tests for formatExecuteRunSummary's no-ready-compute
// branch. The interesting case is the self-stuck-claim hint:
// when the cascade returns no_ready_compute AND the calling
// citizen holds claims on this run that didn't finish (most often
// because a prior execute_run was ESC'd), the summary must surface
// the stuck task IDs and the recovery recipe ("call enju_release_task
// for each, then retry"). Otherwise the operator is left staring at
// "run is idle" with no signal that release_task is what they need.
//
// These are pure render tests — they exercise the formatter against
// canned inputs and don't touch the coord. The detection logic
// (findSelfHeldStuckTasks) is exercised end-to-end by the integration
// test that paired with this fix.

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// TestFormatExecuteRun_NoReadyCompute_NoStuckClaims pins the
// pre-existing "run is idle" message for the healthy path: the
// cascade really has nothing to do and the operator holds no
// stale claims. We must NOT regress this with a misleading
// "you have stuck claims" line.
func TestFormatExecuteRun_NoReadyCompute_NoStuckClaims(t *testing.T) {
	out := formatExecuteRunSummary(
		nil,
		service.StopNoReadyCompute,
		nil,
		nil, // empty SelfStuckClaims — the healthy idle case
		100, 4,
	)
	if !strings.Contains(out, "run is idle or complete") {
		t.Errorf("missing healthy-idle message:\n%s", out)
	}
	if strings.Contains(out, "stuck claim") {
		t.Errorf("should not mention stuck claims when none held:\n%s", out)
	}
	if strings.Contains(out, "enju_release_task") {
		t.Errorf("should not advertise release_task when no stuck claims:\n%s", out)
	}
}

// TestFormatExecuteRun_NoReadyCompute_WithStuckClaims is the
// load-bearing assertion for the orphan-recovery hint. When the
// cascade reports no_ready_compute and the coord says "you hold
// these in claimed/running," the operator MUST see:
// - the count
// - each stuck task ID (so they can release_task each one)
// - the recovery recipe (release_task + retry)
// - the reaper-fallback note (so they know inaction also works
// eventually)
func TestFormatExecuteRun_NoReadyCompute_WithStuckClaims(t *testing.T) {
	out := formatExecuteRunSummary(
		nil,
		service.StopNoReadyCompute,
		nil,
		[]string{"1:3:i06:s6", "1:3:i12:s4"},
		100, 4,
	)
	for _, want := range []string{
		"2 stuck claim",
		"1:3:i06:s6",
		"1:3:i12:s4",
		"enju_release_task",
		"reaper", // the wait-it-out fallback note
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in stuck-claim summary:\n%s", want, out)
		}
	}
	// Healthy-idle phrasing must NOT appear when stuck claims
	// are present — the operator needs the actionable message,
	// not a misleading "all done" cue.
	if strings.Contains(out, "run is idle or complete") {
		t.Errorf("should not show healthy-idle text when stuck claims are listed:\n%s", out)
	}
}

// TestFormatExecuteRun_StuckClaimsIgnoredOutsideNoReadyCompute
// pins the gate: the stuck-claim hint must ONLY render under
// StopNoReadyCompute. Other stop reasons (max_tasks, async_started,
// etc.) have their own messaging; piling on a "you also have stuck
// claims" footer would confuse the actually-actionable signal.
func TestFormatExecuteRun_StuckClaimsIgnoredOutsideNoReadyCompute(t *testing.T) {
	out := formatExecuteRunSummary(
		nil,
		service.StopMaxTasks,
		nil,
		[]string{"1:3:i06:s6"}, // populated, but wrong stop reason
		100, 4,
	)
	if strings.Contains(out, "stuck claim") {
		t.Errorf("stuck-claim hint leaked into non-NoReadyCompute stop reason:\n%s", out)
	}
}
