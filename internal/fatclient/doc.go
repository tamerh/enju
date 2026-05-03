// Package fatclient is the client side: project clone, MCP tool
// handlers, supervisor, git operations, inbox/notify projection.
//
// Fat-client owns everything that lives on the user's machine:
// the workspace, the live.jsonl event log, the local git repo.
// It talks to the coordinator exclusively over HTTP — no direct
// DB access. It MUST NOT import anything from
// internal/coordinator/ — enforced at compile time via
// tools/check-imports.sh.
//
// Fat-client may import internal/common/* for shared pure logic.
package fatclient
