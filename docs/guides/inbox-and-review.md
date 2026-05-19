# Inbox & Review

When an agent finishes a task that needs human approval, it lands in your inbox. This guide covers how to see what's waiting on you and how to submit a verdict.

---

## Concepts

**Inbox** — the list of ready tasks assigned to you. Each entry shows the upstream work (the agent's output, the draft it produced) so you can read and judge without hunting for it separately.

**Review** — the act of submitting a verdict on a `review` task. Your verdict determines what happens next: the work ships, the agent revises, or the branch fails.

---

## Three ways to review

| Surface | Best for |
|---------|----------|
| **Web UI** — `http://localhost:8333/ui` | Browsing, quick reads, submitting verdicts from a browser |
| **LLM via MCP** — ask your LLM "what's in my inbox?" | Reviewing while already in an LLM session; LLM reads and summarizes the work for you |
| **CLI** — `enju inbox` / `enju review` | Terminal-only; quick verdict without opening a browser |

All three surfaces show the same content and submit to the same coordinator.

---

## Web UI

Open `http://localhost:8333/ui` and click **Inbox** in the nav. You'll see ready tasks assigned to you, with the upstream submissions inlined.

To review a task:
1. Click into the task.
2. Read the upstream work shown on the task page.
3. Use the **Submit verdict** form: pick a decision, write your notes, and submit.

The web UI also shows run status in real time — you can monitor an entire run from the run page and jump to any task that needs attention.

---

## LLM via MCP

If you're already in a Claude, Gemini, or Codex session connected to Enju, ask your LLM:

```
What's in my inbox for project 3?
```

The LLM calls `enju_inbox` and shows you the ready tasks with their upstream content inlined. You can then instruct it to submit a verdict:

```
Approve that. Looks good.
```

or:

```
Request changes — ask it to add a confidence interval to the results section.
```

The LLM calls `enju_review` with the task ID, decision, and your feedback. The verdict is committed to git as a co-authored commit (you as author, model as `Co-Authored-By`).

**Monitoring a run in progress:**

```
What's the current status of run 2 in project 3?
```

The LLM calls `enju_run_status` and gives you a summary: which tasks are done, which are running, which are waiting on you.

For an event-level view:

```
What's been happening in the last hour?
```

→ `enju_recent_events with for_me=true`

---

## CLI

**See what's waiting on you:**

```sh
enju inbox 3        # project_id = 3
```

Output:
```
Inbox: 1 task(s) waiting on you.

[3:2:review_abstract] review

  Upstream [3:2:write_abstract] answer (commit a1b2c3d):
  The TP53 gene encodes a tumor suppressor protein that is mutated
  in approximately 50% of human cancers...
```

The upstream submission is read directly from git — no coordinator round-trip at render time.

**Submit a verdict:**

```sh
enju review 3:2:review_abstract
```

This opens `$EDITOR` with a template. Write your review prose, save and exit, then choose a decision when prompted:

```
Decision (approve/request_changes/reject/comment): request_changes
Submitted request_changes on 3:2:review_abstract.
```

Non-interactive form (for scripted use):

```sh
enju review 3:2:review_abstract -decision approve -content "Clear and accurate. Ship it."
```

> **Note:** the CLI review path requires the task to already be claimed. Claim via the web UI or MCP first if you haven't done so.

---

## Verdicts

| Decision | Effect |
|----------|--------|
| `approve` | Task moves to ACCEPTED; downstream tasks unblock and become READY |
| `request_changes` | Task returns to READY for revision; the agent reclaims it and resubmits |
| `reject` | Task moves to FAILED (terminal); all downstream tasks cascade to SKIPPED |
| `comment` | Non-blocking; the task state is unchanged; your note is logged |

**`request_changes` with remediation:** if the workflow declares `on_review_request_changes: spawn_remediation`, a new fix task is created with your feedback pre-injected into the prompt — the agent doesn't see the raw original, it sees the reviewer's notes. This is the default pattern for multi-round revision loops.

**Review fallback for agent reviewers:** when an agent is the reviewer, the daemon extracts the verdict from the LLM's text output. If no verdict is found, it defaults to `request_changes` — the safe non-destructive choice. See [Agents — Review and vote response conventions](agents.md#review-and-vote-response-conventions).

---

## Multi-citizen review

When a task has `citizens: N`, multiple reviewers must weigh in before a verdict is reached. Each of you claims an independent slot. The tally resolves once quorum is met and the threshold policy evaluates:

```yaml
- id: peer_review
  action: review
  reviews: draft
  citizens: 3
  min_quorum: 2        # at least 2 must submit
  threshold: 2         # at least 2 must approve
```

With `anonymize: true`, reviewers see `citizen-1`, `citizen-2` labels instead of usernames — useful for blind review where you shouldn't be influenced by who else reviewed before you.

---

## Checking run progress

Beyond the inbox, you can get a full picture of a run's state:

**Web UI:** open the run page — task graph with states, live event feed.

**MCP:**
```
enju_run_status run_id=2 project_id=3
enju_recent_events project_id=3 for_me=true
```

**CLI:**
```sh
enju go enju.yaml --status        # (if available; check enju go --help)
```

When a run stalls — no tasks are moving — the most common cause is a task waiting for a human reviewer. Check the inbox first.

---

## Next steps

- [Writing Workflows](writing-workflows.md) — `review` tasks, `on_review_request_changes`, multi-citizen gates
- [Agents](agents.md) — agent review behavior, DECISION: marker convention
- [Credentials & Identity](credentials.md) — `assign_to` targets specific reviewers
