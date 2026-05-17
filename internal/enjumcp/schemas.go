package enjumcp

// MCP tool-schema constructors. Each toolX() returns the
// Tool describing a single enju_X tool: its name,
// description, and argument schema. Handlers live in their
// feature-specific files (claim.go, submit.go, project.go,
// etc.) — keeping the schemas here as a flat catalogue makes
// it easy to audit the tool surface in one read.

import "github.com/mark3labs/mcp-go/mcp"

// --- Tool Definitions ---

func ListRuns() mcp.Tool {
	return mcp.NewTool("enju_list_runs",
		mcp.WithDescription("List runs. Optionally filter by project."),
		mcp.WithNumber("project_id",
			mcp.Description("Filter by project ID (integer, optional)"),
		),
	)
}

func ListReadyTasks() mcp.Tool {
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

func ClaimTask() mcp.Tool {
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

func ClaimReadyMatching() mcp.Tool {
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

func GetTaskInputs() mcp.Tool {
	return mcp.NewTool("enju_get_task_inputs",
		mcp.WithDescription("Get the upstream dependency results for a task. Use this to see what previous tasks produced."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func SubmitResult() mcp.Tool {
	return mcp.NewTool("enju_submit_result",
		mcp.WithDescription(`Submit a result for a claimed task. The task must be claimed by you first.

For simple tasks: provide 'content' as a string.
For tasks with named outputs: provide 'outputs_json' as a JSON object mapping output names to their values.
For tasks with writes: provide 'artifacts_json' mapping each declared artifact path to its new content. You may write any subset of declared paths (permissive — declared is an upper bound).
For action:review tasks: provide 'decision' — one of:
  - "approve"          — target → ACCEPTED, downstream unblocks
  - "request_changes"  — retry cascade: target → READY, artifact rolls back, descendants → PENDING (author revises + resubmits)
  - "reject"           — fail cascade: target → FAILED (terminal), artifact rolls back, descendants → SKIPPED
  - "comment"          — non-blocking; target state unchanged
Your prose content is the reviewer's feedback in all cases.
For action:vote tasks: provide 'option' as one of the declared option ids from the task's 'options:' list. Your prose content is free-form commentary. If the winning option has 'activates:' set, the DAG routes down that branch and tasks on losing branches flip to SKIPPED. Votes without 'activates:' are pure decisions — downstream tasks can still read the choice via {{task.winning_option}}.
The task detail shows the schema (outputs, writes, reviews target, options) so you know what's expected.
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
			mcp.Description(`For tasks with writes: a JSON string mapping each artifact path to its new content. Example: '{"src/analyze.py": "def analyze():\n    pass\n"}'. Paths must be in the task's writes list.`),
		),
		mcp.WithString("decision",
			mcp.Description(`Required for action:review tasks: "approve", "request_changes", "reject", or "comment". approve = ship it; request_changes = send back for revision; reject = hard stop (FAILED); comment = non-blocking note.`),
		),
		mcp.WithString("option",
			mcp.Description(`Required for action:vote tasks: one of the declared option ids from the task's 'options:' YAML list (as shown in the claim response's Options block). Ignored on non-vote tasks.`),
		),
		mcp.WithString("model",
			mcp.Description(`OPTIONAL attribution: which LLM produced this result. Supply it if you want the work credited to a model; leave it empty if no LLM produced it (a script / compute step, an unaided hand-decision) — empty is stored as "no model", never an error. It is never required: being an agent does not imply an LLM ran (an agent may run a script). A model is a free-form label, not a registered entity — any string is stored verbatim; there is no model registration and no catalog requirement (an unknown name just renders raw). Use it to switch models mid-session (e.g. opened with claude-opus-4-7 but produced this result with claude-sonnet-4-6) so the credit matches reality; empty falls back to the session -model default.`),
		),
	)
}

func SubmitResultsBatch() mcp.Tool {
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

func ListArtifacts() mcp.Tool {
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

func GetArtifact() mcp.Tool {
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

func GetArtifactHistory() mcp.Tool {
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

func ReleaseTask() mcp.Tool {
	return mcp.NewTool("enju_release_task",
		mcp.WithDescription("Release a claimed task back to the pool if you can't complete it. No penalty for voluntary release."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to release"),
		),
	)
}

func GetTask() mcp.Tool {
	return mcp.NewTool("enju_get_task",
		mcp.WithDescription("Get details of a specific task including its state, prompt, and dependencies. Paste the output verbatim in your reply — it's pre-formatted."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task"),
		),
	)
}

func RunStatus() mcp.Tool {
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

func CreateRun() mcp.Tool {
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

List-valued params support a {{param[*]}} expansion in writes / reads / assign_to / depends_on — one declared element expands to N entries, one per value in the list<string> param. Useful for one-shot tasks that emit or read N files without enumerating every path.

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
		mcp.WithBoolean("auto_agents",
			mcp.Description(`Opt-in: spin up every agent declared in the workflow's inline agents: section before the run starts, and stop them automatically when the run reaches a terminal state. Reference-counted so concurrent runs that share agents are safe — the last-finishing run triggers the stop. Agents started manually with enju_agent_start are left alone (manual wins). Requires path= mode; inline yaml= has no on-disk workflow file for the agent daemons to read. Default false: the operator drives agent lifecycle explicitly with enju_agent_start / enju_agent_stop_all.`),
		),
		mcp.WithString("sync_mode_override",
			mcp.Description(`Override the workflow YAML's sync: block for this run. Controls what happens to the run branch when the run completes. "merge": merge the run branch into base_branch locally (default if not set in YAML). "push": merge locally then push base_branch to origin. "none": skip both — useful for dry runs or workflows where you manage branching yourself. Omit to use the workflow's own sync: setting.`),
		),
	)
}

// toolListTemplates is the LLM's template-discovery entry
// point. Returns every YAML file under the project clone's
// enju/templates/ directory with its name, description, and
// parameter summary so the LLM can pick a recipe that fits
// the user's request without reading each file.
func FailTask() mcp.Tool {
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

func ExecuteRun() mcp.Tool {
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

func ExecuteTask() mcp.Tool {
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

Declared writes paths are picked up from disk post-exit-0 and registered in the artifact index.

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
// data lives in the coordinator's events +
// task_claims; this tool just snapshots it into git so the
// run's directory becomes self-documenting for audits /
// postmortems / preprint figures.
func ExportRunEvents() mcp.Tool {
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
func ExportDiagram() mcp.Tool {
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

func ExportRun() mcp.Tool {
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

func ListTemplates() mcp.Tool {
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
func DescribeTemplate() mcp.Tool {
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

func ListProjects() mcp.Tool {
	return mcp.NewTool("enju_list_projects",
		mcp.WithDescription("List all long-lived projects. Paste the output verbatim in your reply — it's pre-formatted."),
	)
}

func CreateProject() mcp.Tool {
	return mcp.NewTool("enju_create_project",
		mcp.WithDescription(`Create or adopt an Enju project at an absolute path you provide. The folder IS the project's working tree — Enju writes its scaffold (enju/templates/) into it and runs git init if needed. Smart-detects what's at the path and dispatches:

  - Empty or non-existent folder: git init + seed + managed bare remote.
  - Populated folder, no .git: git init + commit existing files + managed bare. Refuses populated git repos with no Enju marker (the LLM-typoed-path footgun); pass force=true to override.
  - Folder with .git, no origin: leaves the existing repo alone, wires a managed bare so commits have somewhere to land.
  - Folder with .git + origin: registers as-is. Your remote stays the push target.

Path is required and must be explicit: pass the absolute path to the folder. There is no cwd default — the calling LLM may be running inside a different project's directory than the one being adopted (very common when running Claude in /repo/A while creating an Enju project for /repo/B), and silently adopting cwd would be a footgun. If the user says "this folder" or "the current directory," confirm what cwd is and pass it explicitly.

To migrate from a managed bare to a real GitHub/GitLab remote later, use enju_set_project_remote.`),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Unique project name"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description(`Absolute path for the project's working tree. Folder may be empty, populated, or already a git repo — see tool description for what each state triggers. The folder will hold all of Enju's per-project state under enju/ (templates, runs, events, agent state). Mutually exclusive with remote_url.`),
		),
		mcp.WithString("description",
			mcp.Description("Optional project description"),
		),
		mcp.WithString("remote_url",
			mcp.Description("Optional external git remote URL (e.g., git@github.com:org/repo.git) to clone into the default workspace location. Mutually exclusive with path. Auth follows the host's SSH/credential configuration."),
		),
		mcp.WithString("default_branch",
			mcp.Description(`Optional git branch new runs land on by default. When adopting an existing repo, auto-detected from HEAD; otherwise defaults to "main". Set explicitly to e.g. "enju/work" if you want Enju activity off your main branch.`),
		),
		mcp.WithBoolean("force",
			mcp.Description("Override the populated-git-repo safety gate. Default false. Set true only when the user has explicitly confirmed they want to add Enju orchestration on top of an existing populated git repository with no Enju marker."),
		),
	)
}

func FileIssue() mcp.Tool {
	return mcp.NewTool("enju_file_issue",
		mcp.WithDescription(`File a project-level issue (living-workflow phase 3). Issues outlive runs — file in run #2, fix in run #7 is normal. Tester agents use this to record structured findings (one issue per failure mode); humans use it for ad-hoc bug reports. Filing ≠ fixing — triage and fix-task linkage are separate steps. Emits an issue_filed event.`),
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

func ListIssues() mcp.Tool {
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

func GetIssue() mcp.Tool {
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

func TriageIssue() mcp.Tool {
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

func CloseIssue() mcp.Tool {
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

func ListIterations() mcp.Tool {
	return mcp.NewTool("enju_list_iterations",
		mcp.WithDescription(`List the iteration history of a task — one row per claim attempt, with per-task seq counter, claimant, claim/submit timestamps, commit SHA, review decision, and outcome (active | completed | invalidated | released | timed_out). Living-workflow phase 5: this is how you reconstruct "what happened with this task" without grepping the event log. For aggregate timelines across the whole run, use enju_show_events instead.`),
		mcp.WithString("task_id", mcp.Required(),
			mcp.Description("Fully-qualified task id (project:run:task_def_id [:instance])"),
		),
	)
}

func ShowEvents() mcp.Tool {
	return mcp.NewTool("enju_show_events",
		mcp.WithDescription(`Query the project event log and return JSONL (one event per line, newest first). Read-only projection over the event store. Filters compose: leave them empty to get the project-wide stream, narrow with run_id/citizen/event_types/since/limit. Distinct from enju_export_run_events (git-tracked snapshots) and from enju_recent_events (in-conversation "what's new?" surfacing) — use this tool for filter-driven historical queries; use enju_recent_events when the assistant just wants a concise heads-up of the latest activity. If results come back empty unexpectedly, run enju_events_status to check whether the event-store kill-switch was flipped (disabled stores serve empty results without an error).`),
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

// toolRequestClarification is the agent-asks-human idiom — the
// natural complement to notifications. Encapsulates the spawn
// pattern that agents use mid-task when they hit ambiguity:
// "should X mean A or B?" Without this tool, every agent author
// has to learn the spawn_task shape (action=answer, citizens=1,
// assign_to=<human>, etc.). With it, the idiom is one line.
//
// Mechanics: a thin wrapper over enju_spawn_task with sensible
// defaults (action=answer, citizens=1, trigger=agent). The
// resulting task is assigned to the named human; when they
// answer, a task_completed event fires. The agent can either
// poll, subscribe via the notification subsystem (Phase 4), or
// rely on parent_task_id linkage to discover the resolution.
//
// In v1 the tool does NOT auto-pause the calling agent's task —
// that requires post-creation dependency mutation which Enju's
// engine doesn't support today. Pattern for the agent's prompt:
// "If you need clarification, call enju_request_clarification
// then submit your current task with a 'pending clarification'
// marker; reviewer will request_changes once the answer lands."
func RequestClarification() mcp.Tool {
	return mcp.NewTool("enju_request_clarification",
		mcp.WithDescription(`Agent-asks-human pattern: spawn a clarification task assigned to a named human, encapsulating the spawn-task idiom in one tool call. Use mid-task when you hit ambiguity in the spec, the upstream content, or the user's intent. The clarification task is action=answer, single-citizen, assigned to the human you name. Their answer becomes a normal task result the audit log records. Returns the new task ID. Note: in v1 this does NOT auto-pause your current task — submit your current work with a 'awaiting clarification' marker, or wait for the human's task_completed event before resuming. Future notification-agent work will auto-resume on answer.`),
		mcp.WithNumber("project_id", mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id", mcp.Required(),
			mcp.Description("Run sequence number within the project"),
		),
		mcp.WithString("task_def_id", mcp.Required(),
			mcp.Description("Descriptive id for the clarification task — e.g. 'clarify_review_format', 'clarify_input_units'. Helps the human + audit log identify which question they're answering."),
		),
		mcp.WithString("prompt", mcp.Required(),
			mcp.Description("The question for the human, in plain language"),
		),
		mcp.WithString("assign_to", mcp.Required(),
			mcp.Description("Citizen username to ask (e.g. 'tamer'). The clarification task is single-citizen, assigned to exactly this person."),
		),
		mcp.WithString("parent_task_id",
			mcp.Description("Optional: your current task's full id (project:run:task_def [:instance]). Recorded for audit so 'which task asked this question?' is queryable."),
		),
	)
}

// toolRecentEvents is the assistant-friendly polling tool for
// in-session "what's new" surfacing. Differs from show_events:
//
//   - Tighter description framing ("call at natural pause
//     points" vs show_events' "ad-hoc filter queries"). The
//     LLM picks the right tool for the right intent.
//   - Smaller default limit (20 vs 100) — recent context, not
//     full history.
//   - Concise output formatting suited for inline conversation
//     ("Run #5 completed at 14:32; @agent submitted task X") vs
//     show_events' raw JSONL.
//
// Both tools call the same underlying endpoint; the difference
// is intent + presentation.
func RecentEvents() mcp.Tool {
	return mcp.NewTool("enju_recent_events",
		mcp.WithDescription(`Surface what's recently happened in a project — designed for the assistant to call at natural pause points (after a long bash returns, when the user asks "what's new?", before answering a follow-up). Returns a concise human-readable summary of the latest events.

To answer "any updates for me?" pass for_me=true. To make a delta query (only items since a previous check), remember the highest seq from your last call and pass it as since_seq next time — this replaces the implicit read/unread cursor the deleted enju_notifications tool used to manage. For full filter queries (by run, by citizen, by event type) use enju_show_events. For git-tracked snapshots use enju_export_run_events.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to check"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Max events to surface (default 20, max 100). Smaller is better for in-conversation use. With for_me=true the limit applies pre-filter, so the result may be smaller than limit."),
		),
		mcp.WithString("since",
			mcp.Description("Optional RFC3339 timestamp lower bound — only surface events after this. Useful when the assistant has a 'last checked at' anchor to avoid re-surfacing the same items."),
		),
		mcp.WithNumber("since_seq",
			mcp.Description("Optional monotone-seq lower bound (strict >). Preferred over `since` for delta queries — pass the highest seq from your last response to get only newer events."),
		),
		mcp.WithBoolean("for_me",
			mcp.Description(`Default false. When true, return only events the calling citizen is named in: event.citizen == me OR event.assign_to == me. Limitations: events on tasks you submitted but didn't claim (branch_merged after approval, task_completed where the closer is the reviewer), issue_filed events you authored without explicit assignment, and project-wide events without a citizen (run_completed) are NOT surfaced. The honest "events about entities I authored" join is a future refinement.`),
		),
	)
}

// toolReview is the action-verb counterpart to enju_inbox: a
// constrained submit for action:review tasks. Same coordinator
// contract as enju_submit_result with a review-specific decision,
// but with a narrower input schema so the assistant doesn't have
// to think about outputs/artifacts/options it never uses on a
// review.
func Review() mcp.Tool {
	return mcp.NewTool("enju_review",
		mcp.WithDescription(`Submit a verdict on an action:review task you've claimed. Companion to enju_inbox: read what's waiting, then decide. The four decisions match enju_submit_result's verbatim — semantics are identical, this is just a narrower wrapper. Calls the same underlying submit endpoint.

  - "approve"          — target → ACCEPTED, downstream unblocks
  - "request_changes"  — retry cascade: target → READY, descendants → PENDING (author revises + resubmits)
  - "reject"           — fail cascade: target → FAILED (terminal), descendants → SKIPPED
  - "comment"          — non-blocking; target state unchanged

Always pair the decision with prose 'content' explaining your reasoning — the author/team reads it. For non-review tasks (answer/vote/compute) use enju_submit_result instead.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The review task you've claimed. Must be action:review."),
		),
		mcp.WithString("decision",
			mcp.Required(),
			mcp.Description(`One of "approve", "request_changes", "reject", "comment". approve = ship it; request_changes = send back for revision; reject = hard stop; comment = non-blocking note.`),
		),
		mcp.WithString("content",
			mcp.Description("Your review prose. Required in practice — readers need to know why you decided this. Empty content is technically accepted by the coordinator but produces a useless review row."),
		),
		mcp.WithString("model",
			mcp.Description(`Optional per-call model override. Same semantics as enju_submit_result's model field; defaults to the session's -model flag.`),
		),
	)
}

// toolInbox surfaces the caller's inbox for one project — every
// ready task assigned to the calling citizen, with each parent's
// latest submission inlined so the assistant/user can read the
// work without claiming the reviewer task first.
func Inbox() mcp.Tool {
	return mcp.NewTool("enju_inbox",
		mcp.WithDescription(`List tasks waiting on the calling citizen in a project — review/vote/answer tasks where assign_to includes you and state is ready. Each item carries the task's prompt plus the latest submission(s) of the upstream task(s) it depends on, so you can read the work in-place. Designed as the "what's on my plate?" action queue, complementing enju_recent_events?for_me=true (which is descriptive — what happened recently — rather than actionable) and enju_my_dashboard (multi-project summary). Cap on inlined prompt is ~2KB; follow up with enju_get_task for the full text. Known v1 limitation: compute and vote parents leave content empty (their work lives in git artifacts or the option column, respectively); the upstream's task_id + commit_sha are still surfaced.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to surface inbox for."),
		),
	)
}

// toolEventsStatus surfaces the EventStore's runtime state.
// operators reach for this when diagnosing "is
// the audit log healthy?" Returns enabled/disabled state +
// monotone counters (enqueued, persisted, dropped, queue
// depth). The dropped counter is the load-bearing signal:
// non-zero means the writer can't keep up and gaps are
// accumulating.
func EventsStatus() mcp.Tool {
	return mcp.NewTool("enju_events_status",
		mcp.WithDescription(`Report the EventStore's runtime state — enabled flag + monotone counters (enqueued, persisted, dropped, queue depth). Operators read this when triaging audit-log health. Non-zero dropped means the writer can't keep up and gaps are accumulating in the per-project sequence.`),
	)
}

func SpawnTask() mcp.Tool {
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
			mcp.Description(`"human" (default) | "agent" | "template_rule" | "auto_triage"`),
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

func SetCycleBudget() mcp.Tool {
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

func PauseRun() mcp.Tool {
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

func ResumeRun() mcp.Tool {
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

func TerminateRun() mcp.Tool {
	return mcp.NewTool("enju_terminate_run",
		mcp.WithDescription(`Irreversibly abandon a run. Use when a run is structurally stuck (agent looping on request_changes, requirements changed mid-run, design flaw discovered) and continuing isn't the answer — pause is reversible, terminate is not.

Effect: run state moves to "terminated"; every non-terminal task in the run is cascade-marked "skipped" with skip_reason="run_terminated"; every open claim closes with outcome="abandoned". Topic branches stay in git (immutable audit). Late-arriving submits — compute that was running when terminate fired — are refused at the coordinator with a clear error; the work is honestly lost.

Distinct from pause/resume (reversible) and from a fail cascade (system-said-no semantics). The audit signal is "operator aborted" not "validation failed" — different stories for dashboards.

Limitations to be honest about:
  - In-flight compute on this or other machines may run for up to one notify-poll-interval (~30s) after terminate before its fat-client receives the event and kills it. Bounded waste, no correctness loss — the late submit is refused.
  - No cross-machine kill primitive: the coordinator can't reach into other citizens' machines. Each fat-client cleans up its own local processes when it sees the run_terminated event. If a citizen's MCP isn't running, their compute keeps going until natural exit, then submit is refused.

Refused on already-terminal runs (completed / failed / terminated). Pause→terminate IS valid; a paused run rolls forward into terminated cleanly.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project containing the run"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project"),
		),
		mcp.WithString("reason",
			mcp.Description(`Optional free-text explanation, capped at 500 chars (longer strings are silently truncated). Lands verbatim in the run_terminated event metadata for audit. Examples: "agent stuck in request_changes loop on review:gate", "requirements changed after kickoff", "design flaw — restarting with the new template".`),
		),
	)
}

func SetProjectDefaultBranch() mcp.Tool {
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

func SetProjectRemote() mcp.Tool {
	return mcp.NewTool("enju_set_project_remote",
		mcp.WithDescription("Set the external git remote URL for a project. Use this to graduate a path-mode project (currently using its local managed bare under <project>/enju/.bare.git/) to a real GitHub/GitLab/Gitea/self-hosted remote, or to migrate from one external remote to another. The fat-client pushes every local branch to the new remote and resets scan cursors so the artifact index catches up to the migrated history. Pass the new URL directly to migrate; use enju_leave_project to stop using the project on this machine. Empty strings are rejected — clearing a remote on a multi-machine project would silently fork the team."),
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

func ProjectRemoteStatus() mcp.Tool {
	return mcp.NewTool("enju_project_remote_status",
		mcp.WithDescription("Show live git remote status for a project: local HEAD vs remote HEAD (via ls-remote), last push timestamp, and last push error if any. Use this when enju_list_projects shows a remote warning."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project to inspect"),
		),
	)
}

func ProjectSync() mcp.Tool {
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

func LeaveProject() mcp.Tool {
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

func AddProjectMember() mcp.Tool {
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

func RemoveProjectMember() mcp.Tool {
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

func ListProjectMembers() mcp.Tool {
	return mcp.NewTool("enju_list_project_members",
		mcp.WithDescription("List every member on a project, with role and when they were added. Members only. Paste the output verbatim in your reply — it's pre-formatted."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose members to list"),
		),
	)
}

func PromoteMember() mcp.Tool {
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

func DemoteOwner() mcp.Tool {
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

func UpdateProfile() mcp.Tool {
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

func MyDashboard() mcp.Tool {
	return mcp.NewTool("enju_my_dashboard",
		mcp.WithDescription("Show your citizen dashboard: stats, active tasks, and recent completions. Paste the output verbatim in your reply — it's pre-formatted for the human."),
	)
}

func MyProfile() mcp.Tool {
	return mcp.NewTool("enju_my_profile",
		mcp.WithDescription(`Show your own citizen profile. Paste the output verbatim in your reply — it's pre-formatted.

Identity model (so you don't get confused): a human's global identity is their EMAIL — mandatory and unique. Your username is just a tenant-scoped handle, not a global id; two different owners may each have a "dev-bot". Registration is idempotent by email: if your citizen was wiped, re-registering with the same email returns the SAME citizen with a fresh token — it never duplicates you and never errors with "already exists". A model (claude-opus-4-7, etc.) is NOT a citizen — it has no profile; it's just a label stamped on the work.`),
	)
}

func InvalidateTask() mcp.Tool {
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

func RetryTask() mcp.Tool {
	return mcp.NewTool("enju_retry_task",
		mcp.WithDescription(`Re-run a single failed compute task — without re-running the whole run.

For tasks in state 'failed_retryable' (a compute script that errored on its own merits — exit non-zero). The task is sent back to READY and its script is executed again in one call. The failed attempt is preserved as its own closed iteration; this retry runs as a fresh iteration (iter_seq advances), so the history shows every attempt.

from:
  "head" (default) — re-materialize the run snapshot from the RUN BRANCH's current tip first. Use this after you've committed a fix to the failing script. Commit the fix to the run branch (NOT the default branch — the run executes against its own branch; a commit to main is invisible to it). The refresh is overwrite-in-place, not a clean checkout: a modified script is picked up, but a file deleted/renamed on the branch since run creation lingers — a delete/rename fix needs a fresh run, not a retry.
  "snapshot"        — re-run the exact pinned snapshot script unchanged. Use this for a transient failure (flaky network, a busy box) where the code was never the problem.

Only 'failed_retryable' tasks can be retried. A terminal 'failed' task is dead (its descendants already cascaded to SKIPPED) — retry does not apply.`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The fully-qualified ID of the failed_retryable task to retry"),
		),
		mcp.WithString("from",
			mcp.Description("Which script version to run: \"head\" (default — re-materialize from the RUN BRANCH tip; commit your fix to the run branch, not main, and note the refresh is overwrite-in-place so a delete/rename needs a fresh run) or \"snapshot\" (re-run the pinned snapshot unchanged, for a transient failure)."),
			mcp.Enum("head", "snapshot"),
		),
	)
}

func ListUntrackedArtifacts() mcp.Tool {
	return mcp.NewTool("enju_list_untracked_artifacts",
		mcp.WithDescription(`List artifacts produced by this project that are NOT tracked in git (declared with track:false in writes). For each entry, reports whether the file is visible in this citizen's workspace so you can spot missing untracked dependencies before claiming a downstream task.

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

func TallyTask() mcp.Tool {
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

// --- operator/model design: agent + model registration ---

func RegisterBot() mcp.Tool {
	return mcp.NewTool("enju_register_agent",
		mcp.WithDescription(`Register an agent citizen owned by you (the authenticated caller). Returns the agent's username AND its initial token — STASH THE TOKEN NOW, it cannot be retrieved later. Drop it into the agent's launcher (CI env var, daemon config, ~/.enju/agent-credentials.json) so the agent can authenticate as itself.

Ownership & identity (so you don't get confused): you MUST be authenticated for this to work — the agent is owned by you and the call fails closed (hard error, nothing written) if the owner can't be resolved; an ownerless agent cannot exist. The agent's username is a tenant-scoped HANDLE, not a global identity: it only needs to be unique among YOUR agents, and a different owner may have an agent with the same name. An agent has no email (no real-world identity); a human's email is the global identity, an agent's owner chain is. The agent acts under its own identity in audit logs but accountability chains back to you. Multiple agents per owner are fine — clone freely for parallel workloads.`),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Display name (e.g. \"Tamer's Reviewer Agent\")"),
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

func ListMyBots() mcp.Tool {
	return mcp.NewTool("enju_list_my_agents",
		mcp.WithDescription("List every agent you own, with each agent's active and revoked tokens (token VALUES are not returned — only labels and timestamps; token strings are shown once at registration). Paste the output verbatim in your reply — it's pre-formatted."),
	)
}

func RevokeToken() mcp.Tool {
	return mcp.NewTool("enju_revoke_token",
		mcp.WithDescription("Revoke a token. The token is preserved for audit (revoked_at timestamp set, row never deleted) but stops authenticating immediately. Self-service: callable by the token's owner directly — humans rotating their own session, or the parent of an agent whose token leaked. Pass either token (the raw string) OR token_id (from enju_list_my_agents)."),
		mcp.WithString("token",
			mcp.Description("Raw token string. Use this when the token leaked into logs / a CI env / your shell history."),
		),
		mcp.WithNumber("token_id",
			mcp.Description("Token row id from enju_list_my_agents. Use this when revoking a labeled deployment (e.g. \"the ci-server token\")."),
		),
	)
}


// --- Agent supervisor (Phase 4) ---
//
// These tools are fatclient-side: they manage local agent daemon
// subprocesses. The coordinator never sees them — agent lifecycle
// is fatclient-local per the design memo (project_bot_execution_architecture.md).

func BotStart() mcp.Tool {
	return mcp.NewTool("enju_agent_start",
		mcp.WithDescription(`Start an agent daemon defined inline in a workflow YAML's agents: section. The fatclient forks 'enju agent run --agent=<name> --workflow=<path>' as a subprocess, captures stdout/stderr to a per-agent log file (~/.enju/agents/logs/<name>.log), and tracks the PID for graceful stop.

One daemon per (machine, agent) for v1. Calling start on an already-running agent returns an error — call enju_agent_stop first if you want to restart.

Run-mode notes: the daemon polls the coordinator continuously, claiming tasks assigned to this agent and submitting verdicts. The daemon outlives this MCP tool call — call enju_agent_status / _stop to manage it.

When the workflow declares exactly one agent, the 'agent' argument may be omitted — the supervisor uses that single entry.`),
		mcp.WithString("agent",
			mcp.Description("Agent name from the workflow's inline agents: section. Optional when the workflow declares exactly one agent."),
		),
		mcp.WithString("workflow",
			mcp.Required(),
			mcp.Description("Path to the workflow YAML whose inline agents: section declares this fleet."),
		),
		mcp.WithNumber("project_id",
			mcp.Description("Optional project id to scope task discovery (0 / omitted = across every project the agent is a member of)"),
		),
	)
}

func BotStop() mcp.Tool {
	return mcp.NewTool("enju_agent_stop",
		mcp.WithDescription(`Gracefully stop a running agent daemon. The supervisor closes the daemon's stdin pipe, which triggers its watchStdinEOF goroutine to cancel the run loop and release any in-flight claim. Falls back to hard-kill (SIGKILL on Unix, TerminateProcess on Windows) after a 5s graceful timeout if the daemon is unresponsive.

Returns an error if the named agent isn't running (or wasn't started by this fatclient session — agents started by a previous fatclient instance are orphans the operator must kill manually).`),
		mcp.WithString("agent",
			mcp.Required(),
			mcp.Description("Agent name (must match a previously-started daemon)"),
		),
	)
}

func BotStatus() mcp.Tool {
	return mcp.NewTool("enju_agent_status",
		mcp.WithDescription(`List every agent daemon the fatclient is currently supervising. For each: name, PID, started_at timestamp, log file path. Use this to confirm what's running before reading logs or deciding to stop.

Empty list = no agents running in this fatclient session.`),
	)
}

func BotLogs() mcp.Tool {
	return mcp.NewTool("enju_agent_logs",
		mcp.WithDescription(`Tail the most recent N lines of an agent daemon's log file. Logs are append-mode across restarts so post-crash investigation can read what the agent was doing before it died. Returns the empty list when the agent was never started (no log file exists yet).`),
		mcp.WithString("agent",
			mcp.Required(),
			mcp.Description("Agent name"),
		),
		mcp.WithNumber("lines",
			mcp.Description("How many trailing lines to return (default 50, max 10000)"),
		),
	)
}

func BotStartAll() mcp.Tool {
	return mcp.NewTool("enju_agent_start_all",
		mcp.WithDescription(`Start every agent declared inline in a workflow YAML's agents: section. Convenience for first-touch demos where the operator wants the whole fleet up in one command. Skips agents that are already running. Returns the per-agent result list.`),
		mcp.WithString("workflow",
			mcp.Required(),
			mcp.Description("Path to the workflow YAML whose inline agents: section declares this fleet."),
		),
		mcp.WithNumber("project_id",
			mcp.Description("Optional project id passed through to each daemon"),
		),
	)
}

func BotStopAll() mcp.Tool {
	return mcp.NewTool("enju_agent_stop_all",
		mcp.WithDescription(`Stop every agent the supervisor is currently tracking. Best-effort — individual stop failures are reported per-agent but don't short-circuit the loop. Useful at session end so the operator's agents don't outlive their fatclient.`),
	)
}
