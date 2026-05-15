package bots

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewHandler_DefaultsToClaudeBinary(t *testing.T) {
	// Pre-Phase-4b manifests omit the Handler field. The factory
	// must keep producing a working handler with claude as the
	// binary so existing projects don't need a migration.
	b := &Bot{Name: "x", Model: "claude-sonnet-4-6", Handler: ""}
	h, err := NewHandler(b, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	sub, ok := h.(*SubprocessHandler)
	if !ok {
		t.Fatalf("empty handler should yield *SubprocessHandler; got %T", h)
	}
	if sub.Binary != "claude" {
		t.Errorf("empty handler should default Binary to %q, got %q", "claude", sub.Binary)
	}
}

func TestNewHandler_ExplicitClaude(t *testing.T) {
	b := &Bot{Name: "x", Model: "claude-sonnet-4-6", Handler: "claude"}
	h, err := NewHandler(b, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	sub, ok := h.(*SubprocessHandler)
	if !ok {
		t.Fatalf("handler=claude should yield *SubprocessHandler; got %T", h)
	}
	if sub.Binary != "claude" {
		t.Errorf("Binary: got %q, want %q", sub.Binary, "claude")
	}
	if sub.Model != "claude-sonnet-4-6" {
		t.Errorf("Model not threaded through: got %q", sub.Model)
	}
}

func TestNewHandler_ArbitraryBinaryName(t *testing.T) {
	// The Phase 4b plug-in contract: any non-stub, non-empty
	// handler string is the name of an external binary. No
	// enum allowlist, no Go change per LLM. gemini, aider,
	// my-rule-bot — all valid.
	for _, bin := range []string{"gemini", "aider", "./bin/my-linter", "/opt/foo/runner"} {
		t.Run(bin, func(t *testing.T) {
			b := &Bot{Name: "x", Model: "m", Handler: bin}
			h, err := NewHandler(b, "")
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			sub, ok := h.(*SubprocessHandler)
			if !ok {
				t.Fatalf("handler=%q should yield *SubprocessHandler; got %T", bin, h)
			}
			if sub.Binary != bin {
				t.Errorf("Binary: got %q, want %q", sub.Binary, bin)
			}
		})
	}
}

func TestNewHandler_StubForTests(t *testing.T) {
	b := &Bot{Name: "x", Handler: "stub"}
	h, err := NewHandler(b, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if _, ok := h.(*StubHandler); !ok {
		t.Errorf("handler=stub should yield *StubHandler; got %T", h)
	}
}

func TestSubprocessHandler_PassesAllowToolsFromManifest(t *testing.T) {
	b := &Bot{
		Name:     "x",
		Model:    "claude-sonnet-4-6",
		Handler:  "claude",
		MCPTools: &MCPTools{Allow: []string{"Read", "Edit"}},
	}
	h := NewSubprocessHandler(b, "")
	if got, want := strings.Join(h.AllowTools, ","), "Read,Edit"; got != want {
		t.Errorf("AllowTools: got %q, want %q", got, want)
	}
}

func TestSubprocessHandler_NilMCPToolsMeansNoAllowlist(t *testing.T) {
	b := &Bot{Name: "x", Model: "claude-sonnet-4-6", Handler: "claude"}
	h := NewSubprocessHandler(b, "")
	if len(h.AllowTools) != 0 {
		t.Errorf("nil MCPTools should produce no allowlist; got %v", h.AllowTools)
	}
}

func TestStubHandler_RecordsAndReturns(t *testing.T) {
	s := &StubHandler{Response: "APPROVE"}
	out, err := s.ProcessTask(context.Background(), HandlerInput{TaskID: "1:1:T", Action: "review", Prompt: "p"})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if out.Response != "APPROVE" {
		t.Errorf("Response: got %q, want APPROVE", out.Response)
	}
	if s.Calls != 1 {
		t.Errorf("Calls: got %d, want 1", s.Calls)
	}
	if len(s.Inputs) != 1 || s.Inputs[0].TaskID != "1:1:T" {
		t.Errorf("Inputs not recorded correctly: %+v", s.Inputs)
	}
}

func TestStubHandler_PropagatesError(t *testing.T) {
	want := errors.New("rate limit")
	s := &StubHandler{Err: want}
	_, err := s.ProcessTask(context.Background(), HandlerInput{})
	if !errors.Is(err, want) {
		t.Errorf("Err not propagated: got %v", err)
	}
}

func TestStubHandler_SentinelOnUnsetResponse(t *testing.T) {
	// Tests that forget to set Response should fail loudly,
	// not submit empty strings to the coord.
	s := &StubHandler{}
	out, err := s.ProcessTask(context.Background(), HandlerInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Response, "stub") {
		t.Errorf("unset response should yield a sentinel marker, got %q", out.Response)
	}
}
