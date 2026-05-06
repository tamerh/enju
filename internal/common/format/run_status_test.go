package format

// Parallel-merge phase 5 contract: format.RunStatus surfaces
// system-spawned merge_resolve tasks in their own attention-
// grabbing block so non-assignees scanning run_status see the
// "human needed" signal.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRunStatus_RendersMergeResolveBlock(t *testing.T) {
	run, _ := json.Marshal(map[string]interface{}{
		"id":          int64(1),
		"project_id":  int64(7),
		"seq":         float64(3),
		"name":        "demo",
		"state":       "running",
		"task_count":  3,
		"created_at":  "2026-05-06T00:00:00Z",
	})
	tasks, _ := json.Marshal([]map[string]interface{}{
		{"id": "7:3:alpha", "task_def_id": "alpha", "state": "accepted", "action": "answer"},
		{"id": "7:3:beta", "task_def_id": "beta", "state": "accepted", "action": "answer"},
		{
			"id":          "7:3:beta_merge_resolve_1",
			"task_def_id": "beta_merge_resolve_1",
			"state":       "ready",
			"action":      "merge_resolve",
		},
	})
	out := RunStatus(run, tasks)
	if !strings.Contains(out, "Merge resolutions awaiting human") {
		t.Errorf("expected merge_resolve block header in output:\n%s", out)
	}
	if !strings.Contains(out, "7:3:beta_merge_resolve_1") {
		t.Errorf("expected merge_resolve task id in output:\n%s", out)
	}
	if !strings.Contains(out, "[ready]") {
		t.Errorf("expected merge_resolve state badge in output:\n%s", out)
	}
}

func TestRunStatus_NoMergeResolveBlockWhenAbsent(t *testing.T) {
	run, _ := json.Marshal(map[string]interface{}{
		"id":          int64(1),
		"project_id":  int64(7),
		"seq":         float64(1),
		"name":        "vanilla",
		"state":       "running",
		"task_count":  1,
		"created_at":  "2026-05-06T00:00:00Z",
	})
	tasks, _ := json.Marshal([]map[string]interface{}{
		{"id": "7:1:alpha", "task_def_id": "alpha", "state": "ready", "action": "answer"},
	})
	out := RunStatus(run, tasks)
	if strings.Contains(out, "Merge resolutions") {
		t.Errorf("merge_resolve block should be absent when no such tasks exist:\n%s", out)
	}
}

// Accepted merge_resolve tasks shouldn't surface — the resolution
// is done; rendering them in the attention block would be noise.
func TestRunStatus_HidesAcceptedMergeResolve(t *testing.T) {
	run, _ := json.Marshal(map[string]interface{}{
		"id":          int64(1),
		"project_id":  int64(7),
		"seq":         float64(2),
		"name":        "after-merge",
		"state":       "completed",
		"task_count":  1,
		"created_at":  "2026-05-06T00:00:00Z",
	})
	tasks, _ := json.Marshal([]map[string]interface{}{
		{"id": "7:2:beta_merge_resolve_1", "task_def_id": "beta_merge_resolve_1", "state": "accepted", "action": "merge_resolve"},
	})
	out := RunStatus(run, tasks)
	if strings.Contains(out, "Merge resolutions awaiting human") {
		t.Errorf("accepted merge_resolve should not appear in 'awaiting human' block:\n%s", out)
	}
}
