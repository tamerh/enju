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
//   - ClaudeHandler — spawns `claude -p` (the canonical local-LLM
//     bridge: it owns model invocation, MCP host launch, tool
//     allowlisting, streaming output).
//   - StubHandler — canned responses for tests so the daemon loop
//     can be exercised without an LLM subprocess.
//
// Future implementations (deliberately not built yet — extend when
// a real bot needs them):
//
//   - ShellHandler — runs an arbitrary command and captures stdout.
//     Lets a "linter-bot" wrap golangci-lint / pyright / similar.
//   - RuleHandler — deterministic verdicts from a declarative
//     ruleset. Useful for review bots that only care about commit
//     metadata, file paths, etc.
//   - Other LLM backends (OpenAI, Gemini, local llama.cpp, ...).
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

// HandlerType is the manifest discriminator for which Handler
// implementation a bot uses. Default is HandlerTypeClaude when the
// manifest omits the handler: field — preserves back-compat with
// pre-Phase-7.2 manifests where every bot was implicitly Claude.
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
// Empty Handler field is treated as "claude" — every pre-existing
// manifest stays valid without an opt-in migration.
func NewHandler(b *Bot) (Handler, error) {
	switch HandlerType(b.Handler) {
	case "", HandlerTypeClaude:
		return NewClaudeHandler(b), nil
	case HandlerTypeStub:
		return NewStubHandler(), nil
	}
	return nil, fmt.Errorf("bot %q: unknown handler type %q (supported: claude, stub)", b.Name, b.Handler)
}
