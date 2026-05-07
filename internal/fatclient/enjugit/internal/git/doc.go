// Package git wraps go-git with Enju's concurrency model.
//
// This package is the plumbing layer beneath enjugit. It knows about
// refs, commits, branches, conflicts, push, fetch — pure git
// concepts. It does NOT know about tasks, iterations, citizens,
// trailers, writes_artifacts, template snapshots, or any other
// Enju-specific concept. If a method needs an Enju concept to do
// its job, that method belongs in enjugit, not here.
//
// # Importable from
//
// Only by enjugit. Go's internal/ rule enforces this. The
// enclosing package path is internal/fatclient/enjugit/internal/git
// so any package outside enjugit/ that tries to import it fails
// to compile. This is the whole point: workflow decisions live in
// enjugit, plumbing lives here, and the boundary is unbypassable.
//
// # Forbidden imports
//
// The git package may import only:
//
//   - github.com/go-git/go-git/v5 and subpackages
//   - github.com/gofrs/flock (cross-process locking)
//   - Go standard library
//   - log/slog
//
// It must NOT import anything under github.com/enju-ai/enju/.
// A lint check enforces this.
//
// # Concurrency model
//
// Each Clone owns two locks:
//
//   - sync.Mutex for in-process serialization (multiple goroutines
//     in the same process touching the same clone)
//   - flock for cross-process serialization (multiple processes
//     sharing the same workdir, e.g. Claude Desktop + Claude Code
//     running enju mcp simultaneously)
//
// All mutating methods on Clone acquire both locks internally.
// Read methods (ReadFileAtCommit, Head, ResolveRef, LocalBranches)
// do not acquire any lock. For atomic multi-op sequences, callers
// use WithLock(fn) which holds both locks across the closure.
//
// # Worktree state model
//
// Each verb's contract names valid pre-states and the post-state.
// State() returns the current state. Invalid pre-state returns
// ErrInvalidWorktreeState rather than silently corrupting.
//
// # Errors
//
// All errors are typed and exported. Each verb's docstring lists
// which errors it can return. Callers use errors.Is, never
// string match.
package git
