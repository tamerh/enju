# Enju (槐)

**Enju is a DAG workflow system where the unit of work is a task** — something a human, an LLM agent, or a script can answer, review, vote on, or compute. Like Snakemake or Nextflow you get reproducible, declarative pipelines; unlike them, the same DAG carries human judgement, autonomous AI agents, and computational steps as peers.

The graph is **live**: tasks can spawn further tasks inside a running run. A `request_changes` review, for example, spawns a revision task with the reviewer's feedback pre-injected — so a workflow adapts as work proceeds, bounded by a per-run cycle budget. Every state change emits an event, so humans get notified when a task needs their judgement and agents see what's ready to claim.

The coordinator is **content-neutral** — it only manages task state. Execution is distributed: humans handle review gates, scripts run in containers (Docker, Apptainer), and LLM agents — autonomous, each with its own model — claim the tasks they're assigned. Compute, tokens, and attention come from whoever joins the run.

Every contribution lands as a git commit, so **attribution**, **audit**, and **authentication** all come from git itself — no separate identity or audit system to wire up. Enju ships as a single binary that speaks MCP, a plain CLI, and a web interface.




```
╔═════════════════════ COORDINATOR · DAG state engine ═════════════════════╗
║                                                                          ║
║                ✓ ──→ ✓                                                   ║
║                       ╲                                                  ║
║                        ◑ ──→ ◇  ↺                                        ║
║                       ╱        ╲                                         ║
║                ✓ ──→ ◐          ✓ ──→ ○                                  ║
║                       ╲                                                  ║
║                        · ──→ ·                                           ║
║                                                                          ║
║   · pending ─→ ○ ready ─→ ◐ claimed ─→ ◑ running ─→ ◇ review ─→ ✓ done   ║
║                 ▲                                         │              ║
║                 ╰─────────────────── ↺ ──────────────────╯               ║
║                                                                          ║
║         ↻ retry    ✗ failed    ⊘ skipped    ‖ parked                     ║
║                                                                          ║
║                       ╭── state DB ──╮                                   ║
║                       ╰──────────────╯                                   ║
║                                                                          ║
╚══════════════════════════════════════════════════════════════════════════╝
             ▲▼                      ▲▼                      ▲▼
    ┌────────┴────────┐     ┌────────┴────────┐     ┌────────┴────────┐
    │    ALICE 👤     │     │     BOB 👤      │     │   AGENT 🤖 ×N   │   …
    │                 │     │                 │     │                 │
    │  ⚙ compute      │     │  ⚙ compute      │     │  ⚙ compute      │
    │  ✦ llm          │     │  ✦ llm          │     │  ✦ llm          │
    │  ◇ review       │     │  ◇ review       │     │  ◇ review       │
    │  ⊙ vote         │     │  ⊙ vote         │     │  ⊙ vote         │
    │                 │     │                 │     │                 │
    │   ┌─────────┐   │     │   ┌─────────┐   │     │   ┌─────────┐   │
    │   │   git   │   │     │   │   git   │   │     │   │   git   │   │
    │   └────┬────┘   │     │   └────┬────┘   │     │   └────┬────┘   │
    └────────┼────────┘     └────────┼────────┘     └────────┼────────┘
             ▲▼                      ▲▼                      ▲▼
╔══════════════════════════ REMOTE GIT · content ══════════════════════════╗
║                                                                          ║
║   workflow-2/<task>                            ●   ●                     ║
║                                                 ╲ ╱                      ║
║   workflow-2                                    ●─●─●─●                  ║
║                                                ╱       ╲                 ║
║   main             ●───●───●───●───●───●───●─●─────────●─●               ║
║                     ╲                 ╱                                  ║
║   workflow-1         ●───●───●───●───●                                   ║
║                       ╲       ╲                                          ║
║   workflow-1/<task>    ●       ●                                         ║
║                                                                          ║
╚══════════════════════════════════════════════════════════════════════════╝
```
<!-- vision-led README — iterating on the opening paragraph; v1 preserved in README.v1.md -->
