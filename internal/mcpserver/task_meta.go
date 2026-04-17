package mcpserver

// Task-metadata fetch layer used by claim / submit / inputs
// paths. The fat client consults task records across many tool
// handlers — centralizing the lookup + shape here keeps each
// handler small and the "what does the client know about a
// task" contract in one place.

import (
	"context"
	"encoding/json"
	"fmt"
)

// taskMeta captures the fields the MCP client needs to drive the
// fat-client submit + claim paths: project identity, run layout,
// and the named-outputs schema (if any) so multi-file submits can
// compute per-output filenames without a second round-trip.
type taskMeta struct {
	ID               string
	ProjectID        int64
	ProjectRemoteURL string
	ProjectName      string
	RunSeq           int
	TaskDefID        string
	InstanceKey      string
	// State is the task's current lifecycle state. Populated
	// from the coordinator's task record so the fat-client
	// submit helper can pre-reject submissions against tasks
	// that are already in a terminal state, avoiding a
	// phantom-commit-style round-trip (commit+push → server
	// rejects with "task cannot accept result").
	State string
	// Action is the task's action type ("answer", "review", etc).
	// Used by the fat-client submit helper to pre-validate
	// action-specific fields (e.g. decision on review) BEFORE
	// touching the local clone, so a rejected submission never
	// leaves a phantom commit in git history.
	Action string
	// ReviewsTarget is the short task id this review task
	// evaluates. Empty for non-review tasks. Surfaced so the
	// client-side formatter can show the reviewer what they're
	// reviewing without a separate fetch.
	ReviewsTarget string
	// VoteOptionsJSON is the declared options list for
	// action:vote tasks, copied verbatim from the coordinator's
	// task record. Used by client-side pre-validation (to
	// reject unknown option ids before any git write) and by
	// the claim-response formatter (to show the voter what the
	// choices are). Empty for non-vote tasks.
	VoteOptionsJSON string
	// Citizens is the declared citizens count for multi-voter /
	// multi-reviewer tasks. Defaults to 1. When > 1, the
	// fat-client submit path writes to per-citizen result
	// subdirectories so parallel submissions don't race on the
	// same result.md file.
	Citizens int
	// OutputsSchemaJSON is the serialized outputs schema from the
	// task's YAML, or empty if the task has no named outputs.
	// Parsed via mcpgit.ParseNamedOutputSchema by the fat-client
	// submit helper.
	OutputsSchemaJSON string
	// Script is the script path for action:compute tasks.
	Script string
}

// fetchTaskMeta reads a task's metadata from the coordinator. Used
// by handleClaimTask, handleGetTaskInputs, and handleSubmitResult to
// decide whether to use the fat-client or legacy path.
func (c *apiClient) fetchTaskMeta(ctx context.Context, taskID string) (*taskMeta, error) {
	data, err := c.get(ctx, "/api/v1/tasks/"+taskID)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing task: %w", err)
	}
	if errMsg, ok := raw["error"].(string); ok {
		return nil, fmt.Errorf("%s", errMsg)
	}
	meta := &taskMeta{ID: taskID}
	if v, ok := raw["project_id"].(float64); ok {
		meta.ProjectID = int64(v)
	}
	if v, ok := raw["project_remote_url"].(string); ok {
		meta.ProjectRemoteURL = v
	}
	if v, ok := raw["project_name"].(string); ok {
		meta.ProjectName = v
	}
	if v, ok := raw["run_seq"].(float64); ok {
		meta.RunSeq = int(v)
	}
	if v, ok := raw["task_def_id"].(string); ok {
		meta.TaskDefID = v
	}
	if v, ok := raw["instance_key"].(string); ok {
		meta.InstanceKey = v
	}
	if v, ok := raw["outputs"].(string); ok {
		meta.OutputsSchemaJSON = v
	}
	if v, ok := raw["action"].(string); ok {
		meta.Action = v
	}
	if v, ok := raw["reviews_target"].(string); ok {
		meta.ReviewsTarget = v
	}
	if v, ok := raw["vote_options"].(string); ok {
		meta.VoteOptionsJSON = v
	}
	if v, ok := raw["state"].(string); ok {
		meta.State = v
	}
	if v, ok := raw["citizens"].(float64); ok {
		meta.Citizens = int(v)
	}
	if v, ok := raw["script"].(string); ok {
		meta.Script = v
	}
	return meta, nil
}

// useFatClient reports whether the MCP client should take the
// iteration A.2 path for a given task: the client has a workspace
// configured AND the project has an external remote URL.
func (c *apiClient) useFatClient(meta *taskMeta) bool {
	if c.workspace == nil || meta == nil {
		return false
	}
	if meta.ProjectRemoteURL != "" {
		return true
	}
	// External-dir projects (from enju_init) have no remote URL
	// but do have a workspace registered.
	return c.workspace.HasExternalDir(meta.ProjectID)
}
