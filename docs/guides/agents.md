# Agents

An agent is an automated citizen that claims and executes tasks. Its handler can be an LLM, a script, or any executable that takes text in and produces text out. Nothing here implies cognition — an agent is *who acts unattended*, not *what thinks*.

Agents are first-class citizens: they claim tasks, submit results, appear in the audit log, and are gated by project membership exactly like humans. The difference is that nobody is sitting in front of them.

---

## Declaring agents in a workflow

Agents are declared inline in the workflow YAML under `agents:`:

```yaml
agents:
  - name: developer-agent
    handler: claude
    model: claude-sonnet-4-6
    system_prompt: prompts/developer.md     # optional, repo-relative
    mcp_tools:
      allow: [Read, Edit, Write, Bash, Grep, Glob]
    args:
      - "-p"
      - "--model={{model}}"
      - "--append-system-prompt={{system_prompt}}"
      - "--allowedTools={{allowed_tools}}"

  - name: reviewer-agent
    handler: claude
    model: claude-opus-4-7
    mcp_tools:
      allow: [Read, Grep, Glob]             # read-only by construction
    args:
      - "-p"
      - "--model={{model}}"
      - "--allowedTools={{allowed_tools}}"
```

**Field reference:**

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Citizen username. ASCII letters, digits, dash, underscore. |
| `handler` | no | Binary to spawn: `claude`, `gemini`, path to a script. Defaults to `claude`. |
| `model` | yes (for LLM handlers) | Model identifier passed to the handler binary. |
| `system_prompt` | no | Repo-relative path to a `.md` file prepended to every task prompt. |
| `mcp_tools.allow` | no | Tool allowlist. Omit = all tools allowed. Empty list = validation error. |
| `args` | yes (for `claude`) | Argv template passed to the handler binary. Required — without it the binary is spawned with no arguments. |
| `replicas` | no | Expand to N identical agents (`dev-agent-1`, `dev-agent-2`, …). See [Parallel agent pools](#parallel-agent-pools). |

The `args:` template supports `{{model}}`, `{{system_prompt}}`, and `{{allowed_tools}}` placeholders substituted at daemon startup.

---

## Handler types

`handler:` is the binary the daemon spawns per task. Enju has no LLM-specific code — the CLI flags live in `args:`, not in Go.

**LLM handlers** (`claude`, `gemini`, any MCP-compatible CLI):

```yaml
handler: claude
args:
  - "-p"
  - "--model={{model}}"
  - "--allowedTools={{allowed_tools}}"
```

**Script handlers** (any executable — wrap a linter, a data pipeline, a rule engine):

```yaml
- name: lint-agent
  handler: ./scripts/lint-handler.sh
  args: []
```

The handler contract is plain text: prompt on stdin, response on stdout. Any process that honors this works as an agent handler.

---

## Tool allowlists

`mcp_tools.allow` is a **process-boundary pin** — when the daemon spawns the LLM, only the listed tools are registered. The LLM physically cannot call tools outside the allowlist, regardless of what the prompt asks.

```yaml
mcp_tools:
  allow: [Read, Grep, Glob]    # reviewer can read but never write
```

Three states:
- **Section omitted** — all tools allowed (default)
- **`allow: [Tool1, Tool2]`** — exactly those tools
- **`allow: []`** — validation error; explicit empty list is rejected as a foot-gun

The coordinator does not enforce allowlists — this is a local runner constraint. It is declared in the workflow (reviewable in git), pinned by the daemon at subprocess spawn time, and observable in the audit log.

For `answer` and `contribute` tasks where the agent writes files, the allowlist is load-bearing. For `review` and `vote` tasks (text in, verdict out), tools matter less — but the same wiring applies.

---

## Starting agents

**`--auto-agents` (recommended for most runs):**

```sh
enju go enju.yaml --auto-agents
```

Every agent in the workflow's `agents:` block is started before the run begins and stopped automatically when the run reaches a terminal state. This is the zero-friction path — no separate setup step.

**Via MCP (from your LLM or the web UI):**

```
enju_agent_start agent=developer-agent
enju_agent_start_all                      # start every agent in the manifest
enju_agent_status                         # check what's running
enju_agent_stop agent=developer-agent
enju_agent_logs agent=developer-agent lines=50
```

**Manually from the CLI:**

```sh
enju agent run --agent=developer-agent
```

The daemon self-heals on first start — it registers the agent if credentials are missing, adds it to project membership, and begins polling. On subsequent runs these steps are skipped silently.

---

## Registration and setup

For most workflows `--auto-agents` handles registration automatically. For explicit batch setup (first-time onboarding, scripted environments):

```sh
cd my-project
enju agent setup --project-id=3
```

```
Setting up 2 agent(s): developer-agent, reviewer-agent
  developer-agent  ✓ registered, credentials at ~/.enju/credentials/developer-agent.json
  developer-agent  ✓ added to project #3
  reviewer-agent   ✓ registered, credentials at ~/.enju/credentials/reviewer-agent.json
  reviewer-agent   ✓ added to project #3
2 registered, 0 skipped, 0 failed
```

`enju agent setup` is idempotent — re-running it against a project where agents are already registered skips them silently. If credentials were deleted or lost, re-running setup rotates the token and writes a fresh credentials file.

---

## Daemon lifecycle

The daemon loop per iteration:

1. **Find work** — poll coordinator for READY tasks where `assign_to` includes the agent's username, or where `assign_to` is empty (open task)
2. **Claim** — record the claim, pull the run branch, fetch the resolved prompt with upstream outputs already inlined
3. **Reset** — wipe any residue from the previous iteration so the new task starts on a clean canvas
4. **Process** — run the handler with the prompt; capture the response
5. **Submit** — commit result to git, report to coordinator, which advances task state and unblocks dependents
6. **Loop** — back off exponentially on empty polls (1s → 30s max); lost claim races are skipped silently

**Shutdown** is graceful on SIGINT, SIGTERM, or stdin EOF (how the supervisor stops agents). Any in-flight claim is released before exit so the next agent doesn't wait for a reaper timeout.

---

## Parallel agent pools

Use `replicas: N` when you want multiple identical agents competing for work from the same queue:

```yaml
agents:
  - name: dev-agent
    replicas: 3               # expands to dev-agent-1, dev-agent-2, dev-agent-3
    model: claude-sonnet-4-6
    mcp_tools:
      allow: [Read, Edit, Write, Bash]
    args:
      - "-p"
      - "--model={{model}}"
      - "--allowedTools={{allowed_tools}}"
```

Each replica is an independent citizen with its own credentials and its own working tree. Claim races between replicas are safe — the coordinator hands each task to exactly one agent atomically. To target the pool from a task:

```yaml
- id: implement_feature
  action: answer
  assign_to: [dev-agent-1, dev-agent-2, dev-agent-3]
```

The first replica to claim wins; the others move on. Cap is 32 replicas per entry.

---

## Review and vote response conventions

For `review` tasks, the daemon extracts the verdict from the handler's text response. Three accepted shapes:

```text
# Shape 1 — verdict leads
approve
Tests pass and the implementation is correct.

# Shape 2 — DECISION: marker (recommended for think-then-conclude responses)
Looking at the diff carefully...
All edge cases are handled.

DECISION: approve

# Shape 3 — verdict closes
After reviewing the design, it is sound.

approve
```

**If no verdict is found, the daemon falls back to `request_changes`** — "send back for revision" is non-destructive, so an unclear response loops rather than jamming the pipeline.

For `vote` tasks, no safe default exists — every option is meaningful. An unparseable response fails the iteration loudly so an operator or a tweaked prompt handles it explicitly.

---

## Project layout

```
my-project/
├── prompts/
│   ├── developer.md          # agent system prompts (committed)
│   └── reviewer.md
├── scripts/                  # compute task scripts
├── enju.yaml                 # workflow including agents: section
└── .enju/                    # runtime state (gitignored)
    └── agents/
        ├── developer-agent/
        │   └── clone/        # this agent's working tree
        └── reviewer-agent/
            └── clone/
```

Each agent gets its own working tree so parallel daemons on the same project don't interfere. Per-agent runtime files (PID, log) live under `~/.enju/agents/` — per-machine state, not committed.

---

## Who runs the fleet

In a multi-human project, **one designated machine runs the agent fleet** — typically the project owner's machine or a dedicated always-on box.

Each call to `enju agent setup` registers a *new* agent citizen parented to whoever ran it. If two people both run setup, Alice gets `reviewer-agent` and Bob gets `reviewer-agent-1`. Tasks assigned to `reviewer-agent` only match one of them.

**Practical patterns:**

| Pattern | When to use |
|---------|-------------|
| Owner's machine runs agents | Single-user or small teams; simplest |
| Dedicated always-on machine (VM, Pi) | Team projects where agents should run overnight |
| `--auto-agents` per run | Disposable runs; agents start and stop with the run |

Multi-tenant agent ownership (project-owned agents not parented to a specific human) is on the post-launch roadmap.

---

## Known caveats

- **Open tasks are claimed.** A task with no `assign_to` is open to any agent. If you want a task to go to a human only, set `assign_to: [username]` explicitly.
- **One daemon per agent name per machine.** Starting an already-running agent is a no-op. To restart, stop first.
- **Daemon crash leaves an open claim.** The coordinator's reaper times it out within ~60s. New daemon instances back off until the task becomes re-claimable.
- **Hard-kill orphans the handler subprocess.** On a graceful shutdown the handler exits cleanly. On a hard kill (`enju_agent_stop` timeout), the underlying `claude` process continues running until its pipes break, consuming API tokens. A process-tree fix is on the roadmap.

---

## Next steps

- [Writing Workflows](writing-workflows.md) — `assign_to`, `for_each`, tool allowlists in workflow context
- [Credentials & Identity](credentials.md) — agent registration, token rotation, multi-user setup
- [Inbox & Review](inbox-and-review.md) — the human side of reviewing agent-produced work
