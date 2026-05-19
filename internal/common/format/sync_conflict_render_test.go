package format

// B1 text-surface closure: a run whose output never reached the
// base branch (run-completion sync conflict) must not present as a
// clean "completed 100%" on ANY text surface. The durable flag /
// event / JSON were wired by the lifecycle lane; this pins the
// human-text render (run_status + runs list). Plus B4-secondary:
// ready-tasks group label uses the per-project run seq.

import (
	"strings"
	"testing"
)

const syncConflictJSON = `{"kind":"conflict","run_branch":"load-test-sweep-2","base_branch":"main","conflict_files":["out/a.txt","out/b.txt"],"hint":"git checkout main && git merge load-test-sweep-2"}`

func TestRenderSyncConflict(t *testing.T) {
	out := RenderSyncConflict(syncConflictJSON)
	if !strings.Contains(out, "⚠ SYNC CONFLICT") {
		t.Fatalf("expected the unmissable conflict marker; got %q", out)
	}
	if !strings.Contains(out, "main") || !strings.Contains(out, "2 file(s)") {
		t.Errorf("should name the base branch + file count; got %q", out)
	}
	if !strings.Contains(out, "Resolve: git checkout main") {
		t.Errorf("should surface the resolve hint; got %q", out)
	}
	// Forward-compat / negative cases render nothing (never a stray
	// scary line on clean/old/unknown data).
	for _, raw := range []string{"", "   ", "not json", `{"kind":""}`, `{"kind":"clean"}`, `{"kind":"future_kind"}`} {
		if got := RenderSyncConflict(raw); got != "" {
			t.Errorf("RenderSyncConflict(%q) = %q, want \"\"", raw, got)
		}
	}
	// Missing hint → derived resolve command from branches.
	derived := RenderSyncConflict(`{"kind":"conflict","run_branch":"rb","base_branch":"bb"}`)
	if !strings.Contains(derived, "git checkout bb && git merge rb") {
		t.Errorf("missing hint should derive a resolve command; got %q", derived)
	}
}

func TestRunStatus_CompletedRunWithSyncConflict_NotPresentedClean(t *testing.T) {
	// The exact B1 trap: state=completed, all tasks accepted →
	// "Progress 1/1 100%", but the output never landed on base.
	run := []byte(`{"name":"sweep","state":"completed","project_id":1,"seq":2,` +
		`"sync_status":"{\"kind\":\"conflict\",\"base_branch\":\"main\",\"run_branch\":\"sweep-2\",\"conflict_files\":[\"x\"],\"hint\":\"git checkout main && git merge sweep-2\"}"}`)
	tasks := []byte(`[{"id":"1:2:t","state":"accepted","task_def_id":"t"}]`)

	out := RunStatus(run, tasks)
	if !strings.Contains(out, "100%") {
		t.Fatalf("precondition: a completed run should still show its progress bar; got:\n%s", out)
	}
	if !strings.Contains(out, "⚠ SYNC CONFLICT") {
		t.Fatalf("B1: a completed-but-sync-conflicted run MUST surface the conflict next to the 100%% bar; got:\n%s", out)
	}
	if !strings.Contains(out, "Resolve:") {
		t.Errorf("operator needs the resolve hint inline; got:\n%s", out)
	}
}

func TestRunList_TagsSyncConflictedRun(t *testing.T) {
	runs := []byte(`[
	  {"name":"clean","state":"completed","project_id":1,"seq":1,"task_count":3},
	  {"name":"lost","state":"completed","project_id":1,"seq":2,"task_count":3,"sync_status":"{\"kind\":\"conflict\",\"base_branch\":\"main\"}"}
	]`)
	out := RunList(runs)
	if !strings.Contains(out, "⚠ SYNC CONFLICT") {
		t.Fatalf("enju runs must tag the conflicted run, not list it as clean [completed]; got:\n%s", out)
	}
	// The clean run line must NOT get the tag.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "clean") && strings.Contains(line, "SYNC CONFLICT") {
			t.Errorf("clean run wrongly tagged: %q", line)
		}
	}
}

func TestReadyTasks_GroupsByRunSeqNotInternalID(t *testing.T) {
	// run_id (internal) 4044 but per-project seq 7 — the label
	// must read "Run #7".
	data := []byte(`[{"id":"1:7:a","run_id":4044,"run_seq":7,"prompt":"p","state":"ready"}]`)
	out := ReadyTasks(data)
	if !strings.Contains(out, "── Run #7 ──") {
		t.Errorf("ready-tasks must group by per-project run seq (#7), got:\n%s", out)
	}
	if strings.Contains(out, "#4044") {
		t.Errorf("internal run_id leaked into the label:\n%s", out)
	}
	// Fallback: payload without run_seq still renders (run_id).
	old := []byte(`[{"id":"1:9:a","run_id":9,"prompt":"p","state":"ready"}]`)
	if got := ReadyTasks(old); !strings.Contains(got, "── Run #9 ──") {
		t.Errorf("missing run_seq should fall back to run_id, got:\n%s", got)
	}
}
