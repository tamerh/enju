# Spec: CLI resume / retry / drive

Status: Layer A SHIPPED (A.1 resume, A.2 retry, A.3 keep-going); Layer B
(drive) + Layer C (retries: auto-retry) pending.
Scope: fat-client CLI only. No coordinator changes. No new engine
capability — every behavior below already exists and is exercised
over MCP; this surfaces it as `enju` subcommands and adds one loop.

## 1. Problem

A workflow run advances by two distinct actions:

- **start** a ready task (claim + run its script) — only `ExecuteRun` does this.
- **finish** a task (turn its result into coordinator state) — for sync
  this happens inline; for async the *reaper* (`ReapWrapperFailuresWF`)
  picks up the `.wrap-result.json` a detached subprocess leaves behind.

Sync runs self-drive: each task finishes inline, so `ExecuteRun`'s loop
fetches the next ready task and keeps going until the DAG is drained.

Two gaps fall out of this:

1. **No CLI resume.** `enju go` *always creates a new run* and has no verb
   to drain an existing one. After any stop (failure, async launch,
   Ctrl-C) the only CLI move is another `enju go`, which forks a fresh
   run and redoes completed work. The resume primitive exists over MCP
   (`enju_execute_run` on a `(project, run)` pair) — just not on the CLI.

2. **No CLI retry.** A failed task parks `failed_retryable` and is
   recoverable via MCP `enju_retry_task`, but there's no CLI equivalent.

3. **`enju go` halts the whole batch on the first compute failure**
   (`stop_reason=compute_failed`, exit 1). In a fan-out (e.g. 100 genes),
   one transient failure abandons the other 99.

4. **Async does not self-drive headless.** `ExecuteRun` starts one async
   task (serial) or one wave (`--parallel N`) and returns. The reaper
   *finishes* tasks but never *starts* new ones, and it only runs
   autonomously inside an MCP session's 20s ticker. With no MCP session,
   a multi-stage async pipeline stalls after the first wave.

5. **No task-level auto-retry.** A failed compute task parks
   `failed_retryable` (non-terminal, recoverable) and waits for a
   *manual* retry. There is no Snakemake-style `retries: N` directive
   that auto-re-runs a transiently-failed task before giving up — even
   though the pieces exist (non-terminal failed state, a `from=snapshot`
   "re-run unchanged" mode for exactly this case, and a per-claim
   attempt count via `iter_seq`).

Repro for 1–3 and 5: a synchronous-compute fan-out, one section hits a
transient `error_max_turns` → batch dies → recovery is MCP-only, and a
`retries: 2` would have re-run it silently.

## 2. Building blocks reused (no new engine code)

- `service.FatClient.ExecuteRun(ctx, ExecuteRunParams{ProjectID, RunSeq, MaxTasks, Parallel})`
  — the resume primitive. Drains ready compute on an existing run.
- `service.FatClient.RetryComputeTask(ctx, taskID, from)` + the coord
  `POST /tasks/:id/retry {from}` — the retry primitive (`from` ∈
  `head` | `snapshot`; `head` re-materializes from the run-branch tip,
  `snapshot` re-runs the pinned snapshot script unchanged).
- `service.FatClient.ReapWrapperFailuresWF` / `PullBranchWithReconcileWF`
  — the reaper (finishes async tasks; advances the run branch).
- `cmd/enju/go.go:driveAutoBotsRun` — the existing loop template
  (ExecuteRun + terminal poll on an interval). The drive loop generalizes
  this, decoupled from the bot supervisor.
- `cmd/enju/project.go:resolveActiveProject` — project resolution
  (`--project id`, else the project owning cwd).

## 3. Layers

- **Layer A** (`resume`, `retry`, `--keep-going`): thin CLI mirrors of
  existing MCP capability. Solves the sync fan-out repro on its own.
- **Layer B** (`drive`): Layer A's resume wrapped in a start→wait→reap
  loop so headless async self-completes. Built on top of A.
- **Layer C** (`retries:` auto-retry): a per-task YAML directive,
  enforced **coordinator-side**, that auto-re-runs a transiently-failed
  task up to N times before parking `failed_retryable`. Independent of A
  and B — it's a schema + coordinator change, not CLI surfacing — but it
  composes with both (§7).

Layer A ships first and stands alone; B depends on A's resume path; C is
orthogonal (different subsystem) and can land in any order.

## 4. Commands

Run-scoped commands resolve the project like `enju project`
(`--project id`, else cwd's project) and take the run's **per-project
seq** (the `#7` the user sees), matching every coord run endpoint.

### 4.1 `enju resume <seq>` — Layer A

Drain ready compute on an existing run. Direct mirror of
`enju_execute_run`.

```
enju resume <seq> [--parallel N] [--max-tasks N] [--keep-going|--fail-fast]
                  [--project id] [--json] [--coordinator url]
```

Behavior: resolve `(project, seq)` → `ExecuteRun{ProjectID, RunSeq,
MaxTasks, Parallel}` → render the same per-task lines + stop summary as
`enju go`. Does **not** create a run, materialize a snapshot, or touch
the project default branch. A nonexistent seq is the coord's clear "run
not found" (not "idle run").

Flags mirror `enju go` where they overlap (`--parallel` clamped via the
shared `service.MaxParallel`; `--max-tasks`; `--json`). `--keep-going` /
`--fail-fast` per §5. Default `--parallel 1`.

Exit codes: identical to `enju go`'s execute tail — 0 = drained or
stopped at a citizen gate; 1 = compute/git failure (unless keep-going,
see §5).

### 4.2 `enju retry <task-id>` — Layer A

Re-open and re-run a single `failed_retryable` task. Mirror of
`enju_retry_task`.

```
enju retry <task-id> [--from head|snapshot] [--project id] [--json]
                     [--coordinator url]
```

`<task-id>` is the full `project:run:task` id (as printed in failure
output). `--from` defaults to `head` (matches the MCP default).

Behavior, two-half compose exactly as the MCP handler:
1. `POST /tasks/:id/retry {from}` — coord re-opens `failed_retryable →
   READY`, returns `is_compute`.
2. If `is_compute` → `RetryComputeTask(ctx, taskID, from)` and render the
   outcome. If not compute (answer/review/vote) → print "re-opened to
   READY; its assignee re-claims and re-runs" and stop (no operator
   execute step — calling the compute path would emit a spurious "not
   compute" error).

Exit: 0 on successful re-open (+ successful compute re-run if compute);
1 on coord refusal (task not retryable / not found) or a failed re-run.

### 4.3 `--keep-going` on `enju go` and `enju resume` — Layer A

See §5. Default behavior, opt-out via `--fail-fast`.

### 4.4 `enju drive <seq>` — Layer B

Drive a run to terminal: loop start → wait → reap until nothing is left
to do. This is the async self-driver and the re-attach path.

```
enju drive <seq> [--parallel N] [--max-tasks N] [--interval 5s]
                 [--once] [--keep-going|--fail-fast] [--project id]
                 [--json] [--coordinator url]
```

- `--interval` — sleep between passes (default 5s; ignored with `--once`).
- `--once` — run exactly one start+reap pass and exit (cron-friendly).
- Other flags as `resume`.

Re-attach: `drive` operates purely on `(project, seq)`. It can attach to
a run created earlier (by an earlier `enju go`, by another operator, or
by an MCP session) and pick up from wherever the run currently is —
reaping any already-finished subprocesses on the first pass.

The loop (§6) is the whole novelty; `--once` makes it a single tick.

## 5. Failure policy

Distinguish two failure classes (already distinct in the stop-reason
vocabulary):

- **Task-level, recoverable** — `compute_failed` (script non-zero) and
  `git_operation_failed` (commit/push failed). The task parks
  `failed_retryable`; its downstream blocks (cascade); sibling branches
  are unaffected.
- **Driver-level, fatal** — `compute_errored` (claim/fetch/wrapper
  pre-exec), `context_cancelled`. Cannot make further progress.

**Default = keep-going.** On a task-level failure: record it and keep
draining the rest of the run; do not abort. On a driver-level failure:
stop immediately.

`--fail-fast` restores the current `enju go` behavior: stop on the first
task-level failure too.

Implementation: a `KeepGoing bool` on `ExecuteRunParams`. In both the
serial loop and `runCascadeParallel`'s `recordEntry`, when `KeepGoing` is
set and the outcome is `compute_failed`/`git_failed`, append the entry
but do **not** set the terminal `stopReason` that breaks the loop —
let `fetchReadyTasksForRun` naturally drop the failed task and its
blocked descendants, and continue until `no_ready_compute`. Driver-level
errors break regardless of `KeepGoing`.

Result reporting: keep-going stops at `no_ready_compute` (or a citizen
gate) with the failed entries present in `Entries`. Renderer prints a
trailing "N task(s) failed (retry with `enju retry <id>`)" block listing
each failed task id. Exit code: **1 if any task-level failure was
recorded**, else 0 — so a keep-going batch still signals failure to a
script while having finished everything it could.

Default-on rationale (pre-launch, no back-compat owed): a fan-out that
abandons 99 good branches because 1 failed is the wrong default; the
opt-out exists for genuinely linear runs where an early failure means the
rest is meaningless.

Ordering vs Layer C: a task's `retries:` budget (§7) is consumed
**first** — auto-retry exhausts before a task is ever surfaced as a
task-level failure here. So keep-going / `--fail-fast` only ever see
failures that already burned through their retries.

## 6. The drive loop (Layer B)

```
drive(project, seq, parallel, maxTasks, interval, once, keepGoing):
    loop:
        # ① START — launch every ready compute task we can.
        res = ExecuteRun{project, seq, maxTasks, parallel, keepGoing}
        render(res)
        if res.stopReason is driver-level fatal:      # compute_errored, cancelled
            return 1
        if res.stopReason is a citizen gate:           # citizen_task_ready / assigned_elsewhere
            report blocker; return 0                   # nothing the driver can advance

        # ② TERMINAL? — settled = run terminal AND no async in flight.
        if runIsTerminal(project, seq) and not asyncInFlight(project, seq):
            report failures; return (1 if any failed else 0)
        if once:
            report failures; return (1 if any failed else 0)

        # ③ WAIT
        sleep(interval)  # ctx.Done → clean exit, note detached jobs keep running

        # ④ REAP — finish any subprocess that completed during the wait.
        #    (ExecuteRun's cold-reconcile already reaps on an empty scan;
        #     this is an explicit reap so a still-RUNNING-async pass that
        #     returned async_task_started also gets its results picked up.)
        reconcile(project, seq)   # PullBranchWithReconcileWF / ReconcileRunBranch
        # loop back to ①: downstream that the reap unblocked is now READY.
```

Notes:

- `ExecuteRun` returning `async_task_started` is **not** terminal for the
  driver — it means "launched, keep looping." (Today's serial/parallel
  ExecuteRun sets that stop reason and returns; the driver simply treats
  it as "go reap then re-launch", no engine change needed.)
- `asyncInFlight` = any task in this run in RUNNING with an outstanding
  `.wrap-job.json`/`.wrap-result.json` not yet `.done`, OR any RUNNING
  async task per the coord. Conservative: if unsure, do one more
  reap+poll rather than exiting early (the cost is one interval).
- For a **pure sync** run, ② is reached on the first pass (ExecuteRun
  drained it inline) — so `drive` on a sync run behaves like `resume` and
  exits in one pass. The loop only spins for async.
- Citizen gate: `drive` stops and reports (does not wait). Driving
  citizen-gated runs is `--auto-agents`' job (it has the bot supervisor);
  `drive` is for unattended compute.

## 7. Layer C: `retries:` auto-retry (coordinator-side)

A per-task YAML directive that auto-re-runs a transiently-failed compute
task before parking it `failed_retryable`. Independent of A/B; lands in
the coordinator + YAML schema, not the CLI.

### 7.1 Schema

```yaml
tasks:
  - id: section_5_orthologs
    action: compute
    script: run.sh
    retries: 2        # auto re-run up to 2 extra times (3 attempts total). Default 0.
```

- `retries: N` is optional, default `0` (current behavior — no auto-retry).
- Task-scoped. A run-level / global default is **out of scope for v1**
  (add later only if the per-task form proves too verbose).
- `validate` lints it: integer ≥ 0; warn if set on a non-`compute` action
  (citizen tasks recover through the contract-gate path, not this one).

### 7.2 Coordinator behavior

The coordinator already owns the failed-compute transition (the
`compute_error` / `compute_failed` → `failed_retryable` park) and a
per-claim attempt count (`iter_seq`, which advances on each re-claim). So
auto-retry is a small extension of an existing transition, not new
machinery:

```
on compute failure of task T with kind=compute_error:     # recoverable class only
    attempts = number of failed claim attempts for T       # from iter_seq
    if attempts <= T.retries:
        re-admit T to READY                                # auto-retry (no human action)
        emit a task event: auto_retry {attempt, of: retries}
    else:
        park failed_retryable                              # exhausted → current behavior
```

- **Only the recoverable class** (`kind=compute_error`: script non-zero
  or wrapper-level abort) consumes a retry. A terminal cascade
  (`enju_fail_task`, review-reject) never auto-retries.
- The re-run uses the **pinned snapshot, unchanged** — semantically the
  same as `enju retry --from snapshot` (auto-retry assumes transience;
  the code wasn't the problem). The re-admitted task is claimed + run by
  whatever is driving (interactive execute, `resume`, or `drive`) on its
  next pass — so auto-retry needs no client cooperation beyond the normal
  claim loop.
- **Survives client death**: the budget + attempt count live coord-side,
  so a retry counts even if the CLI that triggered the failing run is
  long gone. This is the reason for coord-side over client-side.
- **Idempotency**: counting keys off the existing `iter_seq` attempt
  ledger, which is already idempotent on `(task_id, iter_seq)` — a
  duplicate failure report for the same attempt does not double-spend the
  budget.

### 7.3 Observability

- An `auto_retry` task event per re-admission (`{attempt, of}`) so the
  event log and `enju runs`/`status` can show "section_5 (retry 2/2)"
  rather than a silent re-run.
- A task that exhausts its budget lands `failed_retryable` exactly as
  today — `enju retry` / keep-going pick up from there.

### 7.4 Out of scope (v1)

- Backoff / delay between retries (Snakemake has none by default; immediate
  re-admit. Add `retry_delay` later only if flaky-infra cases demand it).
- Distinguishing "transient" from "deterministic" failure — `retries` is a
  blunt count, opted in per task by an author who knows the script.
- Per-failure-kind budgets (e.g. retry `git_failed` separately).

## 8. Concurrency & lock safety

- `--parallel` reuses `runCascadeParallel` and the shared
  `service.MaxParallel` cap. For async, `parallel` bounds how many
  kickoffs a single START pass launches at once (the in-flight budget).
- The reap step takes the project write lock (via the
  pull-with-reconcile path) exactly as MCP reconcile does. A `drive`
  process and a concurrent MCP ticker reaping the same run is **safe and
  idempotent**: results rename to `.done`, and the coord rejects
  state changes on already-terminal tasks. They may contend on the lock
  but cannot corrupt state. No new coordination required for v1.
- `drive` does not need to win the reconcile-ownership lease
  (`OwnsReconcile`); that lease only de-dupes the *autonomous* MCP
  tickers. An operator-invoked `drive` is an explicit request to make
  progress and reaps unconditionally.

## 9. Out of scope (v1)

- Expanding `--auto-agents` semantics — `drive` is compute-only; citizen
  gates stop it.
- A long-lived daemon / service. `drive` is a foreground process (or a
  cron'd `--once`); it is not installed or supervised.
- Exposing `base`/run-creation on resume — `resume`/`drive` operate on
  runs that already exist; creation stays with `enju go` / `enju_create_run`.
- Coordinator-side driving. The coordinator stays a referee.

## 10. Testing

Mirror-tests, no coverage lost (per repo convention):

- `resume`: resolves `(project, seq)`; unregistered/zero seq → clear
  error + project-id-0 usage discriminator (same shape as
  `runProjectDefaultBranch` test); threads `Parallel`/`MaxTasks`.
- `retry`: `from` defaults to `head`; compute vs citizen branch (citizen
  → re-open-only, no spurious "not compute" error — mirror
  `TestHandleRetryTask_CitizenTask_*`).
- keep-going: an `ExecuteRun` over a DAG where one branch fails and a
  sibling succeeds drains the sibling, records the failure, returns
  `no_ready_compute`, and the renderer exits 1 with the failed id listed.
  `--fail-fast` stops on the first failure (current behavior pinned).
- drive loop (pure logic, no live coord): given a fake `ExecuteRun` +
  terminal/in-flight probes, the loop relaunches after
  `async_task_started`, stops on terminal, stops+reports on a citizen
  gate, returns 1 on driver-level fatal, and `--once` runs exactly one
  pass.
- Layer C (coord-side): `retries: 2` re-admits a `compute_error` task to
  READY for attempts 1–2 (emitting `auto_retry`), then parks
  `failed_retryable` on attempt 3; a terminal `enju_fail_task` never
  consumes a retry; `retries: 0` (default) parks immediately as today;
  `validate` flags `retries` on a non-compute action.

## 11. Phasing

1. **A.1** ✅ `enju resume <seq>` — execute tail extracted to
   `executeRunAndRender`, shared with `enju go`; `resolveResumeTarget`
   testable.
2. **A.2** ✅ `enju retry <task-id>` — two-half compose, PARITY with
   `handleRetryTask`.
3. **A.3** ✅ `--keep-going` / `--fail-fast` — `ExecuteRunParams.KeepGoing`,
   `stopReasonForOutcome` (shared by serial + parallel loops),
   `failedTaskIDs` report block + exit-1 rule; on `go` and `resume`.
4. **B** `enju drive <seq>` (the loop + `--once` + `--interval`,
   generalized from `driveAutoBotsRun`).
5. **C** `retries:` auto-retry (YAML schema + `validate` lint +
   coordinator re-admit on `compute_error` within budget + `auto_retry`
   event). Orthogonal — can land before, between, or after A/B.

After A.1–A.3 the sync fan-out repro fully recovers from the CLI; C makes
the transient case (the actual repro trigger) self-heal without any
operator action; B adds headless async self-completion.
