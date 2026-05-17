package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// writeWorkflowWithBots wraps body (the YAML for the workflow's
// version + agents: section) in a minimal workflow YAML that the
// parser accepts, writes it to a fresh temp dir, and returns
// the workflow file's path. Callers feed that path to
// LoadFromWorkflow.
//
// The wrapper adds name/base_branch/tasks so the workflow parser
// is satisfied; tests focus on bot-section behavior only.
func writeWorkflowWithBots(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	wf := "name: test-workflow\nbase_branch: main\n" + body + "\ntasks:\n  - id: noop\n    action: answer\n    prompt: ok\n"
	path := filepath.Join(root, "workflow.yaml")
	if err := os.WriteFile(path, []byte(wf), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFromWorkflow_MissingFile(t *testing.T) {
	// Pointing at a nonexistent workflow YAML is an error — the
	// operator passed a bad --workflow path and we surface it
	// rather than silently returning nil.
	root := t.TempDir()
	_, err := LoadFromWorkflow(filepath.Join(root, "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing workflow file, got nil")
	}
	if !strings.Contains(err.Error(), "reading workflow") {
		t.Errorf("expected error to mention reading workflow, got: %v", err)
	}
}

func TestLoadFromWorkflow_NoInlineBots(t *testing.T) {
	// Workflow YAML with no agents: section parses cleanly and
	// returns a nil manifest — the workflow may use system
	// citizens only (humans / pre-registered bots) and not
	// declare any inline.
	root := t.TempDir()
	wf := "name: solo\nbase_branch: main\ntasks:\n  - id: noop\n    action: answer\n    prompt: ok\n"
	path := filepath.Join(root, "workflow.yaml")
	if err := os.WriteFile(path, []byte(wf), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadFromWorkflow(path)
	if err != nil {
		t.Fatalf("LoadFromWorkflow: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil manifest when workflow has no inline bots, got %+v", m)
	}
}

func TestLoadFromWorkflow_MalformedYAML(t *testing.T) {
	path := writeWorkflowWithBots(t, "agents: not-a-list: oops")
	_, err := LoadFromWorkflow(path)
	if err == nil {
		t.Fatal("expected parse error on malformed YAML, got nil")
	}
}

func TestLoad_HappyPath_DefaultsResolved(t *testing.T) {
	path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: developer-bot
    model: claude-sonnet-4-6
    args: ["-p"]
    mcp_tools:
      allow: [Read, Edit, Write, Bash]
`)
	m, err := LoadFromWorkflow(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m == nil || len(m.Bots) != 1 {
		t.Fatalf("expected 1 bot, got: %+v", m)
	}
	b := m.Bots[0]
	if b.Name != "developer-bot" {
		t.Errorf("name: got %q", b.Name)
	}
	if b.Model != "claude-sonnet-4-6" {
		t.Errorf("model: got %q", b.Model)
	}
	// No auto-default for system_prompt — empty means the bot
	// runs without one. Authors who want a prompt declare the
	// path explicitly via system_prompt: path/to/file.md.
	if b.SystemPrompt != "" {
		t.Errorf("system_prompt: expected empty (no auto-default), got %q", b.SystemPrompt)
	}
	// Default credentials = ~/.enju/credentials/<name>.json.
	home, _ := os.UserHomeDir()
	wantCreds := filepath.Join(home, ".enju", "credentials", "developer-bot.json")
	if b.Credentials != wantCreds {
		t.Errorf("credentials: got %q, want %q", b.Credentials, wantCreds)
	}
	if b.MCPTools == nil {
		t.Fatal("mcp_tools section was set, MCPTools should be non-nil")
	}
	if got := b.MCPTools.Allow; len(got) != 4 {
		t.Errorf("mcp_tools.allow: got %v", got)
	}
}

func TestLoad_MCPToolsOmitted_NilPointer(t *testing.T) {
	// Manifest without mcp_tools — the section is omitted
	// entirely. Pointer must be nil so the runner knows
	// "all tools" rather than "empty allowlist."
	path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: x
    model: m
    args: ["-p"]
`)
	m, err := LoadFromWorkflow(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Bots[0].MCPTools != nil {
		t.Errorf("MCPTools should be nil when section omitted, got %+v", m.Bots[0].MCPTools)
	}
}

func TestValidate_RejectsExplicitEmptyAllow(t *testing.T) {
	// Present-but-empty allowlist must be rejected. Otherwise
	// a user writing `allow: []` thinking it means "no tools"
	// would silently get the opposite if we treated empty as
	// "all" — exactly the wrong direction for a security-
	// motivated allowlist.
	path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: x
    model: m
    args: ["-p"]
    mcp_tools:
      allow: []
`)
	_, err := LoadFromWorkflow(path)
	if err == nil || !strings.Contains(err.Error(), "mcp_tools.allow is present but empty") {
		t.Errorf("expected explicit-empty-allow rejection, got: %v", err)
	}
}

// (Removed: TestValidate_RejectsUnknownVersion. Inline mode hard-
// codes Manifest.Version = 1 in FromInlineNode — the workflow
// YAML's top-level `version:` field is the WORKFLOW version,
// not the bot-manifest version. There's no separate manifest
// version knob to validate anymore.)

func TestLoad_MissingVersionDefaultsToOne(t *testing.T) {
	// Backwards compat: existing manifests without version: 1
	// continue to load (Resolve sets version=1 silently).
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
    model: m
    args: ["-p"]
`)
	m, err := LoadFromWorkflow(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("Version: got %d, want 1 (defaulted)", m.Version)
	}
}

func TestEnsureGitignored_PreservesMode(t *testing.T) {
	// Hardened repo: .gitignore is 0600. EnsureGitignored
	// must not relax it to 0644.
	root := t.TempDir()
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, []byte("# hardened\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGitignored(root); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if got := st.Mode().Perm(); got != 0600 {
		t.Errorf("mode after update: got %o, want 0600 (preserved)", got)
	}
}

func TestLoad_TildeExpansion(t *testing.T) {
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
    model: m
    args: ["-p"]
    credentials: ~/custom-creds/x.json
`)
	m, err := LoadFromWorkflow(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "custom-creds", "x.json")
	if m.Bots[0].Credentials != want {
		t.Errorf("tilde not expanded: got %q, want %q", m.Bots[0].Credentials, want)
	}
}

func TestValidate_RejectsMissingName(t *testing.T) {
	path := writeWorkflowWithBots(t, `
agents:
  - model: m
`)
	_, err := LoadFromWorkflow(path)
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected name-required error, got: %v", err)
	}
}

func TestValidate_RejectsMissingModel(t *testing.T) {
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
`)
	_, err := LoadFromWorkflow(path)
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("expected model-required error, got: %v", err)
	}
}

// (Removed: TestLoad_ProjectIDOptional + TestLoad_ProjectIDParsed.
// Phase 8 dropped Manifest.ProjectID — workflow YAMLs don't carry
// project IDs since they're instance-specific. Operators pass
// --project-id explicitly at bot setup time.)

// TestValidate_RejectsClaudeHandlerWithoutArgs pins the showcase_v15
// fix: `handler: claude` (or empty=default) without an `args:` block
// is rejected at manifest load. Pre-fix the daemon would happily
// spawn `claude` with zero arguments, claude would idle ~20s waiting
// for nothing in particular and exit silently, and the operator
// would see a baffling "writes_artifacts missing" failure with no
// trace of what (didn't) happen.
func TestValidate_RejectsClaudeHandlerWithoutArgs(t *testing.T) {
	for _, h := range []string{"", "claude"} {
		t.Run("handler="+h, func(t *testing.T) {
			body := "agents:\n  - name: x\n    model: m\n"
			if h != "" {
				body += "    handler: " + h + "\n"
			}
			path := writeWorkflowWithBots(t, body)
			_, err := LoadFromWorkflow(path)
			if err == nil {
				t.Fatalf("expected validation error for handler %q with no args:", h)
			}
			if !strings.Contains(err.Error(), "args: is required") {
				t.Errorf("error should mention args being required, got: %v", err)
			}
		})
	}
}

// TestValidate_StubHandlerSkipsArgsGate confirms the gate is scoped
// to claude. stub handlers (test fixtures) don't drive an LLM, so
// requiring args: there would force callers to author boilerplate
// for tests that don't even invoke the binary.
func TestValidate_StubHandlerSkipsArgsGate(t *testing.T) {
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
    model: m
    handler: stub
`)
	if _, err := LoadFromWorkflow(path); err != nil {
		t.Errorf("stub handler should not require args:, got: %v", err)
	}
}

func TestValidate_AcceptsKnownHandlerTypes(t *testing.T) {
	for _, h := range []string{"", "claude", "stub"} {
		t.Run("handler="+h, func(t *testing.T) {
			body := "agents:\n  - name: x\n    model: m\n"
			if h != "" {
				body += "    handler: " + h + "\n"
			}
			// claude (and empty=claude-default) require args:;
			// stub skips the gate. Add args for the LLM cases.
			if h == "" || h == "claude" {
				body += "    args: [\"-p\"]\n"
			}
			path := writeWorkflowWithBots(t, body)
			if _, err := LoadFromWorkflow(path); err != nil {
				t.Errorf("expected handler %q to validate, got: %v", h, err)
			}
		})
	}
}

// TestValidate_RejectsUnknownArgsTemplate pins review-fix #3:
// a Bot.Args entry referencing an unrecognized {{var}} name
// fails manifest load with a clear error. Without this,
// operator typos like {{tsk_id}} for {{task_id}} would
// substitute to empty + drop the arg, surfacing as "the flag
// vanished" at first daemon claim with no diagnostic.
func TestValidate_RejectsUnknownArgsTemplate(t *testing.T) {
	path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: x
    model: m
    args:
      - "-p"
      - "--task-id={{tsk_id}}"
`)
	_, err := LoadFromWorkflow(path)
	if err == nil {
		t.Fatal("expected validation error for typo'd {{tsk_id}}")
	}
	if !strings.Contains(err.Error(), "{{tsk_id}}") {
		t.Errorf("error should name the bad placeholder, got: %v", err)
	}
	if !strings.Contains(err.Error(), "args[1]") {
		t.Errorf("error should name the bad arg index, got: %v", err)
	}
}

// TestValidate_RejectsMalformedArgsTemplate pins review-fix
// #10: unterminated braces fail loud at manifest load.
func TestValidate_RejectsMalformedArgsTemplate(t *testing.T) {
	path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: x
    model: m
    args:
      - "--model={{model"
`)
	_, err := LoadFromWorkflow(path)
	if err == nil {
		t.Fatal("expected validation error for unterminated {{")
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("error should mention unterminated brace, got: %v", err)
	}
}

// TestValidate_AcceptsKnownArgsVars passes when args use any
// recognized static var or any handler_args.<key>.
func TestValidate_AcceptsKnownArgsVars(t *testing.T) {
	path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: x
    model: m
    args:
      - "-p"
      - "--model={{model}}"
      - "--system={{system_prompt}}"
      - "--tools={{allowed_tools}}"
      - "--branch={{branch}}"
      - "--effort={{handler_args.effort}}"
      - "--any-operator-key={{handler_args.foo-bar}}"
`)
	if _, err := LoadFromWorkflow(path); err != nil {
		t.Errorf("manifest with valid {{var}} references should load, got: %v", err)
	}
}

// TestValidate_AcceptsArbitraryHandlerBinary pins the architecture
// claim: handler: <name> works for ANY binary, not just claude/stub.
// Closed-enum validation was wrong — the handler field doubles as
// the binary name, and operators add support for new LLMs (gemini,
// etc.) or custom scripts by naming the binary, not by patching Go.
//
// Reverses the prior TestValidate_RejectsUnknownHandler which
// pinned the (incorrect) closed-enum behavior. The validator no
// longer second-guesses the binary's existence at manifest-load —
// the daemon's Preflight does that at startup.
func TestValidate_AcceptsArbitraryHandlerBinary(t *testing.T) {
	for _, h := range []string{"gemini", "shell", "./bin/lint-bot.sh", "/usr/local/bin/foo"} {
		t.Run("handler="+h, func(t *testing.T) {
			path := writeWorkflowWithBots(t, `
agents:
  - name: x
    args: ["-p"]
    handler: `+h+`
`)
			if _, err := LoadFromWorkflow(path); err != nil {
				t.Errorf("handler %q should be accepted (binary names are open-set), got: %v", h, err)
			}
		})
	}
}

func TestValidate_StubHandlerNoModelRequired(t *testing.T) {
	// Non-LLM handlers don't need a model. A future linter-bot
	// or rule-bot wouldn't have a model to declare; insisting
	// would be cargo from the LLM-only past.
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
    handler: stub
`)
	if _, err := LoadFromWorkflow(path); err != nil {
		t.Errorf("stub handler should validate without model, got: %v", err)
	}
}

func TestValidate_RejectsDuplicateName(t *testing.T) {
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
    model: m
    args: ["-p"]
  - name: x
    model: n
`)
	_, err := LoadFromWorkflow(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("expected duplicate-name error, got: %v", err)
	}
}

func TestValidate_RejectsBadName(t *testing.T) {
	cases := []string{"bot with spaces", "bot/slash", "bot.dot", ""}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeWorkflowWithBots(t, `
agents:
  - name: "`+name+`"
    model: m
    args: ["-p"]
`)
			_, err := LoadFromWorkflow(path)
			if err == nil {
				t.Errorf("expected validation error for name %q, got nil", name)
			}
		})
	}
}

func TestValidate_RejectsAbsoluteSystemPrompt(t *testing.T) {
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
    model: m
    args: ["-p"]
    system_prompt: /etc/passwd
`)
	_, err := LoadFromWorkflow(path)
	if err == nil || !strings.Contains(err.Error(), "repo-relative") {
		t.Errorf("expected repo-relative rejection on absolute path, got: %v", err)
	}
}

func TestValidate_RejectsParentTraversal(t *testing.T) {
	path := writeWorkflowWithBots(t, `
agents:
  - name: x
    model: m
    args: ["-p"]
    system_prompt: ../../../etc/passwd
`)
	_, err := LoadFromWorkflow(path)
	if err == nil || !strings.Contains(err.Error(), "..") {
		t.Errorf("expected .. rejection, got: %v", err)
	}
}

func TestEnsureGitignored_FreshProject(t *testing.T) {
	root := t.TempDir()
	changed, err := EnsureGitignored(root)
	if err != nil {
		t.Fatalf("EnsureGitignored: %v", err)
	}
	if !changed {
		t.Error("expected changed=true on fresh project")
	}
	body, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	// Post-Phase-8: only the .enju/ umbrella entry remains.
	// It transitively covers .enju/bots/, .enju/runs/,
	// .enju/scratch/, and any future runtime-cache sibling.
	// No more enju/.bare.git/ entry — Phase 8 removed bare creation.
	if !strings.Contains(string(body), ".enju/") {
		t.Errorf("expected .enju/ in .gitignore, got:\n%s", body)
	}
	// Verify legacy entries are NOT present — regression guards.
	if strings.Contains(string(body), "\nenju/bots/\n") {
		t.Errorf("legacy enju/bots/ entry should be gone post-Phase-4a (covered transitively by .enju/), got:\n%s", body)
	}
	if strings.Contains(string(body), "enju/.bare.git/") {
		t.Errorf("enju/.bare.git/ entry should be gone post-Phase-8 (no bare creation anymore), got:\n%s", body)
	}
	if !strings.Contains(string(body), "enju-untracked") {
		t.Error(".gitignore should contain the managed block markers")
	}
}

func TestEnsureGitignored_Idempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := EnsureGitignored(root); err != nil {
		t.Fatal(err)
	}
	// Second call should be a no-op (changed=false).
	changed, err := EnsureGitignored(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected changed=false on second call")
	}
}

func TestEnsureGitignored_PreservesUserContent(t *testing.T) {
	root := t.TempDir()
	preexisting := "# user's own ignores\nnode_modules/\n*.log\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(preexisting), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGitignored(root); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	for _, want := range []string{"node_modules/", "*.log", ".enju/"} {
		if !strings.Contains(string(body), want) {
			t.Errorf(".gitignore missing %q after EnsureGitignored:\n%s", want, body)
		}
	}
}

// TestExpandReplicas covers the full replicas matrix: backward-
// compat (absent + 1 stay single), expansion (>=2 produces N
// suffixed entries with shared fields), validation (negative,
// over-cap), and the per-replica defaults Resolve fills in
// after expansion (credentials per replica, prompt shared).
func TestExpandReplicas(t *testing.T) {
	t.Run("absent: single entry unchanged", func(t *testing.T) {
		path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: only-bot
    model: claude-sonnet-4-6
    args: ["-p"]
`)
		m, err := LoadFromWorkflow(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(m.Bots) != 1 || m.Bots[0].Name != "only-bot" {
			t.Errorf("expected 1 entry named only-bot, got %+v", m.Bots)
		}
	})

	t.Run("replicas: 1 stays single (sentinel for explicit single)", func(t *testing.T) {
		path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: solo-dev
    model: claude-sonnet-4-6
    args: ["-p"]
    replicas: 1
`)
		m, err := LoadFromWorkflow(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(m.Bots) != 1 {
			t.Fatalf("replicas:1 should produce 1 entry, got %d", len(m.Bots))
		}
		if m.Bots[0].Name != "solo-dev" {
			t.Errorf("expected name unchanged, got %q", m.Bots[0].Name)
		}
		if m.Bots[0].Replicas != 0 {
			t.Errorf("Replicas should be cleared post-expansion, got %d", m.Bots[0].Replicas)
		}
	})

	t.Run("replicas: 3 expands to 3 suffixed entries", func(t *testing.T) {
		path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: dev-bot
    model: claude-sonnet-4-6
    args: ["-p"]
    replicas: 3
`)
		m, err := LoadFromWorkflow(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(m.Bots) != 3 {
			t.Fatalf("replicas:3 should produce 3 entries, got %d", len(m.Bots))
		}
		wantNames := []string{"dev-bot-1", "dev-bot-2", "dev-bot-3"}
		for i, want := range wantNames {
			if m.Bots[i].Name != want {
				t.Errorf("Bots[%d].Name: got %q, want %q", i, m.Bots[i].Name, want)
			}
			if m.Bots[i].Model != "claude-sonnet-4-6" {
				t.Errorf("Bots[%d].Model not copied: %q", i, m.Bots[i].Model)
			}
			if m.Bots[i].Replicas != 0 {
				t.Errorf("Bots[%d].Replicas should be cleared post-expansion, got %d", i, m.Bots[i].Replicas)
			}
		}
	})

	t.Run("replicas share authored prompt, separate credentials", func(t *testing.T) {
		path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: dev-bot
    model: claude-sonnet-4-6
    args: ["-p"]
    system_prompt: prompts/dev.md
    replicas: 3
`)
		m, err := LoadFromWorkflow(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// All replicas share the explicit prompt path the author
		// declared on the base bot — three identical bots, one
		// prompt file.
		for i, b := range m.Bots {
			if b.SystemPrompt != "prompts/dev.md" {
				t.Errorf("Bots[%d].SystemPrompt: got %q, want %q (shared)", i, b.SystemPrompt, "prompts/dev.md")
			}
		}
		// But each replica has its own credentials file (distinct
		// identity = distinct token = distinct on-disk file).
		seenCreds := make(map[string]bool)
		for i, b := range m.Bots {
			if seenCreds[b.Credentials] {
				t.Errorf("Bots[%d] credentials duplicate: %q", i, b.Credentials)
			}
			seenCreds[b.Credentials] = true
			wantSuffix := "credentials/" + b.Name + ".json"
			if !strings.HasSuffix(b.Credentials, wantSuffix) {
				t.Errorf("Bots[%d].Credentials: got %q, want suffix %q", i, b.Credentials, wantSuffix)
			}
		}
	})

	t.Run("replicas: -1 rejected", func(t *testing.T) {
		path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: dev-bot
    model: claude-sonnet-4-6
    args: ["-p"]
    replicas: -1
`)
		_, err := LoadFromWorkflow(path)
		if err == nil {
			t.Fatal("expected error on negative replicas")
		}
		if !strings.Contains(err.Error(), "replicas must be >= 1") {
			t.Errorf("error should explain the rule, got: %v", err)
		}
	})

	t.Run("replicas exceeding cap rejected", func(t *testing.T) {
		path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: dev-bot
    model: claude-sonnet-4-6
    args: ["-p"]
    replicas: 100
`)
		_, err := LoadFromWorkflow(path)
		if err == nil {
			t.Fatal("expected error on over-cap replicas")
		}
		if !strings.Contains(err.Error(), "exceeds cap") {
			t.Errorf("error should mention the cap, got: %v", err)
		}
	})

	t.Run("name collision between replica and explicit entry rejected", func(t *testing.T) {
		// dev-bot replicas:2 expands to dev-bot-1 + dev-bot-2;
		// the explicit dev-bot-1 below collides.
		path := writeWorkflowWithBots(t, `
version: 1
agents:
  - name: dev-bot
    model: claude-sonnet-4-6
    args: ["-p"]
    replicas: 2
  - name: dev-bot-1
    model: claude-sonnet-4-6
    args: ["-p"]
`)
		_, err := LoadFromWorkflow(path)
		if err == nil {
			t.Fatal("expected duplicate-name error post-expansion")
		}
		if !strings.Contains(err.Error(), "duplicate name") {
			t.Errorf("error should mention duplicate name, got: %v", err)
		}
	})
}

func TestByName(t *testing.T) {
	m := &Manifest{Bots: []Bot{
		{Name: "a", Model: "m1"},
		{Name: "b", Model: "m2"},
	}}
	if got := m.ByName("a"); got == nil || got.Model != "m1" {
		t.Errorf("ByName(a): got %+v", got)
	}
	if got := m.ByName("missing"); got != nil {
		t.Errorf("ByName(missing): got %+v, want nil", got)
	}
	var nilMan *Manifest
	if got := nilMan.ByName("x"); got != nil {
		t.Errorf("ByName on nil manifest: got %+v, want nil", got)
	}
}

// inlineNode parses a YAML fragment as a sequence of Bot
// entries and returns the resulting yamlv3.Node — the same
// shape captured by yaml.Run.Bots when a workflow YAML carries
// an inline `agents:` block.
func inlineNode(t *testing.T, body string) yamlv3.Node {
	t.Helper()
	var wrapper struct {
		Bots yamlv3.Node `yaml:"agents"`
	}
	if err := yamlv3.Unmarshal([]byte(body), &wrapper); err != nil {
		t.Fatalf("parsing test inline fragment: %v", err)
	}
	return wrapper.Bots
}

// TestFromInlineNode_AbsentBlock pins the zero-value Node
// shape: when a workflow YAML has no `agents:` key, the Run
// struct's Bots field is the empty yamlv3.Node (Kind==0).
// FromInlineNode must return (nil, nil) so callers can chain
// to the legacy Load path without special casing.
func TestFromInlineNode_AbsentBlock(t *testing.T) {
	var empty yamlv3.Node // Kind == 0
	m, err := FromInlineNode(empty)
	if err != nil {
		t.Errorf("absent block should be silent, got: %v", err)
	}
	if m != nil {
		t.Errorf("absent block should return nil Manifest, got %+v", m)
	}
}

// TestFromInlineNode_EmptySequence pins the explicit empty
// shape (`agents: []`): same outcome as absent — fall back to
// legacy file. Authors who want to actively suppress bots
// should leave the block out entirely; an explicit empty list
// is treated as "no inline" for forward-compat.
func TestFromInlineNode_EmptySequence(t *testing.T) {
	node := inlineNode(t, "agents: []\n")
	m, err := FromInlineNode(node)
	if err != nil {
		t.Errorf("empty sequence should be silent, got: %v", err)
	}
	if m != nil {
		t.Errorf("empty sequence should return nil Manifest, got %+v", m)
	}
}

// (Removed: TestFromInlineNode_RoundTripsWithStandalone. Phase 8.h.3
// dropped the standalone bots.yaml file format; the test compared
// inline-parse against file-parse to verify they produced identical
// Manifests, which is no longer meaningful with one parse path.)

// TestFromInlineNode_RunsResolveAndValidate makes sure the
// inline path goes through the same expand-resolve-validate
// pipeline as Load. An invalid bot in the inline block must
// surface a validation error (with an "inline agents:" prefix
// so it's attributable), not produce a silently-bad Manifest.
func TestFromInlineNode_RunsResolveAndValidate(t *testing.T) {
	// Missing model on a claude-handler bot fails Validate.
	body := `
agents:
  - name: broken
    handler: claude
`
	_, err := FromInlineNode(inlineNode(t, body))
	if err == nil {
		t.Fatal("expected validation error for claude bot with no model")
	}
	if !strings.Contains(err.Error(), "inline agents:") {
		t.Errorf("error should be attributed to inline bots, got: %v", err)
	}
	if !strings.Contains(err.Error(), "model is required") {
		t.Errorf("error should mention model requirement, got: %v", err)
	}
}

// TestFromInlineNode_ExpandsReplicas pins that inline blocks
// honor the `replicas: N` expansion the same way the standalone
// file does. Three replicas of one entry → three Bot entries
// with suffixed names.
func TestFromInlineNode_ExpandsReplicas(t *testing.T) {
	body := `
agents:
  - name: dev
    model: claude-sonnet-4-6
    args: ["-p"]
    handler: claude
    replicas: 3
`
	m, err := FromInlineNode(inlineNode(t, body))
	if err != nil {
		t.Fatalf("FromInlineNode: %v", err)
	}
	if len(m.Bots) != 3 {
		t.Fatalf("expected 3 expanded replicas, got %d (%+v)", len(m.Bots), m.Bots)
	}
	wantNames := []string{"dev-1", "dev-2", "dev-3"}
	for i, want := range wantNames {
		if m.Bots[i].Name != want {
			t.Errorf("Bots[%d].Name = %q, want %q", i, m.Bots[i].Name, want)
		}
	}
}

// (Removed: TestLoadPreferringInline_* tests. Phase 8.h.3 dropped
// LoadPreferringInline and the standalone bots.yaml fallback it
// bridged to. Workflow YAML's inline agents: is the only source of
// truth; tests above cover that path directly via LoadFromWorkflow
// + FromInlineNode.)
