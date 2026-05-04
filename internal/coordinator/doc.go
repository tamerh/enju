// Package coordinator is the server side: state.db, events.db,
// HTTP API, cascade, scheduler.
//
// Coordinator owns the source of truth for project state. It
// answers HTTP queries, runs the readiness cascade, persists
// events. It MUST NOT import anything from internal/fatclient/ —
// the runtime separation is enforced at lint time via
// ./build.sh check-imports.
//
// Coordinator may import internal/common/* for shared pure logic.
package coordinator
