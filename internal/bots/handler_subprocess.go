// SubprocessHandler — spawns any external binary that satisfies
// enju's handler protocol and captures its stdout as the task's
// response.
//
// Per-run-snapshot redesign Phase 4b renamed this from
// ClaudeHandler and generalized it: the binary name comes from
// the bot manifest's `handler:` field rather than being
// hardcoded to "claude". LLM CLIs (claude, gemini, anything that
// follows the convention), shell scripts, custom Go binaries,
// rule-based reviewers — all are first-class handlers as long
// as they satisfy the protocol.
//
// The protocol (full version at docs/handler-protocol.md):
//
//	enju spawns:  <binary> [--<key1> <value1> ...]   (HandlerArgs)
//	stdin       = rendered task prompt
//	stdout      = response text (the submission)
//	exit 0      = success
//	exit ≠ 0    = failure; stderr surfaced in the wrapped error
//	cwd         = the bot's worktree (P4b) or ephemeral CWD (P4c)
//	env         = ENJU_TASK_ID / ENJU_ACTION /
//	              ENJU_SYSTEM_PROMPT / ENJU_REPO_DIR /
//	              ENJU_GIT_DIR / ENJU_BRANCH / ENJU_SCRATCH /
//	              ENJU_REVIEW_FEEDBACK (plus inherited daemon env)
//
// Why a subprocess protocol instead of in-Go handlers? Out-of-
// tree extensibility. Operators can write handlers in any
// language without forking enju. LLM CLIs already own their own
// retries, streaming, MCP host launch, tool allowlisting; we
// don't re-implement any of that here.

package bots

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
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
	// back-compat with pre-Phase-4b manifests where every bot
	// was implicitly claude.
	Binary string

	// Model is the model id passed via `--model <X>`. Empty =
	// suppress the flag entirely — handlers that don't drive an
	// LLM (shell scripts, rule-based bots) leave this blank.
	// Non-empty values land as a model flag regardless of the
	// target binary's exact flag name; if a CLI uses a
	// different name, the operator wraps it in a shell script
	// or omits Model and provides the equivalent via
	// HandlerArgs.
	Model string

	// AllowTools is the claude-specific MCP allowlist passed via
	// `--allowedTools`. Empty = no allowlist; non-claude
	// handlers naturally ignore it (the flag is just unrecognized
	// argv they reject — operators who don't want it set the
	// bot's mcp_tools to nil).
	//
	// Long-term: this should move into HandlerArgs as
	// {allowed-tools: "a,b,c"} so it's not special-cased. Kept
	// dedicated for now to preserve the pre-Phase-4b behavior of
	// the validator that already understands MCPTools as a
	// distinct concept.
	AllowTools []string

	// HandlerArgs are bot-level extra CLI flags. Translated to
	// argv via:
	//   {key: "value"}  → --<key> <value>
	//   {key: "true"}   → --<key>           (bare flag)
	//   {key: "false"}  → (omitted)
	//   {key: ""}       → --<key>           (bare flag)
	// Keys are sorted at invoke time for deterministic argv.
	// Per-task overrides (TaskDef.HandlerArgs) merge on top at
	// ProcessTask time with task-wins semantics.
	HandlerArgs map[string]string

	// Timeout caps per-invocation wall-clock. Zero = no
	// per-handler timeout (caller's ctx is the only bound).
	Timeout time.Duration
}

// NewSubprocessHandler builds a handler from a manifest entry.
// Reads handler (binary name), model, mcp_tools.allow, and
// handler_args off the bot.
func NewSubprocessHandler(b *Bot) *SubprocessHandler {
	bin := b.Handler
	if bin == "" {
		// Pre-Phase-4b default: empty handler meant claude.
		bin = string(HandlerTypeClaude)
	}
	h := &SubprocessHandler{
		Binary: bin,
		Model:  b.Model,
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
//   - Path containing `/`: stat the path directly; must exist
//     and be executable.
//   - Bare name: resolved via exec.LookPath ($PATH).
//   - "stub" / "claude" / any name: handled identically here;
//     the NewHandler switch in handler.go routes "stub" away
//     from SubprocessHandler before Preflight runs.
func (h *SubprocessHandler) Preflight() error {
	bin := h.Binary
	if bin == "" {
		bin = "claude"
	}
	if strings.ContainsAny(bin, "/\\") {
		info, err := os.Stat(bin)
		if err != nil {
			return fmt.Errorf("handler binary %q: %w", bin, err)
		}
		if info.IsDir() {
			return fmt.Errorf("handler binary %q: is a directory", bin)
		}
		// Executable bit check (any of user/group/other). On
		// Windows the executable bit isn't meaningful; skip
		// the check there if we ever cross-compile.
		if info.Mode().Perm()&0o111 == 0 {
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
	bin := h.Binary
	if bin == "" {
		bin = "claude"
	}

	args := []string{"-p"}
	if h.Model != "" {
		args = append(args, "--model", h.Model)
	}
	if in.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", in.SystemPrompt)
	}
	if len(h.AllowTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(h.AllowTools, ","))
	}

	// Merge bot-level + task-level HandlerArgs. Task wins on
	// collision. Done at invoke time (not handler construction)
	// because the override is per-claim.
	merged := mergeHandlerArgs(h.HandlerArgs, in.HandlerArgs)
	args = append(args, handlerArgsToArgv(merged)...)

	if h.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	// WaitDelay caps how long Wait() blocks after ctx is done.
	// Without it, an I/O-copy goroutine can hang in Read() even
	// after the process is SIGKILL'd, holding the daemon up
	// behind a dead subprocess. 5s is generous: the process is
	// already dead by then; this just bounds Wait's tail.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = bytes.NewReader([]byte(in.Prompt))
	cmd.Dir = in.Workspace
	cmd.Env = buildSubprocessEnv(in)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return HandlerOutput{}, fmt.Errorf("%s -p failed: %w (stderr: %s)", bin, err, truncateStderr(stderr.String()))
	}
	return HandlerOutput{Response: stdout.String()}, nil
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

// handlerArgsToArgv translates HandlerArgs map entries to a
// stable, deterministic argv slice. Convention:
//
//   - {key: "value"}  → --<key> value
//   - {key: "true"}   → --<key>          (bare flag)
//   - {key: "false"}  → (omitted)
//   - {key: ""}       → --<key>          (bare flag)
//
// Keys are sorted for deterministic argv shape — helps tests,
// log-grepping, and any cache-key stability work later. Values
// pass through exec.Command's argv handling, which is safe
// against shell metacharacters by construction (each arg is one
// syscall slot; no shell is spawned).
func handlerArgsToArgv(args map[string]string) []string {
	if len(args) == 0 {
		return nil
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		v := args[k]
		flag := "--" + k
		switch v {
		case "false":
			// Explicitly off — omit the flag entirely. Most
			// CLIs treat absence as the default; passing
			// `--foo false` would be wrong for bare-flag CLIs
			// and a noop for value-flag CLIs.
			continue
		case "true", "":
			// Bare flag — present without a value.
			out = append(out, flag)
		default:
			out = append(out, flag, v)
		}
	}
	return out
}

// buildSubprocessEnv assembles the env slice passed to the
// subprocess. Inherits the daemon's env (so PATH, HOME,
// ANTHROPIC_API_KEY, etc. carry through) and overlays the
// handler-protocol vars when they're populated on the
// HandlerInput.
//
// Empty values are suppressed rather than exported empty — that
// keeps test fixtures (which leave the new fields blank) from
// confusing CLIs that distinguish "unset" from "set-to-empty."
//
// Protocol vars (from docs/handler-protocol.md):
//   - ENJU_TASK_ID         coord task identifier
//   - ENJU_ACTION          answer|compute|contribute|review|vote
//   - ENJU_SYSTEM_PROMPT   bot's system prompt body
//   - ENJU_REPO_DIR        run snapshot (frozen project tree)
//   - ENJU_GIT_DIR         operator's .git/ (read-only convention)
//   - ENJU_BRANCH          this run's branch name
//   - ENJU_SCRATCH         writable workspace (CWD today)
//   - ENJU_REVIEW_FEEDBACK reviewer prose on iter > 1
func buildSubprocessEnv(in HandlerInput) []string {
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
	return env
}

// osEnviron is a seam for tests that want to control the
// inherited env. Production: os.Environ(). Tests override by
// setting testEnviron at the package level.
func osEnviron() []string {
	if testEnviron != nil {
		return testEnviron()
	}
	return os.Environ()
}

// testEnviron, when non-nil, replaces os.Environ in tests. Keep
// as a package var so production never picks up a dependency on
// runtime env-table mutation.
var testEnviron func() []string

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
