// Package webui is the v1 browser UI for Enju, served by the
// `enju ui` subcommand. It is a peer consumer of
// internal/fatclient/service.FatClient — the same surface the
// MCP handlers consume — over an HTTP transport instead of stdio.
//
// Boundary rules (enforced by ./build.sh check-imports rule 5):
//
//   - webui MAY import internal/common/* and
//     internal/fatclient/service.
//   - webui MUST NOT import internal/fatclient/{workspace,inbox,
//     notify,mcphandlers,...} — reach through the FatClient
//     surface.
//   - webui MUST NOT import internal/coordinator/* — coord
//     access goes through FatClient → coord HTTP client.
//
// When the UI needs something the FatClient doesn't expose, the
// gap-fill discipline applies: raise the gap, add the method to
// FatClient, then consume. No reach-arounds.
//
// See web-ui-spec.md for the full spec.
package webui
