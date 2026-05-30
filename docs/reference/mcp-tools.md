# MCP Tools Reference

All MCP tools exposed by `enju mcp`. Your LLM can call these directly; you can also refer to them when scripting or debugging. Tools are grouped by concern.

For context on how the MCP server fits into the overall architecture, see [How Enju Works](../how-it-works.md).

---

## Orientation

When starting a session, these tools give you a quick picture of state:

| Tool | What it answers |
|------|----------------|
| `enju_my_dashboard` | Your active claims, inbox count, recent activity |
| `enju_my_profile` | Your identity, username, token status |
| `enju_list_projects` | All projects you have access to |
| `enju_run_status` | Full state of a run: tasks, which are blocked, which are done |
| `enju_inbox` | Tasks currently assigned and ready for you |
| `enju_recent_events` | Event stream — what has happened recently |

**Common patterns:**

- "What's on my plate?" → `enju_inbox`
- "What's been happening?" → `enju_recent_events with for_me=true`
- "Where is run #3 stuck?" → `enju_run_status`
- "What changed since last check?" → `enju_recent_events with since_seq=<last_seq>`

---

## Project management

| Tool | Description |
|------|-------------|
| `enju_create_project` | Adopt a local directory as an Enju project. Smart-detects: empty → init + seed README; populated, no git → init + commit; existing repo → adopt as-is. |
| `enju_list_projects` | List all projects you're a member of. |
| `enju_archive_project` | Hide a project (soft-delete; restorable). |
| `enju_restore_project` | Restore an archived project. |
| `enju_set_project_remote` | Set or update the project's git remote URL. |
| `enju_set_project_default_branch` | Change the project's default branch. |
| `enju_set_project_push_topic_branches` | Toggle whether per-task topic branches push to origin (default true; flip to false for solo bulk pipelines). |
| `enju_project_remote_status` | Check whether the local clone is in sync with the remote. |
| `enju_project_sync` | Push/pull the project to/from its remote. |
| `enju_add_project_member` | Add a citizen to the project. |
| `enju_remove_project_member` | Remove a citizen from the project. |
| `enju_list_project_members` | List all members and their roles. |
| `enju_promote_member` | Promote a member to owner. |
| `enju_demote_owner` | Demote an owner to member. |
| `enju_leave_project` | Remove your own membership (and optionally the local clone). |

---

## Run management

| Tool | Description |
|------|-------------|
| `enju_create_run` | Create a run from a workflow YAML. `path=` mode forks a run branch and materializes the snapshot. Params override workflow defaults. |
| `enju_list_runs` | List runs for a project, with optional state filters. |
| `enju_run_status` | Full task-by-task status of a run: states, blockers, recent events. |
| `enju_pause_run` | Pause a running run — prevents new claims; in-flight tasks complete. |
| `enju_resume_run` | Resume a paused run. |
| `enju_terminate_run` | Terminate a run — cascade-skips non-terminal tasks, abandons open claims. Irreversible. |
| `enju_set_cycle_budget` | Set or adjust the agent cycle budget for a run (limits total agent iterations). |
| `enju_execute_run` | Drive compute tasks in a run — claims and executes all ready compute tasks. Used by `enju go` internally. |
| `enju_list_workflows` | List every YAML file in the project repo on the default branch (paths only, hidden dirs excluded; picking which file is a workflow is the caller's call). |
| `enju_describe_workflow` | Show one workflow's name, description, and declared params. Parses the YAML. |

---

## Task lifecycle

| Tool | Description |
|------|-------------|
| `enju_get_task` | Read a task's full state: prompt, action, current state, submissions, upstream deps. |
| `enju_get_task_inputs` | Read all resolved upstream inputs for a task — what the agent will see in the prompt. |
| `enju_list_ready_tasks` | List tasks currently READY across a project (all claimable work). |
| `enju_list_iterations` | List all iterations of a fan-out task. |
| `enju_claim_task` | Claim a specific task by ID. |
| `enju_claim_ready_matching` | Claim any ready task matching given criteria (action type, assign_to). Used by agent daemons. |
| `enju_release_task` | Release a claimed task back to READY without submitting. |
| `enju_submit_result` | Submit a result for a claimed task. For answer/contribute: provide `content` and optionally commit file changes first. For review: provide `content` and `decision`. |
| `enju_submit_results_batch` | Submit results for multiple tasks atomically. |
| `enju_review` | Narrow wrapper for review tasks: `task_id`, `decision`, `content`. Validates the task is action:review before submitting. |
| `enju_invalidate_task` | Reset a task to PENDING — re-derives it from scratch. Cascades to descendants. |
| `enju_retry_task` | Retry a FAILED or FAILED_RETRYABLE task. |
| `enju_fail_task` | Mark a task as failed with a reason. |
| `enju_tally_task` | Force-tally a COLLECTING task before quorum is met (override for stuck multi-citizen tasks). |
| `enju_spawn_task` | Spawn a new task into a running run (used by remediation flows; available for ad-hoc task injection). |
| `enju_request_clarification` | Pause a task and ask the assigner a question; suspends the claim without releasing it. |
| `enju_execute_task` | Execute a specific compute task by ID (claim + run + submit). |

---

## Inbox & review

| Tool | Description |
|------|-------------|
| `enju_inbox` | List tasks currently READY and assigned to you, with upstream submissions inlined. Zero coordinator round-trips — reads from local `live.jsonl` + git. |
| `enju_review` | Submit a verdict on an action:review task. Commits the verdict to git as part of the full fat-client flow. |

---

## Artifacts

| Tool | Description |
|------|-------------|
| `enju_list_artifacts` | List all tracked artifacts for a run (files committed to git). |
| `enju_get_artifact` | Read a tracked artifact's content at its latest accepted commit. |
| `enju_get_artifact_history` | Read an artifact's full commit history across the run. |
| `enju_list_untracked_artifacts` | List untracked artifacts (files in `.enju/bigfiles/`). |

---

## Events & monitoring

| Tool | Description |
|------|-------------|
| `enju_recent_events` | Recent events from the event log. Filter with `for_me=true` (events about you), `since_seq=N` (incremental), `project_id`, `run_id`. |
| `enju_show_events` | Show raw events for a specific task or run segment. |
| `enju_events_status` | Check whether the event store is enabled and its current state. |

---

## Issues

| Tool | Description |
|------|-------------|
| `enju_file_issue` | File an issue against a project (bugs, blockers, questions). |
| `enju_list_issues` | List open issues for a project. |
| `enju_get_issue` | Read a specific issue. |
| `enju_triage_issue` | Update issue priority or status. |
| `enju_close_issue` | Close an issue. |

---

## Exports

| Tool | Description |
|------|-------------|
| `enju_export_run` | Export a full run as a single document: all task prompts, submissions, and verdicts stitched together. |
| `enju_export_diagram` | Export the run's task DAG as a Mermaid diagram or JSON. |
| `enju_export_run_events` | Export the event log for a run as NDJSON. |

---

## Agents

| Tool | Description |
|------|-------------|
| `enju_list_my_agents` | List agents registered under your identity. |
| `enju_register_agent` | Register a new agent citizen (creates the citizen record; credentials are written by `enju agent setup`). |
| `enju_revoke_token` | Revoke all active tokens for a citizen. |
| `enju_reissue_agent_token` | Revoke and reissue an agent's token. |
| `enju_agent_start` | Start the daemon for a named agent. |
| `enju_agent_stop` | Stop the daemon for a named agent. |
| `enju_agent_start_all` | Start daemons for all agents in the active workflow. |
| `enju_agent_stop_all` | Stop all running agent daemons. |
| `enju_agent_status` | Show which agents are running, idle, or stopped. |
| `enju_agent_logs` | Read recent log lines from an agent daemon. |

---

## Identity

| Tool | Description |
|------|-------------|
| `enju_my_profile` | Your identity, username, email, and model attribution setting. |
| `enju_update_profile` | Update your display name, email, or model attribution. |

---

## Tool allowlists

When agent daemons are spawned, the workflow's `mcp_tools.allow` list pins which tools are registered for that agent. This is a process-boundary constraint — the LLM physically cannot call tools outside the allowlist, regardless of what the prompt requests.

For human MCP sessions, all tools are available by default. The `--allow-tools` flag on `enju mcp` can restrict them if needed (used by the agent runner to pin per-agent toolboxes).
