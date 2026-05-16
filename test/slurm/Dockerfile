# Dev-only SLURM validation image. NOT part of CI or any
# shippable code path — it exists solely to run the
# ENJU_SLURM_IT=1 live executor test (TestSlurmLive) against a
# REAL single-node SLURM with working accounting (sacct), which
# the cluster-free unit/FakeExecutor tests deliberately can't.
#
# The Go toolchain + module cache are bind-mounted at run time
# (see run.sh), not baked, so the image stays small and the test
# builds offline with the exact host go1.26.2.
#
# Ubuntu 24.04 == the host base, so glibc matches the
# bind-mounted host Go binary.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# slurm-wlm pulls slurmctld+slurmd+client; slurmdbd + mariadb are
# what make `sacct` (accounting) actually work — that's the
# fragile, must-have piece since SlurmExecutor.Poll shells sacct.
RUN apt-get update && apt-get install -y --no-install-recommends \
        slurm-wlm slurmdbd munge mariadb-server \
        ca-certificates iproute2 procps gosu tini \
    && rm -rf /var/lib/apt/lists/*

# Spool / state / log dirs SLURM expects, owned correctly.
RUN install -d -o slurm -g slurm /var/spool/slurmctld /var/spool/slurmd /var/log/slurm \
    && install -d -o mysql -g mysql /var/lib/mysql /run/mysqld

COPY slurm.conf      /etc/slurm/slurm.conf
COPY cgroup.conf     /etc/slurm/cgroup.conf
COPY slurmdbd.conf   /etc/slurm/slurmdbd.conf
COPY entrypoint.sh   /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && chown slurm:slurm /etc/slurm/slurmdbd.conf && chmod 600 /etc/slurm/slurmdbd.conf

# tini reaps the daemons we background; entrypoint boots the
# stack then exec's whatever command run.sh passes (the go test).
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
