// Package gitv6 is the go-git/v6 implementation of the Ops
// interface. It exists alongside the v5-backed `git` package
// during the v5 → v6 migration so each method can be ported
// and verified in isolation. Once parity is proven, the
// enjugit Workflow flips its backend to gitv6 and `git` is
// retired.
//
// Why two implementations: v6 is a breaking release. The auth
// model was rewritten (transport.AuthMethod is gone; auth flows
// through client.WithSSHAuth / WithHTTPAuth registered on the
// transport client). Several function signatures changed
// (PlainClone dropped its isBare arg in favor of CloneOptions.Bare;
// PlainInit takes variadic InitOptions; FetchOptions/PushOptions/
// ListOptions no longer carry an Auth field). Doing those changes
// in-place would break the working v5 implementation; doing them
// side-by-side lets us land them piecemeal with tests that pin
// parity before we flip the default.
//
// What v6 buys us: native preservation of untracked files
// across Force checkouts (upstream PR #1903), so preserve.go
// disappears in this package. That's the load-bearing reason
// for the migration. Other v6 changes are migration tax we
// pay once.
//
// Migration phases (per project plan):
//   Phase A — scaffolding (this file + types/errors/ops + stub Clone)
//   Phase B — port read paths
//   Phase C — port write paths (with the auth rewrite)
//   Phase D — port checkout (drop preserve.go in v6)
//   Phase E — port merge (CLI shellout stays version-independent)
//   Phase F — backend gate + integration tests
//   Phase G — flip default + delete `git` package
package gitv6
