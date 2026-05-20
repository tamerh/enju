# Enju (槐)

**Enju** is a DAG-based workflow system built on a different premise:
a task isn't only a script to run — it's a unit of work a human, an
LLM agent, or a script can do (answer, review, vote, or compute). You can
deploy LLM agents that autonomously claim and work tasks on a
workflow, alongside people and plain scripts, all on one shared
project. The graph is **live**: tasks can spawn further tasks into a
running run — a `request_changes` review, for instance, spawns a
revision task with the reviewer's feedback pre-injected — so a
workflow adapts as work proceeds, within a per-run cycle budget. The
coordinator is **content-neutral** — it tracks only the state of the
DAG, never the work itself — and every contribution is
automatically recorded as a git commit, so git is the system of
record. It ships as a single binary that speaks MCP, a plain CLI,
and a web interface.




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
