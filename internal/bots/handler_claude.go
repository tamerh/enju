// ClaudeHandler — spawns `claude -p` (Claude Code's CLI in
// non-interactive mode) to produce the bot's response.
//
// Composition rule:
//
//	claude -p --model=<model>
//	          --append-system-prompt=<system>
//	          [--allowedTools=<tool1>,<tool2>,...]
//	          <task prompt on stdin>
//
// Stdin is the rendered task prompt (no escaping concerns at the
// process boundary); stdout is the response we capture; stderr is
// returned in the error message on non-zero exit so triage doesn't
// require digging in the daemon log.
//
// Why subprocess and not an HTTP client to Anthropic's API? The
// CLI already owns model invocation, MCP host launch, tool
// allowlisting, retries, and streaming — re-doing that in Go
// would be reimplementation, not abstraction. Other LLM backends
// (OpenAI, Gemini, local llama.cpp) get their own Handler
// implementation, not their own branch inside this one.

package bots

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ClaudeHandler is the production handler for LLM-driven bots.
type ClaudeHandler struct {
	// Model is the model id passed via --model. Empty = let the
	// CLI pick its default (usually whatever's set in
	// ~/.config/claude/config.json). The manifest validator
	// already requires Model, so the empty case shouldn't reach
	// here in practice.
	Model string

	// AllowTools is the MCP tool allowlist passed via
	// --allowedTools. Empty = no allowlist (CLI's default = all
	// tools the configured MCP host exposes).
	AllowTools []string

	// Path overrides the executable lookup. Empty = "claude"
	// resolved via PATH. Tests set this to a fake binary.
	Path string

	// Timeout caps per-invocation wall-clock. Zero = no
	// per-handler timeout (caller's ctx is the only bound).
	// Default applied by the daemon if the manifest doesn't
	// override.
	Timeout time.Duration
}

// NewClaudeHandler builds a ClaudeHandler from a manifest entry.
// Reads model + mcp_tools.allow off the bot; everything else
// (Path, Timeout) keeps zero defaults.
func NewClaudeHandler(b *Bot) *ClaudeHandler {
	h := &ClaudeHandler{Model: b.Model}
	if b.MCPTools != nil {
		h.AllowTools = append(h.AllowTools, b.MCPTools.Allow...)
	}
	return h
}

func (h *ClaudeHandler) ProcessTask(ctx context.Context, in HandlerInput) (HandlerOutput, error) {
	bin := h.Path
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return HandlerOutput{}, fmt.Errorf("claude -p failed: %w (stderr: %s)", err, truncateStderr(stderr.String()))
	}
	return HandlerOutput{Response: stdout.String()}, nil
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
