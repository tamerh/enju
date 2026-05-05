// LLM backend abstraction for the bot runner.
//
// Each backend turns (system prompt, task prompt, model) into
// the model's text response. Backends are swappable so tests
// can inject a deterministic stub instead of shelling out to
// claude / openai / etc.
//
// The interface is deliberately narrow — the runner doesn't
// stream tokens, doesn't pass tool schemas, doesn't manage
// context windows. Anything richer than "give me text back"
// belongs in a future MCP-host-spawning backend (Phase 2.2+
// follow-up) where the LLM gets MCP tools to read/edit files
// itself. Walking-skeleton scope = text-in, text-out.

package bots

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// LLMBackend invokes a language model with a system prompt and
// a task prompt, returning the model's text response. Errors
// surface to the runner which logs them and either retries the
// task or marks the iteration failed depending on the error
// shape.
//
// Implementations must be safe for concurrent use — the runner
// is single-iteration today but Phase 4+ (per-bot daemon
// supervision) may call into the same backend instance from
// goroutines if multiple bots share an executor process.
type LLMBackend interface {
	Invoke(ctx context.Context, in LLMRequest) (string, error)
}

// LLMRequest is the per-invocation payload. Aggregated into a
// struct (vs positional args) so adding fields later — temperature,
// max-tokens, attachments — doesn't break every backend signature.
type LLMRequest struct {
	Model        string // e.g. "claude-sonnet-4-6"
	SystemPrompt string // bot's persona / role instructions
	TaskPrompt   string // the task's prompt + injected upstream context
}

// StubBackend returns a canned response without invoking any
// real model. Used by tests to drive the runner deterministically
// — the runner's claim/submit roundtrip is the system under test,
// not the LLM.
//
// If RespondWith is empty, returns a sentinel string so the
// failure mode (test forgot to set the response) is loud.
type StubBackend struct {
	RespondWith string
	// Err lets a test inject a failure path through the
	// backend (network error, rate limit, etc.) so the runner's
	// error handling can be exercised without flakiness.
	Err error
	// Calls increments on every Invoke. Tests assert against
	// it to confirm the runner reached the LLM step.
	Calls int
}

func (s *StubBackend) Invoke(ctx context.Context, in LLMRequest) (string, error) {
	s.Calls++
	if s.Err != nil {
		return "", s.Err
	}
	if s.RespondWith == "" {
		return "<stub: no canned response set>", nil
	}
	return s.RespondWith, nil
}

// ClaudeBackend shells out to `claude -p` (claude code CLI in
// non-interactive mode). The CLI must be on PATH. Composition
// rule:
//
//	claude -p --model=<model> --append-system-prompt=<system>
//	         <task prompt on stdin>
//
// stdin gets the task prompt (no escaping concerns), stdout is
// the response we capture. Stderr is logged separately by the
// runner so a non-zero exit surfaces enough context for triage.
//
// Model attribution: the --model flag tells claude which model
// to use; the runner separately passes Model to enju_submit_result
// so the coord records the right model_id.
type ClaudeBackend struct {
	// Path overrides the executable lookup. Empty = "claude"
	// (resolved via PATH). Tests set this to a fake binary.
	Path string
	// ExtraArgs are appended after the standard flags. Reserved
	// for advanced operator config (e.g. --temperature, --effort
	// high). Manifest doesn't expose these yet — when it does,
	// the bot.go file builds them and passes through.
	ExtraArgs []string
	// Timeout caps the per-invocation wall-clock. Default 5min
	// applied by the runner if zero. Long-tail LLM hangs
	// shouldn't pin a worktree forever.
	Timeout time.Duration
}

func (c *ClaudeBackend) Invoke(ctx context.Context, in LLMRequest) (string, error) {
	bin := c.Path
	if bin == "" {
		bin = "claude"
	}
	args := []string{"-p", "--model", in.Model}
	if in.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", in.SystemPrompt)
	}
	args = append(args, c.ExtraArgs...)

	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// WaitDelay caps how long Wait() blocks after the context
	// is done. Without it, I/O-copy goroutines may hold up Wait
	// forever even after SIGKILL fires (the process dies but
	// the stdout-copy goroutine hangs in read until something
	// closes the pipe — usually fine, but on some Linux kernels
	// or with subprocess sessions that delay propagation, the
	// race is uncomfortable). 5s is generous: the process is
	// already dead by then; this just bounds Wait()'s tail.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = bytes.NewReader([]byte(in.TaskPrompt))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Surface stderr in the error so the runner's log
		// captures what the LLM CLI complained about — better
		// than just "exit status 1."
		return "", fmt.Errorf("claude -p failed: %w (stderr: %s)", err, truncateForError(stderr.String()))
	}
	return stdout.String(), nil
}

// truncateForError keeps stderr in the error string short enough
// to be log-friendly. Full stderr is captured separately by the
// runner for the daemon's log file.
func truncateForError(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
