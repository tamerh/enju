package main

// `enju review <task_id>` — terminal counterpart to the
// enju_review MCP tool. Opens $EDITOR for the prose review,
// prompts for a decision, and POSTs to the same coordinator
// submit endpoint enju_review uses.
//
// Scope: bare-bones text-only review submission. The fat-client
// path (commits to topic branch + push) is not used — that's an
// MCP-only flow today. Limitation: the task must already be
// claimed by the calling citizen (claim via Claude/MCP first);
// the coordinator rejects unclaimed submits.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/mcpserver"
)

// validReviewDecisions is the canonical verb set, kept verbatim
// in lockstep with internal/mcpserver/submit.go's
// validateReviewDecision. Any change there should mirror here.
var validReviewDecisions = []string{"approve", "request_changes", "reject", "comment"}

func cmdReview(args []string) {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	credsPath := fs.String("credentials", "", "Path to credentials.json (default ~/.enju/credentials.json)")
	decision := fs.String("decision", "", `Review decision: "approve", "request_changes", "reject", or "comment". Prompted interactively if omitted.`)
	contentFlag := fs.String("content", "", "Review prose. If omitted, $EDITOR opens for you to write it.")
	model := fs.String("model", "", "Optional per-call model override (defaults to whatever your credentials say or the coordinator picks).")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: enju review <task_id> [flags]

Submit a verdict on an action:review task you've claimed. Opens
$EDITOR for the prose if -content is omitted, then prompts for a
decision if -decision is omitted.

The task must already be claimed by you. Use Claude/MCP (or any
existing claim path) to claim first.

Decisions: approve | request_changes | reject | comment

Limitation: this CLI uses the legacy POST submit path. The
fat-client flow (commits to the iteration topic branch and
pushes) is MCP-only today. The audit row lands in events.db,
but 'git log' won't show a per-iteration verdict commit for
CLI-submitted reviews. Run via Claude/MCP if you need the
git-side audit trail.

Flags:`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	taskID := fs.Arg(0)

	resolvedCredsPath := resolveCredentialsPath(*credsPath)
	creds := loadCredentialsAt(*coordinator, resolvedCredsPath)
	if creds == nil || creds.Token == "" {
		fmt.Fprintf(os.Stderr, "no credentials found for coordinator %s at %s — run `enju mcp` once to register first.\n", *coordinator, resolvedCredsPath)
		os.Exit(1)
	}

	// Compose content. -content wins; otherwise $EDITOR.
	content := *contentFlag
	if content == "" {
		// Best-effort: fetch the task's inbox row so the editor
		// template includes the prompt + upstream submission.
		// On any failure (task not in inbox, network blip, parse
		// error) we fall back to a bare template — better than
		// blocking the user.
		ctx := tryFetchReviewContext(*coordinator, creds.Token, taskID)

		var err error
		content, err = composeReviewInEditor(taskID, ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if strings.TrimSpace(content) == "" {
			fmt.Fprintln(os.Stderr, "aborting: empty review content (nothing to submit).")
			os.Exit(1)
		}
	}

	// Resolve decision. -decision wins; otherwise interactive prompt.
	dec := strings.TrimSpace(*decision)
	if dec == "" {
		dec = promptForDecision()
	}
	if !isValidDecision(dec) {
		fmt.Fprintf(os.Stderr, "invalid decision %q (must be one of: %s)\n", dec, strings.Join(validReviewDecisions, ", "))
		os.Exit(2)
	}

	if err := submitReview(*coordinator, creds.Token, taskID, dec, content, *model); err != nil {
		fmt.Fprintf(os.Stderr, "submit failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Submitted %s on %s.\n", dec, taskID)
}

// composeReviewInEditor opens $EDITOR (or vi) on a temp file
// pre-populated with a comment-marked template. Returns the
// non-comment body. Lines starting with `#` (after optional
// whitespace) are stripped — same convention git uses.
//
// ctx, when non-nil, is the inbox row for this task — its
// prompt and upstream submissions get rendered as comment lines
// at the top of the template so the reviewer sees what they're
// reviewing without leaving the editor.
func composeReviewInEditor(taskID string, ctx *mcpserver.InboxRow) (string, error) {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}

	dir := os.TempDir()
	// Sanitize the task id for the filename — colons are fine on
	// most filesystems but we replace defensively.
	safe := strings.NewReplacer(":", "-", "/", "-").Replace(taskID)
	path := filepath.Join(dir, fmt.Sprintf("enju-REVIEW-%s.md", safe))

	tmpl := buildReviewTemplate(taskID, ctx)
	if err := os.WriteFile(path, []byte(tmpl), 0o600); err != nil {
		return "", fmt.Errorf("creating review buffer: %w", err)
	}
	defer os.Remove(path) // best-effort cleanup; OS would reap eventually anyway

	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited non-zero: %w", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading review buffer: %w", err)
	}
	return stripCommentLines(string(raw)), nil
}

// buildReviewTemplate composes the editor pre-fill. When ctx is
// nil, falls back to a bare template (task id only) — the
// best-effort fetch failed and we don't want to block the user.
// Comment lines use `#` so stripCommentLines drops them on save.
func buildReviewTemplate(taskID string, ctx *mcpserver.InboxRow) string {
	var b strings.Builder
	b.WriteString("# Reviewing ")
	b.WriteString(taskID)
	if ctx != nil && ctx.Action != "" {
		fmt.Fprintf(&b, " — %s", ctx.Action)
	}
	b.WriteString("\n")

	if ctx != nil && ctx.Prompt != "" {
		b.WriteString("#\n# This task's prompt:\n")
		for _, line := range strings.Split(strings.TrimRight(ctx.Prompt, "\n"), "\n") {
			fmt.Fprintf(&b, "# > %s\n", line)
		}
		if ctx.PromptTruncated {
			b.WriteString("# > [truncated — run `enju_get_task` for full text]\n")
		}
	}

	if ctx != nil {
		for _, up := range ctx.Upstream {
			b.WriteString("#\n# Upstream ")
			b.WriteString(up.TaskID)
			if up.Action != "" {
				fmt.Fprintf(&b, " (%s)", up.Action)
			}
			if up.CommitSHA != "" {
				fmt.Fprintf(&b, " commit %s", up.CommitSHA)
			}
			b.WriteString(":\n")
			if up.Content == "" {
				b.WriteString("# > (no inlined content — pull from git via the commit_sha)\n")
				continue
			}
			for _, line := range strings.Split(strings.TrimRight(up.Content, "\n"), "\n") {
				fmt.Fprintf(&b, "# > %s\n", line)
			}
		}
	}

	b.WriteString(`#
# Write your review prose below. Lines starting with '#' are
# stripped before submission. Save and exit; empty body aborts.
# Decision: pass -decision flag, or you'll be prompted after save.

`)
	return b.String()
}

// tryFetchReviewContext attempts to find the task in the
// caller's inbox so the editor template can show the prompt +
// upstream content. Best-effort — returns nil on any failure
// (task not in inbox, parse errors, network blips). The user
// gets a bare template in that case, not an error.
//
// Task IDs are <projectID>:<runSeq>:<taskName> by convention,
// so we parse the project from the id and call the inbox
// endpoint. No new endpoint needed.
func tryFetchReviewContext(coordinator, token, taskID string) *mcpserver.InboxRow {
	parts := strings.SplitN(taskID, ":", 3)
	if len(parts) < 1 {
		return nil
	}
	projectID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || projectID <= 0 {
		return nil
	}
	rows, err := fetchInbox(coordinator, token, projectID)
	if err != nil {
		return nil
	}
	for i, r := range rows {
		if r.TaskID == taskID {
			return &rows[i]
		}
	}
	return nil
}

// stripCommentLines removes lines whose first non-whitespace
// character is `#`. Same behavior as git's COMMIT_EDITMSG.
// Trailing whitespace is trimmed; the inner shape is preserved.
func stripCommentLines(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// promptForDecision reads a line from stdin and validates it.
// Re-prompts up to 3 times on invalid input before giving up.
func promptForDecision() string {
	r := bufio.NewReader(os.Stdin)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(os.Stderr, "Decision (%s): ", strings.Join(validReviewDecisions, "/"))
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Fprintln(os.Stderr, "\naborting: no input.")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "read error: %v\n", err)
			os.Exit(1)
		}
		v := strings.TrimSpace(line)
		if isValidDecision(v) {
			return v
		}
		fmt.Fprintf(os.Stderr, "invalid; expected one of: %s\n", strings.Join(validReviewDecisions, ", "))
	}
	fmt.Fprintln(os.Stderr, "aborting: too many invalid attempts.")
	os.Exit(2)
	return ""
}

func isValidDecision(s string) bool {
	for _, v := range validReviewDecisions {
		if s == v {
			return true
		}
	}
	return false
}

// submitReview POSTs to the coordinator's existing submit
// endpoint. Same wire shape the legacy branch of
// handleSubmitResult uses; the fat-client path stays MCP-only.
func submitReview(coordinator, token, taskID, decision, content, model string) error {
	body := map[string]interface{}{
		"decision": decision,
	}
	if content != "" {
		body["content"] = content
	}
	if model != "" {
		body["model"] = model
	}
	payload, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/api/v1/tasks/%s/result", coordinator, taskID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling coordinator: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &errBody) == nil && errBody.Error != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errBody.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
