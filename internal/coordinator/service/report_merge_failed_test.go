package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// submittedComputeTask builds project + active run + a compute task
// parked in SUBMITTED — the state a task is in at post-accept merge
// time (deferred-accept model), which is when ReportMergeFailed fires.
// Returns (coord, projectID, runSeq, taskID).
func submittedComputeTask(t *testing.T) (*Coordinator, int64, int, string) {
	t.Helper()
	st, coord := newCVFStore(t)
	now := time.Now()

	res, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateProject{Project: store.ProjectRecord{Name: "p", CreatedAt: now, UpdatedAt: now}},
	}})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID := res.ProjectID

	const runYAML = `name: r
tasks:
  - id: section_3
    action: compute
    script: run.sh
`
	res, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateRun{Run: store.RunRecord{
			ProjectID: projectID, Name: "r", YAMLData: runYAML,
			State: store.RunActive, CreatedAt: now, UpdatedAt: now,
		}},
	}})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID, runSeq := res.RunID, res.RunSeq

	tid := fmt.Sprintf("%d:%d:section_3", projectID, runSeq)
	if _, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateTask{Task: store.TaskRecord{
			ID: tid, RunID: runID, Seq: 1, TaskDefID: "section_3",
			Action: "compute", Script: "run.sh", ResultType: "text",
			State: store.TaskSubmitted, Citizens: 1, CreatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("create compute task: %v", err)
	}
	return coord, projectID, runSeq, tid
}

// A transient merge failure (push race) parks the task
// failed_retryable — recoverable via enju_retry_task — rather than
// terminally failing it. This is fix #2 for the parallel push race:
// before, every merge_failed was terminal and a transient push
// rejection abandoned the whole fan-out.
func TestReportMergeFailed_Transient_ParksRetryable(t *testing.T) {
	coord, projectID, runSeq, taskID := submittedComputeTask(t)

	resp, err := ReportMergeFailed(coord, nil, projectID, runSeq, ReportMergeFailedParams{
		TopicBranch: "topic-section_3",
		RunBranch:   "preview",
		Error:       "git push origin preview: exit status 1 (non-fast-forward)",
		TaskID:      taskID,
		Transient:   true,
	})
	if err != nil {
		t.Fatalf("ReportMergeFailed(transient): %v", err)
	}
	if resp.Status != "failed_retryable" {
		t.Errorf("Status = %q, want failed_retryable", resp.Status)
	}
	if len(resp.SkippedDescendants) != 0 {
		t.Errorf("transient failure must not skip-cascade descendants, got %v", resp.SkippedDescendants)
	}
	task, _ := coord.Store.GetTask(taskID)
	if task == nil || store.TaskState(task.State) != store.TaskFailedRetryable {
		t.Fatalf("task state = %v, want failed_retryable", task.State)
	}
}

// A non-transient merge failure (genuine misconfig / Enju bug) stays
// TERMINAL — pins the original behavior so the fix doesn't soften
// real failures into infinitely-retryable ones.
func TestReportMergeFailed_NonTransient_StaysTerminal(t *testing.T) {
	coord, projectID, runSeq, taskID := submittedComputeTask(t)

	resp, err := ReportMergeFailed(coord, nil, projectID, runSeq, ReportMergeFailedParams{
		TopicBranch: "topic-section_3",
		RunBranch:   "preview",
		Error:       "remote repository not found",
		TaskID:      taskID,
		Transient:   false,
	})
	if err != nil {
		t.Fatalf("ReportMergeFailed(non-transient): %v", err)
	}
	if resp.Status != "failed" {
		t.Errorf("Status = %q, want failed", resp.Status)
	}
	task, _ := coord.Store.GetTask(taskID)
	if task == nil || store.TaskState(task.State) != store.TaskFailed {
		t.Fatalf("task state = %v, want failed (terminal)", task.State)
	}
}
