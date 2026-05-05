package bots

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewHandler_DefaultsToClaude(t *testing.T) {
	// Pre-Phase-7.2 manifests omit the Handler field. The
	// factory must keep returning a ClaudeHandler so existing
	// projects don't need a migration.
	b := &Bot{Name: "x", Model: "claude-sonnet-4-6", Handler: ""}
	h, err := NewHandler(b)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if _, ok := h.(*ClaudeHandler); !ok {
		t.Errorf("empty handler type should default to claude; got %T", h)
	}
}

func TestNewHandler_ExplicitClaude(t *testing.T) {
	b := &Bot{Name: "x", Model: "claude-sonnet-4-6", Handler: "claude"}
	h, err := NewHandler(b)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	cl, ok := h.(*ClaudeHandler)
	if !ok {
		t.Fatalf("explicit claude should yield *ClaudeHandler; got %T", h)
	}
	if cl.Model != "claude-sonnet-4-6" {
		t.Errorf("Model not threaded through: got %q", cl.Model)
	}
}

func TestNewHandler_StubForTests(t *testing.T) {
	b := &Bot{Name: "x", Handler: "stub"}
	h, err := NewHandler(b)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if _, ok := h.(*StubHandler); !ok {
		t.Errorf("handler=stub should yield *StubHandler; got %T", h)
	}
}

func TestNewHandler_UnknownType(t *testing.T) {
	b := &Bot{Name: "x", Handler: "shell"} // not yet supported
	_, err := NewHandler(b)
	if err == nil {
		t.Fatal("unknown handler type should error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown handler type") {
		t.Errorf("error message should name the failure: %v", err)
	}
}

func TestClaudeHandler_PassesAllowToolsFromManifest(t *testing.T) {
	b := &Bot{
		Name:     "x",
		Model:    "claude-sonnet-4-6",
		MCPTools: &MCPTools{Allow: []string{"Read", "Edit"}},
	}
	h := NewClaudeHandler(b)
	if got, want := strings.Join(h.AllowTools, ","), "Read,Edit"; got != want {
		t.Errorf("AllowTools: got %q, want %q", got, want)
	}
}

func TestClaudeHandler_NilMCPToolsMeansNoAllowlist(t *testing.T) {
	b := &Bot{Name: "x", Model: "claude-sonnet-4-6"}
	h := NewClaudeHandler(b)
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
