package yaml

// Mode-field tests: compute tasks accept mode: sync|async, other
// actions reject the field entirely, bad values fail parse up
// front. Keeps the async kickoff path (phase 4b) from getting
// garbage inputs it would have to re-validate.

import (
	"strings"
	"testing"
)

func TestParseComputeAcceptsSyncMode(t *testing.T) {
	yamlData := []byte(`
name: "compute sync"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    mode: sync
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	task := parsed.ExpandedTasks[""][0]
	if task.Mode != "sync" {
		t.Errorf("expected mode=sync, got %q", task.Mode)
	}
	if got := ResolvedMode(&task.TaskDef); got != "sync" {
		t.Errorf("ResolvedMode expected sync, got %q", got)
	}
}

func TestParseComputeAcceptsAsyncMode(t *testing.T) {
	yamlData := []byte(`
name: "compute async"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/long.sh
    mode: async
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	task := parsed.ExpandedTasks[""][0]
	if task.Mode != "async" {
		t.Errorf("expected mode=async, got %q", task.Mode)
	}
}

func TestParseComputeDefaultsToSyncWhenModeUnset(t *testing.T) {
	yamlData := []byte(`
name: "compute default"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	task := parsed.ExpandedTasks[""][0]
	if task.Mode != "" {
		t.Errorf("expected Mode field empty when unset, got %q", task.Mode)
	}
	if got := ResolvedMode(&task.TaskDef); got != "sync" {
		t.Errorf("ResolvedMode must default to sync, got %q", got)
	}
}

func TestParseRejectsInvalidMode(t *testing.T) {
	yamlData := []byte(`
name: "bad mode"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    mode: later
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected parse error for invalid mode")
	}
	if !strings.Contains(err.Error(), "mode") || !strings.Contains(err.Error(), "later") {
		t.Errorf("expected error to cite mode and the bad value; got: %v", err)
	}
}

// Non-compute actions must reject mode: to catch template-author
// confusion early. Reviewer/answer/contribute tasks don't have a
// script to detach from, so the field has no meaning there.
// (Vote's validation path is covered indirectly — the validator
// rule keys off t.Action != "compute", so any non-compute action
// exercises the same code path.)
func TestParseRejectsModeOnNonComputeActions(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"answer", `
name: "mode reject answer"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "x"
    mode: async
`},
		{"contribute", `
name: "mode reject contribute"
version: 1
tasks:
  - id: t
    action: contribute
    prompt: "x"
    user_prompt: "y"
    mode: async
`},
		{"review", `
name: "mode reject review"
version: 1
tasks:
  - id: target
    action: answer
    prompt: "original"
  - id: r
    action: review
    reviews: target
    prompt: "review it"
    mode: async
`},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.yaml))
		if err == nil {
			t.Errorf("action %s: expected parse to reject mode: on non-compute task", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "mode") {
			t.Errorf("action %s: expected error to mention mode; got: %v", c.name, err)
		}
	}
}

// ResolvedMode on non-compute tasks returns "" — the concept
// doesn't apply. Callers branching on mode scope to compute
// first.
func TestResolvedModeNonComputeReturnsEmpty(t *testing.T) {
	t1 := TaskDef{Action: "answer"}
	if got := ResolvedMode(&t1); got != "" {
		t.Errorf("answer task: expected empty mode, got %q", got)
	}
	t2 := TaskDef{Action: "review"}
	if got := ResolvedMode(&t2); got != "" {
		t.Errorf("review task: expected empty mode, got %q", got)
	}
}
