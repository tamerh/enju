#!/bin/bash
# Run TestSlurmLive against a REAL single-node SLURM in a
# throwaway container. Dev-only; not CI. No host sudo, no host
# mutation — the only thing it touches on the host is read-only
# bind mounts of the Go toolchain + module cache so the test
# builds offline with the exact go.mod toolchain.
#
# Usage:  test/slurm/run.sh            (from the repo root or anywhere)
#
# Honest scope: this validates SlurmExecutor against real
# sbatch/sacct/scancel + slurmdbd accounting. It does NOT cover
# multi-node scheduling, cgroup task confinement, or site SLURM
# configs — it's the "does our CLI integration work against an
# actual SLURM" gate, nothing more.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

GOROOT="$(go env GOROOT)"
GOMODCACHE="$(go env GOMODCACHE)"
img="enju-slurm-it:local"

echo "[run.sh] repo=$repo"
echo "[run.sh] GOROOT=$GOROOT  GOMODCACHE=$GOMODCACHE"
echo "[run.sh] building image $img ..."
docker build -t "$img" "$here"

echo "[run.sh] running TestSlurmLive against real SLURM ..."
# --privileged: slurmd (23.11) always inits the cgroup/v2 plugin
# and needs a writable cgroup2 hierarchy to manage. This is the
# documented norm for SLURM-in-docker. Acceptable here: the
# container is a throwaway, single-use, non-networked test
# sandbox — nothing sensitive, destroyed on exit (--rm).
exec docker run --rm \
  --privileged \
  -v "$GOROOT":/usr/local/go:ro \
  -v "$GOMODCACHE":/gomodcache:ro \
  -v "$repo":/src:ro \
  "$img" \
  bash -c '
    set -e
    export PATH=/usr/local/go/bin:$PATH
    export GOMODCACHE=/gomodcache    # complete host cache, read-only
    export GOCACHE=/tmp/gocache      # writable build cache (container)
    export GOFLAGS=-mod=readonly     # never mutate go.mod/go.sum or modcache
    export GOPROXY=off GOSUMDB=off   # fully offline; fail loud if a dep is missing
    export ENJU_SLURM_IT=1
    # /src is read-only: go test reads it and writes only GOCACHE
    # (/tmp) and t.TempDir (/tmp), so no writable source copy is
    # needed.
    cd /src
    go test -count=1 -run TestSlurmLive -v ./internal/fatclient/executor/
  '
