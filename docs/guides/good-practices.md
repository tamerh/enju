# Good Practices

Patterns that have proven useful across real workflows. Nothing here is enforced by the parser — these are guidelines to avoid common traps.

---

## Separate mechanical from judgment work

Use each task action for what it's designed for:

- `compute` — deterministic / mechanical work: concatenation, formatting, parsing, data transforms
- `answer` — judgment / synthesis: summaries, conclusions, decisions that need an LLM or human
- `review` — quality gates between the two

**Anti-pattern:** asking an LLM to do pure string concatenation via `answer`:

```yaml
- id: assemble
  action: answer
  prompt: |
    Concatenate sections 1–12 verbatim into a final page.
    {{section_1.content}}
    ...
    {{section_12.content}}
```

Three problems: overkill (a script does this deterministically), slow (an unnecessary LLM round-trip), and coupled failure modes (mechanical errors look like judgment errors — you can't tell which happened).

**Clean pattern:** split concerns so each task has one reason to fail:

```
12 parallel answer/compute tasks (one per section)
         ↓
aggregate        (compute)   ← deterministic concat, no LLM
         ↓
review_aggregate (review)    ← human verifies all sections present
         ↓
final_summary    (answer)    ← LLM synthesizes from all sections
         ↓
publish          (compute)   ← deterministic: assemble final page
         ↓
final_approval   (review)    ← human signs off
```

Each node does exactly one thing. LLM is only used when judgment is actually needed.

**Quick self-check for each task you write:**
1. Is this step mechanical or judgment? If both, split it.
2. Does this `answer` prompt actually need an LLM, or is it mostly `{{task.content}}` substitutions with thin framing? If mostly substitutions, rewrite as `compute`.
3. Is there a quality gate between a mechanical step and a judgment step? A `review` task here is cheap insurance against cascading bad data.

---

## Use `{{artifact:path}}` for compute outputs, not `.content`

A compute task's `{{task.content}}` is the script's **stdout** — typically a status line like `wrote 12345 rows`. It is not the file the script wrote.

```yaml
# ✗ Gets stdout, not the file:
prompt: "Summarize: {{run_analysis.content}}"

# ✓ Inlines the file at the producer's accepted commit:
prompt: "Summarize: {{artifact:results/summary.md}}"
```

`{{artifact:path}}` reads from the artifact index at the commit the task produced, and registers as an implicit `reads:` entry — no double declaration needed.

**Rule:** `.content` answers "what did the task report?"; `{{artifact:path}}` answers "what does this output file contain?"

---

## Use implicit dependency edges

Explicit `depends_on` is redundant when you already reference a task in the prompt. The parser infers the edge from `{{task.content}}` and `{{artifact:path}}` references. Keep the YAML DRY:

```yaml
# ✗ Redundant:
- id: write_report
  depends_on: [draft, review_draft]
  prompt: |
    Using the approved draft: {{draft.content}}
    Write a final report.

# ✓ Edge inferred from the reference:
- id: write_report
  prompt: |
    Using the approved draft: {{draft.content}}
    Write a final report.
```

Use explicit `depends_on` only when you have a dependency that doesn't appear in the prompt — for example, a compute task that must run before this one even though its output isn't directly referenced.

---

## Gate compute outputs before feeding them to LLMs

LLMs synthesize faithfully from whatever they're given. A compute step that produced garbled or truncated output will produce confidently wrong conclusions downstream.

Place a human (or agent) `review` gate between a compute step and the LLM task that consumes it:

```yaml
- id: run_analysis
  action: compute
  script: scripts/analyze.sh
  writes: [results/summary.md]

- id: review_analysis
  action: review
  reviews: run_analysis
  reads: [results/summary.md]
  prompt: |
    Review {{artifact:results/summary.md}}.
    Do the numbers look plausible? Are any columns missing?

- id: interpret
  action: answer
  depends_on: [review_analysis]
  prompt: |
    Interpret the validated results: {{artifact:results/summary.md}}
```

This catches mechanical failures at the compute step, not buried inside an LLM response.

---

## Design `assign_to` explicitly for human gates

Any task without `assign_to` is open — any project member (including agent daemons) can claim it. If a task is meant for a specific human, name them:

```yaml
- id: final_approval
  action: review
  reviews: generate_report
  assign_to: tamer          # only this person can claim it
```

For tasks that any human should be able to review but agents shouldn't touch, list the humans explicitly or use a role if your project has role assignments configured.

The `defaults: assign_to:` field sets a project-wide default for tasks that don't override it — useful to prevent agents from accidentally claiming human tasks in workflows that mix both.

---

## Use `for_each` for independent parallel work, not serial loops

`for_each` fans out into fully parallel independent iterations — each gets its own git branch, its own task state, its own claim. Use it when items can be processed simultaneously and their results later aggregated.

Don't use `for_each` to express a serial sequence. If task B must happen after task A and uses A's output, that's a dependency edge (`depends_on` or `{{a.content}}`), not a fan-out.

**Good use:** analyzing a list of genes or papers in parallel, then synthesizing:

```yaml
- id: analyze
  for_each:
    gene: "{{discover.genes}}"
  action: answer
  prompt: Analyze gene {{gene}}

- id: synthesize
  collects: analyze
  action: answer
  prompt: Synthesize across all analyses: {{analyze.content}}
```

**Avoid:** using `for_each` with a single item just to get a unique branch name. Use a named run branch via `--run-branch` instead.

---

## Keep system prompts short and task-focused

Agent system prompts (the `system_prompt:` field) set context that persists across every task the agent claims. Long system prompts that describe irrelevant background consume context window on every task invocation.

Effective system prompts:
- State the agent's role and the project's domain in 2–3 sentences
- Describe the expected output format for the most common task type
- Note any conventions specific to this project (file naming, citation style, etc.)

Task-specific instructions belong in the task `prompt`, not the system prompt.

---

## Check with `enju validate` before committing

Run `enju validate enju.yaml` before pushing a workflow. It catches:
- Undefined variable references
- Missing `depends_on` targets
- Cycle detection
- Duplicate review targets

With `--strict`, warnings also fail: unreachable review gates, `.content` references on compute tasks with declared `writes:`. Useful in CI:

```sh
enju validate workflows/*.yaml --strict || exit 1
```
