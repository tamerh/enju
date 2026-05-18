package inbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit is a stand-in inbox.Deps for unit tests. The real
// implementations wrap workspace.Project.ReadFileAtCommit; tests
// inject scripted blob responses keyed by "<sha>:<path>".
type fakeGit struct {
	files   map[string]string
	gitErr  error // set to simulate a transient git read failure
	failKey string
}

func (g *fakeGit) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
	key := commitSHA + ":" + repoRelPath
	if g.gitErr != nil && key == g.failKey {
		return nil, false, g.gitErr
	}
	v, ok := g.files[key]
	if !ok {
		return nil, false, nil
	}
	return []byte(v), true, nil
}

// writeLog is a tiny test helper: writes `lines` to a fresh
// live.jsonl in dir and returns the path.
func writeLog(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBuildInbox_Happy pins the simplest case: a single
// task_ready event for me with a parent that has content in git.
// Inbox surfaces the row with the parent's result.md inlined.
func TestBuildInbox_Happy(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"review","task_id":"5:1:review","assign_to":"tamer","metadata":{"parents":[{"task_id":"5:1:abstract","action":"answer","commit_sha":"abc1234","result_dir":".enju/runs/1-paper/abstract"}]}}`,
	})
	deps := &fakeGit{files: map[string]string{
		"abc1234:.enju/runs/1-paper/abstract/result.md": "The TP53 gene encodes a tumor suppressor.",
	}}
	rows, err := BuildInbox(livePath, "tamer", deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].TaskID != "5:1:review" || rows[0].Action != "review" {
		t.Errorf("row mismatch: %+v", rows[0])
	}
	if len(rows[0].Upstream) != 1 || rows[0].Upstream[0].Content != "The TP53 gene encodes a tumor suppressor." {
		t.Errorf("upstream content not pulled from git: %+v", rows[0].Upstream)
	}
}

// TestBuildInbox_LatestEventWins is the core invariant test:
// a task that became ready and was then claimed (iteration_started)
// must NOT appear in the inbox. The newer event decides.
func TestBuildInbox_LatestEventWins(t *testing.T) {
	livePath := writeLog(t, []string{
		// oldest first — the file is read newest-first
		`{"type":"task_ready","subtype":"review","task_id":"5:1:done","assign_to":"tamer"}`,
		`{"type":"iteration_started","task_id":"5:1:done","citizen":"tamer"}`,
		`{"type":"task_ready","subtype":"review","task_id":"5:1:fresh","assign_to":"tamer"}`,
	})
	rows, err := BuildInbox(livePath, "tamer", &fakeGit{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TaskID != "5:1:fresh" {
		t.Errorf("expected only fresh row (claimed task hidden), got %+v", rows)
	}
}

// TestBuildInbox_FanOutPartner checks the multi-assignee fan-out
// case: task_ready fires once per assignee. Walking backward,
// alice should not be tricked into treating bob's event as a
// state-change for the task — alice continues scanning back to
// find her own ready event.
func TestBuildInbox_FanOutPartner(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"review","task_id":"5:1:joint","assign_to":"alice"}`,
		`{"type":"task_ready","subtype":"review","task_id":"5:1:joint","assign_to":"bob"}`,
	})
	rows, _ := BuildInbox(livePath, "alice", &fakeGit{})
	if len(rows) != 1 || rows[0].TaskID != "5:1:joint" {
		t.Errorf("alice's ready event missed past bob's fan-out event: %+v", rows)
	}
}

// TestBuildInbox_NotMine: a ready task assigned to someone else
// stays out of my inbox. There's no leftover state-changing
// event so we keep scanning, but we never match assign_to and
// rightly come back empty.
func TestBuildInbox_NotMine(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"review","task_id":"5:1:hers","assign_to":"alice"}`,
	})
	rows, _ := BuildInbox(livePath, "tamer", &fakeGit{})
	if len(rows) != 0 {
		t.Errorf("expected empty inbox, got %+v", rows)
	}
}

// TestBuildInbox_Invalidated: a task that became ready and was
// later invalidated drops from the inbox.
func TestBuildInbox_Invalidated(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"answer","task_id":"5:1:gone","assign_to":"tamer"}`,
		`{"type":"task_invalidated","task_id":"5:1:gone"}`,
	})
	rows, _ := BuildInbox(livePath, "tamer", &fakeGit{})
	if len(rows) != 0 {
		t.Errorf("expected invalidated task hidden, got %+v", rows)
	}
}

// TestBuildInbox_ReboundReady: request_changes pulls a task back
// to ready. The cascade emits a fresh task_ready, which is now
// the latest event for that task — the inbox surfaces it again.
func TestBuildInbox_ReboundReady(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"answer","task_id":"5:1:rev","assign_to":"tamer"}`,
		`{"type":"iteration_started","task_id":"5:1:rev"}`,
		`{"type":"task_completed","task_id":"5:1:rev"}`,
		// request_changes cascade rebound:
		`{"type":"task_ready","subtype":"answer","task_id":"5:1:rev","assign_to":"tamer"}`,
	})
	rows, _ := BuildInbox(livePath, "tamer", &fakeGit{})
	if len(rows) != 1 || rows[0].TaskID != "5:1:rev" {
		t.Errorf("rebound ready missed: %+v", rows)
	}
}

// TestBuildInbox_SkippedParent: a parent in the parents list
// with empty commit_sha (the task was skipped via vote-cascade
// or fail-cascade) renders as an upstream row with empty
// content — no git read attempted.
func TestBuildInbox_SkippedParent(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"review","task_id":"5:1:rev","assign_to":"tamer","metadata":{"parents":[{"task_id":"5:1:skipped","action":"answer","commit_sha":"","result_dir":""}]}}`,
	})
	rows, _ := BuildInbox(livePath, "tamer", &fakeGit{})
	if len(rows) != 1 || len(rows[0].Upstream) != 1 {
		t.Fatalf("expected 1 row + 1 upstream, got %+v", rows)
	}
	if rows[0].Upstream[0].Content != "" {
		t.Errorf("expected empty content for skipped parent, got %q", rows[0].Upstream[0].Content)
	}
}

// TestBuildInbox_MissingFile: live.jsonl doesn't exist (project
// clone materialized but supervisor hasn't run yet) → empty
// inbox, no error.
func TestBuildInbox_MissingFile(t *testing.T) {
	dir := t.TempDir()
	rows, err := BuildInbox(filepath.Join(dir, "nope.jsonl"), "tamer", &fakeGit{})
	if err != nil {
		t.Errorf("expected no error for missing file, got %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// TestBuildInbox_GitErrorIsBestEffort: a transient git read
// failure on a parent's result.md leaves the upstream row in
// place but without inlined content. Inbox doesn't fail the
// whole call for one missing parent.
func TestBuildInbox_GitErrorIsBestEffort(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"review","task_id":"5:1:rev","assign_to":"tamer","metadata":{"parents":[{"task_id":"5:1:p","action":"answer","commit_sha":"deadbeef","result_dir":".enju/runs/1-r/p"}]}}`,
	})
	deps := &fakeGit{
		gitErr:  errors.New("simulated git error"),
		failKey: "deadbeef:.enju/runs/1-r/p/result.md",
	}
	rows, err := BuildInbox(livePath, "tamer", deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Upstream[0].Content != "" {
		t.Errorf("expected empty content (git error best-effort), got %q", rows[0].Upstream[0].Content)
	}
}

// TestBuildInbox_MultiCitizenCoAssigneeNotHidden pins the
// citizen-scoped event handling. On a multi-reviewer task
// assigned to alice + bob, alice claiming first emits
// iteration_started with citizen=alice. Bob's inbox must still
// show the task — alice's progress is HER state change, not the
// task's. Without this, the inbox silently degrades on multi-
// citizen flows (review-of-review, parallel multi-reviewer).
func TestBuildInbox_MultiCitizenCoAssigneeNotHidden(t *testing.T) {
	livePath := writeLog(t, []string{
		// joint task readied for both — fan-out
		`{"type":"task_ready","subtype":"review","task_id":"5:1:joint","assign_to":"alice"}`,
		`{"type":"task_ready","subtype":"review","task_id":"5:1:joint","assign_to":"bob"}`,
		// alice claimed first
		`{"type":"iteration_started","task_id":"5:1:joint","citizen":"alice"}`,
	})
	rows, _ := BuildInbox(livePath, "bob", &fakeGit{})
	if len(rows) != 1 || rows[0].TaskID != "5:1:joint" {
		t.Errorf("co-assignee bob's inbox lost the task after alice claimed: %+v", rows)
	}
	// And alice (who claimed) must NOT see it.
	rowsAlice, _ := BuildInbox(livePath, "alice", &fakeGit{})
	if len(rowsAlice) != 0 {
		t.Errorf("alice claimed the task but it's still in her inbox: %+v", rowsAlice)
	}
}

// TestBuildInbox_TaskScopedTerminalHidesEveryone pins that a
// task-scoped terminal event (task_completed, task_invalidated,
// etc.) hides the task from every assignee, regardless of
// whether they ever acted. After the whole task is done, no
// co-assignee should still see it.
func TestBuildInbox_TaskScopedTerminalHidesEveryone(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"review","task_id":"5:1:done","assign_to":"alice"}`,
		`{"type":"task_ready","subtype":"review","task_id":"5:1:done","assign_to":"bob"}`,
		`{"type":"iteration_started","task_id":"5:1:done","citizen":"alice"}`,
		`{"type":"task_completed","task_id":"5:1:done"}`,
	})
	for _, user := range []string{"alice", "bob"} {
		rows, _ := BuildInbox(livePath, user, &fakeGit{})
		if len(rows) != 0 {
			t.Errorf("task_completed should hide from %s's inbox, got: %+v", user, rows)
		}
	}
}

// TestBuildInbox_OwnSubmitTerminatesMyView pins that my own
// iteration_completed (single- or multi-citizen) marks the task
// as decided for me — I submitted, my inbox shouldn't still
// show "you have work to do here." Co-assignees who haven't
// acted are unaffected; tested in MultiCitizenCoAssigneeNotHidden.
func TestBuildInbox_OwnSubmitTerminatesMyView(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"task_ready","subtype":"answer","task_id":"5:1:mine","assign_to":"tamer"}`,
		`{"type":"iteration_started","task_id":"5:1:mine","citizen":"tamer"}`,
		`{"type":"iteration_completed","task_id":"5:1:mine","citizen":"tamer"}`,
	})
	rows, _ := BuildInbox(livePath, "tamer", &fakeGit{})
	if len(rows) != 0 {
		t.Errorf("expected my own submit to clear from inbox, got: %+v", rows)
	}
}

// TestBuildInbox_RunTerminatedRetiresReadyTasks pins the bug
// fix: enju_terminate_run emits ONE coarse run_terminated event
// (run_seq in metadata, no task_id) instead of N per-task
// task_skipped. A task that was task_ready+me in that run must
// drop out of the inbox — it's skipped, not actionable.
func TestBuildInbox_RunTerminatedRetiresReadyTasks(t *testing.T) {
	livePath := writeLog(t, []string{
		// oldest first; file read newest-first.
		`{"type":"task_ready","subtype":"answer","task_id":"3:7:draft","assign_to":"tamer"}`,
		`{"type":"task_ready","subtype":"review","task_id":"3:8:keep","assign_to":"tamer"}`,
		`{"type":"run_terminated","metadata":{"run_seq":7,"reason":"aborted"}}`,
	})
	rows, err := BuildInbox(livePath, "tamer", &fakeGit{})
	if err != nil {
		t.Fatal(err)
	}
	// Run 7's ready task is retired; run 8's is untouched.
	if len(rows) != 1 || rows[0].TaskID != "3:8:keep" {
		t.Errorf("run_terminated(7) should retire 3:7:draft but keep 3:8:keep; got %+v", rows)
	}
}

// TestBuildInbox_RunTerminatedOnlyScopesItsRun: a run_terminated
// for an unrelated run must not retire a ready task in a live run.
func TestBuildInbox_RunTerminatedOnlyScopesItsRun(t *testing.T) {
	livePath := writeLog(t, []string{
		`{"type":"run_terminated","metadata":{"run_seq":9}}`,
		`{"type":"task_ready","subtype":"answer","task_id":"3:7:draft","assign_to":"tamer"}`,
	})
	rows, _ := BuildInbox(livePath, "tamer", &fakeGit{})
	if len(rows) != 1 || rows[0].TaskID != "3:7:draft" {
		t.Errorf("run_terminated(9) must not affect run 7's ready task; got %+v", rows)
	}
}

// TestFormatInbox_Empty pins the no-items rendering — assistants
// pattern-match this to skip rendering an empty inbox.
func TestFormatInbox_Empty(t *testing.T) {
	if got := FormatInbox(nil); got != "(no tasks waiting on you)" {
		t.Errorf("empty rendering drifted: %q", got)
	}
}
