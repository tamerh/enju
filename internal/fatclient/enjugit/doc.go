// Package enjugit encodes Enju's policy for using git.
//
// This is the layer between service orchestration (which combines
// coord HTTP + git ops) and the raw git plumbing in
// internal/fatclient/enjugit/internal/git. It owns:
//
//   - Branch naming conventions (iter-N format, run-slug encoding)
//   - Fork-base policy (review iter from upstream's topic, dev iter
//     from run branch, etc.)
//   - Commit message + trailer composition (Enju-Verdict,
//     Enju-Iter-Seq, Enju-Task-ID, AI-Model, Co-Authored-By)
//   - Author identity per verb (citizen vs system)
//   - Per-project disk layout (<project>/enju/.bare.git/,
//     <project>/enju/bots/<name>/clone/)
//   - Iteration semantics (iter_seq → branch name)
//   - Template snapshots, scanner, refs, sharedroot — all the
//     Enju-specific concepts that operate on git
//
// The line is: if a method needs to know an Enju concept (task,
// iteration, citizen, trailer, writes_artifacts, template), it
// belongs here. If a method is pure git plumbing, it belongs in
// internal/git.
//
// # Importable from
//
// service is the only intended consumer outside enjugit. Runners
// (bots, webui, mcphandlers, compute) talk to service. service
// talks to enjugit.
//
// # Architecture
//
//	runner → service → enjugit → git (internal)
//	                       ↘ coord (HTTP, separate)
//
// # The Conventions seam
//
// All policy that varies — branch naming, system author identity,
// trailer order, default branches, disk layout — lives in a
// Conventions struct. NewProductionConventions() returns the
// canonical Enju defaults. Tests pass synthetic conventions to
// pin specific behaviors.
//
// # Test strategy
//
// Workflow methods depend on git.Ops (interface), not *git.Clone.
// Workflow tests pass a fake git.Ops that records calls and
// returns canned responses. Each verb's contract becomes
// "right git ops invoked, in right order, with right author and
// trailers." Property tests assert pre/post worktree state and
// documented errors fire when expected.
package enjugit
