# Writing Workflows

A workflow is a YAML file in your git repository that describes a graph of tasks. This guide covers everything you need to write real workflows — from a minimal two-task pipeline to fan-out over dynamic lists with compute tasks and human gates.

---

## Minimal workflow

The smallest possible workflow: one task, one agent.

```yaml
name: My Workflow

agents:
  - name: writer
    handler: claude
    model: claude-sonnet-4-6
    args:
      - "-p"
      - "--model={{model}}"

tasks:
  - id: write_summary
    action: answer
    assign_to: writer
    prompt: |
      Write a three-sentence summary of the state of fusion energy research.
```

Run it:

```sh
enju go enju.yaml --auto-agents
```

---

## Top-level structure

```yaml
name: string           # human label (required)
description: string    # LLM-facing prose — helps agents understand the workflow's purpose
version: 1             # schema version, always 1 for now
base_branch: main      # git branch to fork from; defaults to project default

params: [...]          # run parameters, substituted into {{param.name}}
defaults: {...}        # field defaults applied to every task
agents: [...]          # agent roster
tasks: [...]           # the work
for_each: {...}        # run-level fan-out (see Fan-out)
publish: {...}         # what lands on the base branch at completion (see Publish)
```

---

## Parameters

Params let you write a single workflow that runs over different inputs.

```yaml
params:
  - name: dataset
    type: string
    required: true
    description: "Dataset name to analyse"

  - name: threshold
    type: int
    default: 80
    description: "Minimum confidence percentage"
```

Reference them in prompts as `{{param.dataset}}` or just `{{dataset}}`. At run creation you pass values:

```
enju_create_run path=enju.yaml params='{"dataset": "tp53", "threshold": 90}'
```

---

## Agents

The `agents:` section defines the automated participants for this workflow. Each agent is a daemon that claims and executes tasks.

```yaml
agents:
  - name: developer-agent
    handler: claude            # binary name resolved via $PATH
    model: claude-sonnet-4-6
    system_prompt: prompts/developer.md   # optional, repo-relative path
    mcp_tools:
      allow: [Read, Edit, Write, Bash]   # tool allowlist for this agent
    args:
      - "-p"
      - "--model={{model}}"
      - "--append-system-prompt={{system_prompt}}"
      - "--allowedTools={{allowed_tools}}"

  - name: reviewer-agent
    handler: claude
    model: claude-opus-4-7
    mcp_tools:
      allow: [Read, Grep, Glob]          # read-only
    args:
      - "-p"
      - "--model={{model}}"
      - "--allowedTools={{allowed_tools}}"
```

`handler:` is the binary the daemon spawns — `claude`, `gemini`, or any executable. Enju has no LLM-specific code; the CLI flags live in `args:`, not in Go.

---

## Tasks

Every task has at minimum an `id`, an `action`, and a `prompt`.

```yaml
tasks:
  - id: draft          # unique within the run
    action: answer     # what kind of work
    assign_to: writer  # which citizen can claim it
    prompt: |
      Write a draft...
```

### Action types

| Action | Who executes | When to use |
|--------|-------------|-------------|
| `answer` | Agent (LLM) or human | Open-ended generation, research, writing |
| `compute` | Script / container | Deterministic processing, data pipelines, analysis scripts |
| `review` | Human or agent | Quality gate — approve, request changes, or reject |
| `vote` | Multiple citizens | Group decision with quorum and threshold rules |
| `contribute` | Agent or human | Multiple submissions merged into a shared artifact |

### Task dependencies

Dependencies are declared in two ways:

**Explicit** — list upstream task IDs:
```yaml
- id: write_report
  action: answer
  depends_on: [draft, review_draft]
  prompt: Write a final report based on the approved draft.
```

**Implicit** — reference an upstream task's output in the prompt. The dependency edge is inferred automatically:
```yaml
- id: write_report
  action: answer
  prompt: |
    Using the approved draft:
    {{draft.content}}
    Write a final report.
    # depends_on: [draft] ← inferred from the {{draft.content}} reference
```

Use implicit references wherever possible — they keep the YAML DRY and make the data flow visible in the prompt itself.

---

## Review tasks

A `review` task creates a quality gate over another task. The reviewer reads the upstream result and submits a verdict.

```yaml
- id: review_draft
  action: review
  reviews: draft          # names the task being reviewed; auto-adds dependency
  assign_to: reviewer-agent
  prompt: |
    Review the draft in {{draft.content}}.
    Approve if the writing is clear and accurate.
    Use request_changes to ask for specific edits.
```

**Verdicts:**
- `approve` — task moves to ACCEPTED; downstream tasks unblock
- `request_changes` — task returns to READY for re-claim and revision
- `reject` — terminal failure; downstream tasks cascade to SKIPPED

**Remediation on rejection or request_changes:**

```yaml
- id: review_draft
  action: review
  reviews: draft
  on_review_request_changes: spawn_remediation
  remediation_template: |
    Revise the draft based on this feedback:
    {{review_draft.content}}
    Original draft: {{draft.content}}
```

With `spawn_remediation`, instead of returning the original task to READY, a new fix task is spawned with the reviewer's feedback pre-injected.

---

## Compute tasks

A `compute` task runs a script or container directly — no LLM required.

```yaml
- id: run_analysis
  action: compute
  script: scripts/analyze.sh          # path relative to enju.yaml's directory
  depends_on: [review_plan]
  prompt: "Run analysis on {{dataset}}"
  writes:
    - results/summary.md              # tracked — commits to git
    - path: results/raw.bam
      track: false                    # untracked — lands in .enju/bigfiles/
```

The script runs with these environment variables set:

```
ENJU_TASK_ID        task identifier
ENJU_PROJECT_DIR    root of the git repository
ENJU_RUN_DIR        .enju/runs/<n>/ for this run
ENJU_TEMPLATE_DIR   snapshot directory (workflow files at run-creation SHA)
ENJU_PARAM_<NAME>   each workflow param as an env var
```

**Sync vs async:**

```yaml
- id: quick_transform
  action: compute
  mode: sync              # default — blocks until script exits
  script: scripts/transform.sh

- id: long_pipeline
  action: compute
  mode: async             # detaches — task stays RUNNING across sessions
  script: scripts/pipeline.sh
```

Use `async` for jobs that outlast your MCP session — SLURM submissions, overnight pipelines, multi-hour analyses. The script commits and pushes its output on its own schedule; the next time any fat-client session touches the project, completion is reconciled automatically.

**Auto-retry (`retries:`):**

```yaml
- id: section_5
  action: compute
  script: scripts/run.sh
  retries: 2              # auto re-run up to 2 extra times before giving up (default 0)
```

When a compute script fails transiently (a flaky API, `error_max_turns`, a busy box), the coordinator automatically re-runs it — using the pinned snapshot unchanged — up to `retries` extra times before parking it `failed_retryable`. `retries: 2` means up to 3 attempts total. Default `0` (no auto-retry). The budget is enforced coordinator-side off the per-attempt count, so it holds even if the driving process dies. Compute-only; citizen tasks recover through review/voting, not this. Each auto-retry emits an `auto_retry` event. Once the budget is exhausted the task parks `failed_retryable` — recover it manually with `enju retry <id>`.

**Container:**

```yaml
- id: run_in_docker
  action: compute
  script: scripts/analysis.sh
  container: python:3.12-slim         # Docker image
```

---

## Artifacts

Tasks declare what files they read and write so Enju can manage git commits and pass file content between tasks.

```yaml
- id: generate_report
  action: answer
  writes:
    - report.md                       # simple path — tracked, committed to git
    - path: data/large_output.csv
      track: false                    # not committed — lands in .enju/bigfiles/
      optional: true                  # silent if file is missing at submit time

- id: review_report
  action: review
  reviews: generate_report
  reads:
    - report.md
  prompt: |
    Review the report: {{artifact:report.md}}
```

Use `{{artifact:path}}` in prompts to inline file content from a previously committed artifact. Use `{{task.content}}` to reference a task's text output (the agent's reply or script stdout).

---

## Named outputs

Outputs give a task's result a typed schema that downstream tasks and `for_each` can use.

```yaml
- id: discover_genes
  action: answer
  outputs:
    gene_list:
      description: "Genes identified in the dataset"
      format: list<string>     # enables use as a for_each source
  prompt: |
    Identify all genes mentioned in the dataset. Return them as a
    comma-separated list under the key gene_list.
```

Reference in downstream prompts as `{{discover_genes.gene_list}}`.

---

## Publish

The `publish:` block controls **what happens to the base (deliverable) branch when the run completes** — specifically, whether the run's declared outputs get written onto it, and whether that's pushed to the remote.

```yaml
publish:
  mode: local        # none | local | push   (default: local)
  remote: origin     # remote to push to when mode: push (default: origin)
```

| mode | effect |
|------|--------|
| `none` | Don't touch the base branch. The run branch still holds all the outputs (and the full audit trail). |
| `local` | Write the run's declared outputs onto the base branch as a single curated commit, locally. Push nothing. **(default)** |
| `push` | Same publish, then push `{ base branch, run branch }` to the remote. |

Two things worth knowing:

- **It's a curated copy, not a branch merge.** `local`/`push` lay *only* the declared output files (from `outputs:` / `collects:` / declared write artifacts) onto the base branch — never a whole-branch merge of the run branch. That keeps the deliverable branch clean: no `.enju/` provenance, no iteration history. The run branch remains the full record.
- **It does *not* govern pushes during the run.** Per-task run-branch and topic-branch pushes happen throughout the run regardless of this setting — that's how other citizens (and other machines) see and review work in progress. `publish:` is only about the final deliverable. So `publish: none` means "don't publish the deliverable to base," **not** "never push anything."

Override per run from the CLI with `enju go --publish none|local|push`, or over MCP with `enju_create_run`'s `sync_mode_override`.

---

## Fan-out with `for_each`

`for_each` fans a task (or entire run) over a list of inputs. Each item gets its own independent iteration — its own state, its own git branch, its own claim — so they run in parallel.

**Static list:**

```yaml
for_each:
  topic:
    - "remote work productivity"
    - "async communication patterns"
    - "cross-timezone team rituals"

tasks:
  - id: analyze
    action: answer
    prompt: |
      Write a one-paragraph analysis of: "{{topic}}"
```

This creates three independent `analyze` tasks, one per topic.

**Dynamic list — from an upstream output:**

```yaml
tasks:
  - id: discover
    action: answer
    outputs:
      diseases:
        format: list<string>
    prompt: List all diseases mentioned in the dataset.

  - id: analyze
    action: answer
    for_each:
      disease: "{{discover.diseases}}"   # materialises when discover is accepted
    prompt: |
      Analyse the literature on: {{disease}}
```

The `analyze` iterations are not created until `discover` is accepted — the list is dynamic.

**Fan-in with `collects`:**

A singular downstream task can wait for all iterations of an upstream fan-out using `collects`:

```yaml
  - id: synthesise
    action: answer
    collects: analyze           # waits for ALL analyze iterations
    prompt: |
      Synthesise across these independent analyses:
      {{analyze.content}}       # joined content of all iterations
```

Without `collects`, a task under the same `for_each` would itself be fanned out. With `collects`, it stays singular and waits for all upstream instances before becoming READY.

---

## Multi-citizen tasks

Tasks can require multiple citizens — useful for blind review, group voting, or peer review.

```yaml
- id: peer_review
  action: review
  reviews: draft
  citizens: 3           # requires 3 independent reviewers
  min_quorum: 2         # at least 2 must submit
  threshold: 2          # at least 2 must approve to pass
  anonymize: true       # reviewers can't see each other's submissions
```

**Vote tasks** let citizens choose between named options:

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

---

## Defaults

Set field defaults once at the top level to avoid repetition:

```yaml
defaults:
  assign_to: tamer      # every task without an explicit assign_to goes to tamer
```

Explicit task-level values always win over defaults.

---

## Workflow layout

Enju imposes no directory structure. Put `enju.yaml` wherever makes sense:

```
my-project/
  enju.yaml             # root-level, simple projects
  workflows/
    analyse/enju.yaml   # nested, multi-workflow projects
  scripts/
    analyze.sh          # compute scripts, repo-relative paths
  prompts/
    developer.md        # optional agent system prompts
```

The workflow is snapshotted at run-creation time — edits to `enju.yaml` after a run starts do not affect that run. Scripts resolve relative to the workflow YAML's directory inside the snapshot.

---

## Next steps

- [Agents](agents.md) — handler types, daemon lifecycle, tool allowlists in depth
- [Credentials & Identity](credentials.md) — assigning tasks to specific citizens, team setup
- [MCP Tools Reference](../reference/mcp-tools.md) — full list of MCP tools for run management
