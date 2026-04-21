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

	enjuYaml "github.com/enju-ai/enju/internal/yaml"
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
	// WritesArtifacts is the resolved list of artifact entries
	// this task declares it produces. Each entry carries both
	// the path (per-instance substituted — "summaries/alpha.md"
	// not "summaries/{{stem}}.md") and a Track flag that tells
	// the fat-client whether to commit the file (Track=true) or
	// record it as metadata-only (Track=false, written by the
	// wrapper but excluded from git via a managed .gitignore
	// block). The coordinator honors the same flag when writing
	// the artifact-index entry — untracked entries land with
	// commit_sha="".
	WritesArtifacts enjuYaml.WriteArtifacts
	// ReadsArtifacts is the resolved list of upstream artifact
	// paths this task consumes. Populated per-instance (so
	// "summaries/alpha.md", not "summaries/{{stem}}.md").
	// Surfaced here for the claim-time presence check: if an
	// upstream artifact is flagged untracked in the project's
	// artifact index but its file is absent from this citizen's
	// workspace, the claim is refused before coordinator state
	// flips — the producer never pushed the content and this
	// citizen has no way to read it.
	ReadsArtifacts []string
	// RunSourcePath mirrors the parent run's source_path —
	// the template bundle directory that was snapshotted into
	// `enju/runs/{run_seq}/template/` at create_run time.
	// Empty for inline-YAML runs. The compute executor uses
	// this to resolve `script:` from the snapshot instead of
	// the live enju/templates/ path, so a template edit after
	// the run was created can't change its behavior.
	RunSourcePath string
	// RunParams is the submitted run-level params map (after
	// defaults filled in). Exposed to compute-task scripts
	// as ENJU_PARAM_<name> env vars. nil when the run has
	// no params: block.
	RunParams map[string]interface{}
	// InstanceParams is the per-iteration for_each variable
	// map for this task instance (e.g. {"stem": "alpha"} for
	// alpha:describe). Merged with RunParams when emitting
	// ENJU_PARAM_* env vars. nil for singleton tasks.
	InstanceParams map[string]interface{}
	// Env is the task-definition-level env: block for compute
	// tasks — keys become env var names, values become env
	// var values, injected into the compute script's process.
	// Values have already been {{param}}-substituted at parse
	// time. nil or empty for non-compute tasks and for compute
	// tasks that didn't declare env:.
	Env map[string]string
	// Branch is the git branch this task's run commits to —
	// populated from the run's branch field (falling back to
	// the project's default_branch when the run was created
	// without a branch override). The fat-client submit path
	// checks this out before writing + pushing so runs on
	// parallel branches don't stomp on each other's files.
	Branch string
	// Mode is the compute-task execution mode ("sync" / "async")
	// when the task was declared, as stored on the task record.
	// Empty for non-compute tasks. Use yaml.ResolvedMode (or
	// the resolvedMode helper) at read sites to get the
	// default-applied value.
	Mode string
	// Container is the Docker image reference the compute
	// script should run inside (empty = run directly on the
	// host). Threaded verbatim into the compute.Spec so the
	// wrapper can pick the container vs direct-exec branch.
	Container string
	// ResultDir is the pre-computed repo-relative path for
	// this task's result files (e.g. enju/runs/3-gwas/align or
	// enju/runs/3-gwas/align/sample=S1). The server computes
	// it from the task's instance params via
	// engine.ComputeResultDir; clients consume it as-is
	// rather than rebuilding it from (runSeq, instanceKey,
	// taskDefID).
	ResultDir string
	// RunSlug is the per-run slug ("variant-calling",
	// "gwas", or "run" for nameless inline YAML) that shows
	// up in enju/runs/{seq}-{slug}/. Threaded here so the
	// fat-client executor can locate the template snapshot
	// (enju/runs/{seq}-{slug}/template-snapshot/) — without
	// this, the client would have to recompute the slug and
	// risk drifting from the server's stored value.
	RunSlug string
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
	// writes_artifacts on the wire is the typed WriteArtifacts
	// shape — [{"path":...,"track":...}]. For legacy DB rows
	// written as a bare []string the yaml.WriteArtifacts
	// UnmarshalJSON also accepts the old form. Re-marshal the
	// raw element + decode through the typed parser so both
	// flavors hit the same code path.
	if v, ok := raw["writes_artifacts"]; ok && v != nil {
		if b, err := json.Marshal(v); err == nil {
			var w enjuYaml.WriteArtifacts
			if err := json.Unmarshal(b, &w); err == nil {
				meta.WritesArtifacts = w
			}
		}
	}
	if v, ok := raw["reads_artifacts"].([]interface{}); ok {
		for _, p := range v {
			if s, ok := p.(string); ok {
				meta.ReadsArtifacts = append(meta.ReadsArtifacts, s)
			}
		}
	}
	// run_source_path is populated when the parent run was
	// instantiated from a template bundle (the template-bundle
	// feature snapshots the bundle into enju/runs/{seq}/template-snapshot/
	// at create_run time). The compute executor uses it to
	// resolve the script from that snapshot instead of the
	// live enju/templates/ path, which gives each run a frozen
	// recipe that's immune to later edits.
	if v, ok := raw["run_source_path"].(string); ok {
		meta.RunSourcePath = v
	}
	if v, ok := raw["run_params"].(map[string]interface{}); ok {
		meta.RunParams = v
	}
	if v, ok := raw["instance_params_map"].(map[string]interface{}); ok {
		meta.InstanceParams = v
	}
	if v, ok := raw["run_branch"].(string); ok {
		meta.Branch = v
	}
	if v, ok := raw["env"].(map[string]interface{}); ok {
		env := make(map[string]string, len(v))
		for k, raw := range v {
			if s, ok := raw.(string); ok {
				env[k] = s
			}
		}
		if len(env) > 0 {
			meta.Env = env
		}
	}
	if v, ok := raw["mode"].(string); ok {
		meta.Mode = v
	}
	if v, ok := raw["container"].(string); ok {
		meta.Container = v
	}
	if v, ok := raw["result_dir"].(string); ok {
		meta.ResultDir = v
	}
	if v, ok := raw["run_slug"].(string); ok {
		meta.RunSlug = v
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
