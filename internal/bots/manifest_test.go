package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// helper: write enju/bots.yaml inside a fresh temp project dir
// and return the project root.
func writeManifest(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "enju"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "enju", "bots.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoad_MissingFile(t *testing.T) {
	// Empty project — no enju/bots.yaml. Must NOT error;
	// projects without bots are valid.
	root := t.TempDir()
	m, err := Load(root)
	if err != nil {
		t.Fatalf("missing manifest should be silent, got error: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil manifest for missing file, got %+v", m)
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	root := writeManifest(t, "")
	m, err := Load(root)
	if err != nil {
		t.Fatalf("empty manifest should be silent, got error: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil manifest for empty file, got %+v", m)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	root := writeManifest(t, "bots: not-a-list: oops")
	_, err := Load(root)
	if err == nil {
		t.Fatal("expected parse error on malformed YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parsing") {
		t.Errorf("expected error message to mention parsing, got: %v", err)
	}
}

func TestLoad_HappyPath_DefaultsResolved(t *testing.T) {
	root := writeManifest(t, `
version: 1
bots:
  - name: developer-bot
    model: claude-sonnet-4-6
    mcp_tools:
      allow: [Read, Edit, Write, Bash]
`)
	m, err := Load(root)
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
	// Default system_prompt = enju/prompts/<name>.md.
	wantPrompt := "enju/prompts/developer-bot.md"
	if b.SystemPrompt != wantPrompt {
		t.Errorf("system_prompt: got %q, want %q", b.SystemPrompt, wantPrompt)
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
	root := writeManifest(t, `
version: 1
bots:
  - name: x
    model: m
`)
	m, err := Load(root)
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
	root := writeManifest(t, `
version: 1
bots:
  - name: x
    model: m
    mcp_tools:
      allow: []
`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "mcp_tools.allow is present but empty") {
		t.Errorf("expected explicit-empty-allow rejection, got: %v", err)
	}
}

func TestValidate_RejectsUnknownVersion(t *testing.T) {
	root := writeManifest(t, `
version: 99
bots:
  - name: x
    model: m
`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "unsupported manifest version 99") {
		t.Errorf("expected version-mismatch error, got: %v", err)
	}
}

func TestLoad_MissingVersionDefaultsToOne(t *testing.T) {
	// Backwards compat: existing manifests without version: 1
	// continue to load (Resolve sets version=1 silently).
	root := writeManifest(t, `
bots:
  - name: x
    model: m
`)
	m, err := Load(root)
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
	root := writeManifest(t, `
bots:
  - name: x
    model: m
    credentials: ~/custom-creds/x.json
`)
	m, err := Load(root)
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
	root := writeManifest(t, `
bots:
  - model: m
`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected name-required error, got: %v", err)
	}
}

func TestValidate_RejectsMissingModel(t *testing.T) {
	root := writeManifest(t, `
bots:
  - name: x
`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("expected model-required error, got: %v", err)
	}
}

func TestLoad_ProjectIDOptional(t *testing.T) {
	// Manifest without project_id: legal — operator passes it
	// at setup time via --project-id, or skips auto-add
	// entirely.
	root := writeManifest(t, `
bots:
  - name: x
    model: m
`)
	m, err := Load(root)
	if err != nil {
		t.Fatalf("missing project_id should be accepted, got: %v", err)
	}
	if m.ProjectID != 0 {
		t.Errorf("ProjectID: got %d, want 0 when omitted", m.ProjectID)
	}
}

func TestLoad_ProjectIDParsed(t *testing.T) {
	// Manifest with project_id: surfaced through Load so cmdBotSetup
	// can default to it when --project-id isn't passed.
	root := writeManifest(t, `
project_id: 42
bots:
  - name: x
    model: m
`)
	m, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.ProjectID != 42 {
		t.Errorf("ProjectID: got %d, want 42", m.ProjectID)
	}
}

func TestValidate_AcceptsKnownHandlerTypes(t *testing.T) {
	for _, h := range []string{"", "claude", "stub"} {
		t.Run("handler="+h, func(t *testing.T) {
			body := "bots:\n  - name: x\n    model: m\n"
			if h != "" {
				body += "    handler: " + h + "\n"
			}
			root := writeManifest(t, body)
			if _, err := Load(root); err != nil {
				t.Errorf("expected handler %q to validate, got: %v", h, err)
			}
		})
	}
}

func TestValidate_RejectsUnknownHandler(t *testing.T) {
	root := writeManifest(t, `
bots:
  - name: x
    model: m
    handler: shell
`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "unknown handler") {
		t.Errorf("expected unknown-handler error, got: %v", err)
	}
}

func TestValidate_StubHandlerNoModelRequired(t *testing.T) {
	// Non-LLM handlers don't need a model. A future linter-bot
	// or rule-bot wouldn't have a model to declare; insisting
	// would be cargo from the LLM-only past.
	root := writeManifest(t, `
bots:
  - name: x
    handler: stub
`)
	if _, err := Load(root); err != nil {
		t.Errorf("stub handler should validate without model, got: %v", err)
	}
}

func TestValidate_RejectsDuplicateName(t *testing.T) {
	root := writeManifest(t, `
bots:
  - name: x
    model: m
  - name: x
    model: n
`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("expected duplicate-name error, got: %v", err)
	}
}

func TestValidate_RejectsBadName(t *testing.T) {
	cases := []string{"bot with spaces", "bot/slash", "bot.dot", ""}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeManifest(t, `
bots:
  - name: "`+name+`"
    model: m
`)
			_, err := Load(root)
			if err == nil {
				t.Errorf("expected validation error for name %q, got nil", name)
			}
		})
	}
}

func TestValidate_RejectsAbsoluteSystemPrompt(t *testing.T) {
	root := writeManifest(t, `
bots:
  - name: x
    model: m
    system_prompt: /etc/passwd
`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "repo-relative") {
		t.Errorf("expected repo-relative rejection on absolute path, got: %v", err)
	}
}

func TestValidate_RejectsParentTraversal(t *testing.T) {
	root := writeManifest(t, `
bots:
  - name: x
    model: m
    system_prompt: ../../../etc/passwd
`)
	_, err := Load(root)
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
	// Both machine-managed dirs must land in the managed block
	// so a stray `git add enju/` doesn't accidentally commit
	// bot worktrees or the bare push target. Per-bot clones
	// nest under enju/bots/<botname>/clone/ so the enju/bots/
	// rule covers them transitively — no separate clone entry
	// needed.
	// Phase 4a moved per-bot worktrees under .enju/bots/, which is
	// transitively covered by the .enju/ entry — so no separate
	// enju/bots/ rule is needed (or wanted). enju/.bare.git/ stays
	// a separate entry until Phase 8 relocates the bare too.
	for _, want := range []string{"enju/.bare.git/", ".enju/"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("expected %q in .gitignore, got:\n%s", want, body)
		}
	}
	// And verify the OLD entry is NOT present — regression guard
	// against accidentally re-adding it.
	if strings.Contains(string(body), "\nenju/bots/\n") {
		t.Errorf("legacy enju/bots/ entry should be gone post-Phase-4a (covered transitively by .enju/), got:\n%s", body)
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
		root := writeManifest(t, `
version: 1
bots:
  - name: only-bot
    model: claude-sonnet-4-6
`)
		m, err := Load(root)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(m.Bots) != 1 || m.Bots[0].Name != "only-bot" {
			t.Errorf("expected 1 entry named only-bot, got %+v", m.Bots)
		}
	})

	t.Run("replicas: 1 stays single (sentinel for explicit single)", func(t *testing.T) {
		root := writeManifest(t, `
version: 1
bots:
  - name: solo-dev
    model: claude-sonnet-4-6
    replicas: 1
`)
		m, err := Load(root)
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
		root := writeManifest(t, `
version: 1
bots:
  - name: dev-bot
    model: claude-sonnet-4-6
    replicas: 3
`)
		m, err := Load(root)
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

	t.Run("replicas share BASE prompt, separate credentials", func(t *testing.T) {
		root := writeManifest(t, `
version: 1
bots:
  - name: dev-bot
    model: claude-sonnet-4-6
    replicas: 3
`)
		m, err := Load(root)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// All replicas point at the same prompt file (the BASE
		// name, not the suffixed name) — three identical bots.
		for i, b := range m.Bots {
			if !strings.HasSuffix(b.SystemPrompt, "/dev-bot.md") {
				t.Errorf("Bots[%d].SystemPrompt: got %q, want suffix /dev-bot.md (shared)", i, b.SystemPrompt)
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
		root := writeManifest(t, `
version: 1
bots:
  - name: dev-bot
    model: claude-sonnet-4-6
    replicas: -1
`)
		_, err := Load(root)
		if err == nil {
			t.Fatal("expected error on negative replicas")
		}
		if !strings.Contains(err.Error(), "replicas must be >= 1") {
			t.Errorf("error should explain the rule, got: %v", err)
		}
	})

	t.Run("replicas exceeding cap rejected", func(t *testing.T) {
		root := writeManifest(t, `
version: 1
bots:
  - name: dev-bot
    model: claude-sonnet-4-6
    replicas: 100
`)
		_, err := Load(root)
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
		root := writeManifest(t, `
version: 1
bots:
  - name: dev-bot
    model: claude-sonnet-4-6
    replicas: 2
  - name: dev-bot-1
    model: claude-sonnet-4-6
`)
		_, err := Load(root)
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
// an inline `bots:` block.
func inlineNode(t *testing.T, body string) yamlv3.Node {
	t.Helper()
	var wrapper struct {
		Bots yamlv3.Node `yaml:"bots"`
	}
	if err := yamlv3.Unmarshal([]byte(body), &wrapper); err != nil {
		t.Fatalf("parsing test inline fragment: %v", err)
	}
	return wrapper.Bots
}

// TestFromInlineNode_AbsentBlock pins the zero-value Node
// shape: when a workflow YAML has no `bots:` key, the Run
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
// shape (`bots: []`): same outcome as absent — fall back to
// legacy file. Authors who want to actively suppress bots
// should leave the block out entirely; an explicit empty list
// is treated as "no inline" for forward-compat.
func TestFromInlineNode_EmptySequence(t *testing.T) {
	node := inlineNode(t, "bots: []\n")
	m, err := FromInlineNode(node)
	if err != nil {
		t.Errorf("empty sequence should be silent, got: %v", err)
	}
	if m != nil {
		t.Errorf("empty sequence should return nil Manifest, got %+v", m)
	}
}

// TestFromInlineNode_RoundTripsWithStandalone verifies that an
// inline block produces a byte-identical Manifest to the
// standalone enju/bots.yaml form for the same content. This
// is the core integration test for Phase 7's "same schema, two
// locations" promise — if the produced Manifests diverge,
// existing daemon code that reads Manifest.Bots won't behave
// the same across the two authoring paths.
func TestFromInlineNode_RoundTripsWithStandalone(t *testing.T) {
	body := `
bots:
  - name: reviewer
    model: claude-sonnet-4-6
    handler: claude
  - name: summarizer
    model: claude-haiku-4-5
    handler: claude
`
	inlineM, err := FromInlineNode(inlineNode(t, body))
	if err != nil {
		t.Fatalf("FromInlineNode: %v", err)
	}
	if inlineM == nil {
		t.Fatal("inline manifest unexpectedly nil")
	}

	// Stand-alone equivalent — wrap the same list in the
	// top-level Manifest shape and feed it through Load.
	root := writeManifest(t, "version: 1\n"+body)
	standaloneM, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if standaloneM == nil {
		t.Fatal("standalone manifest unexpectedly nil")
	}

	if len(inlineM.Bots) != len(standaloneM.Bots) {
		t.Fatalf("bot count mismatch: inline=%d standalone=%d", len(inlineM.Bots), len(standaloneM.Bots))
	}
	for i := range inlineM.Bots {
		if inlineM.Bots[i].Name != standaloneM.Bots[i].Name ||
			inlineM.Bots[i].Model != standaloneM.Bots[i].Model ||
			inlineM.Bots[i].Handler != standaloneM.Bots[i].Handler ||
			inlineM.Bots[i].SystemPrompt != standaloneM.Bots[i].SystemPrompt ||
			inlineM.Bots[i].Credentials != standaloneM.Bots[i].Credentials {
			t.Errorf("bots[%d] mismatch:\n inline:     %+v\n standalone: %+v",
				i, inlineM.Bots[i], standaloneM.Bots[i])
		}
	}
}

// TestFromInlineNode_RunsResolveAndValidate makes sure the
// inline path goes through the same expand-resolve-validate
// pipeline as Load. An invalid bot in the inline block must
// surface a validation error (with an "inline bots:" prefix
// so it's attributable), not produce a silently-bad Manifest.
func TestFromInlineNode_RunsResolveAndValidate(t *testing.T) {
	// Missing model on a claude-handler bot fails Validate.
	body := `
bots:
  - name: broken
    handler: claude
`
	_, err := FromInlineNode(inlineNode(t, body))
	if err == nil {
		t.Fatal("expected validation error for claude bot with no model")
	}
	if !strings.Contains(err.Error(), "inline bots:") {
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
bots:
  - name: dev
    model: claude-sonnet-4-6
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

// TestLoadPreferringInline_InlineWinsOverFile pins the
// precedence rule: when both inline and stand-alone manifests
// are present, inline takes precedence. The transition from
// stand-alone to inline must be opt-in — once an author adds
// inline bots, the legacy file becomes shadowed.
func TestLoadPreferringInline_InlineWinsOverFile(t *testing.T) {
	root := writeManifest(t, `
version: 1
bots:
  - name: from-file
    model: claude-haiku-4-5
    handler: claude
`)
	inlineBody := `
bots:
  - name: from-inline
    model: claude-sonnet-4-6
    handler: claude
`
	m, err := LoadPreferringInline(root, inlineNode(t, inlineBody))
	if err != nil {
		t.Fatalf("LoadPreferringInline: %v", err)
	}
	if m == nil || len(m.Bots) != 1 {
		t.Fatalf("expected single bot from inline source, got %+v", m)
	}
	if m.Bots[0].Name != "from-inline" {
		t.Errorf("inline should have won; got bot %q", m.Bots[0].Name)
	}
}

// TestLoadPreferringInline_FallsBackToFile pins the
// transition behavior: when inline is absent, the legacy
// enju/bots.yaml is read. Without this fallback, every
// existing project authoring stand-alone bots would silently
// lose its roster once the schema acquires the inline knob.
func TestLoadPreferringInline_FallsBackToFile(t *testing.T) {
	root := writeManifest(t, `
version: 1
bots:
  - name: legacy-bot
    model: claude-haiku-4-5
    handler: claude
`)
	var empty yamlv3.Node // no inline block
	m, err := LoadPreferringInline(root, empty)
	if err != nil {
		t.Fatalf("LoadPreferringInline: %v", err)
	}
	if m == nil || len(m.Bots) != 1 {
		t.Fatalf("expected single bot from legacy file, got %+v", m)
	}
	if m.Bots[0].Name != "legacy-bot" {
		t.Errorf("fallback should have surfaced legacy bot, got %q", m.Bots[0].Name)
	}
}

// TestLoadPreferringInline_BothEmptyReturnsNil pins the
// no-bots-anywhere case: a project that authors neither
// inline nor a stand-alone file returns (nil, nil) so the
// daemon can treat "no bots" uniformly.
func TestLoadPreferringInline_BothEmptyReturnsNil(t *testing.T) {
	root := t.TempDir()
	var empty yamlv3.Node
	m, err := LoadPreferringInline(root, empty)
	if err != nil {
		t.Errorf("both empty should be silent, got: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil Manifest, got %+v", m)
	}
}
