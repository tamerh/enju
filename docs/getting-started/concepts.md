# Concepts

Enju organises work around five ideas. Understanding these before you write your first real workflow will save you a lot of confusion later.

---

## Citizens

A **citizen** is any participant in the system — human or agent. Every action (submitting a result, approving a review, claiming a task) is attributed to a citizen.

| Kind | Description |
|------|-------------|
| Human | Registered from your git identity on `enju start`. Has a username (`@alice`), display name, and bearer token stored in `~/.enju/credentials.json`. |
| Agent (bot) | An automated participant registered against the coordinator. The most common form is an LLM daemon (Claude, Gemini, etc.) that claims and executes tasks. But agents are not limited to LLMs — a `compute` task runs a script or container directly as part of the workflow, with no LLM involved. |

Citizens are global to a coordinator. On a local single-user setup there is typically one human citizen and one or more agent citizens. In a team setup, each person registers against the shared coordinator.

---

## Projects

A **project** is a git repository registered with the coordinator.

```sh
# creates the project in coord and links it to this repo
enju_create_project path="/home/alice/my-project"
```

Enju uses git as its storage and audit layer. Every result an agent submits, every file a compute task produces, and every human approval is a git commit — with full authorship attribution. You get a complete, tamper-evident history of who did what and when, for free, using tools you already know.

Enju does not manage your repository — it works inside it. All output (agent commits, run branches, artifacts) lands in your own `.git`. The coordinator tracks the project by ID and remembers its path on disk.

---

## Workflows

A **workflow** is a YAML file in your repository that describes a graph of tasks. It is the recipe; a run is one execution of it.

```yaml
name: My Workflow

agents:
  - name: writer-agent
    handler: claude
    model: claude-sonnet-4-6

tasks:
  - id: draft
    action: answer
    assign_to: writer-agent
    prompt: Write a one-paragraph summary of...

  - id: review
    action: review
    reviews: draft
    prompt: Approve if accurate; request changes otherwise.
```

Workflows support parameters, fan-out (`for_each`), compute scripts, multi-citizen voting, and more. See [Writing Workflows](../guides/writing-workflows.md).

---

## Runs

A **run** is one execution of a workflow, always tied to a git branch.

```
project/
  main branch          ← your work lives here
  run-1 branch         ← everything enju produces goes here
    .enju/runs/1/      ← snapshot of the workflow at run creation
    report.md          ← agent output, committed
```

When you create a run, the coordinator:

1. Parses and validates the workflow YAML
2. Forks a branch from your project's current HEAD
3. Materialises the task graph — each task starts in `PENDING` or `READY`

The run branch accumulates all commits produced during the run. When the run completes, Enju merges it back to the base branch.

---

## Tasks

A **task** is the unit of work. Every task has an action type, a prompt, and a state.

**Action types:**

| Action | Who does it | What it produces |
|--------|-------------|-----------------|
| `answer` | Agent (LLM) or human | Free-form text or files |
| `compute` | Script or container (no LLM required) | Files, structured output. Two modes: `sync` (blocks until done) or `async` (detaches — for long-running jobs like SLURM or multi-hour pipelines) |
| `review` | Human or agent | Approve / request_changes / reject |
| `vote` | Multiple citizens | Tallied verdict |
| `contribute` | Agent or human | Contribution merged into a shared artifact |

**Task states:**

```
PENDING → READY → CLAIMED → SUBMITTED → ACCEPTED   (terminal ✓)
                  CLAIMED → RUNNING   → ACCEPTED   (compute tasks)
                                      ↘ FAILED_RETRYABLE   (non-terminal, can retry)
                                      ↘ FAILED             (terminal ✗, cascades)
                                      ↘ SKIPPED            (terminal, vote path)
```

- **PENDING** — upstream dependencies not yet satisfied
- **READY** — all dependencies met; any eligible citizen can claim it
- **CLAIMED** — a citizen is working on it
- **RUNNING** — a compute script is executing. In `sync` mode this resolves within the same session; in `async` mode the script runs detached and the task stays RUNNING until any subsequent session reconciles its completion
- **SUBMITTED** — result is in, waiting for review tally or quorum
- **ACCEPTED** — result accepted; downstream tasks may become READY
- **FAILED** — terminal; caused by explicit fail, review rejection, or vote. Cascades to dependents as SKIPPED
- **FAILED_RETRYABLE** — non-terminal; a compute script exited non-zero. Operator can fix and retry without re-running upstream work
- **SKIPPED** — terminal but not a failure; downstream of a losing vote branch. Treated as done for run completion

---

## The DAG

Tasks within a run form a directed acyclic graph (DAG). Dependencies are declared explicitly with `depends_on`, or inferred automatically when one task references another's output in its prompt:

```yaml
tasks:
  - id: research
    action: answer
    ...

  - id: write_report
    action: answer
    prompt: |
      Using the findings from research:
      {{research.content}}
      Write a report...
    # depends_on: [research]  ← inferred automatically from the reference above
```

When `research` reaches DONE, `write_report` automatically becomes READY. This is how multi-step pipelines progress without any orchestration code.

`review` tasks are a special edge: a review that approves its target allows downstream tasks to proceed; a rejection can re-queue the target task for remediation.

---

## Putting it together

```
Coordinator
  └── Project (git repo)
        └── Run (branch)
              └── Tasks (DAG)
                    ├── task A  DONE
                    ├── task B  DONE  (depended on A)
                    └── task C  READY (waiting for you)
```

The coordinator tracks state. The git repository tracks everything produced. Citizens — human and agent — do the actual work by claiming tasks and submitting results.

---

## Next steps

- [How It Works](../how-it-works.md) — the components behind the scenes (coordinator, MCP server, agents, web UI)
- [Writing Workflows](../guides/writing-workflows.md) — the full workflow YAML reference with examples
