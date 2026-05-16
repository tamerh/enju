# Real-SLURM validation harness (opt-in, NOT part of `go test ./...`)

This directory runs `TestSlurmLive` against a **real single-node
SLURM** (with working `sacct` accounting) inside a throwaway
Docker container. It is deliberately **not** part of the normal
test suite or CI.

## Why this is separate

The SLURM executor is covered in three layers. Only the last one
needs a cluster:

| Layer | Where | In `go test ./...`? | Covers |
|---|---|---|---|
| Unit | `internal/fatclient/executor/*_test.go` | ✅ yes | `MapSacctState` matrix, `sbatch` script gen, sidecar round-trip, `sacct` output parsing, `killProcessTree` |
| FakeExecutor e2e | `internal/fatclient/service/slurm_e2e_test.go` | ✅ yes | the **whole deferred loop** in-process: dispatch → produce → host-side `CommitDeferred` → `/result` → sidecar retire → terminate→Cancel |
| Real SLURM | **here** (`test/slurm/`) | ❌ **no — opt-in** | the actual `sbatch`/`sacct`/`scancel` CLI wiring against a real `slurmctld`/`slurmd`/`slurmdbd` |

`go test ./...` already skips the real-SLURM layer: `TestSlurmLive`
is `t.Skip`'d unless `ENJU_SLURM_IT=1`, and nothing imports the
scripts in this directory. So the fast, cluster-free coverage runs
everywhere; only the heavyweight real-binary check is on demand.

## What it validates (and what it doesn't)

**Does:** the real `SlurmExecutor` against real binaries —
`Submit` → `sbatch --parsable` (job-id parse, real `slurmctld`
accepts the generated batch script), `Poll` → real `sacct -j …
-P -o JobID,State` (real output parsed + classified), the submit
/poll loop reaches a terminal state, `scancel` wiring.

**Doesn't:** `TestSlurmLive` uses a throwaway job, so it exercises
the `StateLost` terminal path (the job exits instantly and races
the `sacct` accounting flush), **not** the `COMPLETED → StateDone`
+ host-side-commit happy path end-to-end on real SLURM. That
mapping is covered by the unit `TestMapSacctState` and the
FakeExecutor e2e; reproducing it on real SLURM would need a real
wrapper + git workflow inside the container (intentionally out of
scope here). Multi-node scheduling, cgroup confinement, and site
SLURM configs are also out of scope — this is a *CLI integration*
gate, not a SLURM conformance suite.

## Prerequisites

- Docker usable by your user (no host `sudo` needed, no host
  packages installed, no host mutation — the only host touch is
  **read-only** bind mounts of your Go toolchain + module cache so
  the test builds offline with the exact `go.mod` toolchain).
- The container runs `--privileged`. SLURM 23.11's `slurmd`
  always initializes the cgroup/v2 plugin and needs a writable
  cgroup2 hierarchy; `--privileged` gives it the container's
  **own** private cgroup namespace (the host cgroup tree is not
  shared or modified). Acceptable because the container is
  single-use, non-networked, and destroyed on exit (`--rm`).

## Run it

```sh
./test/slurm/run.sh
```

That builds `enju-slurm-it:local`, boots munge → mariadb →
slurmdbd → slurmctld → slurmd (the entrypoint gates on each
daemon's readiness and dumps logs on any failure), then runs:

```
ENJU_SLURM_IT=1 go test -count=1 -run TestSlurmLive -v ./internal/fatclient/executor/
```

Expected tail on success:

```
[slurm-entrypoint] sinfo: debug* up 1 idle localhost
[slurm-entrypoint] SLURM ready — handing off to: … go test
--- PASS: TestSlurmLive
ok  github.com/enju-ai/enju/internal/fatclient/executor
```

## On a real submit host (no Docker)

If you already have SLURM, skip the container entirely:

```sh
ENJU_SLURM_IT=1 go test -run TestSlurmLive -v ./internal/fatclient/executor/
```

## Cleanup

The image is ~600 MB:

```sh
docker rmi enju-slurm-it:local
```

## Files

- `Dockerfile` — Ubuntu 24.04 + `slurm-wlm` + `slurmdbd` + `munge`
  + `mariadb-server`. Go is bind-mounted at run time, not baked.
- `slurm.conf` / `cgroup.conf` / `slurmdbd.conf` — minimal
  single-node config; no real resource confinement (this is a
  launcher test, not a scheduler test).
- `entrypoint.sh` — ordered, readiness-gated daemon bring-up; the
  cgroup scaffold for systemd-less containers; `exec`s the test.
- `run.sh` — host-side: build + `docker run` with the read-only
  Go/​module-cache mounts.
</content>
