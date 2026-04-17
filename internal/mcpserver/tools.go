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
		mcp.WithDescription("Claim a task to work on. This opens a collaboration window — iterate with the human, discuss, refine. Only submit when the result is ready. Returns the task prompt and upstream context. After claiming, tell the human which task you're now working on."),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The ID of the task to claim"),
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
  - "approve" — work is good, pass downstream
  - "request_changes" — work needs revision, send back for another round (target bounces to READY)
  - "reject" — work is fundamentally wrong, hard stop (target becomes FAILED, terminal)
  - "comment" — non-blocking note, no state change on the target
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
		mcp.WithDescription("Get the status of a run including DAG tree view of all tasks. Paste the output verbatim in your reply — it's pre-formatted with progress bar, state icons, and tree structure."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID"),
		),
		mcp.WithNumber("run_id",
			mcp.Required(),
			mcp.Description("The run sequence number within the project (#1, #2, #3)"),
		),
	)
}

func toolCreateRun() mcp.Tool {
	return mcp.NewTool("enju_create_run",
		mcp.WithDescription(`Create a new Enju run. Three ways to provide the run definition, pick one:

1. WRITE IT DIRECTLY: pass "yaml" with the full run definition — use this for one-off runs the user is authoring from scratch.
2. FROM A SAVED TEMPLATE: pass "path" pointing at a enju_templates/*.yaml recipe in the project clone, plus "params" with the values that template asks for. Use this whenever a user's request matches a known recipe — see enju_list_templates.
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

If you don't have a project yet, create one first with enju_create_project.`),
		mcp.WithString("yaml",
			mcp.Description("The run definition in YAML format. Required unless 'path' is provided."),
		),
		mcp.WithString("path",
			mcp.Description("Repo-relative path to a template under enju_templates/, e.g. 'enju_templates/gwas.yaml'. The template is read from the local project clone. Mutually exclusive with 'yaml'."),
		),
		mcp.WithObject("params",
			mcp.Description("Parameter values for a run that declares a top-level 'params:' block. Keys are parameter names; values must match the declared types. Use enju_describe_template to see what a template expects."),
		),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID to create this run in (use enju_list_projects to see existing projects)"),
		),
	)
}

// toolListTemplates is the LLM's template-discovery entry
// point. Returns every YAML file under the project clone's
// enju_templates/ directory with its name, description, and
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

func toolExecuteTask() mcp.Tool {
	return mcp.NewTool("enju_execute_task",
		mcp.WithDescription(`Execute a compute task's script, capture its output, and submit the result — all in one call.

For action:compute tasks only. Claims the task if not already claimed, runs the declared script in the project's local clone, captures stdout as the result, and submits automatically.

Environment variables available to the script:
  ENJU_TASK_ID      — the full task ID
  ENJU_PROJECT_DIR  — the project's local clone root
  ENJU_RUN_DIR      — the result directory for this task

Exit code semantics:
  0     → submit as completed (stdout → result.md)
  non-0 → task fails (stderr shown as the failure reason)

The script runs in the project's workspace directory. It has full access to the local clone (upstream results, artifacts, etc.).`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The task to execute"),
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
		mcp.WithDescription(`List the reusable run recipes (templates) available in a project. Each entry shows the template's name, description, and its declared parameters. Use this first when a user asks to do something that matches a known recipe — the template saves them from hand-writing a run YAML.

Templates are just regular run YAML files that live under enju_templates/ in the project git repo. Any run can be promoted to a template by copying its YAML file into enju_templates/; no conversion step.

To see full parameter docs for one template (types, defaults, descriptions), call enju_describe_template <path>. To instantiate a template into a run, call enju_create_run with 'path' and 'params'.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose enju_templates/ directory to scan"),
		),
	)
}

// toolDescribeTemplate returns the full parameter block for a
// single template so the LLM can turn each param into a
// user-facing question ("which disease?", "which tissue?")
// before filling in values and calling enju_create_run.
func toolDescribeTemplate() mcp.Tool {
	return mcp.NewTool("enju_describe_template",
		mcp.WithDescription(`Show the full metadata for one template: name, description, and every declared parameter with its type, default, and prose description. Use this when a user picks a template from enju_list_templates and you need to gather the parameter values before calling enju_create_run.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose template to describe"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Repo-relative template path, e.g. 'enju_templates/gwas.yaml'"),
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
	)
}

func toolInit() mcp.Tool {
	return mcp.NewTool("enju_init",
		mcp.WithDescription(`Adopt an existing folder as an Enju project. Use this when the user already has a directory (with or without git) and wants to add Enju orchestration on top. Enju writes its scaffold (.enju/, enju_templates/) into the folder and respects all existing files. If the user wants to start fresh with nothing, use enju_create_project instead.`),
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

func toolSetProjectRemote() mcp.Tool {
	return mcp.NewTool("enju_set_project_remote",
		mcp.WithDescription("Set or clear the external git remote URL for a project. Subsequent task result commits will be pushed to this remote. Pass an empty string to clear the remote."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose remote to update"),
		),
		mcp.WithString("remote_url",
			mcp.Required(),
			mcp.Description("Git remote URL, or empty string to clear"),
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
		mcp.WithDescription("Forget a project's local clone and delete its workspace directory. Use this to reclaim disk space when you're done working on a project, or to recover from a corrupted local clone. The remote repo is untouched — this is a local cache wipe only. The next time you touch the project (claim a task, read an artifact, etc.) it will be re-cloned from the remote."),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose local clone should be deleted"),
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
		mcp.WithDescription(`Mark an accepted task as invalid because its result turned out to be wrong. Cascades to all downstream dependents — they transition back to PENDING and wait for the target to re-complete. The target itself goes back to READY so any citizen can re-claim and re-run it.

Git history preserves the previous result; the new one overwrites it when submitted.

Only tasks in the 'accepted' state can be invalidated. Use this when you notice a task produced a bad result after the fact (hallucination, wrong data, missing piece).`),
		mcp.WithString("task_id",
			mcp.Required(),
			mcp.Description("The fully-qualified ID of the task to invalidate"),
		),
		mcp.WithString("reason",
			mcp.Description("Short explanation for the invalidation — shown in logs and the response"),
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
