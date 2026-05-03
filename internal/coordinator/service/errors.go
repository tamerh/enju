// Package service is the coordinator's shared business-logic
// layer. Read and write operations live here as pure Go
// functions; transport layers (REST in internal/coordinator/api,
// future Web UI) are thin presenters that translate their wire
// format → service call → response.
//
// Design contract:
//   - Service functions take typed inputs (params struct) +
//     a *store.CitizenRecord caller (never nil — auth is a
//     transport concern).
//   - Service functions return typed responses + sentinel
//     errors. Transports map sentinels to wire-appropriate
//     status codes.
//   - Service functions never touch HTTP, MCP, or HTML — they
//     read and write the store.
//
// MCP today is fat-client only: tools translate via REST. If
// hosted-mode adds a coord-embedded MCP transport, it would
// add a thin handler package that calls the same service
// functions — but that's not built yet.
package service

import "errors"

// ErrNotFound — the requested resource doesn't exist.
// REST → 404; MCP → "X not found" tool error.
var ErrNotFound = errors.New("not found")

// ErrNotMember — the caller isn't a member of the project
// they're trying to read or write. REST → 403; MCP →
// "not a member of this project" tool error.
var ErrNotMember = errors.New("not a member of this project")
