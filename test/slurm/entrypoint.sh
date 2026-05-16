#!/bin/bash
# Boot a single-node SLURM with accounting, in order, then exec
# the command passed by run.sh (the go test). Every step is
# logged + waited-on; SLURM-in-one-container fails silently if
# you don't gate on readiness, so we do.
set -euo pipefail

log() { echo "[slurm-entrypoint] $*"; }
die() { echo "[slurm-entrypoint][FATAL] $*" >&2; dump_logs; exit 1; }
dump_logs() {
  for f in /var/log/slurm/slurmdbd.log \
           /var/log/slurm/slurmctld.log /var/log/slurm/slurmctld.console \
           /var/log/slurm/slurmd.log /var/log/slurm/slurmd.console \
           /var/log/slurm/mariadb.log; do
    [ -f "$f" ] && { echo "----- $f -----"; tail -n 40 "$f"; }
  done
}

# --- 1. munge (shared-secret auth every slurm daemon needs) ---
log "starting munge"
# munged refuses to use a socket dir that isn't traversable by
# all (it wants o+x on /run/munge); lib/log stay private 0700.
install -d -o munge -g munge -m 0700 /var/lib/munge /var/log/munge
install -d -o munge -g munge -m 0755 /run/munge
if [ ! -s /etc/munge/munge.key ]; then
  dd if=/dev/urandom bs=1 count=1024 2>/dev/null > /etc/munge/munge.key
  chown munge:munge /etc/munge/munge.key
  chmod 0400 /etc/munge/munge.key
fi
gosu munge /usr/sbin/munged
for i in $(seq 1 20); do munge -n >/dev/null 2>&1 && break; sleep 0.3; done
munge -n | unmunge >/dev/null 2>&1 || die "munge not authenticating"
log "munge up"

# --- 2. mariadb (slurmdbd's backing store → makes sacct work) ---
log "starting mariadb"
if [ ! -d /var/lib/mysql/mysql ]; then
  mariadb-install-db --user=mysql --datadir=/var/lib/mysql >/dev/null 2>&1 \
    || mysql_install_db --user=mysql --datadir=/var/lib/mysql >/dev/null 2>&1
fi
gosu mysql /usr/sbin/mariadbd --datadir=/var/lib/mysql --socket=/run/mysqld/mysqld.sock >/var/log/slurm/mariadb.log 2>&1 &
for i in $(seq 1 40); do
  mariadb --socket=/run/mysqld/mysqld.sock -e 'SELECT 1' >/dev/null 2>&1 && break
  sleep 0.5
done
mariadb --socket=/run/mysqld/mysqld.sock -e 'SELECT 1' >/dev/null 2>&1 || die "mariadb not accepting connections"
# slurmdbd connects to StorageHost=localhost via the MySQL C
# connector, which resolves to TCP 127.0.0.1 — a 'slurm'@'localhost'
# grant (socket-only in MySQL's host matching) would NOT apply.
# Grant to '%' as well; this image is a throwaway sandbox with no
# network exposure, so the broad grant is harmless.
mariadb --socket=/run/mysqld/mysqld.sock <<'SQL'
CREATE DATABASE IF NOT EXISTS slurm_acct_db;
CREATE USER IF NOT EXISTS 'slurm'@'localhost' IDENTIFIED BY 'slurmpw';
CREATE USER IF NOT EXISTS 'slurm'@'127.0.0.1' IDENTIFIED BY 'slurmpw';
CREATE USER IF NOT EXISTS 'slurm'@'%'         IDENTIFIED BY 'slurmpw';
GRANT ALL PRIVILEGES ON slurm_acct_db.* TO 'slurm'@'localhost';
GRANT ALL PRIVILEGES ON slurm_acct_db.* TO 'slurm'@'127.0.0.1';
GRANT ALL PRIVILEGES ON slurm_acct_db.* TO 'slurm'@'%';
FLUSH PRIVILEGES;
SQL
log "mariadb up + slurm_acct_db created"

# --- 3. slurmdbd (accounting daemon, listens :6819) ---
log "starting slurmdbd"
gosu slurm /usr/sbin/slurmdbd
for i in $(seq 1 40); do
  (exec 3<>/dev/tcp/127.0.0.1/6819) 2>/dev/null && { exec 3>&-; break; }
  sleep 0.5
done
(exec 3<>/dev/tcp/127.0.0.1/6819) 2>/dev/null || die "slurmdbd not listening on 6819"
exec 3>&- 2>/dev/null || true
log "slurmdbd up"

# --- 4. register the cluster in accounting (sacct needs this) ---
sacctmgr -i add cluster enjutest >/dev/null 2>&1 || log "cluster enjutest already registered"

# --- 4b. cgroup scaffold for slurmd (no systemd in container) ---
# slurmd 23.11 always inits cgroup/v2; with IgnoreSystemd it
# expects /sys/fs/cgroup/system.slice/<node>_slurmstepd.scope to
# be creatable. Under --privileged the container has its OWN
# writable cgroup2 root (NOT the host's — no host mutation), but
# the systemd-style "system.slice" dir doesn't exist because
# there's no systemd. Pre-create it, and delegate subtree
# controllers so slurmd can make the scope cgroup under it.
if [ -d /sys/fs/cgroup -a -w /sys/fs/cgroup ]; then
  mkdir -p /sys/fs/cgroup/system.slice 2>/dev/null || true
  # cgroup2 "no internal processes": move our shell out of the
  # root into an init leaf so controllers can be delegated.
  if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    mkdir -p /sys/fs/cgroup/init.scope 2>/dev/null || true
    echo $$ > /sys/fs/cgroup/init.scope/cgroup.procs 2>/dev/null || true
    for c in $(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null); do
      echo "+$c" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
    done
  fi
  log "cgroup scaffold: $(ls -d /sys/fs/cgroup/system.slice 2>/dev/null || echo MISSING)"
fi

# --- 5. slurmctld + slurmd ---
# Run in the foreground (-D) and backgrounded by us, so a config
# error surfaces in the log instead of being swallowed by set -e
# when the daemonizing form exits non-zero before forking.
log "starting slurmctld"
/usr/sbin/slurmctld -D >/var/log/slurm/slurmctld.console 2>&1 &
SLURMCTLD_PID=$!
for i in $(seq 1 40); do
  scontrol ping >/dev/null 2>&1 && break
  kill -0 "$SLURMCTLD_PID" 2>/dev/null || die "slurmctld exited during startup"
  sleep 0.5
done
scontrol ping >/dev/null 2>&1 || die "slurmctld not responding to scontrol ping"
log "slurmctld up"

log "starting slurmd"
/usr/sbin/slurmd -D >/var/log/slurm/slurmd.console 2>&1 &
SLURMD_PID=$!
sleep 1
kill -0 "$SLURMD_PID" 2>/dev/null || die "slurmd exited during startup"
log "slurmd launched"

# --- 6. wait for the node to be usable ---
for i in $(seq 1 40); do
  state=$(sinfo -h -n localhost -o '%T' 2>/dev/null | tr -d '*~#$@' || true)
  case "$state" in
    idle|mixed|allocated) break ;;
  esac
  # Nudge a DOWN/UNKNOWN node back into service.
  scontrol update nodename=localhost state=resume >/dev/null 2>&1 || true
  sleep 0.5
done
log "sinfo: $(sinfo -h -o '%P %a %D %T %N' 2>/dev/null || echo '<none>')"
sinfo -h -n localhost -o '%T' 2>/dev/null | grep -Eq 'idle|mixed|alloc' \
  || die "node localhost never reached a runnable state"

# Sanity: the three CLIs SlurmExecutor shells out to must work.
sbatch --version  >/dev/null || die "sbatch missing"
sacct  --version  >/dev/null || die "sacct missing"
scancel --version >/dev/null || die "scancel missing"
log "SLURM ready — handing off to: $*"

exec "$@"
