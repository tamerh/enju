// Package core is the leaf tree: pure logic, no I/O, no
// internal-package dependencies on either side.
//
// Anything in here may be imported by both internal/coordinator/
// and internal/fatclient/. To enforce that, core itself must not
// import internal/coordinator/* or internal/fatclient/* — see
// tools/check-imports.sh and docs/architecture-boundaries.md.
//
// What lives here:
//   - Pure value types (no DB tags, no HTTP wire concerns)
//   - State-machine rules ("given state X and event Y, what's allowed?")
//   - Path layout helpers (run dir, result dir, slug rules)
//   - YAML / template / DAG primitives
//
// What does NOT live here: anything that touches a database, the
// filesystem (beyond pure path computation), the network, or git.
package core
