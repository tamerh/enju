// Bot Handler — the bot's "brain."
//
// A Handler turns a rendered prompt + task context into a textual
// response. Everything else (claim, workspace prep, git commit,
// submit) lives in the fatclient. The daemon is a thin loop that
// glues the two together:
//
//	loop:
//	  task := fc.Claim(...)
//	  out  := handler.ProcessTask(ctx, taskBrief)
//	  fc.Submit(task, out.Response)
//
// Implementations included here:
//
//   - SubprocessHandler — spawns any external binary that
//     satisfies the handler protocol (claude, gemini, custom
//     shell scripts, rule-based reviewers, anything that fits).
//     See handler_subprocess.go and docs/handler-protocol.md.
//   - StubHandler — canned responses for tests so the daemon
//     loop can be exercised without spawning a subprocess.
//
// New handlers are NOT added here. Under the plug-in model
// SubprocessHandler implements, "different handler" means
// "different binary the SubprocessHandler invokes" — a
// ShellHandler / RuleHandler / HTTPHandler is just an
// operator-provided script that satisfies the protocol, no
// Go change needed.
//
// In-tree bespoke handlers remain possible (write a new file,
// add a NewHandler case) only when a binary genuinely can't fit
// the protocol — interactive CLIs, multi-turn state machines,
// transport-level customization. None exist today.
//
// The interface is intentionally narrow. Anything richer than
// "text in, text out" (streaming, tool schemas, multi-turn) is a
// new method, not a wider input struct — handlers that don't need
// it shouldn't have to think about it.

package bots

import (
	"context"
	"fmt"
)

// Handler is the bot's brain — anything that can compute a textual
// response for a claimed task.
type Handler interface {
	ProcessTask(ctx context.Context, in HandlerInput) (HandlerOutput, error)
}

// HandlerInput is the per-task brief the daemon hands to the
// handler. Struct (vs positional args) so we can grow the brief
// without breaking implementations.
type HandlerInput struct {
	// TaskID is the coord task identifier (the full
	// "{project}:{run}:{def}" string the coord uses). Diagnostic
	// only — handlers don't talk to the coord; the fatclient
	// does.
	TaskID string

	// Action is one of answer / contribute / compute / review /
	// vote. Some handlers branch on action (a vote handler picks
	// an option, a review handler emits APPROVE/REQUEST_CHANGES);
	// most handlers don't care.
	Action string

	// Prompt is the already-rendered task prompt — every {{ref}}
	// substitution resolved by the fatclient. Hand it to the
	// model verbatim.
	Prompt string

	// SystemPrompt is the bot's system prompt content (read from
	// the manifest's system_prompt path). Empty for handlers
	// where a system prompt has no meaning (e.g. ShellHandler).
	SystemPrompt string

	// Workspace is the absolute path to the iteration checkout.
	// Handlers that need filesystem context (ShellHandler cd's
	// here, ClaudeHandler passes it as cwd so the LLM's MCP
	// tools see project-relative paths) read this; pure-text
	// handlers can ignore it.
	Workspace string

	// ReviewFeedback is the reviewer's prose from the previous
	// iteration's request_changes verdict (empty on iter-1 or
	// when the prior outcome wasn't request_changes). Daemons
	// fold this into the prompt so the LLM understands what the
	// reviewer asked to change. Surfaced as a separate field so
	// non-LLM handlers (rule-based, shell) can choose to ignore
	// it — for the Claude handler the daemon already prepends
	// it to the user-facing prompt.
	ReviewFeedback string

	// RepoDir is the absolute path to the run's frozen
	// snapshot — <project>/.enju/runs/<seq>-<slug>/snapshot/.
	// Exposed to the subprocess as $ENJU_REPO_DIR so handlers
	// (LLM or otherwise) can read project context that's pinned
	// to the run's base SHA, immune to operator edits on the
	// live working tree. Empty when the daemon hasn't resolved
	// the snapshot yet (legacy paths, test fixtures); the
	// subprocess sees no env var.
	RepoDir string

	// GitDir is the absolute path to the .git/ that holds the
	// project's full history. Exposed as $ENJU_GIT_DIR so
	// handlers can query history via `git --git-dir=...`
	// without needing a checked-out worktree. Read-only by
	// convention (the protocol forbids handlers from running
	// `git commit` / `git push` — commits are citizen actions
	// mediated by enju's submit path).
	GitDir string

	// Branch is the run's git branch name. Exposed as
	// $ENJU_BRANCH so handlers know what to pass to
	// `git --git-dir=$ENJU_GIT_DIR log $ENJU_BRANCH` for
	// in-run history (prior tasks' committed outputs).
	Branch string

	// HandlerArgs are per-task CLI-flag overrides that merge on
	// top of the bot's manifest-level HandlerArgs at invoke
	// time, with task-wins semantics. The subprocess handler
	// translates the merged map to `--key value` argv slots.
	// Empty when the task didn't override.
	HandlerArgs map[string]string
}

// HandlerOutput is what the handler returns to the daemon. The
// shape supports two parser disciplines:
//
//   - **Text-only**: handler returns plain Response, daemon does
//     action-specific parsing (review-verdict heuristics, vote-
//     option scan). This is the default for `claude -p` style
//     handlers that emit free text — the daemon's
//     parseReviewResponse / parseVoteResponse own the
//     heuristics so the handler author doesn't need to.
//
//   - **Structured**: handler pre-extracts Decision (review) or
//     Option (vote) and sets the field directly. The daemon
//     uses it verbatim and skips text parsing entirely. This is
//     for handlers that KNOW the response shape — JSON-mode
//     LLMs, rule-based RuleHandlers, ShellHandler wrapping a
//     linter that exit-codes its verdict, custom prompt
//     conventions ("VERDICT: approve" instead of "DECISION:").
//
// The text-only path keeps `claude -p` ergonomic; the structured
// path keeps the daemon out of the business of guessing every
// user's response convention. Users plugging in their own
// Handler implementation pick whichever path fits their data.
//
// Field semantics:
//
//   - Response: rationale / body / free text. Always required.
//     For action=answer/contribute/compute it IS the submission.
//     For review/vote it's the rationale; the daemon submits it
//     as Content.
//
//   - Decision: review verdict. Empty = "use text parsing on
//     Response." Non-empty = "use this directly" — must be one
//     of approve/request_changes/reject/comment (lowercase). The
//     daemon trusts but normalizes (lowercases, trims trailing
//     punctuation).
//
//   - Option: vote choice. Same shape: empty falls back to text
//     parsing, non-empty is used verbatim.
type HandlerOutput struct {
	Response string

	// Decision is the pre-extracted review verdict. Optional —
	// empty triggers the daemon's text-parsing fallback over
	// Response. Set by handlers that produce structured output
	// (JSON-mode LLM, rule-based handler, custom-marker parser).
	Decision string

	// Option is the pre-extracted vote choice. Same optionality
	// as Decision.
	Option string
}

// HandlerType is the manifest discriminator. Today only two
// values get special treatment in NewHandler:
//
//   - HandlerTypeStub  → in-process StubHandler (testing).
//   - empty            → SubprocessHandler with binary="claude"
//                        (back-compat with pre-Phase-4b manifests).
//
// Any OTHER value is treated as the name of an external binary
// — claude, gemini, ./bin/my-linter, /opt/foo, whatever. The
// SubprocessHandler invokes it per the handler protocol; no
// in-tree code per LLM or per tool.
//
// HandlerTypeClaude is retained as a constant because it remains
// the implicit default for empty-handler manifests; "stub" is
// the only OTHER name that needs an enum-like reference.
type HandlerType string

const (
	HandlerTypeClaude HandlerType = "claude"
	HandlerTypeStub   HandlerType = "stub"
)

// NewHandler returns the Handler implementation for a manifest
// entry. The daemon calls this once at startup; the returned
// Handler is reused across iterations for the lifetime of the
// daemon process.
//
// Routing (see docs/handler-protocol.md):
//   - handler: "stub"      → in-process StubHandler
//   - handler: ""          → SubprocessHandler("claude")
//                             (back-compat default)
//   - handler: <anything>  → SubprocessHandler(<anything>) where
//                             <anything> is the binary name
//                             (resolved via $PATH) or an absolute
//                             / repo-relative path
//
// The switch never grows. Adding a new "handler type" means
// providing a binary that satisfies the protocol — no Go change.
// projectRoot anchors a repo-relative handler: path (e.g.
// ./bin/foo.sh) to the project clone root; pass "" when the
// caller has no project context (StubHandler ignores it; an
// empty root is a documented no-op). Threading it through the
// constructor — rather than a mutate-after-construct call —
// makes the repo-relative anchor unforgettable by construction.
func NewHandler(b *Bot, projectRoot string) (Handler, error) {
	if HandlerType(b.Handler) == HandlerTypeStub {
		return NewStubHandler(), nil
	}
	return NewSubprocessHandler(b, projectRoot), nil
}

// Preflighter is implemented by handlers that want a startup
// readiness check before the daemon enters its poll loop. The
// SubprocessHandler implements this to verify the configured
// binary is locatable + executable; the StubHandler doesn't
// need it. Daemon callers do an interface assertion and skip
// the check for handlers that don't implement it.
//
// Returning a non-nil error from Preflight fails daemon startup
// with a clear actionable message (typo'd path, missing binary,
// wrong permissions) instead of silently waiting until the
// first claim to surface the problem.
type Preflighter interface {
	Preflight() error
}

// ClaimCWDOptOut is the opt-OUT seam for per-claim ephemeral
// CWD materialization. Handlers that don't need a project-
// shaped working directory — StubHandler in tests, future
// rule-based handlers that operate purely on coord state —
// implement this returning true. The daemon skips the
// (potentially expensive) iter-branch tree walk for them.
//
// Default (handler doesn't implement the interface) is
// "needs CWD": the daemon prepares the ephemeral CWD.
// SubprocessHandler doesn't implement this — it always
// wants the CWD.
//
// "OptOut" naming makes the polarity explicit: returning
// false skips the materialization. The alternative
// (NeedsClaimCWD with default false) would silently skip
// materialization for any new handler that forgot the
// method, producing confusing "where are my files" failures.
//
// Why a Go-side interface and not a manifest field: the
// opt-out is a property of the handler IMPLEMENTATION (does
// the handler use its CWD or not?), not of the operator's
// choice — a stub handler can never sensibly want a CWD,
// regardless of how the operator configured the bot. A
// manifest field would let operators override the handler's
// intent, which is rarely what they want and easy to
// misconfigure.
type ClaimCWDOptOut interface {
	SkipClaimCWD() bool
}

// Ensure SubprocessHandler satisfies Preflighter at compile time
// so future refactors can't silently drop the method.
var _ Preflighter = (*SubprocessHandler)(nil)

// keep fmt used so future error wraps in this file build clean
var _ = fmt.Errorf
