# Quickstart

Run your first Enju workflow in about 10 minutes.

**Prerequisites:** Enju installed, Git 2.38+, and your git identity configured:

```sh
git config --global user.name  "Alice"
git config --global user.email "alice@example.com"
```

Enju uses these to register your identity automatically on first start.

---

## 1. Start Enju

```sh
enju start
```

On first run this registers you from your git config, then starts the coordinator and web UI in the background:

```
Coordinator started  PID 12345   http://localhost:8333
Registered as @alice (Alice)
Web UI started       PID 12346   http://localhost:8484
```

Open **http://localhost:8484** — this is your dashboard for monitoring runs, tracking issues, and reviewing tasks across all projects. Keep it open as you work through the next steps.

Run `enju stop` to shut everything down when you're done.

---

## 2. Connect your LLM

Enju exposes an MCP server that any MCP-compatible LLM can drive — Claude, Gemini, Codex, and others. Add it to your LLM client's MCP config:

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

Restart your LLM client after saving the config. It can now create projects, launch runs, and interact with your inbox — all through natural language.

> Once connected you can ask your LLM to build and run the example workflow in the next steps for you. Or follow the CLI path below if you prefer hands-on.

---

## 3. Create a project

Create a git repository and add a workflow:

```sh
mkdir my-first-project && cd my-first-project
git init && git commit --allow-empty -m "init"
```

Create `enju.yaml`:

```yaml
name: My First Workflow

agents:
  - name: writer-agent
    handler: claude
    model: claude-sonnet-4-6
    args:
      - "-p"
      - "--model={{model}}"

tasks:
  - id: write_report
    action: answer
    assign_to: writer-agent
    writes:
      - report.md
    prompt: |
      Write a short (3–5 sentence) plain-English report on the current
      state of solar energy adoption. Write the report to report.md.

  - id: human_review
    action: review
    reviews: write_report
    prompt: |
      Review the report in report.md. Approve if it is accurate and
      well-written; use request_changes to ask for edits.
```

Commit it:

```sh
git add enju.yaml && git commit -m "Add workflow"
```

---

## 4. Run the workflow

```sh
enju go enju.yaml --auto-agents
```

`--auto-agents` starts the `writer-agent` daemon automatically. You will see per-task progress in the terminal. The `write_report` task is claimed and completed by the agent. The `human_review` task then blocks, waiting for you.

---

## 5. Review the report

The `human_review` task is now in your inbox. Two ways to review it:

**Web UI** — open http://localhost:8484, navigate to the pending review, and submit your verdict there.

**Via your LLM** — with the MCP server connected from step 2, ask it to check your inbox:

```
enju_inbox
```

The pending review appears with the agent's report inlined. Submit your verdict:

```
enju_review task_id=<id> verdict=approve
```

Once approved the workflow completes.

---

## 6. See the results

```sh
git log run-1 --oneline
git show run-1:report.md
```

The agent's report was committed to the run branch. Everything Enju produced lives in your own git repository.

---

## What just happened

```
enju start        → coordinator + UI in background, identity registered
enju go           → project registered, run created, tasks executed
  write_report    → agent claimed it, ran the LLM, committed report.md
  human_review    → you approved via UI or MCP inbox
run-1 branch      → all commits, merged back to main
```

---

## Next steps

- [Concepts](concepts.md) — the Project → Run → Task model, citizens, and the DAG.
- [Credentials & Identity](../guides/credentials.md) — MCP config, multi-user setup, teams.
- [Writing Workflows](../guides/writing-workflows.md) — compute tasks, `for_each` fan-out, multi-citizen voting.
- [Agents](../guides/agents.md) — handler types, tool allowlists, multi-bot fleets.
