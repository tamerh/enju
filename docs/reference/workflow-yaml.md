# Workflow YAML Reference

Complete field reference for `enju.yaml` workflow files. For a narrative guide with examples, see [Writing Workflows](../guides/writing-workflows.md).

---

## Top-level structure

```yaml
name: string           # required — human label for the workflow
description: string    # optional — shown to agents; describe the workflow's purpose
version: 1             # schema version, always 1

base_branch: main      # git branch to fork the run from; defaults to project default

params: [...]          # run parameters
defaults: {...}        # field defaults applied to every task
agents: [...]          # agent roster
tasks: [...]           # the work graph
for_each: {...}        # run-level fan-out
```

---

## `params`

Declare inputs the workflow accepts at run creation. Reference them in prompts as `{{param_name}}` or `{{name}}`.

```yaml
params:
  - name: dataset
    type: string
    required: true
    description: "Dataset name"

  - name: threshold
    type: int
    default: 80
    description: "Confidence threshold"
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Parameter name. Used as `{{name}}` in prompts. |
| `type` | yes | `string`, `int`, `float`, `bool`, `list<string>`, `list<record>` |
| `required` | no | If `true`, run creation fails without this param. Default: `false`. |
| `default` | no | Value used when param is absent. Mutually exclusive with `required: true`. |
| `description` | no | Shown in `enju_describe_template` output and `enju validate` errors. |

---

## `defaults`

Field defaults applied to every task that doesn't set them explicitly. Explicit task-level values always win.

```yaml
defaults:
  assign_to: tamer     # all unassigned tasks go to this citizen
```

Currently only `assign_to` is supported as a default.

---

## `agents`

The agent roster for this workflow. Each entry is a daemon that claims and executes tasks.

```yaml
agents:
  - name: developer-agent
    handler: claude
    model: claude-sonnet-4-6
    system_prompt: prompts/developer.md
    mcp_tools:
      allow: [Read, Edit, Write, Bash, Grep, Glob]
    args:
      - "-p"
      - "--model={{model}}"
      - "--append-system-prompt={{system_prompt}}"
      - "--allowedTools={{allowed_tools}}"
    replicas: 1
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Citizen username. ASCII letters, digits, dash, underscore; 1–39 chars. |
| `handler` | no | Binary to spawn. `claude`, `gemini`, or a path to any executable. Default: `claude`. |
| `model` | yes (LLM handlers) | Model identifier passed to the handler. |
| `system_prompt` | no | Repo-relative path to a `.md` file prepended to every task prompt. |
| `mcp_tools.allow` | no | Tool allowlist — only these tools are registered when the daemon spawns the LLM. Omit = all tools. Empty list = validation error. |
| `args` | yes (for `claude`) | Argv template. Placeholders: `{{model}}`, `{{system_prompt}}`, `{{allowed_tools}}`. |
| `replicas` | no | Expand to N identical agents (`name-1`, `name-2`, …). Max 32. |

---

## `tasks`

The task graph. Every task has at minimum an `id`, an `action`, and a `prompt`.

### Common fields

| Field | Required | Description |
|-------|----------|-------------|
| `id` | yes | Unique identifier within the run. Used in `depends_on`, `reviews`, `collects`, and `{{id.content}}` references. |
| `action` | yes | Task type: `answer`, `compute`, `review`, `vote`, `contribute`. |
| `prompt` | yes | Task instructions. Supports template references. |
| `assign_to` | no | Username or list of usernames who can claim this task. Open to any project member if omitted. |
| `depends_on` | no | List of upstream task IDs. Adds explicit dependency edges. Implicit edges are also inferred from `{{id.content}}` references in the prompt. |
| `reads` | no | Artifact paths this task reads. Registers as implicit dependency edges. |
| `writes` | no | Artifact paths this task produces. |
| `outputs` | no | Named typed outputs (for use with `for_each` dynamic lists). |
| `for_each` | no | Fan-out over a list. Produces one independent iteration per item. |
| `collects` | no | Fan-in: wait for all iterations of the named fan-out task before becoming READY. |

### `writes` entries

```yaml
writes:
  - results/summary.md          # simple path — tracked, commits to git
  - path: results/raw.bam
    track: false                # untracked — lands in .enju/bigfiles/<branch>/
    optional: true              # silent if missing at submit time
```

### `outputs` (named typed outputs)

```yaml
outputs:
  gene_list:
    description: "Genes found in the dataset"
    format: list<string>        # enables use as a for_each source
```

Reference downstream as `{{task_id.gene_list}}`.

---

## Action types

### `answer`

Open-ended generation or writing. Executed by an agent or human.

```yaml
- id: draft
  action: answer
  assign_to: developer-agent
  prompt: |
    Write a draft analysis of {{dataset}}.
  writes:
    - draft.md
```

### `compute`

Runs a script or container deterministically — no LLM required.

```yaml
- id: run_analysis
  action: compute
  script: scripts/analyze.sh        # relative to the workflow YAML's directory
  mode: sync                        # sync (default) or async
  container: python:3.12-slim       # optional Docker image
  depends_on: [review_plan]
  prompt: "Run analysis on {{dataset}}"
  writes:
    - results/summary.md
```

| Field | Default | Description |
|-------|---------|-------------|
| `script` | — | Path to script, relative to workflow YAML directory |
| `mode` | `sync` | `sync` = blocks until exit; `async` = detaches for long-running jobs |
| `container` | — | Docker image to run the script inside |

**Environment variables set for the script:**

| Variable | Value |
|----------|-------|
| `ENJU_TASK_ID` | Task identifier |
| `ENJU_PROJECT_DIR` | Root of the git repository |
| `ENJU_RUN_DIR` | `.enju/runs/<n>/` for this run |
| `ENJU_TEMPLATE_DIR` | Snapshot directory (workflow files at run-creation SHA) |
| `ENJU_PARAM_<NAME>` | Each workflow param and iteration variable |

A structured `$ENJU_RUN_DIR/context.json` with typed params and artifact declarations is also written.

### `review`

Quality gate. The reviewer reads upstream work and submits a verdict.

```yaml
- id: review_draft
  action: review
  reviews: draft                    # the task being reviewed; auto-adds dependency
  assign_to: reviewer-agent
  on_review_request_changes: spawn_remediation   # optional
  remediation_template: |
    Revise the draft based on: {{review_draft.content}}
    Original: {{draft.content}}
  prompt: |
    Review {{draft.content}}. Approve if clear and accurate.
```

**Verdicts:** `approve` → ACCEPTED; `request_changes` → task back to READY; `reject` → FAILED cascade; `comment` → non-blocking.

| Field | Description |
|-------|-------------|
| `reviews` | Target task ID. Automatically adds a dependency. |
| `on_review_request_changes` | `spawn_remediation` — create a new fix task on request_changes instead of returning the original to READY. |
| `remediation_template` | Prompt for the spawned fix task. Uses the same template syntax. |

### `vote`

Group decision with quorum and threshold rules.

```yaml
- id: pick_approach
  action: vote
  citizens: 5
  min_quorum: 3
  threshold: 3
  options:
    - id: approach_a
      label: "Use microservices"
    - id: approach_b
      label: "Keep monolith"
```

### `contribute`

Multiple citizens each submit work that is merged into a shared artifact.

```yaml
- id: parallel_sections
  action: contribute
  citizens: 4
  prompt: |
    Write your assigned section.
  writes:
    - sections/{{section}}.md
```

---

## Multi-citizen fields

Apply to `review`, `vote`, and `contribute`:

| Field | Default | Description |
|-------|---------|-------------|
| `citizens` | 1 | Number of concurrent claim slots |
| `min_quorum` | `citizens` | Minimum submissions before tally can resolve |
| `threshold` | varies | Resolution policy. Review: `any-reject-kills` \| `unanimous-approve` \| `majority-approve` \| `percent:N`. Vote: `plurality` \| `majority` \| `unanimous` \| `percent:N`. |
| `deadline` | none | Go duration string (`5m`, `2h`, `24h`). Clock starts on first claim. |
| `anonymize` | `false` | Show `citizen-N` placeholders instead of real usernames. |
| `visibility` | `open` | `open` or `blind`. Blind hides sibling ballots during COLLECTING; reveals on resolution. |

---

## `for_each`

Fan-out over a list of inputs. Each item produces an independent iteration with its own git branch and task state.

**Run-level (static):**

```yaml
for_each:
  topic:
    - "remote work"
    - "async communication"
```

**Task-level (dynamic — from an upstream output):**

```yaml
- id: analyze
  action: answer
  for_each:
    disease: "{{discover.diseases}}"   # materializes when discover is ACCEPTED
  prompt: |
    Analyze {{disease}}
```

**Fan-in with `collects`:**

```yaml
- id: synthesise
  action: answer
  collects: analyze        # waits for ALL analyze iterations
  prompt: |
    Synthesise: {{analyze.content}}
```

---

## Template references in prompts

| Reference | Resolves to |
|-----------|-------------|
| `{{param_name}}` | Run parameter value, substituted at run creation |
| `{{task_id.content}}` | The task's text output (stdout for compute, result for answer/review) |
| `{{task_id.field_name}}` | A named output field declared in the task's `outputs:` |
| `{{task_id.responses}}` | All submissions in a multi-citizen task, formatted |
| `{{task_id.winning_option}}` | Winning option ID from a vote task |
| `{{artifact:path}}` | Contents of a file at `path`, read from the producer's commit |

References to `{{task_id.*}}` create implicit dependency edges — the task waits for `task_id` to be ACCEPTED before becoming READY.

`{{artifact:path}}` reads from the artifact index, resolves to the file at the commit the producer wrote, and registers as an implicit `reads:` entry.

**Choosing between `.content` and `{{artifact:path}}`:**
- `.content` → "what did the task report?" — stdout for compute, result text for answer/review
- `{{artifact:path}}` → "what does this output file contain?" — use for compute tasks that write files

---

## Snapshot and git layout

When a run is created, the workflow YAML is snapshotted at the current committed state. Later edits to the workflow do not affect in-flight runs. Scripts in `compute` tasks resolve relative to the workflow YAML's directory inside the snapshot.

```
<project>/
├── enju.yaml                    ← committed workflow
├── scripts/analyze.sh           ← compute scripts
├── prompts/developer.md         ← agent system prompts
└── .enju/                       ← runtime (gitignored)
    └── runs/<n>/
        └── snapshot/            ← read-only copy of run base SHA
```
