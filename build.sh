#!/usr/bin/env bash
#
# build.sh — single orchestrator for build / test / lint / dev tasks.
#
# Replaces the previous Makefile + tools/check-imports.sh setup.
# Pure bash so multi-line shell logic stays natural (Make recipes
# weren't a great fit — every line ran in a fresh subshell, the
# `$$` escaping was a constant trap). Portable to Windows via Git
# Bash, WSL, or MSYS2 — same story as the old Makefile, since make
# wasn't installed on Windows natively either.
#
# Usage:
#   ./build.sh <command>
#
# Commands: see help.

set -euo pipefail

cd "$(dirname "$0")"

# ---------------------------------------------------------------
# Build / run
# ---------------------------------------------------------------

cmd_build() {
    go build -o enju ./cmd/enju/
    # Hardlink the binary as enju-coord so the coordinator runs
    # under a distinct argv[0]/comm. This makes a coord process
    # invisible to "pkill -f enju" (which is too broad — matches
    # any command line containing the word, including bots / ui /
    # mcp / serve). After this link, only "pkill -f enju-coord"
    # or "pkill enju-coord" targets the coord specifically.
    # Hardlink (not symlink) keeps it as a real binary path so
    # ELF-loader behaviour is identical to ./enju.
    ln -f ./enju ./enju-coord
}

cmd_run() {
    cmd_build
    ./enju serve --port 8000
}

cmd_clean() {
    rm -f enju
    rm -f -- *.db
    rm -rf /tmp/enju-test-output
}

# ---------------------------------------------------------------
# Tests
# ---------------------------------------------------------------

cmd_test() {
    go test ./... -count=1
}

cmd_test_v() {
    go test ./... -v -count=1
}

# Full test suite with the LLM-mode variants enabled (uses
# `claude -p`, costs tokens). Only a small number of tests check
# ENJU_LLM_TEST and branch on it; the rest run as-is.
cmd_test_llm() {
    ENJU_LLM_TEST=1 go test ./... -v -count=1 -timeout 300s
}

# MCP-layer-only iteration helper. The integration suite has
# clear MCP/non-MCP layering: tests under test/ named TestMCP*
# exercise the MCP transport, the rest exercise the coordinator
# directly. This is the focused-loop variant for MCP work.
cmd_test_mcp() {
    go test ./test/ -run TestMCP -count=1 -v
}

# ---------------------------------------------------------------
# Lints — architectural rules enforced as grep / go-list checks.
# ---------------------------------------------------------------

# Rule set 1: coordinator/fat-client/common/enjumcp import direction.
#
#   - internal/coordinator/* must not (transitively) import internal/fatclient/*
#   - internal/fatclient/*   must not (transitively) import internal/coordinator/*
#   - internal/common/*      must not import either side
#   - internal/enjumcp/*     must not import either side
#
# Production code only — `go list` without `-test` excludes test
# files, so end-to-end integration tests in fat-client may spin up
# coordinator components without violating the runtime-separation
# invariant.
cmd_check_imports() {
    echo "==> Checking import direction (coordinator ↔ fat-client ↔ common ↔ enjumcp)"
    local fail=0

    deps_of() {
        local pattern="$1"
        go list -deps "$pattern" 2>/dev/null | grep -E '^github\.com/enju-ai/enju/internal/' || true
    }

    if deps_of './internal/coordinator/...' | grep -q '/internal/fatclient/'; then
        echo "❌ Rule 1 violated: internal/coordinator/* imports internal/fatclient/*"
        deps_of './internal/coordinator/...' | grep '/internal/fatclient/' | sed 's/^/   /'
        fail=1
    fi

    if deps_of './internal/fatclient/...' | grep -q '/internal/coordinator/'; then
        echo "❌ Rule 2 violated: internal/fatclient/* imports internal/coordinator/*"
        deps_of './internal/fatclient/...' | grep '/internal/coordinator/' | sed 's/^/   /'
        fail=1
    fi

    if deps_of './internal/common/...' | grep -qE '/internal/(coordinator|fatclient)/'; then
        echo "❌ Rule 3 violated: internal/common/* imports a side package"
        deps_of './internal/common/...' | grep -E '/internal/(coordinator|fatclient)/' | sed 's/^/   /'
        fail=1
    fi

    if deps_of './internal/enjumcp/...' | grep -qE '/internal/(coordinator|fatclient)/'; then
        echo "❌ Rule 4 violated: internal/enjumcp/* imports a side package"
        deps_of './internal/enjumcp/...' | grep -E '/internal/(coordinator|fatclient)/' | sed 's/^/   /'
        fail=1
    fi

    # Rule 5: webui is a peer consumer. It may DIRECTLY import
    # common/* and internal/fatclient/service (the FatClient
    # surface) only — no reach-arounds into workspace, inbox,
    # notify, mcphandlers, or coordinator. Transitive deps via
    # service are fine (service legitimately uses workspace etc.
    # under the hood); we only block what webui's own files
    # import. The gap-fill discipline says: if you need something
    # not on FatClient, raise the gap and add the method there.
    direct_imports_of() {
        local pattern="$1"
        go list -f '{{ $p := .ImportPath }}{{ range .Imports }}{{ $p }} {{ . }}{{ "\n" }}{{ end }}' "$pattern" 2>/dev/null \
            | grep -E ' github\.com/enju-ai/enju/internal/' \
            || true
    }
    local webui_offenders
    webui_offenders=$(direct_imports_of './internal/webui/...' \
        | awk '{ print $2 }' \
        | grep -vE '^github\.com/enju-ai/enju/internal/(common|fatclient/service)(/|$)' \
        | sort -u \
        || true)
    if [ -n "$webui_offenders" ]; then
        echo "❌ Rule 5 violated: internal/webui/* directly imports outside the allowed surface"
        echo "$webui_offenders" | sed 's/^/   /'
        echo "   webui may import only internal/common/* and internal/fatclient/service."
        echo "   Reach through service.FatClient — raise a gap if a method is missing."
        fail=1
    fi

    # Rule 6: bots is the second peer consumer of FatClient — bot
    # daemon, manifest, supervisor, Handler interface. Same shape
    # as Rule 5: bots may DIRECTLY import common/* and
    # internal/fatclient/service only. Transitive deps via
    # service are fine. Locks in the Phase 7 "bots are first-class
    # fatclient consumers, not parallel infrastructure" architecture
    # — without this, nothing prevents bots from sneaking into
    # coordinator/* or workspace/* on a future patch.
    local bots_offenders
    bots_offenders=$(direct_imports_of './internal/bots/...' \
        | awk '{ print $2 }' \
        | grep -vE '^github\.com/enju-ai/enju/internal/(common|fatclient/service)(/|$)' \
        | sort -u \
        || true)
    if [ -n "$bots_offenders" ]; then
        echo "❌ Rule 6 violated: internal/bots/* directly imports outside the allowed surface"
        echo "$bots_offenders" | sed 's/^/   /'
        echo "   bots may import only internal/common/* and internal/fatclient/service."
        echo "   Reach through service.FatClient — raise a gap if a method is missing."
        fail=1
    fi

    if [ "$fail" -eq 0 ]; then
        echo "✅ import direction OK"
        return 0
    fi

    echo
    echo "These edges break the coordinator/fat-client/common/enjumcp/webui layering."
    echo "Fix by moving the offending package to its correct tree, or"
    echo "by moving the shared symbol into internal/common/."
    return 1
}

# Rule set 2: chokepoint invariant.
#
#   - s.db.Exec / tx.Exec confined to the SQL exec funnel
#     (apply.go state writes, sqlite.go schema bootstrap,
#      events_sqlite.go event subsystem). Anywhere else means
#     someone is sneaking around ApplyPlan / Record.
#   - store.TestEngineVersion confined to *_test.go files. It's
#     exported only so the cross-package version-drift test in
#     test/ can compare against engine.EngineVersion; production
#     code must use the engine constant directly.
#
# Test files (*_test.go) are intentionally excluded from the
# s.db.Exec rule. Tests need to construct edge-case states
# the production path can't reach (deliberately broken rows,
# pre-migration shapes, fixture races). Allowing tests to
# reach raw .Exec is a deliberate carve-out, not an oversight
# — production paths inside test/* still aren't allowed
# anyway because the chokepoint contract is about the
# RUNTIME chokepoint, not the test scaffolding around it.
cmd_check_chokepoint() {
    echo "==> Checking SQL exec funnel (s.db.Exec / tx.Exec confined to allowlist)"
    local fail=0

    local violations
    violations=$(grep -rn -E '\b(s\.db|tx)\.Exec\b' --include='*.go' \
            --exclude='*_test.go' \
            internal/ cmd/ 2>/dev/null \
        | grep -v '^internal/coordinator/store/apply\.go:' \
        | grep -v '^internal/coordinator/store/sqlite\.go:' \
        | grep -v '^internal/coordinator/store/events_sqlite\.go:' \
        || true)
    if [ -n "$violations" ]; then
        echo "❌ s.db.Exec / tx.Exec used outside the chokepoint funnel:"
        echo "$violations" | sed 's/^/   /'
        echo
        echo "Route state writes through ApplyPlan + apply.go,"
        echo "or event writes through Record + events_sqlite.go."
        fail=1
    fi

    echo "==> Checking store.TestEngineVersion is test-only"
    local leak
    leak=$(grep -rn 'store\.TestEngineVersion' --include='*.go' --exclude='*_test.go' internal/ cmd/ 2>/dev/null || true)
    if [ -n "$leak" ]; then
        echo "❌ store.TestEngineVersion used in non-test file:"
        echo "$leak" | sed 's/^/   /'
        echo
        echo "Production code must use engine.EngineVersion directly."
        echo "store.TestEngineVersion exists only for the cross-package drift test."
        fail=1
    fi

    if [ "$fail" -eq 0 ]; then
        echo "✅ chokepoint OK"
        return 0
    fi
    return 1
}

# All architectural lints. Use this in CI / pre-commit.
cmd_lint() {
    cmd_check_imports
    cmd_check_chokepoint
}

# ---------------------------------------------------------------
# Dev workflow: restart coordinator with wiped state.
# ---------------------------------------------------------------

# Build and restart coordinator. Preserves DB + workspaces by
# default — pass --wipe to clean state for a fresh-install dev
# loop. Credentials at ~/.enju/credentials.json are NEVER
# wiped (re-registering would invalidate the MCP client).
#
# Usage: ./build.sh dev-restart [PORT] [--wipe]
cmd_dev_restart() {
    local port="8333"
    local wipe=false
    while [ $# -gt 0 ]; do
        case "$1" in
            --wipe) wipe=true ;;
            *)      port="$1" ;;
        esac
        shift
    done

    cmd_build
    echo "==> Stopping existing enju coordinator..."
    # Targeted pattern — only matches the renamed coord binary,
    # never matches bots/ui/fat-client mcp processes.
    pkill -f "enju-coord serve" 2>/dev/null || true
    sleep 1

    if $wipe; then
        echo "==> --wipe: clearing DB + events DB + git-dir + workspaces + notify state (keeping credentials)..."
        rm -f ~/.enju/enju.db ~/.enju/enju.db-wal ~/.enju/enju.db-shm
        rm -f ~/.enju/enju-events.db ~/.enju/enju-events.db-wal ~/.enju/enju-events.db-shm
        # Live notify state is project-scoped (under each clone's
        # enju/events/) and goes with the workspaces wipe below.
        # Defensively clean any legacy ~/.enju/notify-* files left
        # behind from pre-redesign installs — once cleared, notify
        # never recreates them.
        rm -f ~/.enju/notify-state-*.json ~/.enju/notify-active.json ~/.enju/notify.yaml
        rm -rf ~/.enju/git-dir ~/.enju/workspaces
    else
        echo "==> Preserving DB + workspaces (pass --wipe for fresh state)"
    fi

    # Capture stdout+stderr to a known log file so panics, stack
    # traces, and slog output survive a crash. Without this the
    # backgrounded `&` sends output to the controlling tty, which
    # is lost the moment the shell scrolls or exits — leaving
    # "coordinator suddenly crashed" un-diagnosable. Rotate by
    # truncate (>) so each restart starts a clean log; previous
    # crash output, if any, is preserved at $log.prev.
    # Log lives next to the dev DB at ~/.enju/ — same dir the
    # --wipe target uses, so coord state and coord logs stay
    # collocated. Pre-2026-05 the log lived in the repo root; the
    # move was so the project tree stays clean and tail-friends
    # always know where to look regardless of the operator's cwd.
    mkdir -p ~/.enju
    local log="$HOME/.enju/enju-coord.log"
    if [ -f "$log" ]; then
        mv "$log" "${log}.prev"
    fi
    echo "==> Starting coordinator on port $port (logs: $log, prior run: ${log}.prev)..."
    # setsid puts the coord in its own session and process
    # group, fully detached from the controlling terminal.
    # This is the only thing that actually protects against
    # SIGTERM from a parent process (terminal emulator
    # closing a tab, IDE reloading its terminal panel, etc.).
    # nohup only blocks SIGHUP, not SIGTERM. disown only
    # hides the job from bash. Without setsid, anything that
    # fires "kill all my children" reaches the coord too.
    # < /dev/null severs stdin so the coord doesn't block
    # waiting for terminal input or get EOF on tty hangup.
    setsid nohup ./enju-coord serve --port "$port" >"$log" 2>&1 < /dev/null &
    local pid=$!
    disown "$pid" 2>/dev/null || true
    echo "==> Coordinator running on :$port (PID $pid)"
    echo "==> Tail with: tail -f $log"

    # Kill-source tracer. bpftrace at the kernel level captures
    # every signal sent to the coord PID with sender PID + comm
    # + parent PID. Filters out SIGURG (sig 23 — Go runtime
    # goroutine preemption noise, dozens per second) so the log
    # stays readable.
    #
    # Requires NOPASSWD sudoers entry for bpftrace:
    #   tamer ALL=(ALL) NOPASSWD: /usr/bin/bpftrace
    # When that's not configured (or sudo cache is cold), the
    # `sudo -n true` precheck skips the tracer silently — coord
    # still starts fine.
    # Sibling log of enju-coord.log; same rationale for living
    # under ~/.enju/ rather than the repo root.
    local sig_log="$HOME/.enju/enju-coord-signals.log"
    if [ -f "$sig_log" ]; then
        mv "$sig_log" "${sig_log}.prev"
    fi
    # Probe sudo with bpftrace itself, not `true` — the NOPASSWD
    # sudoers entry whitelists bpftrace specifically, so `sudo -n
    # true` would fail (no NOPASSWD for /usr/bin/true) even when
    # `sudo -n bpftrace` works fine.
    if command -v bpftrace >/dev/null 2>&1 && sudo -n bpftrace --version >/dev/null 2>&1; then
        # nohup runs as tamer (no sudo needed for nohup itself);
        # nohup then invokes sudo bpftrace, which IS whitelisted.
        # Reverse order (sudo nohup) would try to run nohup under
        # sudo and fail — NOPASSWD whitelist names bpftrace, not
        # every shell utility we wrap it in.
        nohup sudo bpftrace -e "
          tracepoint:signal:signal_generate /args->pid == $pid && args->sig != 23/ {
            time(\"%H:%M:%S \");
            printf(\"sig=%d to coord from pid=%d (%s) ppid=%d\\n\",
              args->sig, pid, comm, curtask->real_parent->tgid);
          }
        " >"$sig_log" 2>&1 < /dev/null &
        disown $! 2>/dev/null || true
        echo "==> Kill-source tracer running (logs: $sig_log)"
    else
        echo "==> Kill-source tracer NOT running (no NOPASSWD sudoers for bpftrace)"
    fi
}

# ---------------------------------------------------------------
# Help / dispatch
# ---------------------------------------------------------------

cmd_help() {
    cat <<EOF
Usage: ./build.sh <command> [args]

Build / run:
  build              Compile the enju binary.
  run                Build, then run the coordinator on :8000.
  clean              Remove the binary and stray *.db / test output.

Tests:
  test               Unit + integration tests (fast, no LLM).
  test-v             Same, verbose.
  test-llm           Full suite with ENJU_LLM_TEST=1 (costs tokens).
  test-mcp           Just the MCP-layer integration suite.

Lints:
  check-imports      Coordinator / fat-client / common / enjumcp boundary.
  check-chokepoint   SQL exec funnel + TestEngineVersion confinement.
  lint               Run all architectural lints.

Dev workflow:
  dev-restart [PORT] [--wipe]
                     Build + restart coordinator (default port 8333).
                     Preserves DB + workspaces by default. Pass --wipe
                     to clear state (credentials always preserved).

  help               Show this message.
EOF
}

cmd="${1:-help}"
shift || true
case "$cmd" in
    build)            cmd_build "$@" ;;
    run)              cmd_run "$@" ;;
    clean)            cmd_clean "$@" ;;
    test)             cmd_test "$@" ;;
    test-v)           cmd_test_v "$@" ;;
    test-llm)         cmd_test_llm "$@" ;;
    test-mcp)         cmd_test_mcp "$@" ;;
    check-imports)    cmd_check_imports "$@" ;;
    check-chokepoint) cmd_check_chokepoint "$@" ;;
    lint)             cmd_lint "$@" ;;
    dev-restart)      cmd_dev_restart "$@" ;;
    help|-h|--help)   cmd_help ;;
    *)
        echo "unknown command: $cmd" >&2
        echo "run './build.sh help' for available commands" >&2
        exit 1
        ;;
esac
