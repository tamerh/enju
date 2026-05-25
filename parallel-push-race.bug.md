# Bug: per-task push fails non-FF → terminal failure (exposed by `enju go --parallel N`)

Status: FIXED (both pieces, uncommitted) — pending review
  - Fix #1 (cure): `MergeAcceptedTopic` now catches up to origin and
    re-pushes on non-FF (enjugit/producing.go `pushTargetWithCatchUp`).
    Repro: `TestMergeAcceptedTopic_PushNonFF_CatchesUpAndLands` (green).
  - Fix #2 (seatbelt): a transient push non-FF reported to the coord now
    parks `failed_retryable` (recoverable) instead of terminal+cascade
    (report_merge_failed.go `Transient`; fat client classifies via
    `errors.Is(err, enjugit.ErrPushNonFF)`). Tests:
    `TestReportMergeFailed_Transient_ParksRetryable` +
    `_NonTransient_StaysTerminal` (green).
  - Fix #3 (gate the per-task push on `publish: none`) — REJECTED as a
    fix; see "Optional / design" below. The per-task run-branch push is
    the collaborative/audit mechanism, not the deliverable publish that
    `publish:` governs; gating it re-opens D3 (dropped 202bb78 for
    breaking multi-citizen review). Resolved instead by renaming the
    `sync:` block → `publish:` (modes none|local|push) so the semantics
    are no longer misleading. #1+#2 are the actual fix.
Reported via: `enju go --parallel 4` on a sync-compute fan-out (12
sections × 3 genes), project with an origin remote, workflow `publish: none`.

## Symptom

On an accepted compute task, the per-task topic→target merge pushes the
target branch to origin and the push is rejected non-fast-forward:

```
2:9:APP:section_3_protein_ids — merge_failed: MergeAcceptedTopic failed
  merge-ff:     skipped — non-fast-forward; falling back to merge commit
  merge-commit: ok — 72b725d7
  push:         failed — git push origin preview-gene-categorized: exit status 1
```

The script succeeded and the merge commit was created — only the **push**
lost a race. Verified transient: `git push` immediately afterward reports
"Everything up-to-date" (local/origin 0 0).

## Root cause (the push step never incorporates what it fetched)

`MergeAcceptedTopic` (internal/fatclient/enjugit/producing.go) runs, under
the project lock:

1. `Fetch()` — best-effort; updates the **remote-tracking** ref
   `origin/target` only. On error it logs a Warn and continues.
2. `MergeFFOrFail(target, topic)` / `MergeWithCommit(target, topic)` —
   merges the **topic** branch into the **local** `target` branch.
3. `Push(target)` — pushes local `target` to origin.

The gap: step 2 never merges `origin/target` (what step 1 just fetched)
into local `target`. So when origin has advanced, local `target` is still
behind it at push time, and step 3 is rejected non-FF. **The fetch is
performed but its result is never used.** There is no fetch-merge-retry on
a non-FF rejection — it goes straight to the terminal failure path.

### `--parallel` is the trigger, not the cause

The flaw is latent in serial runs too — it just never shows, because no
one else moves origin between a task's fetch and its push, so local
`target` is never behind. `--parallel` makes a sibling task push in
between, origin moves, and the un-caught-up local branch is exposed.

Note the push is **already serialized** under the project flock (each
parallel task opens its own `Workflow` handle, so the in-process mutex is
per-handle; cross-goroutine ordering rests on the flock). So "serialize
the push" is not the fix — the pushes already run one at a time. The
problem is the stale base each push is built on.

## Second bug: transient push failure is classified terminal

The fat client posts the failure to the coordinator's `/merges/failed`
handler (internal/coordinator/service/report_merge_failed.go), which
drives the task to **FAILED (terminal)** and fires the fail-cascade
(descendants → SKIPPED). Its own doc states the intent:

> "merge_failed is TERMINAL. The claim isn't reopened for retry —
> git-level non-conflict failures are operator misconfig / Enju bugs /
> infrastructure problems, not 'the citizen will fix it on retry.'"

That assumption predates `--parallel`. A push race is a genuinely
**transient** instance of merge_failed that the terminal classification
never anticipated. Consequences:

- `enju retry` / `enju_retry_task` refuse it ("only failed_retryable
  tasks can be retried").
- `enju_invalidate_task` needs ACCEPTED, not FAILED.
- Only recovery is a brand-new run — re-spending all the completed work.

Contrast: an `error_max_turns` compute failure parks `failed_retryable`
and recovers fine. The classification is simply wrong for a transient
push race.

(Cascade behavior itself is correct: a genuinely failed section *should*
terminally fail and skip its dependents. The problem is only that a
transient push race is being treated as that kind of failure.)

## Fix (two pieces)

1. **Push-with-catch-up retry on non-FF (the cure).** Inside the existing
   lock, on `ErrPushNonFF`: fetch + fast-forward/merge `origin/target`
   into local `target`, then re-push; bounded (e.g. 3 attempts). This
   makes the push race-proof regardless of the exact trigger — it closes
   the "fetched but never merged in" gap directly.

2. **Reclassify residual push/transport failures as `failed_retryable`
   (the seatbelt).** If the push still can't land after retries, park it
   recoverable — the same path `error_max_turns` uses — so `enju retry`
   and keep-going work instead of forcing a fresh run. Genuine conflicts
   (already a separate path) and ref-not-found misconfig stay terminal.

#1 prevents the failure; #2 makes any residual failure recoverable.

### Rejected alternative (and why) + the real follow-up

Considered: **gate the per-task push on `publish: none`** — under that
mode, don't push the run branch per-task. For the single-machine repro
this would dodge the race. But it conflates two different things:

- `publish:` governs the **run-completion deliverable publish** to the
  base branch (none = don't publish the deliverable).
- the per-task run-branch push is the **collaborative/audit integration**
  — in a multi-citizen / multi-machine setup the run branch must reach
  origin *during* the run so others can fetch + review. `publish: none`
  + multi-machine is a legitimate combo that still needs this push.

Gating it re-opens D3 (the topic-push gate, dropped in 202bb78 because
gating pushes broke multi-citizen-bare review). So it's **not** a safe
fix. The genuine issue the reviewer surfaced was the misleading name
("`sync: none`" reading as "push nothing"), now resolved by renaming the
block `sync:` → `publish:` (modes none|local|push). Follow-up, if
single-machine-with-backup-origin push churn is ever a real annoyance:
that needs a signal distinguishing "collaborative origin" from "backup
origin" — which publish mode does not encode — and must not break the
multi-machine case. Out of scope here.

## Workaround (today)

Run `--parallel 1` (serial pushes never race), or drive via
`enju_execute_run(parallel=1)`.

## Open question (does not change the fix)

Whether parallel tasks share one `.git` (refs shared) or get isolated
clones (refs not shared) — this decides whether a best-effort fetch alone
explains the stale base, but the push-with-catch-up retry (#1) fixes it
either way.
