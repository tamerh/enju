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
2. FROM A SAVED TEMPLATE: pass "path" pointing at a bundle dir (enju_templates/<name>) or its template.yaml manifest. At create_run, the bundle is snapshotted into .enju/runs/{seq}/template/ and the run is pinned to that frozen copy — later edits to the live template don't affect this run. Script paths resolve from the snapshot. Supply "params" with the values the template declares; see enju_list_templates.
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
			mcp.Description("Template bundle reference. Accepts either the bundle dir ('enju_templates/gwas-analysis') or its manifest ('enju_templates/gwas-analysis/template.yaml'). The bundle is snapshotted into the run's .enju/runs/{seq}/template/ for reproducibility. Mutually exclusive with 'yaml'."),
		),
		mcp.WithObject("params",
			mcp.Description("Parameter values for a run that declares a top-level 'params:' block. Keys are parameter names; values must match the declared types. Use enju_describe_template to see what a template expects."),
		),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project ID to create this run in (use enju_list_projects to see existing projects)"),
		),
		mcp.WithString("branch",
			mcp.Description(`Git branch this run commits to. Omit to use the project's default branch. Pass "auto" to have the coordinator pick an unused name — for template runs this is "<bundle>-1", "<bundle>-2", ... (e.g. path="enju_templates/gene-mapping" → "gene-mapping-1"); for inline YAML it falls back to "run-1", "run-2", .... Useful for parallel parameter sweeps. Pass an explicit name ("experiment-2", "enju/work") for a named isolated branch. The coordinator enforces SERIAL runs per branch: a second run on the same branch is refused until the first finishes. To run several variants at once, give each its own branch.`),
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
// JSONL file under .enju/runs/{seq}/events/. The authoritative
// data lives in the coordinator's contribution_events +
// task_claims; this tool just snapshots it into git so the
// run's directory becomes self-documenting for audits /
// postmortems / preprint figures.
func toolExportRunEvents() mcp.Tool {
	return mcp.NewTool("enju_export_run_events",
		mcp.WithDescription(`Snapshot a run's event timeline (claims, submits, invalidations, tally resolutions) to a git-tracked JSONL file under .enju/runs/{seq}/events/{phase}.jsonl.

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
// source to a git-tracked file under .enju/runs/{seq}/graph/.
// See handleExportDiagram for the semantics (idempotent same-
// phase overwrite, no-op on unchanged content, response shape).
func toolExportDiagram() mcp.Tool {
	return mcp.NewTool("enju_export_diagram",
		mcp.WithDescription(`Snapshot the run's DAG to a git-tracked Mermaid file for archival, preprint figures, or README embedding.

Writes raw .mmd source (no markdown fences) to .enju/runs/{seq}/graph/{phase}.mmd and commits it. Common phase values:
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

A template is a directory bundle under enju_templates/ with template.yaml at its root and any supporting scripts/data bundled alongside:
  enju_templates/
    gwas-analysis/
      template.yaml        # the manifest
      scripts/analyze.py   # bundled, picked up by the snapshot

Scripts + data travel with the manifest as one unit, so a compute task's script: is always co-located. Loose .yaml files directly under enju_templates/ are the legacy single-file shape — they surface with a migration hint in the listing, not a usable template.

Call enju_describe_template for a template's parameters; enju_create_run with path=<bundle> to instantiate.`),
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
		mcp.WithDescription(`Show full metadata for one template bundle: name, description, declared params with types/defaults/descriptions. Use after picking a template from enju_list_templates to gather param values before enju_create_run.`),
		mcp.WithNumber("project_id",
			mcp.Required(),
			mcp.Description("The project whose template to describe"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Bundle reference — either the bundle dir ('enju_templates/gwas-analysis') or the full manifest path ('enju_templates/gwas-analysis/template.yaml'). Both resolve to the same bundle."),
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
