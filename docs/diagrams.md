# Diagrams

## Iteration 1 — Git and Coordinator both on the right

```
  Outside interfaces      enju fat client  ·  service layer         State & persistence
  ──────────────────      ─────────────────────────────────         ───────────────────

  ┌──────────────┐        ┌───────────────────────────────────────┐
  │  LLM client  │        │                                       │  ┌──────────────────────┐
  │  (Claude,    │◀─MCP──▶│  enju mcp          ┐                 │  │      Coordinator      │
  │   Gemini,    │        │  (MCP server)      │                 │  │                      │
  │   Codex ...) │        │                    │   Service       │  │  State engine         │
  └──────────────┘        │  enju go           │   layer     ────┼─HTTP──▶(atomic ApplyPlan)│
                          │  enju inbox     ───┼─  (fat client   │  │                      │
  ┌──────────────┐        │  enju review       │   library)      │  │  ┌──────┐ ┌────────┐ │
  │   Terminal   │──runs──▶  ...               │                 │  │  │State │ │Events  │ │
  └──────────────┘        │  (CLI tools)       │                 │  │  │ DB   │ │  DB    │ │
                          │                    │                 │  │  └──────┘ └────────┘ │
  ┌──────────────┐        │  enju ui        ───┘                 │  └──────────────────────┘
  │   Browser    │──HTTP──▶  (web server)                        │
  └──────────────┘        │                                       │  ┌──────────────────────┐
                          │  ─────────────────────────────────── │  │       Git repo        │
                          │                                       │  │                      │
                          │  Agent daemons  (enju agent run ×N)  ├──rw──▶  run branches    │
                          │  forked by supervisor                 │  │  commits             │
                          │  use service layer to claim,          │  │  artifacts           │
                          │  execute, and submit                  │  └──────────────────────┘
                          │                                       │
                          │  ┌────────────┐ ┌─────────┐ ┌──────┐ │
                          │  │ LLM handler│ │ compute │ │ cont.│ │
                          │  └────────────┘ └─────────┘ └──────┘ │
                          └───────────────────────────────────────┘
```

---

## Iteration 2 — service layer central, agent daemons beside it

```
  ┌─────────────┐      ┌─────────────────┐      ┌─────────────┐
  │  LLM client │      │    Terminal     │      │   Browser   │
  │  (Claude,   │      │                 │      │             │
  │   Gemini…)  │      │                 │      │             │
  └──────┬──────┘      └────────┬────────┘      └──────┬──────┘
    MCP (stdio)            runs CLI              HTTP request
         │                      │                      │
         ▼                      ▼                      ▼
  ┌───────────────────────────────────────────────────────────────────────┐
  │                  enju fat client  ·  service layer                    │
  │                                                                       │
  │  ┌───────────────┐  ┌──────────────────────┐  ┌──────────────────┐   │
  │  │   enju mcp    │  │  enju go             │  │    enju ui       │   │
  │  │  (MCP server) │  │  enju inbox          │  │  (web server)    │   │
  │  │               │  │  enju review  ...    │  │                  │   │
  │  └───────┬───────┘  └──────────┬───────────┘  └────────┬─────────┘   │
  │          └─────────────────────┼──────────────────────── ┘            │
  │                                │                                      │
  │          ┌─────────────────────▼────────────────────┐  ┌───────────────────────┐  │
  │          │                                          │  │    Agent daemons      │  │
  │          │             Service layer                │  │   (enju agent run ×N) │  │
  │          │          (fat client library)            │◀─▶                       │  │
  │          │                                          │  │  forked by supervisor │  │
  │          │  · coordinator HTTP client               │  │  use service layer    │  │
  │          │  · git layer (enjugit)                   │  │                       │  │
  │          │                                          │  │  ┌──────────────────┐ │  │
  │          └──────────────────┬───────────────────────┘  │  │  LLM handler     │ │  │
  │                             │                          │  ├──────────────────┤ │  │
  └─────────────────────────────┼──────────────────────────│  │  compute script  │ │  │
                                │                          │  ├──────────────────┤ │  │
               ┌────────────────┘                         │  │  container       │ │  │
               │ HTTP                  reads/writes        │  └──────────────────┘ │  │
               │                            │             └───────────────────────┘  │
               ▼                            ▼
        ────────────────────────  State & persistence  ────────────────────────
  ┌────────────────────────────┐  ┌────────────────────────────────┐
  │        Coordinator         │  │           Git repo             │
  │                            │  │                                │
  │  ┌──────────────────────┐  │  │  run branches                  │
  │  │  State engine        │  │  │  commits  (with attribution)   │
  │  │  (atomic ApplyPlan)  │  │  │  artifacts                     │
  │  └──────────────────────┘  │  └────────────────────────────────┘
  │  ┌──────────┐ ┌──────────┐ │
  │  │ State DB │ │Events DB │ │
  │  └──────────┘ └──────────┘ │
  └────────────────────────────┘
```

---

## Sequence: Claim task

```
  ┌──────────┐       ┌──────────────┐       ┌─────────────┐       ┌─────────┐
  │  Caller  │       │  Fat client  │       │ Coordinator │       │   Git   │
  │ (LLM/CLI)│       │ service layer│       │             │       │         │
  └────┬─────┘       └──────┬───────┘       └──────┬──────┘       └────┬────┘
       │                    │                      │                    │
       │  claim task        │                      │                    │
       │───────────────────▶│                      │                    │
       │                    │  POST /tasks/claim   │                    │
       │                    │─────────────────────▶│                    │
       │                    │                      │ ApplyPlan          │
       │                    │                      │ (atomic tx)        │
       │                    │                      │ ─ record claim     │
       │                    │                      │ ─ emit event       │
       │                    │◀─ task details ──────│                    │
       │                    │                      │                    │
       │                    │  checkout run branch │                    │
       │                    │───────────────────────────────────────────▶
       │                    │  inject upstream outputs                  │
       │                    │◀──────────────────────────────────────────
       │◀─ task + prompt ───│                      │                    │
       │                    │                      │                    │
```

---

## Sequence: Submit result

```
  ┌──────────┐       ┌──────────────┐       ┌─────────────┐       ┌─────────┐
  │  Caller  │       │  Fat client  │       │ Coordinator │       │   Git   │
  │ (LLM/CLI)│       │ service layer│       │             │       │         │
  └────┬─────┘       └──────┬───────┘       └──────┬──────┘       └────┬────┘
       │                    │                      │                    │
       │  submit result     │                      │                    │
       │───────────────────▶│                      │                    │
       │                    │  git commit result   │                    │
       │                    │  (citizen author +   │                    │
       │                    │   attribution trailer│                    │
       │                    │   if configured)     │                    │
       │                    │───────────────────────────────────────────▶
       │                    │◀──────────────────────────────────────────
       │                    │  POST /tasks/submit  │                    │
       │                    │─────────────────────▶│                    │
       │                    │                      │ ApplyPlan          │
       │                    │                      │ (atomic tx)        │
       │                    │                      │ ─ advance state    │
       │                    │                      │ ─ unblock deps     │
       │                    │                      │ ─ emit event       │
       │                    │◀─ ok ────────────────│                    │
       │◀─ ok ──────────────│                      │                    │
       │                    │                      │                    │
```
