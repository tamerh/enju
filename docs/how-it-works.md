# How Enju Works

This page explains the components behind the scenes and how they fit together. You don't need this to run your first workflow — but it helps when you're debugging, deploying to a team, or building integrations.

---

## Components

See [diagrams.md](diagrams.md) for the full component diagram and request/submit flow sequences. In brief:

- **Outside interfaces** — LLM clients (Claude, Gemini, Codex, …), the terminal, and the browser each connect to enju through different entry points but all go through the same service layer.
- **enju fat client · service layer** — the single binary that runs on each user's machine. All entry points (`enju mcp`, CLI tools, `enju ui`, agent daemons) share the same underlying service layer, which handles coordinator calls and git operations.
- **State & persistence** — the coordinator (state database + events database) and the git repository.

---

## Coordinator

The coordinator is the single source of truth for workflow state. It is a stateless HTTP server backed by two independent databases:

- **State database** — all mutable state: citizens, projects, runs, tasks, claims, submissions, and membership.
- **Events database** — the append-only audit event log. Kept separate so a slow or corrupted events database cannot affect state operations, and vice versa.

Every mutation goes through a single codepath called **ApplyPlan**: the coordinator computes what should change (claim a task, advance a task to ACCEPTED, cascade dependents to READY), applies the entire plan atomically to the state database, notifies the caller, then updates the events database. Either the state commits fully or nothing changes — partial updates cannot corrupt the task graph.

**The coordinator is content-neutral.** It stores task state, prompts, citizen records, and event metadata — but never file content. Artifacts, agent outputs, and produced files all live in git. This is intentional: Enju inherits git's proven durability, history, and diffing for free, without duplicating any of it in the state database.

---

## Fat client

The fat client is the service layer that runs on each user's machine. It bridges the coordinator, the local git repository, and agent daemons — and it can be driven two ways:

- **Via MCP** (`enju mcp`): your LLM talks to it over stdio. Claude, Gemini, Codex, and any other MCP-compatible client can create projects, run workflows, claim tasks, and submit results through natural language.
- **Via CLI** (`enju go`, `enju inbox`, `enju review`, …): you drive it directly from the terminal, like any other workflow system. No LLM required.

Both paths go through the same service layer — the same git operations, the same coordinator calls, the same task state machine. See [diagrams.md](diagrams.md) for the step-by-step claim and submit flows.

---

## Agent daemons

An agent is a long-running subprocess that polls for tasks it is eligible to claim and executes them autonomously. Agents are not limited to LLMs:

| Handler | What runs |
|---------|-----------|
| `claude`, `gemini`, … | LLM subprocess via CLI |
| `compute` (inline script) | Shell script, Python, any executable |
| `compute` + container | Script inside Docker / Apptainer |

Agents are forked and supervised by the fat client — the coordinator is not involved in their lifecycle. Each daemon uses the same service layer as the MCP and CLI entry points. Agents are started with `enju_agent_start` (via MCP) or `--auto-agents` (via `enju go`). Their logs land in `~/.enju/logs/`.

---

## Task types and fan-out

Enju supports five task action types, each with different execution semantics:

| Action | Executor | Notes |
|--------|----------|-------|
| `answer` | Agent (LLM) or human | Open-ended response or file output |
| `compute` | Script / container | `mode: sync` blocks; `mode: async` detaches for long-running jobs |
| `review` | Human or agent | Approve / request_changes / reject; can trigger remediation |
| `vote` | Multiple citizens | Quorum + threshold rules; losing branches cascade to SKIPPED |
| `contribute` | Agent or human | Multiple submissions merged into a shared artifact |

**Fan-out with `for_each`:** A task (or an entire run) can be fanned out over a list of inputs using `for_each`. Each item produces an independent iteration — its own state, its own git branch, its own claim — so they run in parallel. Downstream tasks can then aggregate over the results.

---

## The event log

Every state change emits an event to the events database. Events are append-only and streamed to connected clients in real time. The web UI, `enju_recent_events`, and `enju_run_status` all read from this stream.

Events sit between git commits (final results) and debug logs (everything): they record what the system decided and when, without the noise of implementation detail.

---

## Git as the output layer

The coordinator tracks state; git holds everything produced. Every task result is a commit on the run branch with the submitting citizen as author. When an LLM agent produces a result, proper attribution is given via a `Co-Authored-By` trailer if configured:

```
commit a1b2c3d
Author: Alice <alice@example.com>
Co-Authored-By: claude-sonnet-4-6 <noreply@anthropic.com>

enju: submit write_report
```

This gives you complete history, attribution, and reproducibility — using infrastructure that already exists in every git repository.

When a run completes, Enju merges the run branch back to the base branch. If the run fails, the branch stays for inspection.

---

## Local vs. team deployment

**Local (single user):** `enju start` runs the coordinator and UI on your machine. One credentials file, two databases, everything in `~/.enju/`.

**Team:** Deploy the coordinator on a shared server (or run `enju start --foreground` under systemd / Docker). Each team member runs `enju mcp --coordinator http://your-server:8333` on their own machine — that is the only configuration change. The coordinator handles authentication; each user has their own bearer token. Git operations still happen locally on each member's machine.

---

## Next steps

- [Writing Workflows](guides/writing-workflows.md) — the full workflow YAML reference
- [Agents](guides/agents.md) — handler types, daemon lifecycle, tool allowlists
- [Credentials & Identity](guides/credentials.md) — tokens, multi-user setup, team deployment
