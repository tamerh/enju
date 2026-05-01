package notify

// Adapter dispatch. Each adapter is a function that takes
// (event, rule, cfg) and side-effects (popup, shell command,
// HTTP POST). Adding a new kind = small adapter file +
// register in dispatch().
//
// Adapters are best-effort by contract. A failure (notify-send
// missing, network down, webhook 500s) logs and the loop
// continues. Notifications missing isn't a state corruption
// issue.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// dispatch picks the adapter for rule.Kind and runs it. New
// kinds register here as additional cases.
func dispatch(ev Event, rule Rule, cfg Config) error {
	switch rule.Kind {
	case "desktop", "":
		return dispatchDesktop(ev, rule)
	case "shell":
		return dispatchShell(ev, rule)
	case "slack":
		return dispatchSlack(ev, rule)
	default:
		return fmt.Errorf("unknown adapter kind %q (rule %q)", rule.Kind, rule.Name)
	}
}

// dispatchDesktop fires a native OS notification:
//
//   Linux:   notify-send "<title>" "<body>"
//   macOS:   osascript -e 'display notification "<body>" with title "<title>"'
//   Windows: not supported in v1; logs and continues. Phase 4c
//            can add BurntToast / similar.
//
// Title is the rule name; body is the rendered Message
// template. Both empty → fall back to the event type so the
// user gets *something* even with a misconfigured rule.
func dispatchDesktop(ev Event, rule Rule) error {
	title := rule.Name
	if title == "" {
		title = "Enju"
	}
	body := renderTemplate(rule.Message, ev)
	if body == "" {
		body = ev.Type
		if ev.Subtype != "" {
			body = body + "/" + ev.Subtype
		}
		if ev.TaskID != "" {
			body = body + " — " + ev.TaskID
		}
	}

	switch runtime.GOOS {
	case "linux":
		return exec.Command("notify-send", title, body).Run()
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q`, body, title)
		return exec.Command("osascript", "-e", script).Run()
	default:
		// Windows + others: not yet supported. Skip silently —
		// adapter availability isn't a hard error.
		return nil
	}
}

// dispatchShell runs an arbitrary shell command after
// templating its {{...}} tokens against the event. This is
// the universal escape hatch — anything a shell can do
// (curl Discord, run Python, send Twilio SMS, write a log)
// works without writing a new adapter kind.
//
// Trust model:
//
//   - The RULE is trusted (Layer 1 ships with no shell rules;
//     Layer 3 is the user's own config; Layer 2 from a project
//     is gated by allow_shell_rules_from_projects).
//   - The EVENT FIELDS substituted into Do are NOT trusted.
//     Token substitution is plain string replacement — backticks,
//     `;`, `$(...)`, etc. inside event metadata become shell
//     code at runtime. A malicious citizen of a project you trust
//     for shell rules could land an event with task_id=`; rm -rf
//     ~/` and execute on every machine running that rule.
//
// Implication for rule authors: when writing `kind: shell` rules,
// either (a) only reference fields you generate yourself, or
// (b) pass event tokens via env vars / stdin instead of
// command-line substitution, e.g.:
//
//   do: ENJU_TASK_ID="{{task_id}}" ./scripts/handle.sh
//
// (env-var assignment is parsed by sh BEFORE the value is
// expanded as code). v1 doesn't auto-quote because that would
// break legitimate uses of pipes/redirects in Do. See
// docs/notifications.md § Security boundary.
//
// Runs via `sh -c` so users get pipes, redirects, $env, etc.
// The command's stdout/stderr are discarded — adapters are
// fire-and-forget. If the user wants to capture output, they
// pipe to a file in the rule.
func dispatchShell(ev Event, rule Rule) error {
	if rule.Do == "" {
		return fmt.Errorf("shell rule %q has empty Do command", rule.Name)
	}
	expanded := renderTemplate(rule.Do, ev)
	cmd := exec.Command("sh", "-c", expanded)
	// Inherit env so users can reference $VARS in their commands.
	cmd.Env = os.Environ()
	return cmd.Run()
}

// dispatchSlack POSTs to a Slack incoming webhook. The URL
// comes from the SLACK_WEBHOOK_URL env var — secrets stay on
// the user's machine, never embedded in committed YAML. The
// payload is the standard Slack incoming-webhook format
// ({"text": "..."}); rule.Message is rendered as the text.
//
// One webhook = one channel (Slack-side binding). Users who
// want multiple channels register multiple notify bots, each
// with its own SLACK_WEBHOOK_URL. Adding a Channel field to
// Rule was considered and skipped for v1 — channel routing
// via webhook URLs is the standard pattern; multiplexing one
// app credential across channels is hosted-mode-era work.
func dispatchSlack(ev Event, rule Rule) error {
	url := os.Getenv("SLACK_WEBHOOK_URL")
	if url == "" {
		return fmt.Errorf("slack rule %q requires SLACK_WEBHOOK_URL env var (rule.Message would be: %q)",
			rule.Name, renderTemplate(rule.Message, ev))
	}
	return postSlack(url, ev, rule)
}

// postSlack is split out so tests can swap the URL without
// invoking the env-var path. Same wire format either way.
func postSlack(url string, ev Event, rule Rule) error {
	text := renderTemplate(rule.Message, ev)
	if text == "" {
		// Fallback when no message template — surface
		// SOMETHING. Beats a webhook delivering empty strings.
		text = ev.Type
		if ev.Subtype != "" {
			text = text + "/" + ev.Subtype
		}
		if ev.TaskID != "" {
			text = text + " — " + ev.TaskID
		}
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	// Short timeout — Slack's webhook should respond fast, and
	// we don't want notify dispatch to block the poll loop on a
	// dead webhook. Bigger payloads would warrant tuning;
	// notification text is small.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
