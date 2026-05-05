package bots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStubBackend_HappyPath(t *testing.T) {
	s := &StubBackend{RespondWith: "hello from stub"}
	out, err := s.Invoke(context.Background(), LLMRequest{Model: "m", TaskPrompt: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello from stub" {
		t.Errorf("got %q, want canned response", out)
	}
	if s.Calls != 1 {
		t.Errorf("Calls: got %d, want 1", s.Calls)
	}
}

func TestStubBackend_EmptyResponse(t *testing.T) {
	// Loud sentinel rather than silent empty — tests that
	// forget to set RespondWith should fail visibly, not pass
	// because submitting "" looked plausible.
	s := &StubBackend{}
	out, _ := s.Invoke(context.Background(), LLMRequest{})
	if !strings.Contains(out, "stub") {
		t.Errorf("expected sentinel-like response, got %q", out)
	}
}

func TestStubBackend_ErrorPath(t *testing.T) {
	wantErr := errors.New("simulated rate limit")
	s := &StubBackend{Err: wantErr}
	_, err := s.Invoke(context.Background(), LLMRequest{})
	if !errors.Is(err, wantErr) {
		t.Errorf("err: got %v, want %v", err, wantErr)
	}
	if s.Calls != 1 {
		t.Errorf("Calls should still increment on error path, got %d", s.Calls)
	}
}

// TestClaudeBackend_FakeBinary uses a tiny shell script standing
// in for `claude`. Pins the wire-up: arguments, stdin, stdout
// capture, env passthrough. Skipped on Windows where the script
// trick doesn't work; the actual ClaudeBackend code paths are
// pure-Go and will run there in production.
func TestClaudeBackend_FakeBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake-binary trick is POSIX-specific")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "claude")
	// The fake binary echoes its first arg back as a sanity
	// check (proving args were passed) and then echoes stdin
	// (proving the task prompt got piped through). Trailer
	// confirms we exited cleanly.
	script := `#!/bin/sh
echo "args: $@"
cat
echo "OK"
`
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	c := &ClaudeBackend{Path: fakeBin}
	out, err := c.Invoke(context.Background(), LLMRequest{
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "you are a tester",
		TaskPrompt:   "do the thing",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for _, want := range []string{"-p", "--model", "claude-sonnet-4-6", "--append-system-prompt", "do the thing", "OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestClaudeBackend_NonZeroExitSurfacesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake-binary trick is POSIX-specific")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "claude-fail")
	script := `#!/bin/sh
echo "boom: model unavailable" 1>&2
exit 1
`
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	c := &ClaudeBackend{Path: fakeBin}
	_, err := c.Invoke(context.Background(), LLMRequest{Model: "x", TaskPrompt: "t"})
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	// Error message must surface stderr — operators triaging
	// the daemon's log shouldn't have to grep elsewhere.
	if !strings.Contains(err.Error(), "boom: model unavailable") {
		t.Errorf("error should include stderr, got: %v", err)
	}
}

func TestClaudeBackend_BackendTimeoutKillsSlowProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake-binary trick is POSIX-specific")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "claude-slow")
	script := `#!/bin/sh
sleep 30
`
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	// The Backend.Timeout knob (vs the outer ctx) is what
	// callers use to cap LLM wall-clock — exec.CommandContext's
	// reaction to outer-context cancellation is Go's concern,
	// the per-backend timeout is ours. Set a tight one and
	// confirm Invoke returns within a reasonable margin.
	c := &ClaudeBackend{Path: fakeBin, Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := c.Invoke(context.Background(), LLMRequest{Model: "x", TaskPrompt: "t"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Threshold is 10s: backend Timeout is 200ms, then
	// ClaudeBackend's WaitDelay (5s) bounds the Wait() tail
	// while the I/O copy goroutines drain. Anything close to
	// the script's "sleep 30" means the timeout never fired.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Invoke didn't honor backend timeout; took %s (expected < 10s)", elapsed)
	}
}
