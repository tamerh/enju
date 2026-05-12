// SubprocessHandler — spawns any external binary that satisfies
// enju's handler protocol and captures its stdout as the task's
// response.
//
// Binary-agnostic by way of operator-authored argv. The bot
// manifest declares:
//   - handler:  the binary name (also the discriminator —
//                "stub" routes to StubHandler, everything else
//                here)
//   - args:     literal argv with {{var}} placeholders
//
// At invoke time, ProcessTask substitutes the placeholders from
// the bot's static config (model, system_prompt, allowed_tools)
// and the per-claim context (task_id, branch, repo_dir,
// git_dir, scratch, review_feedback, handler_args). Then
// exec's `<handler> <substituted-args...>` with the prompt on
// stdin.
//
// Enju has zero LLM-specific knowledge: no -p, no --model, no
// --append-system-prompt baked in. claude's flag conventions
// live in the operator's YAML, not in this file. Adding gemini
// or a custom binary = author the right `args:` list in the
// manifest; no Go change.
//
// The ENJU_* env vars also reach the subprocess for handlers
// that prefer env reads over argv substitution — same protocol
// either way. See docs/handler-protocol.md for the open
// contract.
//
// Empty-substitution rule: a {{var}} that resolves to an empty
// string causes its whole arg entry to drop from argv. So
// `--model={{model}}` disappears entirely when no model is
// set, rather than passing `--model=` to the binary.

package bots

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// SubprocessHandler invokes an external binary per the handler
// protocol. Production handlers — claude, gemini, custom shell
// scripts — all use this implementation; only the bot manifest's
// `handler:` field differs.
type SubprocessHandler struct {
	// Binary is the executable name (resolved via $PATH) or an
	// absolute / repo-relative path. Sourced from the bot's
	// `handler:` field. Empty defaults to "claude" for
	// back-compat.
	Binary string

	// Args is the manifest's argv template — the literal slice
	// of strings passed to the binary, after {{var}}
	// substitution. Sourced from Bot.Args. nil means "no args"
	// (the subprocess gets argv with just the binary name).
	//
	// At invoke time, ProcessTask resolves {{var}} placeholders
	// using bot-static values (model, system_prompt body,
	// allowed_tools) and per-claim context (task_id, branch,
	// repo_dir, git_dir, scratch, review_feedback, handler_args).
	// Empty substitutions cause the WHOLE arg to drop from argv.
	// See subArgs / argSubstitute below for the rule's exact
	// shape.
	Args []string

	// Model, SystemPromptPath, AllowTools are bot-static values
	// captured from the manifest at New time so ProcessTask can
	// resolve {{model}} / {{system_prompt}} / {{allowed_tools}}
	// without re-reading the manifest.
	//
	// SystemPromptPath is the path; the body is loaded lazily
	// at first ProcessTask call (and cached) so a startup
	// without read access to the prompt file still permits
	// daemon launch — the failure surfaces at first claim.
	Model            string
	SystemPromptPath string
	AllowTools       []string

	// HandlerArgs is the bot-level map exposed in the manifest
	// as `handler_args:`. The merged (bot ⨁ task) map is
	// available inside Args via the {{handler_args.<key>}}
	// substitution. The map is ALSO threaded through to the
	// subprocess in $ENJU_HANDLER_ARGS_<KEY> form (uppercased,
	// dashes → underscores) for handlers that prefer env reads.
	HandlerArgs map[string]string

	// Timeout caps per-invocation wall-clock. Zero = no
	// per-handler timeout (caller's ctx is the only bound).
	Timeout time.Duration

	// cachedSystemPrompt holds the body read from
	// SystemPromptPath on first use. Empty until populated.
	// Concurrent ProcessTask invocations on the same handler
	// would race here; mutex omitted since the daemon today
	// serializes invocations per bot (one claim at a time).
	// Worst-case race re-reads the file, which is idempotent.
	cachedSystemPrompt string
}

// effectiveBinary returns the binary name a manifest entry
// should spawn. Centralized so the empty-handler default lives
// in exactly one place (NewSubprocessHandler delegates here,
// Preflight delegates here, ProcessTask trusts h.Binary
// without a second fallback).
func effectiveBinary(b *Bot) string {
	if b == nil || b.Handler == "" {
		// Pre-Phase-4b default: empty handler meant claude.
		// Kept as a back-compat ramp; new manifests should
		// set handler: explicitly.
		return string(HandlerTypeClaude)
	}
	return b.Handler
}

// NewSubprocessHandler builds a handler from a manifest entry.
// Captures the bot's argv template + the static values that
// feed {{var}} substitution at invoke time.
func NewSubprocessHandler(b *Bot) *SubprocessHandler {
	h := &SubprocessHandler{
		Binary:           effectiveBinary(b),
		Model:            b.Model,
		SystemPromptPath: b.SystemPrompt,
	}
	if len(b.Args) > 0 {
		// Copy so manifest mutations after construction don't
		// affect this handler.
		h.Args = append([]string(nil), b.Args...)
	}
	if b.MCPTools != nil {
		h.AllowTools = append(h.AllowTools, b.MCPTools.Allow...)
	}
	if len(b.HandlerArgs) > 0 {
		// Copy so per-task merge in ProcessTask doesn't mutate
		// the manifest's shared map.
		h.HandlerArgs = make(map[string]string, len(b.HandlerArgs))
		for k, v := range b.HandlerArgs {
			h.HandlerArgs[k] = v
		}
	}
	return h
}

// Preflight verifies the handler binary is locatable before the
// daemon enters its poll loop. Without this check, a typo'd path
// or missing binary surfaces only at first claim — possibly
// hours or days into a long-running session. With it, the
// daemon fails at startup with a clear actionable error.
//
// Resolution rules:
//   - Path containing `/` or `\`: stat the path directly; must
//     exist and (on POSIX) carry an executable bit. Windows
//     skips the executable-bit check — the bit isn't meaningful
//     under NTFS/Windows's permission model.
//   - Bare name: resolved via exec.LookPath ($PATH).
//   - The NewHandler switch in handler.go routes "stub" away
//     from SubprocessHandler before Preflight runs, so we
//     don't special-case it here.
func (h *SubprocessHandler) Preflight() error {
	bin := h.Binary
	if bin == "" {
		// Defense in depth — NewSubprocessHandler populates
		// Binary via effectiveBinary so this branch should be
		// unreachable. Mirror the same default for safety.
		bin = string(HandlerTypeClaude)
	}
	if strings.ContainsAny(bin, "/\\") {
		info, err := os.Stat(bin)
		if err != nil {
			return fmt.Errorf("handler binary %q: %w", bin, err)
		}
		if info.IsDir() {
			return fmt.Errorf("handler binary %q: is a directory", bin)
		}
		// Executable bit check (any of user/group/other) on
		// POSIX systems. Windows uses NTFS ACLs + file
		// extensions for executable semantics; the +x bit
		// isn't surfaced there, so we'd false-fail every
		// Windows operator using a path-shaped handler.
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("handler binary %q: not executable (mode %v)", bin, info.Mode().Perm())
		}
		return nil
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("handler binary %q not on PATH: %w", bin, err)
	}
	return nil
}

func (h *SubprocessHandler) ProcessTask(ctx context.Context, in HandlerInput) (HandlerOutput, error) {
	// h.Binary is always populated by NewSubprocessHandler via
	// effectiveBinary; trust it. No second-place fallback.

	// Substitute {{var}} placeholders in Bot.Args. The
	// substitution context combines bot-static fields (model,
	// system prompt body, allowed tools) with per-claim
	// HandlerInput context. Empty substitutions drop the
	// whole arg.
	subCtx, sysPromptErr := h.subContext(in)
	if sysPromptErr != nil {
		return HandlerOutput{}, sysPromptErr
	}
	args, subErr := subArgs(h.Args, subCtx)
	if subErr != nil {
		return HandlerOutput{}, fmt.Errorf("argv template substitution: %w", subErr)
	}

	if h.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, h.Binary, args...)
	// WaitDelay caps how long Wait() blocks after ctx is done.
	// Without it, an I/O-copy goroutine can hang in Read() even
	// after the process is SIGKILL'd, holding the daemon up
	// behind a dead subprocess. 5s is generous: the process is
	// already dead by then; this just bounds Wait's tail.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = bytes.NewReader([]byte(in.Prompt))
	cmd.Dir = in.Workspace
	cmd.Env = buildSubprocessEnv(h, in)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return HandlerOutput{}, fmt.Errorf("%s failed: %w (stderr: %s)", h.Binary, err, truncateStderr(stderr.String()))
	}
	return HandlerOutput{Response: stdout.String()}, nil
}

// subContext assembles the {{var}} substitution context for a
// single ProcessTask invocation. Combines bot-static values
// (Model, AllowTools, system prompt body) with per-claim
// HandlerInput context. Returns an error only when
// SystemPromptPath is set but the file can't be read — first
// claim is where that surfaces if startup didn't preload.
func (h *SubprocessHandler) subContext(in HandlerInput) (map[string]string, error) {
	sysPrompt, err := h.systemPromptBody()
	if err != nil {
		return nil, err
	}
	ctx := map[string]string{
		"model":           h.Model,
		"system_prompt":   sysPrompt,
		"allowed_tools":   strings.Join(h.AllowTools, ","),
		"task_id":         in.TaskID,
		"action":          in.Action,
		"branch":          in.Branch,
		"repo_dir":        in.RepoDir,
		"git_dir":         in.GitDir,
		"scratch":         in.Workspace,
		"review_feedback": in.ReviewFeedback,
	}
	// handler_args.<key> entries. Merge bot + task with
	// task-wins semantics.
	merged := mergeHandlerArgs(h.HandlerArgs, in.HandlerArgs)
	for k, v := range merged {
		ctx["handler_args."+k] = v
	}
	return ctx, nil
}

// systemPromptBody returns the system prompt content,
// lazy-loading from SystemPromptPath on first call. Cached
// after that. Empty SystemPromptPath returns ("", nil) — the
// {{system_prompt}} substitution becomes empty and any arg
// referencing it gets dropped.
func (h *SubprocessHandler) systemPromptBody() (string, error) {
	if h.SystemPromptPath == "" {
		return "", nil
	}
	if h.cachedSystemPrompt != "" {
		return h.cachedSystemPrompt, nil
	}
	body, err := os.ReadFile(h.SystemPromptPath)
	if err != nil {
		return "", fmt.Errorf("read system prompt %q: %w", h.SystemPromptPath, err)
	}
	h.cachedSystemPrompt = string(body)
	return h.cachedSystemPrompt, nil
}

// subArgs walks the args template, substitutes {{var}}
// placeholders from ctx, and applies the empty-substitution
// rule: if any {{var}} in an arg resolves to empty, the WHOLE
// arg drops from argv. Args containing no {{var}} pass through
// verbatim regardless.
//
// Unknown {{var}} names (not in ctx) substitute to empty and
// trigger the drop rule — that's how a {{handler_args.foo}}
// referencing an unset handler arg disappears from argv.
//
// Returns an error only when the template syntax is malformed
// (unbalanced braces). Empty / missing values are not errors;
// they're a signal to drop the arg.
func subArgs(template []string, ctx map[string]string) ([]string, error) {
	if len(template) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(template))
	for i, raw := range template {
		expanded, hasRef, hasEmpty, err := argSubstitute(raw, ctx)
		if err != nil {
			return nil, fmt.Errorf("args[%d] %q: %w", i, raw, err)
		}
		// Drop rule: arg has at least one {{var}} AND at least
		// one of them resolved to empty.
		if hasRef && hasEmpty {
			continue
		}
		out = append(out, expanded)
	}
	return out, nil
}

// argSubstitute scans one arg string for {{var}} placeholders
// and replaces them with their context values. Returns the
// expanded string, a flag indicating whether ANY {{var}} was
// present, and a flag indicating whether any of the substituted
// values were empty.
//
// Brace conventions:
//   - "{{var}}" is a placeholder; var is any non-empty key
//     terminated by "}}".
//   - Literal "{" / "}" pass through unchanged when not part
//     of a matched "{{...}}".
//   - "{{" without a closing "}}" returns a malformed-template
//     error.
func argSubstitute(raw string, ctx map[string]string) (expanded string, hasRef bool, hasEmpty bool, err error) {
	var b strings.Builder
	i := 0
	for i < len(raw) {
		// Look for "{{".
		open := strings.Index(raw[i:], "{{")
		if open < 0 {
			b.WriteString(raw[i:])
			break
		}
		// Literal prefix.
		b.WriteString(raw[i : i+open])
		// Find matching "}}".
		rest := raw[i+open+2:]
		close := strings.Index(rest, "}}")
		if close < 0 {
			return "", false, false, fmt.Errorf("unterminated {{ at offset %d", i+open)
		}
		key := rest[:close]
		val, ok := ctx[key]
		_ = ok // we don't differentiate missing-key vs empty-value;
		//        both trigger the drop rule.
		hasRef = true
		if val == "" {
			hasEmpty = true
		}
		b.WriteString(val)
		i += open + 2 + close + 2
	}
	return b.String(), hasRef, hasEmpty, nil
}

// mergeHandlerArgs combines bot-level + task-level handler args.
// Task-level keys win on collision. Returns nil when both inputs
// are empty so callers branch on `len(merged) == 0` without a
// nil/empty dance.
func mergeHandlerArgs(botArgs, taskArgs map[string]string) map[string]string {
	if len(botArgs) == 0 && len(taskArgs) == 0 {
		return nil
	}
	out := make(map[string]string, len(botArgs)+len(taskArgs))
	for k, v := range botArgs {
		out[k] = v
	}
	for k, v := range taskArgs {
		out[k] = v
	}
	return out
}

// (handlerArgsToArgv removed — Phase 4b-r1.)
//
// The previous design auto-translated HandlerArgs to "--key value"
// argv slots. That was an enju-side convention layered on top of
// what the operator wrote, and the review noted the asymmetry:
// some keys made sense as flags (max-turns), others didn't, and
// the convention forced operators to either accept it or work
// around it. The new design exposes handler_args as substitution
// values ({{handler_args.<key>}}) so operators decide if and
// where each entry lands in argv.

// buildSubprocessEnv assembles the env slice passed to the
// subprocess. Inherits the daemon's env (so PATH, HOME,
// ANTHROPIC_API_KEY, etc. carry through) and overlays the
// handler-protocol vars when they're populated.
//
// Empty values are suppressed rather than exported empty — that
// keeps test fixtures (which leave fields blank) from confusing
// CLIs that distinguish "unset" from "set-to-empty."
//
// Two value sources:
//   - HandlerInput (per-claim): task id, action, prompt body,
//     repo dir, git dir, branch, scratch, review feedback.
//   - SubprocessHandler (per-bot): model, allowed-tools.
//
// The split mirrors the YAML: per-bot fields come from the
// manifest's Bot entry; per-claim fields come from the live
// task. Both end up as env vars in the subprocess — wrappers
// like examples/handlers/claude.sh read them and assemble the
// binary-specific argv.
//
// Protocol vars (from docs/handler-protocol.md):
//   - ENJU_TASK_ID         coord task identifier
//   - ENJU_ACTION          answer|compute|contribute|review|vote
//   - ENJU_SYSTEM_PROMPT   bot's system prompt body
//   - ENJU_REPO_DIR        run snapshot (frozen project tree)
//   - ENJU_GIT_DIR         project's .git/ for history reads
//   - ENJU_BRANCH          this run's branch name
//   - ENJU_SCRATCH         writable workspace (CWD)
//   - ENJU_REVIEW_FEEDBACK reviewer prose on iter > 1
//   - ENJU_MODEL           bot's model id (added Phase 4b-r1)
//   - ENJU_ALLOWED_TOOLS   comma-joined MCP tool allowlist
//                           (added Phase 4b-r1)
func buildSubprocessEnv(h *SubprocessHandler, in HandlerInput) []string {
	env := osEnviron()
	if in.TaskID != "" {
		env = append(env, "ENJU_TASK_ID="+in.TaskID)
	}
	if in.Action != "" {
		env = append(env, "ENJU_ACTION="+in.Action)
	}
	if in.SystemPrompt != "" {
		env = append(env, "ENJU_SYSTEM_PROMPT="+in.SystemPrompt)
	}
	if in.RepoDir != "" {
		env = append(env, "ENJU_REPO_DIR="+in.RepoDir)
	}
	if in.GitDir != "" {
		env = append(env, "ENJU_GIT_DIR="+in.GitDir)
	}
	if in.Branch != "" {
		env = append(env, "ENJU_BRANCH="+in.Branch)
	}
	if in.Workspace != "" {
		// ENJU_SCRATCH points at the writable CWD. Pre-P4c
		// this IS the bot's persistent worktree; P4c switches
		// it to the ephemeral per-claim dir under
		// .enju/scratch/<bot>/<task-iter>/. Env-var name stays
		// the same so handlers don't have to migrate.
		env = append(env, "ENJU_SCRATCH="+in.Workspace)
	}
	if in.ReviewFeedback != "" {
		env = append(env, "ENJU_REVIEW_FEEDBACK="+in.ReviewFeedback)
	}
	if h != nil {
		if h.Model != "" {
			env = append(env, "ENJU_MODEL="+h.Model)
		}
		if len(h.AllowTools) > 0 {
			env = append(env, "ENJU_ALLOWED_TOOLS="+strings.Join(h.AllowTools, ","))
		}
	}
	return env
}

// osEnviron is a seam for tests that want to control the
// inherited env. Production: os.Environ(). Tests override via
// setTestEnviron, which also installs the t.Cleanup to restore
// the previous value — review fix R4 (the prior raw mutable
// package var risked test cross-poisoning if a caller forgot to
// reset).
func osEnviron() []string {
	if testEnviron != nil {
		return testEnviron()
	}
	return os.Environ()
}

// testEnviron, when non-nil, replaces os.Environ inside the
// bots package. Tests MUST set this via setTestEnviron (which
// registers cleanup) rather than touching the var directly —
// the helper is the only intended-use entry point.
var testEnviron func() []string

// setTestEnviron installs a test-scoped osEnviron source and
// registers the t.Cleanup that restores the previous value at
// the end of the current test. Encodes the discipline the
// review (R4) flagged: any direct mutation of the package var
// risked poisoning later tests in the same package.
//
// Use:
//
//	setTestEnviron(t, func() []string { return []string{"PATH=/x"} })
//
// The previous value (often nil) is captured and restored
// regardless of test pass/fail.
func setTestEnviron(tb interface {
	Cleanup(func())
}, fn func() []string) {
	prev := testEnviron
	testEnviron = fn
	tb.Cleanup(func() { testEnviron = prev })
}

// truncateStderr keeps stderr in the wrapped error short enough
// to be log-friendly. Full stderr lands in the daemon log via the
// process's own stderr stream when the daemon is configured for
// passthrough; this is the inline-error variant.
func truncateStderr(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
