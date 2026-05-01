package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDispatchShellRunsCommandWithTemplating pins the shell
// adapter's load-bearing behaviors:
//
//   - Do is rendered through the same template substitution
//   used by Message ({{type}}, {{task_id}}, etc).
//   - The command runs via `sh -c` (gives users pipes,
//   redirects, env-var expansion).
//   - Empty Do is a tool-level error, not a silent no-op.
//
// The test writes to a temp file via the shell — the file's
// presence + contents prove both substitution and execution.
func TestDispatchShellRunsCommandWithTemplating(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c not available on Windows; shell adapter is POSIX-only in v1")
	}
	dir := t.TempDir()
	outFile := filepath.Join(dir, "got.txt")

	rule := Rule{
		Name: "shell-test",
		Kind: "shell",
		Do:   "echo {{type}}/{{task_id}} > " + outFile,
	}
	ev := Event{
		Type:   "task_completed",
		TaskID: "1:1:draft",
	}
	if err := dispatchShell(ev, rule); err != nil {
		t.Fatalf("dispatchShell: %v", err)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read shell output: %v", err)
	}
	want := "task_completed/1:1:draft\n"
	if string(got) != want {
		t.Errorf("shell output = %q, want %q", string(got), want)
	}
}

// TestDispatchShellEmptyDoErrors pins the validation. A
// silently-skipped empty Do would mask config bugs.
func TestDispatchShellEmptyDoErrors(t *testing.T) {
	err := dispatchShell(Event{}, Rule{Name: "broken", Kind: "shell", Do: ""})
	if err == nil {
		t.Error("expected error on empty Do command")
	}
	if err != nil && !strings.Contains(err.Error(), "empty Do command") {
		t.Errorf("error message should name the issue, got: %v", err)
	}
}

// TestDispatchShellInheritsEnv pins that the shell adapter
// passes through the parent process's environment, so users
// can reference $VARS in their commands. Without this the
// SLACK_WEBHOOK_URL pattern (env-var indirection for secrets)
// wouldn't compose with kind:shell rules.
func TestDispatchShellInheritsEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c not available on Windows")
	}
	t.Setenv("ENJU_NOTIFY_TEST_VAR", "from-parent")
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")

	rule := Rule{
		Kind: "shell",
		Do:   `echo "$ENJU_NOTIFY_TEST_VAR" > ` + outFile,
	}
	if err := dispatchShell(Event{}, rule); err != nil {
		t.Fatalf("dispatchShell: %v", err)
	}
	got, _ := os.ReadFile(outFile)
	if strings.TrimSpace(string(got)) != "from-parent" {
		t.Errorf("env not inherited: got %q", string(got))
	}
}

// TestDispatchSlackPostsRenderedMessage pins the Slack wire
// format: a JSON {"text": "..."} payload to the webhook URL,
// with template substitution applied to rule.Message.
func TestDispatchSlackPostsRenderedMessage(t *testing.T) {
	var captured atomic.Value // map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("slack handler: invalid JSON body: %v", err)
		}
		captured.Store(payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rule := Rule{
		Name:    "slack-test",
		Kind:    "slack",
		Message: "✓ {{task_id}} done by @{{citizen}}",
	}
	ev := Event{
		Type:    "task_completed",
		TaskID:  "1:1:draft",
		Citizen: "tamer",
	}
	// Use postSlack directly so we can inject the test URL
	// without touching env vars.
	if err := postSlack(srv.URL, ev, rule); err != nil {
		t.Fatalf("postSlack: %v", err)
	}

	payload, _ := captured.Load().(map[string]string)
	if payload == nil {
		t.Fatal("slack handler never received a request")
	}
	want := "✓ 1:1:draft done by @tamer"
	if payload["text"] != want {
		t.Errorf("slack text = %q, want %q", payload["text"], want)
	}
}

// TestDispatchSlackFallsBackWhenNoMessage pins that an empty
// Message template doesn't deliver an empty Slack message —
// we synthesize something useful from the event fields.
func TestDispatchSlackFallsBackWhenNoMessage(t *testing.T) {
	var capturedText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		_ = json.Unmarshal(body, &payload)
		capturedText = payload["text"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := postSlack(srv.URL, Event{
		Type:    "task_failed",
		Subtype: "compute",
		TaskID:  "1:1:t",
	}, Rule{Kind: "slack"}); err != nil {
		t.Fatalf("postSlack: %v", err)
	}
	if !strings.Contains(capturedText, "task_failed") {
		t.Errorf("expected fallback text to mention event type; got %q", capturedText)
	}
	if !strings.Contains(capturedText, "1:1:t") {
		t.Errorf("expected fallback text to mention task id; got %q", capturedText)
	}
}

// TestDispatchSlackErrorsOn4xx pins error surfacing — Slack's
// "invalid_payload" / "channel_not_found" 4xx responses become
// adapter errors that the loop logs (and can later trigger
// auto-pause once that's wired in 4d).
func TestDispatchSlackErrorsOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "channel_not_found", http.StatusBadRequest)
	}))
	defer srv.Close()

	err := postSlack(srv.URL, Event{Type: "x"}, Rule{Kind: "slack", Message: "x"})
	if err == nil {
		t.Error("expected error on 4xx response")
	}
	if err != nil && !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention HTTP status, got: %v", err)
	}
}

// TestDispatchSlackMissingEnvErrors pins the env-var
// indirection: without SLACK_WEBHOOK_URL set, dispatchSlack
// returns a clear error naming the missing var. This is the
// "secrets stay on user machine" property — the rule YAML
// just says kind:slack, the user supplies the URL via env.
func TestDispatchSlackMissingEnvErrors(t *testing.T) {
	t.Setenv("SLACK_WEBHOOK_URL", "") // explicit clear
	err := dispatchSlack(Event{Type: "x"}, Rule{Kind: "slack", Message: "y"})
	if err == nil {
		t.Error("expected error when SLACK_WEBHOOK_URL is unset")
	}
	if err != nil && !strings.Contains(err.Error(), "SLACK_WEBHOOK_URL") {
		t.Errorf("error should name the missing env var, got: %v", err)
	}
}

// TestDispatchUnknownKindErrors pins the routing: a typo'd
// rule.Kind doesn't silently drop, it surfaces as an
// adapter-not-found error so users see their misconfiguration.
func TestDispatchUnknownKindErrors(t *testing.T) {
	err := dispatch(Event{}, Rule{Kind: "telegrm" /* typo */, Name: "t"}, Config{})
	if err == nil {
		t.Error("expected error on unknown kind")
	}
	if err != nil && !strings.Contains(err.Error(), "unknown adapter kind") {
		t.Errorf("error should mention unknown kind, got: %v", err)
	}
}

// TestDispatchSlackTimesOutOnSlowEndpoint pins that a
// hung slack endpoint doesn't stall the notify loop forever.
// Uses a server that never responds; the 10s client timeout
// caps the wait. We use a tighter test harness deadline to
// fail fast if the timeout doesn't fire.
func TestDispatchSlackTimesOutOnSlowEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep past the client's 10s timeout. Test harness
		// caps total wait below this with channel/select.
		time.Sleep(20 * time.Second)
	}))
	defer srv.Close()

	done := make(chan error, 1)
	go func() { done <- postSlack(srv.URL, Event{Type: "x"}, Rule{Kind: "slack", Message: "y"}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected timeout error from slow endpoint")
		}
	case <-time.After(15 * time.Second):
		t.Error("postSlack did not respect its 10s timeout (still running after 15s)")
	}
}
