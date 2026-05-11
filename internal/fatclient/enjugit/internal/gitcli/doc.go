// Package gitcli is the git-CLI shellout implementation of the
// Ops interface. It exists alongside the v5- and v6-backed git
// packages during the migration off go-git. Every method delegates
// to the system `git` binary, parsing porcelain/plumbing output
// where structured results are needed and stderr where typed
// errors are needed.
//
// Why CLI: enju is a developer-facing tool whose users already
// have a full git environment on PATH. The Go-reimplementation
// tax (alpha bugs in v6, preserve.go-style workarounds in v5,
// semantic drift from real git) buys us nothing here — the
// trade-offs that justify go-git in CI tools / embedded servers
// / cross-platform libraries don't apply. Going to the reference
// implementation eliminates an entire class of library-quirk
// debugging.
//
// Design choices:
//   - Fresh subprocess per call. No long-running `git cat-file
//     --batch` reuse. Simple, robust; subprocess overhead is
//     negligible at enju's call volume (dozens/min, not
//     thousands/sec).
//   - All git invocations go through the runGit chokepoint
//     (run.go) so timeouts, env-var setup, and stderr → typed-
//     error mapping live in one place.
//   - Auth has no plumbing layer. `git` inherits the operator's
//     SSH agent / credential helper config exactly as it does
//     at the shell — no client.WithSSHAuth, no per-call Auth
//     options.
//   - The Ops interface contract is identical to gitv5 /
//     gitv6. As of 2026-05-12 gitcli is the sole backend
//     imported by enjugit; gitv5 and gitv6 remain in-tree
//     temporarily for diff-side reviewability and get deleted
//     in Phase 10 cleanup.
package gitcli
