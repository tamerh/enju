package bots

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestHandlerArgsToArgv pins the translation convention:
//
//	{key: "value"} → --key value
//	{key: "true"}  → --key       (bare flag)
//	{key: ""}      → --key       (bare flag)
//	{key: "false"} → (omitted)
//
// Plus the determinism guarantee — keys are sorted so argv
// is stable across calls (tests, cache keys, log grepping).
func TestHandlerArgsToArgv(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", map[string]string{}, nil},
		{"single value", map[string]string{"effort": "high"}, []string{"--effort", "high"}},
		{"true is bare flag", map[string]string{"strict": "true"}, []string{"--strict"}},
		{"empty value is bare flag", map[string]string{"verbose": ""}, []string{"--verbose"}},
		{"false omitted", map[string]string{"strict": "false"}, nil},
		{
			"deterministic ordering",
			map[string]string{"z-last": "z", "a-first": "a", "m-mid": "m"},
			[]string{"--a-first", "a", "--m-mid", "m", "--z-last", "z"},
		},
		{
			"mixed shapes",
			map[string]string{
				"effort":     "high",
				"thinking":   "true",
				"streaming":  "false",
				"max-tokens": "8192",
			},
			[]string{"--effort", "high", "--max-tokens", "8192", "--thinking"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := handlerArgsToArgv(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("handlerArgsToArgv(%v):\n  got  %v\n  want %v", c.in, got, c.want)
			}
		})
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
	// only on the ENJU_* additions.
	oldTestEnviron := testEnviron
	t.Cleanup(func() { testEnviron = oldTestEnviron })
	testEnviron = func() []string { return []string{"PATH=/usr/bin"} }

	got := buildSubprocessEnv(HandlerInput{
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
		"ENJU_REVIEW_FEEDBACK",
	} {
		if _, present := envMap[k]; present {
			t.Errorf("%s should be absent when HandlerInput field is empty; got %q", k, envMap[k])
		}
	}
	// PATH from the inherited env survives.
	if envMap["PATH"] != "/usr/bin" {
		t.Errorf("inherited PATH: got %q, want /usr/bin", envMap["PATH"])
	}
}

// TestBuildSubprocessEnv_ExposesFullProtocol pins that every
// non-empty HandlerInput field lands as the right env var. This
// is the contract operators read against when authoring
// out-of-tree handlers.
func TestBuildSubprocessEnv_ExposesFullProtocol(t *testing.T) {
	oldTestEnviron := testEnviron
	t.Cleanup(func() { testEnviron = oldTestEnviron })
	testEnviron = func() []string { return nil }

	got := buildSubprocessEnv(HandlerInput{
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
	}
	for k, v := range want {
		if envMap[k] != v {
			t.Errorf("%s: got %q, want %q", k, envMap[k], v)
		}
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
	b := &Bot{
		Name:        "x",
		Handler:     scriptPath,
		HandlerArgs: map[string]string{"prompt": inject},
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
// the per-call merge from inside ProcessTask. Bot config sets
// effort=medium; the HandlerInput passes effort=high; the
// subprocess argv must show --effort high.
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
	}
	h := NewSubprocessHandler(b)

	out, err := h.ProcessTask(context.Background(), HandlerInput{
		TaskID: "t",
		HandlerArgs: map[string]string{
			"effort":      "high",       // overrides bot's "medium"
			"task-extra":  "task-only",   // task-only key
		},
	})
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	// effort=high (task won), shared=from-bot (bot survives), task-extra=task-only (task contributed)
	for _, want := range []string{
		"arg:--effort", "arg:high",
		"arg:--shared", "arg:from-bot",
		"arg:--task-extra", "arg:task-only",
	} {
		if !strings.Contains(out.Response, want) {
			t.Errorf("argv should contain %q; got:\n%s", want, out.Response)
		}
	}
	// And the bot's stale "medium" must NOT appear (it was
	// overridden — the test catches a regression where the
	// merge accidentally appended both).
	if strings.Contains(out.Response, "arg:medium") {
		t.Errorf("bot's overridden value medium should not appear in argv; got:\n%s", out.Response)
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
