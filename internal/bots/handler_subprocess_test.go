package bots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestArgSubstitute pins the single-arg substitution behavior:
// {{var}} placeholders resolve from the context map; missing
// or empty values are flagged via hasEmpty so the caller can
// apply the drop rule. Unterminated braces fail loud.
func TestArgSubstitute(t *testing.T) {
	ctx := map[string]string{
		"model":         "claude-sonnet-4-6",
		"system_prompt": "be concise",
		"empty_key":     "",
	}
	cases := []struct {
		name       string
		in         string
		wantOut    string
		wantHasRef bool
		wantEmpty  bool
		wantErr    bool
	}{
		{"no placeholder", "-p", "-p", false, false, false},
		{"single placeholder", "--model={{model}}", "--model=claude-sonnet-4-6", true, false, false},
		{"empty value triggers hasEmpty", "--model={{empty_key}}", "--model=", true, true, false},
		{"missing key treated as empty", "--model={{nonexistent}}", "--model=", true, true, false},
		{"multiple placeholders one empty", "{{model}}/{{nonexistent}}", "claude-sonnet-4-6/", true, true, false},
		{"placeholder in middle", "before-{{model}}-after", "before-claude-sonnet-4-6-after", true, false, false},
		{"unterminated braces error", "--foo={{model", "", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, hasRef, hasEmpty, err := argSubstitute(c.in, ctx)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (out=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.wantOut {
				t.Errorf("out: got %q, want %q", got, c.wantOut)
			}
			if hasRef != c.wantHasRef {
				t.Errorf("hasRef: got %v, want %v", hasRef, c.wantHasRef)
			}
			if hasEmpty != c.wantEmpty {
				t.Errorf("hasEmpty: got %v, want %v", hasEmpty, c.wantEmpty)
			}
		})
	}
}

// TestValidateArgsTemplate pins review-fix #3 + #10: typo'd
// {{var}} names and malformed brace syntax are rejected at
// manifest-load time rather than silently producing dropped
// argv at first claim.
func TestValidateArgsTemplate(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantError string // substring; empty = expects success
	}{
		{"no placeholder", "-p", ""},
		{"known static var", "--model={{model}}", ""},
		{"known: system_prompt", "{{system_prompt}}", ""},
		{"handler_args dynamic key", "{{handler_args.effort}}", ""},
		{"handler_args arbitrary key", "{{handler_args.anything-the-operator-picks}}", ""},
		{"two placeholders in one arg", "{{model}}/{{branch}}", ""},
		// Errors:
		{"unknown static var", "--model={{tsk_id}}", "unknown placeholder {{tsk_id}}"},
		{"unknown static var listed valid names", "{{foo}}", "valid names:"},
		{"unterminated brace", "--foo={{model", "unterminated {{"},
		{"empty placeholder", "{{}}", "empty {{}} placeholder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateArgsTemplate(c.in)
			if c.wantError == "" {
				if err != nil {
					t.Errorf("expected success, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantError)
			}
			if !strings.Contains(err.Error(), c.wantError) {
				t.Errorf("error %q should contain %q", err.Error(), c.wantError)
			}
		})
	}
}

// TestSubArgs_AppliesDropRule pins the empty-substitution
// behavior: a {{var}} resolving to empty causes the whole arg
// to drop from argv, but only when the arg actually CONTAINED
// a {{var}}. Args with no placeholders pass through unchanged
// regardless of context state.
func TestSubArgs_AppliesDropRule(t *testing.T) {
	ctx := map[string]string{
		"model":         "claude-sonnet-4-6",
		"system_prompt": "", // empty → drops args referencing it
	}
	template := []string{
		"-p",                                  // no placeholder → keep
		"--model={{model}}",                    // resolves → keep
		"--append-system-prompt={{system_prompt}}", // empty → DROP
		"--debug",                              // no placeholder → keep
		"{{nonexistent}}",                      // missing → DROP
	}
	got, err := subArgs(template, ctx)
	if err != nil {
		t.Fatalf("subArgs: %v", err)
	}
	want := []string{
		"-p",
		"--model=claude-sonnet-4-6",
		"--debug",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestMergeHandlerArgs pins the task-wins semantics. Bot's
// HandlerArgs is the default; task's HandlerArgs is the per-claim
// override; on collision the task value wins. Mirrors today's
// Env field's merge rule.
func TestMergeHandlerArgs(t *testing.T) {
	cases := []struct {
		name string
		bot  map[string]string
		task map[string]string
		want map[string]string
	}{
		{"both nil", nil, nil, nil},
		{"bot only", map[string]string{"effort": "high"}, nil, map[string]string{"effort": "high"}},
		{"task only", nil, map[string]string{"effort": "low"}, map[string]string{"effort": "low"}},
		{
			"task wins on collision",
			map[string]string{"effort": "medium"},
			map[string]string{"effort": "high"},
			map[string]string{"effort": "high"},
		},
		{
			"disjoint keys union",
			map[string]string{"a": "1"},
			map[string]string{"b": "2"},
			map[string]string{"a": "1", "b": "2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeHandlerArgs(c.bot, c.task)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("merge(%v, %v):\n  got  %v\n  want %v", c.bot, c.task, got, c.want)
			}
		})
	}
}

// TestBuildSubprocessEnv_OmitsEmptyValues pins the protocol
// shape: env vars whose source field on HandlerInput is empty
// are NOT exported as ENJU_FOO="" — they're omitted entirely.
// Some CLIs distinguish "unset" from "set-to-empty" and would
// misbehave on the empty value.
func TestBuildSubprocessEnv_OmitsEmptyValues(t *testing.T) {
	// Lock the test against a known baseline env so we can assert
	// only on the ENJU_* additions. setTestEnviron registers the
	// t.Cleanup that restores the previous value (review fix R4).
	setTestEnviron(t, func() []string { return []string{"PATH=/usr/bin"} })

	got := buildSubprocessEnv(nil, HandlerInput{
		TaskID: "1:1:foo",
		// Action, SystemPrompt, RepoDir, GitDir, Branch,
		// Workspace, ReviewFeedback all empty — none should be
		// exported.
	})
	envMap := envSliceToMap(got)
	if envMap["ENJU_TASK_ID"] != "1:1:foo" {
		t.Errorf("ENJU_TASK_ID: got %q, want %q", envMap["ENJU_TASK_ID"], "1:1:foo")
	}
	for _, k := range []string{
		"ENJU_ACTION", "ENJU_SYSTEM_PROMPT", "ENJU_REPO_DIR",
		"ENJU_GIT_DIR", "ENJU_BRANCH", "ENJU_SCRATCH",
		"ENJU_REVIEW_FEEDBACK", "ENJU_MODEL", "ENJU_ALLOWED_TOOLS",
	} {
		if _, present := envMap[k]; present {
			t.Errorf("%s should be absent when source field is empty; got %q", k, envMap[k])
		}
	}
	// PATH from the inherited env survives.
	if envMap["PATH"] != "/usr/bin" {
		t.Errorf("inherited PATH: got %q, want /usr/bin", envMap["PATH"])
	}
}

// TestBuildSubprocessEnv_ExposesFullProtocol pins that every
// non-empty source field lands as the right env var. This is
// the contract operators read against when authoring out-of-
// tree handlers. Includes ENJU_MODEL + ENJU_ALLOWED_TOOLS
// which are sourced from the handler (per-bot), distinct from
// the HandlerInput fields (per-claim) — review fix R1 moved
// Model + AllowTools off the CLI argv and onto env vars.
func TestBuildSubprocessEnv_ExposesFullProtocol(t *testing.T) {
	setTestEnviron(t, func() []string { return nil })

	h := &SubprocessHandler{
		Model:      "claude-sonnet-4-6",
		AllowTools: []string{"Read", "Edit", "Bash"},
	}
	got := buildSubprocessEnv(h, HandlerInput{
		TaskID:         "p1:r2:t3",
		Action:         "compute",
		SystemPrompt:   "You are a helpful bot.",
		Workspace:      "/tmp/scratch",
		ReviewFeedback: "needs tests",
		RepoDir:        "/proj/.enju/runs/1-foo/snapshot",
		GitDir:         "/proj/.enju/bots/dev/worktree/.git",
		Branch:         "1-foo",
	})
	envMap := envSliceToMap(got)
	want := map[string]string{
		"ENJU_TASK_ID":         "p1:r2:t3",
		"ENJU_ACTION":          "compute",
		"ENJU_SYSTEM_PROMPT":   "You are a helpful bot.",
		"ENJU_SCRATCH":         "/tmp/scratch",
		"ENJU_REVIEW_FEEDBACK": "needs tests",
		"ENJU_REPO_DIR":        "/proj/.enju/runs/1-foo/snapshot",
		"ENJU_GIT_DIR":         "/proj/.enju/bots/dev/worktree/.git",
		"ENJU_BRANCH":          "1-foo",
		"ENJU_MODEL":           "claude-sonnet-4-6",
		"ENJU_ALLOWED_TOOLS":   "Read,Edit,Bash",
	}
	for k, v := range want {
		if envMap[k] != v {
			t.Errorf("%s: got %q, want %q", k, envMap[k], v)
		}
	}
}

// TestSubprocessHandler_NoHardcodedClaudeFlags pins the
// binary-agnostic posture: enju emits zero CLI flags on its
// own. A bot with Model + MCPTools set but no Args: list
// produces an empty argv. To pass claude's flags, the
// operator authors `args:` in the bot manifest with
// {{model}} / {{system_prompt}} / {{allowed_tools}} templates
// — claude's flag conventions live in YAML, not in Go.
func TestSubprocessHandler_NoHardcodedClaudeFlags(t *testing.T) {
	dir := t.TempDir()
	// Script captures argv and exits 0; lets us see exactly
	// what the handler tried to pass.
	scriptPath := filepath.Join(dir, "argv-echo.sh")
	body := "#!/bin/sh\nfor a in \"$@\"; do echo \"arg:$a\"; done\nexit 0\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		Name:     "x",
		Handler:  scriptPath,
		Model:    "claude-sonnet-4-6",
		MCPTools: &MCPTools{Allow: []string{"Read", "Edit"}},
	}
	h := NewSubprocessHandler(b)
	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID:       "t",
		SystemPrompt: "system body",
	})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	// None of the claude-specific flags should appear in argv.
	for _, forbidden := range []string{
		"arg:-p", "arg:--model", "arg:--append-system-prompt",
		"arg:--allowedTools", "arg:claude-sonnet-4-6",
		"arg:system body",
	} {
		if strings.Contains(out.Response, forbidden) {
			t.Errorf("argv leaked claude-specific flag %q; got:\n%s", forbidden, out.Response)
		}
	}
	// And the empty handler_args means argv is effectively
	// empty (no flags appear at all).
	if strings.Contains(out.Response, "arg:") {
		t.Errorf("argv should have been empty (no handler_args); got:\n%s", out.Response)
	}
}

// TestSubprocessHandler_SpawnsConfiguredBinary pins the
// generic-handler promise: the binary spawned is whatever the
// bot manifest's handler: field names, NOT hardcoded claude.
// Uses an echo shell script as a fake "LLM CLI."
func TestSubprocessHandler_SpawnsConfiguredBinary(t *testing.T) {
	dir := t.TempDir()
	// A trivial "handler" — reads stdin, prints a tagged line to
	// stdout so we can verify it ran. Also asserts ENJU_TASK_ID
	// was visible to the subprocess by echoing it.
	scriptPath := filepath.Join(dir, "my-handler.sh")
	body := "#!/bin/sh\necho \"handler=$0 task=$ENJU_TASK_ID branch=$ENJU_BRANCH\"\ncat\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	b := &Bot{Name: "x", Handler: scriptPath}
	h := NewSubprocessHandler(b)

	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID: "p:1:fake",
		Prompt: "hello\n",
		Branch: "main",
	})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if !strings.Contains(out.Response, "handler="+scriptPath) {
		t.Errorf("response missing handler-path echo: %q", out.Response)
	}
	if !strings.Contains(out.Response, "task=p:1:fake") {
		t.Errorf("response missing ENJU_TASK_ID echo: %q", out.Response)
	}
	if !strings.Contains(out.Response, "branch=main") {
		t.Errorf("response missing ENJU_BRANCH echo: %q", out.Response)
	}
	if !strings.Contains(out.Response, "hello") {
		t.Errorf("response missing stdin echo: %q", out.Response)
	}
}

// TestSubprocessHandler_NonLLMBinary pins the bigger claim of
// Phase 4b: handlers aren't LLM-specific. A pure shell script
// that doesn't even know what an LLM is can be a valid handler.
// This exercises the protocol from the lint-handler.sh shape's
// perspective: read $ENJU_REPO_DIR, produce a summary on stdout.
func TestSubprocessHandler_NonLLMBinary(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "line-counter.sh")
	body := "#!/bin/sh\nwc -l \"$ENJU_REPO_DIR/file.txt\" | awk '{print \"lines:\"$1}'\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	b := &Bot{Name: "lint", Handler: scriptPath}
	// Deliberately no Model — this isn't an LLM.
	h := NewSubprocessHandler(b)

	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID:  "p:1:lint",
		RepoDir: repoDir,
	})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if !strings.Contains(out.Response, "lines:2") {
		t.Errorf("non-LLM handler should have counted 2 lines via $ENJU_REPO_DIR; got %q", out.Response)
	}
}

// TestSubprocessHandler_ShellInjectionSafe pins the security
// posture: HandlerArgs values containing shell metacharacters
// reach the subprocess as a single argv slot, NEVER passed to
// a shell. exec.Command's argv handling is safe by
// construction; this test guards that we don't accidentally
// route values through a shell wrapper later.
func TestSubprocessHandler_ShellInjectionSafe(t *testing.T) {
	dir := t.TempDir()
	// Handler echoes each arg back so the test sees what argv
	// actually arrived. The injection attempt is in a value.
	scriptPath := filepath.Join(dir, "argv-echo.sh")
	body := "#!/bin/sh\nfor a in \"$@\"; do echo \"arg:$a\"; done\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// SENTINEL must NOT appear as a file or as command output —
	// only as a literal echoed arg. If it ever appeared by
	// itself, a shell layer evaluated the value.
	const inject = "$(touch " + "/tmp/should-never-be-created" + "); echo OWNED"
	// Wire the injection value into argv via a template substitution.
	// The new design routes handler_args through {{handler_args.<key>}}
	// in the bot's Args list — that's the realistic attack surface.
	b := &Bot{
		Name:        "x",
		Handler:     scriptPath,
		HandlerArgs: map[string]string{"prompt": inject},
		Args:        []string{"{{handler_args.prompt}}"},
	}
	h := NewSubprocessHandler(b)
	out, err := h.ProcessTask(context.Background(), HandlerInput{TaskID: "t"})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	// The injected value must appear as a literal arg, not be
	// evaluated. echo's "$@" shows each arg on its own line.
	wantLine := "arg:" + inject
	if !strings.Contains(out.Response, wantLine) {
		t.Errorf("injection value should have arrived as literal arg %q; got %q", wantLine, out.Response)
	}
	// And the "echo OWNED" sentinel must NOT have been evaluated
	// (it would have appeared as its own output line, separate
	// from arg:...).
	if strings.Contains(out.Response, "\nOWNED") || strings.HasPrefix(out.Response, "OWNED") {
		t.Errorf("shell injection succeeded — saw OWNED in output: %q", out.Response)
	}
	// Belt-and-suspenders: the touched-file path must not exist
	// (the attacker tried to create it via $(touch ...)).
	if _, err := os.Stat("/tmp/should-never-be-created"); err == nil {
		// If THIS ever passes, we have a real problem — but
		// it requires touch to have succeeded in a subshell.
		// Cleanup so re-runs aren't confused, then fail loud.
		_ = os.Remove("/tmp/should-never-be-created")
		t.Errorf("shell injection succeeded — /tmp/should-never-be-created exists")
	}
}

// TestSubprocessHandler_TaskHandlerArgsOverridesBot exercises
// the per-call merge inside ProcessTask via the
// {{handler_args.<key>}} substitution path. Bot config sets
// effort=medium; the HandlerInput passes effort=high. With the
// bot's args: list referencing {{handler_args.effort}}, the
// spawned argv must show "high" (task won on collision).
func TestSubprocessHandler_TaskHandlerArgsOverridesBot(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "argv-echo.sh")
	body := "#!/bin/sh\nfor a in \"$@\"; do echo \"arg:$a\"; done\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		Name:        "x",
		Handler:     scriptPath,
		HandlerArgs: map[string]string{"effort": "medium", "shared": "from-bot"},
		Args: []string{
			"--effort={{handler_args.effort}}",
			"--shared={{handler_args.shared}}",
			"--task-extra={{handler_args.task-extra}}",
		},
	}
	h := NewSubprocessHandler(b)

	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID: "t",
		HandlerArgs: map[string]string{
			"effort":     "high",       // overrides bot's "medium"
			"task-extra": "task-only",  // task-only key
		},
	})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	// effort=high (task won), shared=from-bot (bot value survived),
	// task-extra=task-only (task contributed).
	for _, want := range []string{
		"arg:--effort=high",
		"arg:--shared=from-bot",
		"arg:--task-extra=task-only",
	} {
		if !strings.Contains(out.Response, want) {
			t.Errorf("argv should contain %q; got:\n%s", want, out.Response)
		}
	}
	// And the bot's stale "medium" must NOT appear.
	if strings.Contains(out.Response, "arg:--effort=medium") {
		t.Errorf("bot's overridden value medium should not appear in argv; got:\n%s", out.Response)
	}
}

// TestSubprocessHandler_ArgsTemplateSubstitution is the
// end-to-end pin for the templated-argv design (Phase 4b-r1):
// the operator writes claude's argv shape in YAML; enju
// substitutes the runtime values. No claude-specific knowledge
// in Go.
func TestSubprocessHandler_ArgsTemplateSubstitution(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "argv-echo.sh")
	body := "#!/bin/sh\nfor a in \"$@\"; do echo \"arg:$a\"; done\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// System prompt body must come from a real file since the
	// handler reads it at invoke time.
	sysPromptPath := filepath.Join(dir, "system.md")
	if err := os.WriteFile(sysPromptPath, []byte("be concise"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		Name:         "claude-shape-bot",
		Handler:      scriptPath,
		Model:        "claude-sonnet-4-6",
		SystemPrompt: sysPromptPath,
		MCPTools:     &MCPTools{Allow: []string{"Read", "Write"}},
		Args: []string{
			"-p",
			"--model={{model}}",
			"--append-system-prompt={{system_prompt}}",
			"--allowedTools={{allowed_tools}}",
			"--task-id={{task_id}}",
			"--branch={{branch}}",
		},
	}
	h := NewSubprocessHandler(b)

	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID: "p:1:t",
		Branch: "main",
	})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	for _, want := range []string{
		"arg:-p",
		"arg:--model=claude-sonnet-4-6",
		"arg:--append-system-prompt=be concise",
		"arg:--allowedTools=Read,Write",
		"arg:--task-id=p:1:t",
		"arg:--branch=main",
	} {
		if !strings.Contains(out.Response, want) {
			t.Errorf("argv should contain %q; got:\n%s", want, out.Response)
		}
	}
}

// TestSubprocessHandler_SystemPromptRelativeToClaimCWD pins the
// showcase_v15 fix: `system_prompt: prompts/dev-bot2.md` declared
// inline in a workflow's bots: block is a repo-relative path and
// must be opened relative to the per-claim materialized CWD,
// NOT the daemon's process CWD.
//
// Pre-fix shape: os.ReadFile(SystemPromptPath) used the daemon's
// process CWD, so any inline bot that authored a repo-relative
// path (the natural way to write it) failed every claim with
// "no such file or directory" — the file existed in the
// materialized claim CWD but the read site didn't know that.
func TestSubprocessHandler_SystemPromptRelativeToClaimCWD(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "argv-echo.sh")
	body := "#!/bin/sh\nfor a in \"$@\"; do echo \"arg:$a\"; done\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate the per-claim materialized CWD shape:
	// <project>/.enju/bots/<bot>/scratch/<task-iter>/prompts/dev-bot2.md
	claimCWD := filepath.Join(dir, ".enju", "bots", "dev-bot2", "scratch", "7-1-summarize-iter-1")
	promptDir := filepath.Join(claimCWD, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "dev-bot2.md"),
		[]byte("be concise and project-aware"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		Name:         "dev-bot2",
		Handler:      scriptPath,
		Model:        "claude-haiku-4-5-20251001",
		SystemPrompt: "prompts/dev-bot2.md", // REPO-RELATIVE, as authored inline
		Args: []string{
			"--append-system-prompt={{system_prompt}}",
		},
	}
	h := NewSubprocessHandler(b)

	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID:    "7:1:summarize",
		Workspace: claimCWD,
	})
	if err != nil {
		t.Fatalf("ProcessTask should resolve system_prompt against claim CWD: %v", err)
	}
	if !strings.Contains(out.Response, "arg:--append-system-prompt=be concise and project-aware") {
		t.Errorf("system prompt body didn't make it through {{system_prompt}} substitution; got:\n%s",
			out.Response)
	}
}

// TestSubprocessHandler_SystemPromptAbsolutePathStillWorks pins the
// fallback contract: when an operator authors an absolute path
// (e.g. /etc/enju/prompts/...), the claim-CWD resolution is bypassed
// and the absolute path is used verbatim. Same logic for empty
// Workspace (handler opted out of CWD materialization, test paths).
func TestSubprocessHandler_SystemPromptAbsolutePathStillWorks(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "argv-echo.sh")
	body := "#!/bin/sh\nfor a in \"$@\"; do echo \"arg:$a\"; done\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	absPrompt := filepath.Join(dir, "absolute-prompt.md")
	if err := os.WriteFile(absPrompt, []byte("absolute body"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		Name:         "absolute-bot",
		Handler:      scriptPath,
		SystemPrompt: absPrompt,
		Args:         []string{"--append-system-prompt={{system_prompt}}"},
	}
	h := NewSubprocessHandler(b)

	// Workspace points at a real (but otherwise empty) dir so the
	// handler's chdir succeeds; the system_prompt path is absolute,
	// so the Workspace value isn't consulted for prompt resolution.
	emptyClaimCWD := t.TempDir()
	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID:    "p:1:t",
		Workspace: emptyClaimCWD,
	})
	if err != nil {
		t.Fatalf("ProcessTask with absolute system_prompt: %v", err)
	}
	if !strings.Contains(out.Response, "arg:--append-system-prompt=absolute body") {
		t.Errorf("absolute system_prompt path lost in resolution; got:\n%s", out.Response)
	}
}

// TestSubprocessHandler_ArgsDropEmptySubstitutions pins the
// empty-substitution rule end-to-end: a bot with no model
// shouldn't emit `--model=` (which most CLIs reject); the
// whole arg drops.
func TestSubprocessHandler_ArgsDropEmptySubstitutions(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "argv-echo.sh")
	body := "#!/bin/sh\nfor a in \"$@\"; do echo \"arg:$a\"; done\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Bot{
		Name:    "lint-bot",
		Handler: scriptPath,
		// No Model, no SystemPrompt — those templates resolve empty.
		Args: []string{
			"--format=json",                                       // no template → kept
			"--model={{model}}",                                    // empty → dropped
			"--append-system-prompt={{system_prompt}}",             // empty → dropped
			"--strict",                                             // no template → kept
		},
	}
	h := NewSubprocessHandler(b)
	out, err := h.ProcessTask(context.Background(), HandlerInput{TaskID: "t"})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if !strings.Contains(out.Response, "arg:--format=json") {
		t.Errorf("--format=json arg should survive; got:\n%s", out.Response)
	}
	if !strings.Contains(out.Response, "arg:--strict") {
		t.Errorf("--strict arg should survive; got:\n%s", out.Response)
	}
	if strings.Contains(out.Response, "arg:--model=") {
		t.Errorf("--model= should have been dropped (empty substitution); got:\n%s", out.Response)
	}
	if strings.Contains(out.Response, "arg:--append-system-prompt=") {
		t.Errorf("--append-system-prompt= should have been dropped; got:\n%s", out.Response)
	}
}

// TestSubprocessHandler_Preflight_BinaryMissing pins the
// startup-failure path: a typo'd handler path fails Preflight
// with a clear error pointing at the bad path. Without this,
// the typo only surfaces at first claim — possibly days later.
func TestSubprocessHandler_Preflight_BinaryMissing(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "does-not-exist")
	h := NewSubprocessHandler(&Bot{Name: "x", Handler: bogus})
	err := h.Preflight()
	if err == nil {
		t.Fatal("expected Preflight error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), bogus) {
		t.Errorf("error should mention the bad path %q; got %v", bogus, err)
	}
}

// TestSubprocessHandler_Preflight_NotExecutable pins the
// permission-mode check: a non-executable file at a real path
// fails Preflight with a mode-related message.
func TestSubprocessHandler_Preflight_NotExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-exec.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewSubprocessHandler(&Bot{Name: "x", Handler: path})
	err := h.Preflight()
	if err == nil {
		t.Fatal("expected Preflight error for non-executable file, got nil")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error should mention executable bit; got %v", err)
	}
}

// TestSubprocessHandler_Preflight_PATHResolution pins the
// $PATH-resolution path: a bare handler name (no slash) is
// looked up via exec.LookPath. Verified by pointing the bot at
// "true" (always present on POSIX) — should succeed.
func TestSubprocessHandler_Preflight_PATHResolution(t *testing.T) {
	h := NewSubprocessHandler(&Bot{Name: "x", Handler: "true"})
	if err := h.Preflight(); err != nil {
		t.Errorf("Preflight against `true` should succeed; got %v", err)
	}

	h2 := NewSubprocessHandler(&Bot{Name: "x", Handler: "this-binary-does-not-exist-12345"})
	if err := h2.Preflight(); err == nil {
		t.Error("Preflight against a missing PATH binary should error")
	}
}

// TestDaemon_IntegrationWithSubprocessHandler drives a full
// daemon iteration with a REAL subprocess handler (not the
// in-process stub) against a shell script pretending to be an
// LLM CLI. Closes the loop between the bot's ephemeral per-claim
// CWD and the subprocess handler surface — proves they compose
// end-to-end.
//
// The shell script:
//   - asserts $ENJU_TASK_ID is set
//   - asserts cwd matches the ephemeral claim CWD the daemon
//     prepared (proves the per-claim materialization reaches
//     the handler)
//   - echoes a deterministic response that the daemon then
//     submits
func TestDaemon_IntegrationWithSubprocessHandler(t *testing.T) {
	// Build a stand-in claim CWD on disk so the handler's
	// cmd.Dir is a real directory the script can chdir into.
	tmp := t.TempDir()
	claimCWD := filepath.Join(tmp, ".enju", "scratch", "integ-bot", "1-1-t")
	if err := os.MkdirAll(claimCWD, 0o755); err != nil {
		t.Fatal(err)
	}

	// Handler binary: a shell script that captures its env
	// + cwd, asserts they're right, and emits a deterministic
	// response. The daemon wires the response back as the
	// task's submitted content.
	scriptPath := filepath.Join(tmp, "test-handler.sh")
	script := `#!/bin/sh
set -e
if [ -z "$ENJU_TASK_ID" ]; then
    echo "missing ENJU_TASK_ID" >&2
    exit 2
fi
# Verify CWD matches the daemon's prepared per-claim CWD
# (production shape: .enju/scratch/<bot>/<task-iter>/).
case "$(pwd)" in
    */.enju/scratch/integ-bot/*) : ;;
    *) echo "unexpected cwd: $(pwd)" >&2; exit 3 ;;
esac
# Read the prompt from stdin and emit a deterministic response.
prompt=$(cat)
echo "INTEG-OK task=$ENJU_TASK_ID branch=$ENJU_BRANCH"
echo "prompt-len=${#prompt}"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// fakeFC machinery shared with daemon_test.go. The daemon
	// runs the handler in the per-claim ephemeral CWD that
	// PrepareLLMClaimCWD returns.
	fc := newFCWithTask("integ-bot", "answer", "ignored-default")
	fc.llmClaimCWDPath = claimCWD

	// REAL SubprocessHandler — not a stub.
	bot := &Bot{Name: "integ-bot", Handler: scriptPath}
	h := NewSubprocessHandler(bot)

	d, err := New(Config{FC: fc, Handler: h, Bot: bot, ProjectID: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Daemon must have submitted what the handler produced.
	if len(fc.submits) != 1 {
		t.Fatalf("expected 1 submit, got %d", len(fc.submits))
	}
	got := fc.submits[0].Content
	if !strings.Contains(got, "INTEG-OK") {
		t.Errorf("daemon didn't submit handler's stdout; got %q", got)
	}
	// Spot-check: the handler observed the right env vars
	// (encoded in its response). If absent here it means
	// either buildSubprocessEnv didn't populate them or
	// daemon didn't thread them through HandlerInput.
	if !strings.Contains(got, "task=") {
		t.Errorf("response missing task echo; env-var threading broken? got: %q", got)
	}
}

// recordingHandler is a Handler that captures inputs and
// returns a fixed Response — like StubHandler, but does NOT
// implement ClaimCWDOptOut, so the daemon's per-claim CWD
// preparation path actually fires. Tests pinning the
// CWD-wiring contract use this; tests covering the rest of
// the daemon lifecycle continue to use StubHandler which
// opts out of the materialization to keep test runtime down.
type recordingHandler struct {
	response string
	err      error
	calls    int
	inputs   []HandlerInput
}

func (h *recordingHandler) ProcessTask(ctx context.Context, in HandlerInput) (HandlerOutput, error) {
	h.calls++
	h.inputs = append(h.inputs, in)
	if h.err != nil {
		return HandlerOutput{}, h.err
	}
	return HandlerOutput{Response: h.response}, nil
}

// TestDaemon_LLMClaimCWD_UsedAsHandlerWorkspace pins P4c.2:
// when the FatClient returns a non-empty claim CWD, the
// daemon uses it (not the persistent worktree) as the
// handler's workspace path. Verifies the per-claim
// ephemeral-CWD shape reaches the handler.
func TestDaemon_LLMClaimCWD_UsedAsHandlerWorkspace(t *testing.T) {
	cwd := t.TempDir()
	fc := newFCWithTask("integ-bot", "answer", "ignored")
	fc.llmClaimCWDPath = cwd

	stub := &recordingHandler{response: "done"}
	d, err := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("expected 1 handler invocation, got %d", len(stub.inputs))
	}
	if got := stub.inputs[0].Workspace; got != cwd {
		t.Errorf("handler Workspace: got %q, want claim CWD %q", got, cwd)
	}
}

// TestDaemon_LLMClaimCWD_EmptyWhenPrepareReturnsEmpty pins
// the no-fallback contract: when PrepareLLMClaimCWD returns ""
// (handler opted out, or legacy task without an iter branch),
// the handler's Workspace stays empty. There's no persistent
// worktree to fall back to in the plumbing-everywhere model —
// stub-style handlers don't read from CWD and the empty value
// is harmless to them.
func TestDaemon_LLMClaimCWD_EmptyWhenPrepareReturnsEmpty(t *testing.T) {
	fc := newFCWithTask("integ-bot", "answer", "ignored")
	fc.llmClaimCWDPath = ""

	stub := &recordingHandler{response: "done"}
	d, err := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := stub.inputs[0].Workspace; got != "" {
		t.Errorf("handler Workspace: got %q, want empty (no fallback to persistent worktree)", got)
	}
}

// TestDaemon_LLMClaimCWD_CleanupOnSuccess pins P4c.4 success
// path: a successful submit triggers CleanupLLMClaimCWD with
// successful=true. The fake records the call; production
// CleanupLLMClaimCWD rm -rf's the dir on this branch.
func TestDaemon_LLMClaimCWD_CleanupOnSuccess(t *testing.T) {
	cwd := t.TempDir()
	fc := newFCWithTask("integ-bot", "answer", "done")
	fc.llmClaimCWDPath = cwd

	stub := &recordingHandler{response: "done"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(fc.llmCleanupCalls) != 1 {
		t.Fatalf("expected 1 cleanup call, got %d", len(fc.llmCleanupCalls))
	}
	got := fc.llmCleanupCalls[0]
	if got.path != cwd {
		t.Errorf("cleanup path: got %q, want %q", got.path, cwd)
	}
	if !got.successful {
		t.Errorf("successful submit should trigger cleanup with successful=true")
	}
}

// TestDaemon_LLMClaimCWD_PreserveOnSubmitFail pins P4c.4
// failure path: when submit fails, the deferred
// CleanupLLMClaimCWD is invoked with successful=false — the
// CWD is preserved on disk so the operator's retry can pick
// up the LLM's work. Mirrors the Phase 5 pattern that
// compute tasks already use.
func TestDaemon_LLMClaimCWD_PreserveOnSubmitFail(t *testing.T) {
	cwd := t.TempDir()
	fc := newFCWithTask("integ-bot", "answer", "done")
	fc.llmClaimCWDPath = cwd
	fc.submitErr = "coord rejected: token expired"

	stub := &recordingHandler{response: "done"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected RunOnce error from submit failure, got nil")
	}
	if len(fc.llmCleanupCalls) != 1 {
		t.Fatalf("expected 1 cleanup call, got %d", len(fc.llmCleanupCalls))
	}
	got := fc.llmCleanupCalls[0]
	if got.path != cwd {
		t.Errorf("cleanup path: got %q, want %q", got.path, cwd)
	}
	if got.successful {
		t.Errorf("failed submit should trigger cleanup with successful=false (preserve on disk)")
	}
}

// TestDaemon_LLMClaimCWD_PrepareErrorProceedsWithEmptyCWD
// pins the error-tolerance contract: when PrepareLLMClaimCWD
// returns an error (filesystem failure, missing git object,
// transient I/O), the daemon logs a warning and proceeds with
// an empty handler Workspace. No silent loss; no crash.
// Production SubprocessHandler will surface the failure
// downstream when the handler binary needs to write to disk,
// but stub-style handlers that don't need CWD continue to work.
func TestDaemon_LLMClaimCWD_PrepareErrorProceedsWithEmptyCWD(t *testing.T) {
	fc := newFCWithTask("integ-bot", "answer", "done")
	fc.llmClaimCWDPath = ""
	fc.llmClaimCWDErr = errors.New("mkdir denied: read-only filesystem")

	stub := &recordingHandler{response: "done"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v (daemon should log + proceed on Prepare error)", err)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("handler should have been invoked despite Prepare error; got %d invocations", len(stub.inputs))
	}
	if got := stub.inputs[0].Workspace; got != "" {
		t.Errorf("handler Workspace after Prepare error: got %q, want empty", got)
	}
	// Cleanup is still called (deferred) — with empty path,
	// which CleanupLLMClaimCWD treats as a no-op in production.
	if len(fc.llmCleanupCalls) != 1 {
		t.Fatalf("expected 1 cleanup call (no-op on empty path), got %d", len(fc.llmCleanupCalls))
	}
	if fc.llmCleanupCalls[0].path != "" {
		t.Errorf("cleanup path: got %q, want empty (Prepare returned no path)", fc.llmCleanupCalls[0].path)
	}
}

// TestDaemon_LLMClaimCWD_PreserveOnHandlerError pins the same
// preserve-on-failure invariant for handler failures (LLM
// crashed before submit). The CWD is still preserved so the
// operator can inspect what the LLM produced before it died.
func TestDaemon_LLMClaimCWD_PreserveOnHandlerError(t *testing.T) {
	cwd := t.TempDir()
	fc := newFCWithTask("integ-bot", "answer", "done")
	fc.llmClaimCWDPath = cwd

	stub := &recordingHandler{err: errors.New("LLM blew up")}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected RunOnce error from handler failure")
	}
	if len(fc.llmCleanupCalls) != 1 {
		t.Fatalf("expected 1 cleanup call (preserve), got %d", len(fc.llmCleanupCalls))
	}
	if fc.llmCleanupCalls[0].successful {
		t.Errorf("handler error should trigger cleanup with successful=false")
	}
}

// envSliceToMap splits a []string env list (KEY=VALUE entries)
// into a map for test inspection. Last write wins on duplicate
// keys, matching what the child subprocess would see.
func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i < 0 {
			continue
		}
		out[e[:i]] = e[i+1:]
	}
	return out
}
