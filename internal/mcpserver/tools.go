package mcpserver

// MCP tool-schema constructors. Each toolX() returns the
// mcp.Tool describing a single enju_X tool: its name,
// description, and argument schema. Handlers live in their
// feature-specific files (claim.go, submit.go, project.go,
// etc.) — keeping the schemas here as a flat catalogue makes
// it easy to audit the tool surface in one read.

import "github.com/mark3labs/mcp-go/mcp"

// --- Tool Definitions ---

func toolListRuns() mcp.Tool {
	return mcp.NewTool("enju_list_runs",
		mcp.WithDescription("List runs. Optionally filter by project."),
		mcp.WithNumber("project_id",
			mcp.Description("Filter by project ID (integer, optional)"),
		),
	)
}

func toolListReadyTasks() mcp.Tool {
	return mcp.NewTool("enju_list_ready_tasks",
		mcp.WithDescription("List tasks that are ready to be claimed. Paste the output verbatim in your reply — it's pre-formatted for the human. Optionally filter by project and run."),
		mcp.WithNumber("project_id",
			mcp.Description("Filter by project ID (optional)"),
		),
		mcp.WithNumber("run_id",
			mcp.Description("Filter by run ID within project (optional, requires project_id)"),
		),
	)
}

func toolClaimTask() mcp.Tool {
	return mcp.NewTool("enju_claim_task",
		mcp.WithDescription(`Claim a task to work on. This opens a collaboration window — iterate with the human, discuss, refine. Only submit when the result is ready. Returns the task prompt and upstream context. After claiming, tell the human which task you're now working on.

Set include_context=false for a lean response: task id, deadline, action schema (options/reviews/outputs), and the raw prompt template only — no inlined artifact contents, no resolved-prompt substitution, no upstream fan-in. Useful for scripted citizens that already have the context or can fetch with enju_get_artifact when needed.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to claim"),
		),
		mcp.WithBoolean("include_context",
			mcp.Description("Include the full upstream context block (default: true). Set to false to get a minimal response without inlined artifact content or resolved prompt."),
		),
	)
}

func toolClaimReadyMatching() mcp.Tool {
	return mcp.NewTool("enju_claim_ready_matching",
		mcp.WithDescription(`Bulk-claim every ready task in a run that matches the filter. Symmetric to enju_submit_results_batch — one MCP call instead of N, designed for bulk-work flows: a reviewer approving all ready reviews, a rater handling every item in a labeling cohort, an agent running a paper-scale evaluation.

Scope:
- One project + one run (required). Cross-run selectors aren't supported — keep the claim cohort controllable.
- Optional action filter (review, vote, answer, contribute, compute). Omit to match any ready action.
- Pre-filters on access control (assign_to) before claiming, so the response isn't mostly failed claims on tasks this citizen can't touch.
- Skips tasks already claimed by this citizen — re-running the selector is idempotent.

Response shape:
- Per-entry status: claimed | skipped (already_claimed) | error (with reason).
- By default the response is minimal — task id, action, deadline, declared schema (options/reviews/outputs). Set include_context=true for the full single-claim response (inlined artifact content, resolved prompt) per entry.

Safety:
- Default limit is 50, hard cap 500. Bulk-claiming a 15K-task run in one call is almost always a mistake; the cap forces the caller to pass limit explicitly when they really mean it.
- Ordering: task seq ASC within the run (deterministic, matches DAG construction order).
- Atomicity: the coordinator's per-claim locking applies. A concurrent selector from another citizen won't double-claim the same task — the loser's entry reports "error" for that task.

Pipelined runs:
- The selector operates on tasks that are READY right now — PENDING tasks waiting on upstream are invisible to this call. Re-call it after each wave completes to pick up newly-unblocked work. This is exactly right for staged DAGs (e.g. a run with N judges that all need to accept before the syntheses can run): one selector call claims the ready judges; after they submit, a second call claims the newly-ready syntheses. The DAG gate happens at the edge of each call, not up front.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project"),
		),
		mcp.WithString("action",
			mcp.Description(`Filter by action type: "review", "vote", "answer", "contribute", "compute". Omit to match any action.`),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of tasks to claim (default 50, hard cap 500)."),
		),
		mcp.WithBoolean("include_context",
			mcp.Description("Include the full resolved-prompt + inlined-artifact context per entry (default false — use the lean form for bulk-work flows)."),
		),
	)
}

func toolGetTaskInputs() mcp.Tool {
	return mcp.NewTool("enju_get_task_inputs",
		mcp.WithDescription("Get the upstream dependency results for a task. Use this to see what previous tasks produced."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func toolSubmitResult() mcp.Tool {
	return mcp.NewTool("enju_submit_result",
		mcp.WithDescription(`Submit a result for a claimed task. The task must be claimed by you first.

For simple tasks: provide 'content' as a string.
For tasks with named outputs: provide 'outputs_json' as a JSON object mapping output names to their values.
For tasks with writes_artifacts: provide 'artifacts_json' mapping each declared artifact path to its new content. You may write any subset of declared paths (permissive — declared is an upper bound).
For action:review tasks: provide 'decision' — one of:
  - "approve"          — target → ACCEPTED, downstream unblocks
  - "request_changes"  — retry cascade: target → READY, artifact rolls back, descendants → PENDING (author revises + resubmits)
  - "reject"           — fail cascade: target → FAILED (terminal), artifact rolls back, descendants → SKIPPED
  - "comment"          — non-blocking; target state unchanged
Your prose content is the reviewer's feedback in all cases.
For action:vote tasks: provide 'option' as one of the declared option ids from the task's 'options:' list. Your prose content is free-form commentary. If the winning option has 'activates:' set, the DAG routes down that branch and tasks on losing branches flip to SKIPPED. Votes without 'activates:' are pure decisions — downstream tasks can still read the choice via {{task.winning_option}}.
The task detail shows the schema (outputs, writes_artifacts, reviews target, options) so you know what's expected.
After submitting, call enju_run_status to show the human the updated DAG tree — they want to see progress.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
		mcp.WithString("content",
			mcp.Description("The result content as plain text (for simple tasks)"),
		),
		mcp.WithString("outputs_json",
			mcp.Description(`For tasks with named outputs: a JSON string of the outputs object. Example: '{"gene_list": "BRCA1, TP53", "pathways": "KEGG:hsa04110"}'`),
		),
		mcp.WithString("artifacts_json",
			mcp.Description(`For tasks with writes_artifacts: a JSON string mapping each artifact path to its new content. Example: '{"src/analyze.py": "def analyze():\n    pass\n"}'. Paths must be in the task's writes_artifacts list.`),
		),
		mcp.WithString("decision",
			mcp.Description(`Required for action:review tasks: "approve", "request_changes", "reject", or "comment". approve = ship it; request_changes = send back for revision; reject = hard stop (FAILED); comment = non-blocking note.`),
		),
		mcp.WithString("option",
			mcp.Description(`Required for action:vote tasks: one of the declared option ids from the task's 'options:' YAML list (as shown in the claim response's Options block). Ignored on non-vote tasks.`),
		),
		mcp.WithString("model",
			mcp.Description(`Optional per-call override for which model produced this result. Defaults to the model name from the session's -model flag. Use this when you switch models mid-session (e.g. opened MCP with claude-opus-4-7 but produced this specific result with claude-sonnet-4-6) so attribution lines up with reality. Unknown model names are auto-registered into the catalog on first use; pre-register via enju_register_model if you want a prettier display name. Ignored when empty — the session default applies.`),
		),
	)
}

func toolSubmitResultsBatch() mcp.Tool {
	return mcp.NewTool("enju_submit_results_batch",
		mcp.WithDescription(`Submit N results in one MCP call. Same citizen, same project + run, pre-validated upfront. Each entry mirrors enju_submit_result's body: {task_id, content?, decision?, option?, outputs_json?, artifacts_json?, model?}.

Mixed action types are fine — per-entry validation dispatches on the task's action (review needs decision, vote needs option, others need content/outputs/artifacts).

Per-entry 'model' override is supported and honored independently for each entry — useful for cross-model batches where one entry was produced by Opus and another by Sonnet. Omit 'model' in an entry to fall back to the session default (the -model flag).

Rejected upfront as a whole batch if any entry is missing required fields, names an unknown task, crosses project/run boundaries, or directly depends on another batch entry (submitting an upstream + its downstream in the same batch would let the upstream's cascade silently modify the downstream's pre-submit state). Per-entry runtime failures are reported individually — subsequent entries still attempt.

Use for bulk-approval workflows (one reviewer approving N modules), multi-item labeling (one rater emitting N decisions), or composing multi-entry programmatic submissions.`),
		mcp.WithString("submissions",
			mcp.Required(),
			mcp.Description(`JSON array of submission objects, one per task. Example: '[{"task_id":"1:1:a:review","decision":"approve"},{"task_id":"1:1:b:review","decision":"approve","model":"claude-sonnet-4-6"}]'`),
		),
	)
}

func toolListArtifacts() mcp.Tool {
	return mcp.NewTool("enju_list_artifacts",
		mcp.WithDescription("List artifacts in a project's repository. Artifacts are mutable project-scoped files (source code, datasets, templates, docs) shared across all runs in the project."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to list artifacts from"),
		),
		mcp.WithString("prefix",
			mcp.Description("Optional path prefix filter (e.g., 'src/' or 'data/')"),
		),
	)
}

func toolGetArtifact() mcp.Tool {
	return mcp.NewTool("enju_get_artifact",
		mcp.WithDescription("Read the current content of an artifact in a project's repository, plus its provenance (who last wrote it, in which task and run)."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project the artifact belongs to"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("The artifact path relative to the artifacts/ directory (e.g., 'src/analyze.py')"),
		),
	)
}

func toolGetArtifactHistory() mcp.Tool {
	return mcp.NewTool("enju_get_artifact_history",
		mcp.WithDescription("List the chronological write history of an artifact in a project's repository. Returns each commit that touched the artifact, newest first, with the task that produced it when applicable."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project the artifact belongs to"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("The artifact path relative to the artifacts/ directory"),
		),
	)
}

func toolReleaseTask() mcp.Tool {
	return mcp.NewTool("enju_release_task",
		mcp.WithDescription("Release a claimed task back to the pool if you can't complete it. No penalty for voluntary release."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to release"),
		),
	)
}

func toolGetTask() mcp.Tool {
	return mcp.NewTool("enju_get_task",
		mcp.WithDescription("Get details of a specific task including its state, prompt, and dependencies. Paste the output verbatim in your reply — it's pre-formatted."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func toolRunStatus() mcp.Tool {
	return mcp.NewTool("enju_run_status",
		mcp.WithDescription("Get the status of a run including DAG tree view of all tasks. Paste the output verbatim in your reply — it's pre-formatted with progress bar, state icons, and tree structure. Use format=\"mermaid\" when the user wants a visual diagram (shareable, README-friendly, or for large DAGs where the text tree is too wide)."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project (#1, #2, #3)"),
		),
		mcp.WithString("format",
			mcp.Description("Output format: \"default\" (action-oriented text summary with progress bar and queue, the usual reply) or \"mermaid\" (Mermaid flowchart TD syntax — paste into mermaid.live, a markdown file, or the preprint; GitHub renders it natively in issues/PRs). Defaults to \"default\"."),
			mcp.Enum("default", "mermaid"),
		),
	)
}

func toolCreateRun() mcp.Tool {
	return mcp.NewTool("enju_create_run",
		mcp.WithDescription(`Create a new Enju run. Three ways to provide the run definition, pick one:

1. WRITE IT DIRECTLY: pass "yaml" with the full run definition — use this for one-off runs the user is authoring from scratch.
2. FROM A SAVED TEMPLATE: pass "path" pointing at a bundle dir (enju/templates/<name>) or its enju.yaml manifest. At create_run, the bundle is snapshotted into enju/runs/{seq}/template-snapshot/ and the run is pinned to that frozen copy — later edits to the live template don't affect this run. Script paths resolve from the snapshot. Supply "params" with the values the template declares; see enju_list_templates.
3. DIRECT + PARAMS: pass "yaml" AND "params" together — a one-off run whose prompts reference top-level {{param}} values. Less common; mostly useful when the LLM is composing a parameterized run programmatically without saving it as a template file first.

YAML format (same for inline and template files):
  name: "Run name"
  description: "Optional prose — shown in template menus"
  version: 1
  params:                       # optional; makes the YAML reusable
    - name: disease
      type: string               # string | int | bool | list<string>
      required: true
      description: "The disease to analyze"
  for_each:
    variable: [value1, value2]   # optional, for parallel expansion
  tasks:
    - id: task_name
      action: answer
      prompt: "Analyze {{disease}} using {{other_task.content}}."

To browse available templates in a project, call enju_list_templates first. To see a specific template's parameter docs before filling them in, call enju_describe_template.

Dependencies between tasks are inferred automatically from {{task_id.content}} references. Tasks without references to other tasks run in parallel.

List-valued params support a {{param[*]}} expansion in writes_artifacts / reads_artifacts / assign_to / depends_on — one declared element expands to N entries, one per value in the list<string> param. Useful for one-shot tasks that emit or read N files without enumerating every path.

If you don't have a project yet, create one first with enju_create_project.`),
		mcp.WithString("yaml",
			mcp.Description("The run definition in YAML format. Required unless 'path' is provided."),
		),
		mcp.WithString("path",
			mcp.Description("Template bundle reference. Accepts either the bundle dir ('enju/templates/gwas-analysis') or its manifest ('enju/templates/gwas-analysis/enju.yaml'). The bundle is snapshotted into the run's enju/runs/{seq}/template-snapshot/ for reproducibility. Mutually exclusive with 'yaml'."),
		),
		mcp.WithObject("params",
			mcp.Description("Parameter values for a run that declares a top-level 'params:' block. Keys are parameter names; values must match the declared types. Use enju_describe_template to see what a template expects."),
		),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID to create this run in (use enju_list_projects to see existing projects)"),
		),
		mcp.WithString("branch",
			mcp.Description(`Git branch this run commits to. Omit to use the project's default branch. Pass "auto" to have the coordinator pick an unused name — for template runs this is "<bundle>-1", "<bundle>-2", ... (e.g. path="enju/templates/gene-mapping" → "gene-mapping-1"); for inline YAML it falls back to "run-1", "run-2", .... Useful for parallel parameter sweeps. Pass an explicit name ("experiment-2", "enju/work") for a named isolated branch. The coordinator enforces SERIAL runs per branch: a second run on the same branch is refused until the first finishes. To run several variants at once, give each its own branch.`),
		),
	)
}

// toolListTemplates is the LLM's template-discovery entry
// point. Returns every YAML file under the project clone's
// enju/templates/ directory with its name, description, and
// parameter summary so the LLM can pick a recipe that fits
// the user's request without reading each file.
func toolFailTask() mcp.Tool {
	return mcp.NewTool("enju_fail_task",
		mcp.WithDescription(`Mark a task as failed with a reason. Works for any action type (answer, contribute, compute, review, vote).

Use this when you can't complete a task — missing data, broken upstream, environment issue, or any other blocker. The task moves to a terminal "failed" state, downstream descendants are blocked, and the reason is visible to all citizens in run_status.

Recovery: the run author or any citizen can use enju_invalidate_task to bounce a failed task back to READY for re-assignment.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The task to fail"),
		),
		mcp.WithString("reason",
			mcp.Required(),
			mcp.Description("Why the task failed (shown to all citizens in run_status)"),
		),
	)
}

func toolExecuteRun() mcp.Tool {
	return mcp.NewTool("enju_execute_run",
		mcp.WithDescription(`Drain all ready action:compute tasks in a run in one call. Stops at the next human decision point (any ready citizen task — vote, review, answer, contribute) and reports it as next_blocker.

Use after any citizen submit to flush the deterministic work the submission unblocked. The cascade advances compute tasks by seq order, one at a time, until:
  - a citizen task is ready (stop_reason="citizen_task_ready", next_blocker names the task)
  - no more ready compute tasks (stop_reason="no_ready_compute" — run is idle/complete)
  - a compute task fails (stop_reason="compute_failed" — downstream blocked)
  - an async compute task is kicked off (stop_reason="async_task_started" — subprocess detached, re-run this tool after it lands)
  - max_tasks reached (stop_reason="max_tasks" — call again to continue)

Per-task attribution: the caller's identity authors each commit in the cascade. If a compute task has assign_to restricting it to specific citizens, it's treated as a blocker (skipped, not executed) — respects explicit scoping.

Use this INSTEAD OF looping enju_execute_task yourself. Typical flow: after enju_submit_result on a citizen task, one enju_execute_run call drives the pipeline to the next gate.

Parallel execution: parallel=N (default 4, max 32) dispatches up to N compute tasks concurrently. Scripts run truly in parallel; the git commit/push layer serializes naturally. Best for script-bound workloads (bio pipelines, ML inference, long-running compute) where wall-clock is dominated by script execution. Pass parallel=1 to force serial when debugging or when scripts are RAM-hungry.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project"),
		),
		mcp.WithNumber("max_tasks",
			mcp.Description("Safety cap on how many tasks this call will execute (default 100, hard cap 1000). Call the tool again to continue past the cap."),
		),
		mcp.WithNumber("parallel",
			mcp.Description("Maximum compute tasks dispatched concurrently within this cascade (default 4, max 32). Scripts run in parallel; git commit/push serializes through the per-project lock. Pass 1 to force serial."),
		),
	)
}

func toolExecuteTask() mcp.Tool {
	return mcp.NewTool("enju_execute_task",
		mcp.WithDescription(`Execute a compute task's script, capture its output, and submit the result — all in one call.

For action:compute tasks only. Claims if not already claimed, runs the declared script, captures stdout as result.md, submits automatically.

Environment variables exposed to the script:
  ENJU_TASK_ID       — full task ID
  ENJU_PROJECT_DIR   — project clone root (cwd)
  ENJU_RUN_DIR       — this task's result directory (also holds context.json)
  ENJU_TEMPLATE_DIR  — template snapshot dir, when instantiated from a bundle
  ENJU_PARAM_<name>  — every run param + for_each iteration var (lists comma-joined)

Also writes $ENJU_RUN_DIR/context.json with structured task context (task_id, iteration, params, reads_artifacts, writes_artifacts) for scripts that need typed access beyond env vars. Read via jq/json in any language.

Declared writes_artifacts paths are picked up from disk post-exit-0 and registered in the artifact index.

Exit 0 → submit; non-0 → fail (stderr becomes failure reason).`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The task to execute"),
		),
	)
}

// toolExportRunEvents materializes a run's event timeline
// (claims, submits, invalidations, tally resolutions) as a
// JSONL file under enju/runs/{seq}/events/. The authoritative
// data lives in the coordinator's contribution_events +
// task_claims; this tool just snapshots it into git so the
// run's directory becomes self-documenting for audits /
// postmortems / preprint figures.
func toolExportRunEvents() mcp.Tool {
	return mcp.NewTool("enju_export_run_events",
		mcp.WithDescription(`Snapshot a run's event timeline (claims, submits, invalidations, tally resolutions) to a git-tracked JSONL file under enju/runs/{seq}/events/{phase}.jsonl.

Events live authoritatively in the coordinator DB. This tool is an on-demand materialization — call it when you want the timeline preserved in git (postmortem, preprint figure, shareable audit).

Phase is a free-form label: 'final' on completion, 'checkpoint' mid-run, or any descriptive string. Same-phase re-export overwrites — no accumulating timeline-1 / timeline-2. Response includes the path + the first ~10 events inline so you can show progress without reading the file.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project"),
		),
		mcp.WithString("phase",
			mcp.Required(),
			mcp.Description("Snapshot label — e.g. 'final' on completion, 'checkpoint' mid-run. Must not contain '/', '\\', '..', or the null byte; ≤64 chars."),
		),
	)
}

// toolExportDiagram snapshots a run's DAG as raw Mermaid
// source to a git-tracked file under enju/runs/{seq}/graph/.
// See handleExportDiagram for the semantics (idempotent same-
// phase overwrite, no-op on unchanged content, response shape).
func toolExportDiagram() mcp.Tool {
	return mcp.NewTool("enju_export_diagram",
		mcp.WithDescription(`Snapshot the run's DAG to a git-tracked Mermaid file for archival, preprint figures, or README embedding.

Writes raw .mmd source (no markdown fences) to enju/runs/{seq}/graph/{phase}.mmd and commits it. Common phase values:
- "initial"  — capture right after enju_create_run; topology only, before any task runs
- "final"    — capture once the run completes; fully resolved (winning branches, rejected tasks, expanded for_each)
- <custom>   — any meaningful mid-run label (e.g. "post_vote_stack_choice", "after_reject_v2"); pick names that tell a story

Idempotent: re-exporting the same phase overwrites the file (no accumulating final-1.mmd, final-2.mmd) and is a no-op if the content would be identical. Safe to call repeatedly without cluttering git history.

Skip during routine enju_run_status checks — only call when the snapshot itself is the artifact (preprint, README, an archival checkpoint). The response also returns the rendered Mermaid inline so you can show the user the diagram immediately AND cite the file path in the same reply.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project (#1, #2, #3)"),
		),
		mcp.WithString("phase",
			mcp.Required(),
			mcp.Description("Snapshot label. Use 'initial' right after creation, 'final' on completion, or any safe-filename string for mid-run checkpoints. Must not contain '/', '\\', '..', or the null byte; must be ≤64 chars."),
		),
	)
}

func toolExportRun() mcp.Tool {
	return mcp.NewTool("enju_export_run",
		mcp.WithDescription(`Export a completed run as a single markdown document. Assembles all task results in DAG order — each task becomes a section with its prompt and result. Use this for the preprint appendix or to review the full output of a run in one place.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_seq",
			mcp.Required(),
			mcp.Description("The run sequence number within the project"),
		),
	)
}

func toolListTemplates() mcp.Tool {
	return mcp.NewTool("enju_list_templates",
		mcp.WithDescription(`List reusable run recipes (templates) in a project. Use before hand-writing YAML — a template usually matches a user's request.

A template is a directory bundle under enju/templates/ with enju.yaml at its root and any supporting scripts/data bundled alongside:
  enju/templates/
    gwas-analysis/
      enju.yaml        # the manifest
      scripts/analyze.py   # bundled, picked up by the snapshot

Scripts + data travel with the manifest as one unit, so a compute task's script: is always co-located. Loose .yaml files directly under enju/templates/ are the legacy single-file shape — they surface with a migration hint in the listing, not a usable template.

Call enju_describe_template for a template's parameters; enju_create_run with path=<bundle> to instantiate.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose enju/templates/ directory to scan"),
		),
	)
}

// toolDescribeTemplate returns the full parameter block for a
// single template so the LLM can turn each param into a
// user-facing question ("which disease?", "which tissue?")
// before filling in values and calling enju_create_run.
func toolDescribeTemplate() mcp.Tool {
	return mcp.NewTool("enju_describe_template",
		mcp.WithDescription(`Show full metadata for one template bundle: name, description, declared params with types/defaults/descriptions. Use after picking a template from enju_list_templates to gather param values before enju_create_run.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose template to describe"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Bundle reference — either the bundle dir ('enju/templates/gwas-analysis') or the full manifest path ('enju/templates/gwas-analysis/enju.yaml'). Both resolve to the same bundle."),
		),
	)
}

func toolListProjects() mcp.Tool {
	return mcp.NewTool("enju_list_projects",
		mcp.WithDescription("List all long-lived projects. Paste the output verbatim in your reply — it's pre-formatted."),
	)
}

func toolCreateProject() mcp.Tool {
	return mcp.NewTool("enju_create_project",
		mcp.WithDescription("Create a brand-new Enju project from scratch. Use this when the user wants to start fresh with no existing folder or code. If the user mentions an existing directory, paper draft, or code repository they want to work with, use enju_init instead."),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Unique project name"),
		),
		mcp.WithString("description",
			mcp.Description("Optional project description"),
		),
		mcp.WithString("remote_url",
			mcp.Description("Optional external git remote URL (e.g., git@github.com:org/repo.git). When set, the coordinator pushes every task result commit to this remote. Auth follows the host's SSH/credential configuration."),
		),
		mcp.WithString("default_branch",
			mcp.Description(`Optional git branch new runs land on by default. Defaults to "main". Orgs that want Enju activity to stay off their repo's main branch set this to e.g. "enju/work" — runs will commit to that branch unless an explicit branch= is passed at create_run time.`),
		),
	)
}

func toolInit() mcp.Tool {
	return mcp.NewTool("enju_init",
		mcp.WithDescription(`Adopt an existing folder as an Enju project. Use this when the user already has a directory (with or without git) and wants to add Enju orchestration on top. Enju writes its scaffold (enju/, enju/templates/) into the folder and respects all existing files. If the user wants to start fresh with nothing, use enju_create_project instead.`),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Project name"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Absolute path to the existing folder to adopt"),
		),
	)
}

func toolFileIssue() mcp.Tool {
	return mcp.NewTool("enju_file_issue",
		mcp.WithDescription(`File a project-level issue (living-workflow phase 3). Issues outlive runs — file in run #2, fix in run #7 is normal. Tester bots use this to record structured findings (one issue per failure mode); humans use it for ad-hoc bug reports. Filing ≠ fixing — triage and fix-task linkage are separate steps. Emits an issue_filed event.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project to file the issue against"),
		),
		mcp.WithString("title", mcp.Required(),
			mcp.Description("Short summary line (one sentence). Required."),
		),
		mcp.WithString("body",
			mcp.Description("Optional prose body — reproducer, context, suspected cause"),
		),
		mcp.WithString("severity",
			mcp.Description(`"low" | "medium" (default) | "high" | "critical"`),
		),
		mcp.WithNumber("found_in_run_seq",
			mcp.Description("Optional run sequence number where the issue was discovered"),
		),
		mcp.WithString("found_in_task_id",
			mcp.Description("Optional fully-qualified task id (project:run:taskdef[:instance]) the issue surfaced in"),
		),
	)
}

func toolListIssues() mcp.Tool {
	return mcp.NewTool("enju_list_issues",
		mcp.WithDescription(`List issues in a project, newest-first. Filters compose: leave them empty to see everything, narrow by status/severity. Returns a one-line summary per issue. For full body + frontmatter, use enju_get_issue.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project to query"),
		),
		mcp.WithString("status",
			mcp.Description(`Comma-separated statuses: "open", "triaged", "closed", "wontfix". Empty = all.`),
		),
		mcp.WithString("severity",
			mcp.Description(`Comma-separated severities: "low", "medium", "high", "critical". Empty = all.`),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum issues to return (default 100, max 1000)"),
		),
	)
}

func toolGetIssue() mcp.Tool {
	return mcp.NewTool("enju_get_issue",
		mcp.WithDescription(`Get full detail for one issue, formatted as YAML frontmatter + body — the same shape the future enju/issues/ISSUE-<NNN>.md filesystem mirror will write to disk.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project the issue belongs to"),
		),
		mcp.WithNumber("issue_seq", mcp.Required(),
			mcp.Description("Per-project issue sequence number (the NNN in ISSUE-NNN)"),
		),
	)
}

func toolTriageIssue() mcp.Tool {
	return mcp.NewTool("enju_triage_issue",
		mcp.WithDescription(`Move an open issue to "triaged" state, optionally adjusting severity. Triage is the "we've looked at this and decided what to do" signal — it's distinct from filing (recording the finding) and from closing (the issue's resolved). Phase 4 will let triage spawn fix-tasks; in phase 3 it's just a status update.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project the issue belongs to"),
		),
		mcp.WithNumber("issue_seq", mcp.Required(),
			mcp.Description("Per-project issue sequence number"),
		),
		mcp.WithString("severity",
			mcp.Description("Optional severity override during triage"),
		),
	)
}

func toolCloseIssue() mcp.Tool {
	return mcp.NewTool("enju_close_issue",
		mcp.WithDescription(`Move an issue into terminal status. Use status="closed" when fixed (optionally pass closed_by_task_id pointing at the fix-task that resolved it); use status="wontfix" when the issue is a duplicate, won't-do, or a misclassification. Refuses on already-terminal issues.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project the issue belongs to"),
		),
		mcp.WithNumber("issue_seq", mcp.Required(),
			mcp.Description("Per-project issue sequence number"),
		),
		mcp.WithString("status",
			mcp.Description(`"closed" (default) or "wontfix"`),
		),
		mcp.WithString("closed_by_task_id",
			mcp.Description("Optional fully-qualified task id whose acceptance resolved this issue"),
		),
	)
}

func toolListIterations() mcp.Tool {
	return mcp.NewTool("enju_list_iterations",
		mcp.WithDescription(`List the iteration history of a task — one row per claim attempt, with per-task seq counter, claimant, claim/submit timestamps, commit SHA, review decision, and outcome (active | completed | invalidated | released | timed_out). Living-workflow phase 5: this is how you reconstruct "what happened with this task" without grepping the event log. For aggregate timelines across the whole run, use enju_show_events instead.`),
		mcp.WithString("task_id", mcp.Required(),
			mcp.Description("Fully-qualified task id (project:run:task_def_id [:instance])"),
		),
	)
}

func toolShowEvents() mcp.Tool {
	return mcp.NewTool("enju_show_events",
		mcp.WithDescription(`Query the project event log and return JSONL (one event per line, newest first). Read-only projection over contribution_events — the canonical event log. Filters compose: leave them empty to get the project-wide stream, narrow with run_id/citizen/event_types/since/limit. Distinct from enju_export_run_events, which writes git-tracked snapshots; this tool is for ad-hoc queries.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to query"),
		),
		mcp.WithNumber("run_id",
			mcp.Description("Optional run sequence number (#1, #2, ...) to scope to a single run"),
		),
		mcp.WithString("citizen",
			mcp.Description("Optional citizen username to filter to events emitted by that citizen"),
		),
		mcp.WithString("event_types",
			mcp.Description(`Comma-separated event types to include (e.g. "task_completed,review_given,run_paused"). Empty = all types.`),
		),
		mcp.WithString("since",
			mcp.Description("Optional RFC3339 timestamp lower bound (e.g. 2026-04-30T00:00:00Z)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum events to return (default 100, max 1000)"),
		),
	)
}

func toolSpawnTask() mcp.Tool {
	return mcp.NewTool("enju_spawn_task",
		mcp.WithDescription(`Spawn a new task into an in-flight run at runtime (living-workflow phase 4a). This is the tasks-spawn-tasks primitive — used to add remediation tasks after a review reject, or fix-tasks after a tester files an issue, or any other "we discovered we need this work" pattern. Subject to the per-run cycle budget (default 200); exhaustion auto-pauses the run. Spawned task starts ready unless depends_on names existing tasks. Emits task_spawned event with parent + trigger attribution.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project containing the run"),
		),
		mcp.WithNumber("run_id", mcp.Required(),
			mcp.Description("Run sequence number within the project"),
		),
		mcp.WithString("task_def_id", mcp.Required(),
			mcp.Description("Unique task id within the run (e.g. 'remediation_1', 'fix_BUG_001'). Becomes the YAML-style identifier; the full id is project:run:task_def_id."),
		),
		mcp.WithString("action", mcp.Required(),
			mcp.Description(`"answer" | "compute" | "contribute" | "review" | "vote"`),
		),
		mcp.WithString("prompt",
			mcp.Description("Optional task prompt (instructions for the claimant). Most spawned tasks set this."),
		),
		mcp.WithString("user_prompt",
			mcp.Description("Optional user-prompt addendum (rare)"),
		),
		mcp.WithString("parent_task_id",
			mcp.Description("Optional parent task id whose output triggered this spawn — recorded as lineage in the task_spawned event"),
		),
		mcp.WithString("trigger",
			mcp.Description(`"human" (default) | "bot" | "template_rule" | "auto_triage"`),
		),
		mcp.WithString("depends_on",
			mcp.Description("Optional comma-separated list of fully-qualified task ids the spawned task waits on. Empty = task starts ready."),
		),
		mcp.WithString("assign_to",
			mcp.Description("Optional comma-separated list of usernames eligible to claim. Empty = open to any project member."),
		),
		mcp.WithString("require_role",
			mcp.Description("Optional citizen role required to claim"),
		),
		mcp.WithString("result_type",
			mcp.Description(`"text" (default) or "json"`),
		),
		mcp.WithNumber("citizens",
			mcp.Description("Number of citizens needed (default 1; >1 = multi-citizen task)"),
		),
	)
}

func toolSetCycleBudget() mcp.Tool {
	return mcp.NewTool("enju_set_cycle_budget",
		mcp.WithDescription(`Bump the per-run cycle budget (max number of spawned tasks). Use to extend room after a runaway has been triaged and the underlying loop fixed; the run remains paused until enju_resume_run is called. Default budget is 200 per run.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project containing the run"),
		),
		mcp.WithNumber("run_id", mcp.Required(),
			mcp.Description("Run sequence number"),
		),
		mcp.WithNumber("max", mcp.Required(),
			mcp.Description("New maximum (must be ≥ current used)"),
		),
	)
}

func toolPauseRun() mcp.Tool {
	return mcp.NewTool("enju_pause_run",
		mcp.WithDescription(`Pause a run. The run's state moves to "paused" and stays there until enju_resume_run is called. Refused on terminal (completed/failed) runs. SpawnTask refuses while paused (a runaway can't keep growing the task graph); claim and submit currently pass through, full claim/submit gating lands later. Use this to inspect a run mid-flight without state transitions racing against you, or as a circuit-breaker when something looks wrong.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project containing the run"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project (#1, #2, ...)"),
		),
	)
}

func toolResumeRun() mcp.Tool {
	return mcp.NewTool("enju_resume_run",
		mcp.WithDescription(`Resume a paused run. The run's state is re-evaluated based on current task counts: lands on "active" if there's ready or in-flight work, "idle" if only pending tasks remain, "completed" if everything is terminal. No-op on already-alive runs; refused on terminal runs.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project containing the run"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project"),
		),
	)
}

func toolSetProjectDefaultBranch() mcp.Tool {
	return mcp.NewTool("enju_set_project_default_branch",
		mcp.WithDescription(`Change a project's default branch. Owner-only. The default is where new runs land when enju_create_run is called without an explicit branch=. Use this to move a project's Enju activity off "main" onto e.g. "enju/work" so repo main stays human-curated. Existing runs are unaffected — they stay on the branch they were created with.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose default branch to change"),
		),
		mcp.WithString("branch",
			mcp.Required(),
			mcp.Description(`Git branch name. Standard git ref rules apply (no spaces, no leading "-" or "/", no "..", etc.).`),
		),
	)
}

func toolSetProjectRemote() mcp.Tool {
	return mcp.NewTool("enju_set_project_remote",
		mcp.WithDescription("Set the external git remote URL for a project, or migrate from one remote to another. Subsequent task result commits push to this remote. Pass the new URL directly to migrate; use enju_leave_project to stop using the project on this machine. Empty strings are rejected — clearing a remote on a multi-machine project would silently fork the team."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose remote to update"),
		),
		mcp.WithString("remote_url",
			mcp.Required(),
			mcp.Description("Git remote URL (must be non-empty)."),
		),
	)
}

func toolProjectRemoteStatus() mcp.Tool {
	return mcp.NewTool("enju_project_remote_status",
		mcp.WithDescription("Show live git remote status for a project: local HEAD vs remote HEAD (via ls-remote), last push timestamp, and last push error if any. Use this when enju_list_projects shows a remote warning."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to inspect"),
		),
	)
}

func toolProjectSync() mcp.Tool {
	return mcp.NewTool("enju_project_sync",
		mcp.WithDescription("Push a project's local HEAD to its configured remote without requiring a new commit. Safe by default: a fast-forward push succeeds, a diverged remote is REFUSED unless force=true. Use this to sweep stuck commits (e.g. after a push failure or an earlier invalidation that didn't push). Set force=true ONLY when you intentionally want to overwrite the remote — force-push is destructive and can discard remote-side contributions."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to push"),
		),
		mcp.WithBoolean("force",
			mcp.Description("If true, do a force-push that overwrites the remote branch even when histories have diverged. Default false — diverged remotes are refused with guidance to reconcile manually."),
		),
	)
}

func toolLeaveProject() mcp.Tool {
	return mcp.NewTool("enju_leave_project",
		mcp.WithDescription(`Leave a project. Two things happen: (1) your membership row is removed from the project, and (2) your local clone is wiped to reclaim disk space. The remote repo is untouched — other members keep their access.

Refused when you are the sole remaining owner — promote another member to owner first (enju_promote_member), then leave. If you only want to wipe the local clone but keep your membership, use keep_membership=true.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to leave"),
		),
		mcp.WithBoolean("keep_membership",
			mcp.Description("If true, only wipe the local clone without removing your project membership. Default false — leaving normally removes both."),
		),
	)
}

func toolAddProjectMember() mcp.Tool {
	return mcp.NewTool("enju_add_project_member",
		mcp.WithDescription(`Grant a registered citizen membership of a project. Any existing member can add others — this is the trust-based delegation that lets projects spread without owner-gated invitations.

New members default to 'member' role. Only project owners may add someone as 'owner' directly (members wanting to invite another owner must ask an existing owner, or promote after the normal add).`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to add the member to"),
		),
		mcp.WithString("username",
			mcp.Required(),
			mcp.Description("Username of the citizen to add"),
		),
		mcp.WithString("role",
			mcp.Description("Role to assign. Defaults to 'member'. Pass 'owner' to add as an owner (owners only)."),
		),
	)
}

func toolRemoveProjectMember() mcp.Tool {
	return mcp.NewTool("enju_remove_project_member",
		mcp.WithDescription(`Remove a member from a project. Owner-only — use enju_leave_project to remove yourself. Refused when the target is the last owner (promote a successor first).`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to remove the member from"),
		),
		mcp.WithString("username",
			mcp.Required(),
			mcp.Description("Username of the citizen to remove"),
		),
	)
}

func toolListProjectMembers() mcp.Tool {
	return mcp.NewTool("enju_list_project_members",
		mcp.WithDescription("List every member on a project, with role and when they were added. Members only. Paste the output verbatim in your reply — it's pre-formatted."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose members to list"),
		),
	)
}

func toolPromoteMember() mcp.Tool {
	return mcp.NewTool("enju_promote_member",
		mcp.WithDescription("Promote a member to owner. Owner-only — any owner can promote any member. Projects support multiple owners; this is the main way to grow the owner pool and avoid a single-point-of-failure."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project"),
		),
		mcp.WithString("username",
			mcp.Required(),
			mcp.Description("Username of the member to promote"),
		),
	)
}

func toolDemoteOwner() mcp.Tool {
	return mcp.NewTool("enju_demote_owner",
		mcp.WithDescription("Demote an owner to regular member. Owner-only. Refused when the target is the last owner (promote a successor first to preserve the ≥1 owner invariant)."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project"),
		),
		mcp.WithString("username",
			mcp.Required(),
			mcp.Description("Username of the owner to demote"),
		),
	)
}

func toolUpdateProfile() mcp.Tool {
	return mcp.NewTool("enju_update_profile",
		mcp.WithDescription("Update your citizen profile. Merge semantics: any field you omit from the call is left untouched on both the server and in your local credentials file. Pass only what you want to change. At least one of name or email must be provided."),
		mcp.WithString("name",
			mcp.Description("Your display name. Omit this field to leave the current name unchanged; passing an empty string is rejected."),
		),
		mcp.WithString("email",
			mcp.Description("Your email (must be unique). Omit this field to leave the current email unchanged; pass an empty string to explicitly clear it."),
		),
	)
}

func toolMyDashboard() mcp.Tool {
	return mcp.NewTool("enju_my_dashboard",
		mcp.WithDescription("Show your citizen dashboard: stats, active tasks, and recent completions. Paste the output verbatim in your reply — it's pre-formatted for the human."),
	)
}

func toolMyProfile() mcp.Tool {
	return mcp.NewTool("enju_my_profile",
		mcp.WithDescription("Show your own citizen profile. Paste the output verbatim in your reply — it's pre-formatted."),
	)
}

func toolInvalidateTask() mcp.Tool {
	return mcp.NewTool("enju_invalidate_task",
		mcp.WithDescription(`Mark an accepted task as invalid (its result was wrong). Target → READY for re-claim; intra-run descendants → PENDING until the target re-completes. Artifact index rolls back to the prior writer. Git history preserves the previous result.

When the target is a dynamic for_each source, materialized descendants are PARKED (not deleted) — on the next matching re-accept, matched instance keys restore their prior state (accepted work preserved), stale keys are deleted, new keys are materialized fresh.

Only tasks in the 'accepted' state can be invalidated.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The fully-qualified ID of the task to invalidate"),
		),
		mcp.WithString("reason",
			mcp.Description("Short explanation for the invalidation — shown in logs and the response"),
		),
	)
}

func toolListUntrackedArtifacts() mcp.Tool {
	return mcp.NewTool("enju_list_untracked_artifacts",
		mcp.WithDescription(`List artifacts produced by this project that are NOT tracked in git (declared with track:false in writes_artifacts). For each entry, reports whether the file is visible in this citizen's workspace so you can spot missing untracked dependencies before claiming a downstream task.

Typical causes of "missing" locally:
- The producer task was run by another citizen and this citizen never re-ran it.
- $ENJU_SHARED_ROOT is configured but not pointing at the producer's mount, or the mount isn't up.
- The producer's workspace was cleaned / ephemeral.

Use this to debug a "cannot claim — task reads untracked artifact(s) that aren't in your workspace" error from enju_claim_task.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("Project to list untracked artifacts for"),
		),
		mcp.WithString("branch",
			mcp.Description("Branch to list (defaults to project's default_branch)"),
		),
	)
}

func toolTallyTask() mcp.Tool {
	return mcp.NewTool("enju_tally_task",
		mcp.WithDescription(`Force a tally evaluation on a multi-citizen vote or review task that is currently in the 'collecting' state. Runs the same tally logic as a submission would: counts the per-citizen submissions, applies the task's threshold + quorum + deadline rules, and resolves the task to 'accepted' if a winner emerges. Reports the current tally state either way.

Use this when:
- A vote/review task is stuck in collecting and you want to check whether it has enough votes to resolve under its threshold rule
- The deadline has passed and you want to force the past-deadline resolution (the server's lazy check fires on the next task read, but this tool makes the trigger explicit)
- You're the run author and want to check "is this ready to go?" without waiting for another submission

The tally response includes the current counts, whether the task resolved, and if it resolved the winning option (vote) or verdict (review) + any newly-unlocked downstream tasks.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The fully-qualified ID of the vote or review task to tally"),
		),
	)
}


// --- operator/model design: bot + model registration ---

func toolRegisterBot() mcp.Tool {
	return mcp.NewTool("enju_register_bot",
		mcp.WithDescription(`Register a bot citizen owned by you. Returns the bot's username AND its initial token — STASH THE TOKEN NOW, it cannot be retrieved later. Drop it into the bot's launcher (CI env var, daemon config, ~/.enju/bot-credentials.json) so the bot can authenticate as itself.

Use cases: autonomous overnight agents, CI runners, role-bots (developer-bot / reviewer-bot / tester-bot for the living-workflow pattern). The bot acts under its own identity in audit logs but ownership chains back to you for accountability. Multiple bots per parent are allowed — clone freely for parallel workloads or A/B testing. See docs/operator-model-design.md.`),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Display name (e.g. \"Tamer's Reviewer Bot\")"),
		),
		mcp.WithString("username",
			mcp.Description("Optional explicit username (slug-form, GitHub-username regex). Auto-slugified from name if omitted."),
		),
		mcp.WithString("role",
			mcp.Description("Optional role tag (defaults to 'citizen'). Future work may surface role-based routing."),
		),
		mcp.WithString("label",
			mcp.Description("Optional label for the initial token (e.g. \"laptop\", \"ci-server\") — useful when you'll rotate tokens later and need to tell deployments apart."),
		),
	)
}

func toolListMyBots() mcp.Tool {
	return mcp.NewTool("enju_list_my_bots",
		mcp.WithDescription("List every bot you own, with each bot's active and revoked tokens (token VALUES are not returned — only labels and timestamps; token strings are shown once at registration). Paste the output verbatim in your reply — it's pre-formatted."),
	)
}

func toolRevokeToken() mcp.Tool {
	return mcp.NewTool("enju_revoke_token",
		mcp.WithDescription("Revoke a token. The token is preserved for audit (revoked_at timestamp set, row never deleted) but stops authenticating immediately. Self-service: callable by the token's owner directly — humans rotating their own session, or the parent of a bot whose token leaked. Pass either token (the raw string) OR token_id (from enju_list_my_bots)."),
		mcp.WithString("token",
			mcp.Description("Raw token string. Use this when the token leaked into logs / a CI env / your shell history."),
		),
		mcp.WithNumber("token_id",
			mcp.Description("Token row id from enju_list_my_bots. Use this when revoking a labeled deployment (e.g. \"the ci-server token\")."),
		),
	)
}

func toolListModels() mcp.Tool {
	return mcp.NewTool("enju_list_models",
		mcp.WithDescription("Browse the model catalog. Returns every kind='model' citizen — the seeded popular models (Claude Opus / Sonnet / Haiku, GPT-4o, Gemini, Llama, etc.) plus any custom models registered locally. Use before submitting if you're unsure what -model name to pass."),
	)
}

func toolRegisterModel() mcp.Tool {
	return mcp.NewTool("enju_register_model",
		mcp.WithDescription(`Register a custom model in the catalog so submits can attribute work to it. Local-mode use case: you're running Ollama / llama.cpp / a lab-internal finetune that the seed catalog doesn't cover. Any authenticated citizen can register in local mode; hosted-mode policy gating is deferred.

Note: unknown model names passed to -model auto-register on first use, so explicit registration is mostly for picking nice display names.`),
		mcp.WithString("username",
			mcp.Required(),
			mcp.Description("Slug-form identifier (e.g. \"ollama-llama-3-1-70b\"). Must match the GitHub-username regex (lowercase alphanumerics + hyphen)."),
		),
		mcp.WithString("display_name",
			mcp.Description("Human-readable name (e.g. \"Llama 3.1 70B (local)\"). Defaults to the username."),
		),
	)
}
