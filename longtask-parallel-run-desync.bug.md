# Bug: `enju go --parallel N` leaves run "active" with most tasks un-recorded after long (multi-hour) compute tasks complete

Status: REPORTED (field report from a real run; not yet root-caused or repro'd in a test)

Reported via: `enju go --run-branch auto --parallel 8 --params-file <30 batches> workflows/pubmed-update/enju.yaml`
on project #4 `bioyoda` (path-mode, managed bare, `push_topic_branches=false` solo mode), workflow `publish: none`.
Run #16. Several of the 30 `action: compute` fan-out tasks ran **5+ hours** each (CPU S-BioBERT embedding); the short ones finished in seconds.

## Symptom

After the run, **every task's script had completed and all outputs were on disk and valid**
(1334 `.index` files + matching `.json` sidecars, 0 unreadable), and the `enju go` process had
exited — its last lines were:

```
  ✓ 4:16:29:process (18598846ms)        # ~5.16h wall for the heaviest batch
stopped: no_ready_compute
```

…but the coordinator never marked the run complete. `enju runs` / `enju_run_status` showed:

```
Status: active    Progress: 8/31
  report   1 waiting
  process  22 available · 8 done
```

So: **8 of 30 `process` tasks recorded done; the other 22 sat "available" even though their
scripts had already run to completion and written their declared outputs.** The `collects` `report`
task therefore never became ready. The run could not reach `completed` on its own; `stopped:
no_ready_compute` fired despite 22 tasks showing "available".

The work itself was not lost (scripts are idempotent / skip-existing, outputs all present) — this
is a **coordinator bookkeeping / lifecycle** problem, not data loss. But the run is stuck: it can't
finish, the fan-in never runs, and `enju runs` misreports progress.

## What I could and couldn't confirm

Confirmed by direct inspection:
- All 30 batch scripts ran to completion (every expected output file exists and is valid).
- The driving `enju go` process had exited (the "still running" I first saw was a *separate* bash
  monitor whose `pgrep -f "enju go.*pubmed-update"` matched its **own** command line — unrelated to
  this bug, but worth a doc note: self-matching pgrep patterns give false "still running").
- Coordinator state: active, 8/30 done, 22 "available", report waiting.

Not confirmed (no Go-internal verification):
- *Why* 22 completed tasks landed as "available" rather than "done".

## Hypothesis (unverified)

The long task durations (hours) look like the trigger. Candidate mechanisms:
1. **Claim lease / heartbeat expiry.** If a claim has a max lease and the heavy tasks ran past it,
   the coordinator may have reclaimed/abandoned those claims; the eventual submit from the
   long-running script would then be a "late/abandoned-claim" submit and get **refused** (the same
   class of refusal documented for post-terminate late submits) — so the script's success never
   becomes a recorded `done`, and the task reverts to `available`.
2. **Drain-loop exit while tasks in-flight.** `enju go --parallel 8`'s drain saw "no *ready*
   compute" (the 22 were claimed/in-flight, not `ready`) and exited (`stopped: no_ready_compute`)
   instead of waiting for the in-flight long tasks to land — stranding them as available once their
   claims later lapsed.

If either is right, the fix is to make long-running sync compute heartbeat/renew its claim (or make
the lease generous/while-process-alive), and/or have the `--parallel` drain wait on in-flight claims
rather than exiting on `no_ready_compute` while claims are outstanding.

## Repro sketch (to build a test)

A fan-out of N `action: compute` tasks where some tasks `sleep` well past any claim lease
(e.g. a few tasks sleeping hours, or lower the lease for the test), driven by
`enju go --parallel K`, then assert the run reaches `completed` with all N tasks `done` and the
`collects` fan-in firing. Expect current behavior: run stuck `active`, the long tasks back in
`available`, fan-in never ready.

## Impact / workaround

- Impact: medium. No data loss for idempotent compute (outputs on disk), but long-running
  fan-outs can't self-complete; fan-in never runs; progress misreported. Hits exactly the
  "heavy bio/ML batch" use case `--parallel` is meant for.
- Workaround used: verified outputs on disk independently, then `enju_terminate_run` to close out.
  A clean `enju resume` would re-run the 22 (idempotent → fast skip) but I terminated instead.
