# CLI Reference

The `enju` binary is the single entry point for every role. This page is the operator reference — the commands you type to drive workflows from a terminal.

```
enju start       Start coordinator + web UI (background)
enju stop        Stop background processes started by 'enju start'
enju serve       Run the coordinator in the foreground (team/server deployment)
enju mcp         Start the MCP server (connect your LLM)
enju go          Run a workflow YAML end-to-end
enju status      Snapshot of the current project's state
enju runs        List runs for a project (with filters)
enju validate    Pre-flight check a workflow YAML
enju inbox       Show tasks waiting on you
enju review      Submit a verdict on a review task
enju dag         Render a run's task graph
enju agent       Agent lifecycle (setup, run, rotate-token)
enju version     Print version
```

---

## `enju start`

Start the coordinator and web UI in the background. On first run, registers you automatically using your git global config (`user.name` and `user.email`). Both fields are required — the command fails with a clear error if either is missing.

```sh
enju start
enju start --foreground    # for systemd / Docker deployment
```

| Flag | Default | Description |
|------|---------|-------------|
| `--port N` | 8333 | Coordinator port |
| `--db PATH` | `~/.enju/local.db` | SQLite state database path |
| `--foreground` | off | Run in foreground instead of forking to background |
| `--coordinator URL` | `http://localhost:<port>` | Override coordinator URL written to credentials.json |

After startup, the web UI is available at `http://localhost:8333/ui`.

---

## `enju stop`

Stop background processes started by `enju start`.

```sh
enju stop
```

---

## `enju serve`

Run the coordinator in the foreground — the lower-level command for team or server deployment. Unlike `enju start`, it does not fork to the background and does not start the web UI automatically.

```sh
enju serve
enju serve --port 8333 --db /var/enju/state.db
enju serve --config /etc/enju/enju.conf
```

Configuration can be set via a YAML config file. The default path is `~/.enju/enju.conf`; copy `enju.conf.example` from the repo to get started. CLI flags always override config file values.

**Config file (`~/.enju/enju.conf`):**

```yaml
coordinator:
  port: 8333

data:
  state_db: "~/.enju/local.db"   # events DB derived automatically as a sibling

events:
  emission_enabled: true    # kill-switch; flip without restart via SIGHUP

logging:
  level: "info"             # debug | info | warn | error
  output: "stdout"          # stdout | stderr | /path/to/file

performance:
  reaper_interval: "60s"
  http_request_timeout: "30s"
```

| Flag | Default | Description |
|------|---------|-------------|
| `--config PATH` | `~/.enju/enju.conf` | Config file path. Missing file is fine — built-in defaults apply. |
| `--port N` | from config, else 8000 | Coordinator port |
| `--db PATH` | from config, else `enju.db` | SQLite state database path |
| `--events-enabled` | `true` | Enable the event store at boot |

For team deployment, run `enju serve` under systemd or Docker on a shared server. Each team member then connects with `enju mcp --coordinator http://your-server:8333`.

---

## `enju mcp`

Start the MCP server. Your LLM (Claude, Gemini, Codex, or any MCP-compatible client) communicates with Enju through this server.

```sh
enju mcp                                      # connect to default coordinator
enju mcp --coordinator http://team:8333       # connect to team server
enju mcp --local                              # embed coordinator in this process
```

| Flag | Default | Description |
|------|---------|-------------|
| `--coordinator URL` | from `credentials.json`, else `http://localhost:8333` | Coordinator URL |
| `--local` | off | Embed a coordinator in this process — no separate `enju start` needed |
| `--db PATH` | `~/.enju/local.db` | SQLite path for `--local` mode |
| `--name NAME` | — | Display name (first run only) |
| `--email EMAIL` | — | Email (first run only; falls back to git config) |
| `--username USER` | — | Username override (first run only; auto-generated from name otherwise) |
| `--credentials PATH` | `~/.enju/credentials.json` | Credentials file path — use a custom path when running multiple MCP processes for different citizens on one machine |
| `--model MODEL` | — | LLM model name for git attribution (e.g. `claude-sonnet-4-6`) |

MCP config example for Claude Code:

```json
{
  "mcpServers": {
    "enju": {
      "command": "enju",
      "args": ["mcp", "--coordinator", "http://localhost:8333"]
    }
  }
}
```

---

## `enju go`

Run a workflow YAML end-to-end: discover or register the project, create a run, and execute compute tasks. Stops at human gates (review, vote, answer tasks assigned to a human) unless `--auto-agents` is set.

```sh
enju go enju.yaml
enju go enju.yaml --auto-agents
enju go enju.yaml --params dataset=tp53 --params confidence_threshold=90
enju go --dry-run enju.yaml
```

**What it does:**
1. Discovers the project from the workflow path (via `~/.enju/projects.json`), or auto-registers if none exists.
2. Creates the run (same as `enju_create_run` via MCP).
3. Drains compute tasks. Stops at the first citizen gate unless `--auto-agents` starts agent daemons to handle them.

| Flag | Default | Description |
|------|---------|-------------|
| `--auto-agents` | off | Start every agent declared in the workflow before the run; stop them when the run completes |
| `--params k=v[,k=v]` | — | Run parameter values. Comma-separated key=value pairs. Values starting with `[` or `{` are parsed as JSON. |
| `--params-file FILE` | — | JSON object of typed params. Merged under `--params` (inline keys win). |
| `--name NAME` | cwd basename | Project name when auto-registering |
| `--run-branch NAME` | base branch | The run's own branch (where its commits land before merging back). `auto` generates an isolated `<slug>-N` name. |
| `--base BRANCH` | project default | Branch the run forks **from** and reads the workflow from. `HEAD` (or `.`) = the current checked-out branch. Distinct from `--run-branch`. |
| `--sync none\|merge\|push` | from YAML | Override the workflow's sync mode for this run |
| `--max-tasks N` | 1000 | Safety cap on compute tasks drained per call. `0` = create run without draining. |
| `--parallel N` | 1 | Run up to N compute tasks concurrently. With `mode: sync` this drains a fanned-out run to completion N-at-a-time in one invocation; commits serialize through the project lock. Capped at 32. |
| `--dry-run` | off | Parse and resolve the workflow, print the DAG, exit without touching the coordinator |
| `--coordinator URL` | from `credentials.json` | Coordinator URL override |
| `--json` | off | NDJSON output — one record per task plus a summary record |

**Flags must precede the workflow path.** Go's flag parser stops at the first non-flag argument: `enju go --dry-run enju.yaml`, not `enju go enju.yaml --dry-run`.

**Exit codes:**

| Code | Meaning |
|------|---------|
| 0 | Run drained or stopped at a citizen gate (gates are not failures) |
| 1 | A compute task failed or git operations failed |
| 2 | Bad usage or workflow not found |
| 3 | No credentials for the coordinator — run `enju mcp` once to register |

---

## `enju status`

Snapshot of the current project: identity, coordinator, and active/recent runs.

```sh
enju status           # resolve project from cwd
enju status --all     # list all registered projects on this machine
```

```
Project: my-project (id=7)
Path:    /home/tamer/projects/my-project
As:      @tamer
Coord:   http://localhost:8333 (✓)

Active runs:
  #3 Pipeline run          active     tasks=8  branch=run-3
  #4 Ad-hoc analysis       waiting    tasks=2  branch=run-4

Recent (last 2):
  #2 Pipeline run          completed  tasks=8
  #1 Pipeline run          failed     tasks=8
```

| Flag | Default | Description |
|------|---------|-------------|
| `--project N` | cwd-walk | Numeric project ID override |
| `--all` | off | List every registered project instead of one |
| `--coordinator URL` | from `credentials.json` | |
| `--json` | off | Structured JSON output |

---

## `enju runs`

List runs for the current project, with filters. Also the CLI verb for terminating a run.

```sh
enju runs
enju runs --last 5 --status active
enju runs --terminate 13 --reason "design changed mid-run"
```

```
SEQ   NAME                      STATE       TASKS   BRANCH
17    Research analysis demo    completed   8       run-17
16    Scan deps                 completed   5       run-16
15    Ad-hoc analysis           terminated  2       run-15
```

| Flag | Default | Description |
|------|---------|-------------|
| `--status S[,S]` | all | Filter by state: `active`, `done`, `completed`, `failed`, `terminated`, … |
| `--last N` | 20 | Cap rows returned (`0` = all), most-recent first |
| `--terminate SEQ` | — | Terminate run `SEQ` — cascade-skips non-terminal tasks, abandons open claims |
| `--reason TEXT` | — | Audit reason recorded with `--terminate` |
| `--project N` | cwd-walk | Numeric project ID override |
| `--json` | off | JSON array output |

---

## `enju validate`

Pre-flight check a workflow YAML. Parses, validates, and reports errors and warnings. Safe to run in CI — no coordinator, no state changes.

```sh
enju validate enju.yaml
enju validate workflows/*.yaml --strict
```

```
✓ enju.yaml
✗ workflows/broken.yaml
  task "review_plan": prompt references undefined variable {{draft_plan}};
  known task ids: [draft_plan review_plan run_analysis]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--strict` | off | Treat warnings as failures |
| `--json` | off | One JSON report object per file |

**What it checks:** YAML decode (unknown fields rejected), `name`/`version`, `params:` declarations, task ID uniqueness, `depends_on`/`reviews`/`collects` targets exist, `{{X.field}}` references resolve, cycle detection.

**Warnings** (non-fatal without `--strict`): compute task with no upstream, `{{task.content}}` on a task that declared `writes:` (use `{{artifact:path}}` instead), review task that gates nothing downstream.

**Exit codes:** 0 = all valid, 2 = bad usage, 4 = parse error or warnings with `--strict`.

---

## `enju inbox`

Show ready tasks assigned to you, with upstream submissions inlined. Reads from local files — no coordinator round-trip.

```sh
enju inbox 3        # project_id = 3
```

See [Inbox & Review](../guides/inbox-and-review.md) for the full workflow.

---

## `enju review`

Submit a verdict on a review task you've claimed. Opens `$EDITOR` if `-content` is omitted.

```sh
enju review 3:2:review_abstract
enju review 3:2:review_abstract -decision approve -content "Ship it."
```

| Flag | Default | Description |
|------|---------|-------------|
| `--decision VERB` | prompted | `approve`, `request_changes`, `reject`, or `comment` |
| `--content TEXT` | `$EDITOR` | Review prose |
| `--coordinator URL` | from `credentials.json` | |
| `--credentials PATH` | `~/.enju/credentials.json` | |

The task must already be claimed (claim via the web UI or MCP first).

---

## `enju dag`

Render a run's task graph.

```sh
enju dag 3                          # run seq 3 in the active project, default text
enju dag 3 --project 7              # run seq 3 in project 7
enju dag 3 --format mermaid         # Mermaid diagram source
enju dag 3 --format json           # JSON adjacency list
```

The run sequence is a positional argument. `--project` is optional
(defaults to the project that owns the current directory). `--format`
takes `mermaid` or `json`; omit it for the default text rendering.

---

## `enju agent`

Agent lifecycle commands. See [Agents](../guides/agents.md) for the full guide.

```sh
enju agent setup --project-id=3      # register agents from workflow, write credentials
enju agent run --agent=dev-agent     # start a daemon for one agent
enju agent rotate-token --agent=dev-agent   # revoke and reissue agent token
```

---

## `enju version`

Print the build version, commit SHA, and build date.

```sh
enju version
# enju 0.9.1 (commit a1b2c3d, built 2026-05-19)
```
