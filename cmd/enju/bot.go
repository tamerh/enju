package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/bots"
	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// cmdBot is the parent dispatcher for `enju bot <subcommand>`.
// Subcommands cluster every bot-side concern under one verb so
// the help surface stays predictable as more land:
//
//	enju bot setup           # register manifest bots, stash tokens
//	enju bot run --bot=NAME  # (next phase) run the daemon loop
//	enju bot status          # (later) list local bot processes
//
// The bot subsystem is fatclient-local; the coordinator only knows
// bots as kind=bot citizens. See docs/bots.md for the architecture.
func cmdBot(args []string) {
	if len(args) < 1 {
		printBotUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "setup":
		cmdBotSetup(args[1:])
	case "run":
		cmdBotRun(args[1:])
	// TODO(phase4): add "stop", "status", "logs" cases for
	// supervisor MCP-tool wrappers. Update printBotUsage when
	// each lands so the help text stays in sync with the
	// dispatcher.
	default:
		fmt.Fprintf(os.Stderr, "Unknown bot subcommand: %s\n", args[0])
		printBotUsage()
		os.Exit(1)
	}
}

func printBotUsage() {
	fmt.Println(`Usage:
 enju bot setup    Register every bot in enju/bots.yaml against the
            coordinator and stash each bot's auth token at the
            manifest's credentials path. Idempotent — bots whose
            credentials file already exists are skipped.

 enju bot run     Run the bot daemon — polls the coordinator for
            ready tasks assigned to the bot, invokes the LLM
            (claude -p), and submits the result. Walking-skeleton
            scope: action=review and action=vote tasks only.

Run 'enju bot <command> -h' for command-specific help.`)
}

// cmdBotSetup registers every bot declared in the project's
// enju/bots.yaml that doesn't already have a stashed credentials
// file. Owner identity comes from the operator's default
// credentials (~/.enju/credentials.json by default) — bots are
// parented to the registering owner via the coord's parent_id
// link, so each operator running setup ends up with their own
// fleet of differently-named bots if the manifests diverge.
//
// Idempotency rule: presence of a credentials file at the bot's
// declared path is treated as "already registered." The file
// might be stale (token revoked, deleted bot) — we don't probe
// the coord to verify, because the bot daemon's own auth attempt
// will surface the issue with a clearer error than a setup-time
// liveness check would. Re-running setup never re-registers a
// bot whose credentials file exists; operators rotate tokens via
// TODO(future-phase): `enju bot rotate-token --bot=NAME`.
//
// Partial-failure recovery: if registerBot fails AFTER the coord
// created the bot row but BEFORE the response landed, re-running
// setup tries the same name again. The coord auto-disambiguates
// via generateUniqueUsername, so the operator ends up with both
// `developer-bot` (orphaned, no creds locally) and
// `developer-bot-1` (creds at the manifest's developer-bot.json
// path). Manual cleanup: list owned bots with enju_list_my_bots,
// revoke the orphan via enju_revoke_token. Atomic
// register-or-rollback is out of scope for Phase 1.
func cmdBotSetup(args []string) {
	fs := flag.NewFlagSet("bot setup", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	credsPath := fs.String("credentials", "", "Path to OWNER credentials.json (default ~/.enju/credentials.json). Used only to authenticate the registration calls — bot tokens land at each bot's manifest-declared path.")
	projectDir := fs.String("project", ".", "Project directory containing enju/bots.yaml")
	dryRun := fs.Bool("dry-run", false, "Print what would happen without registering or writing files")
	fs.Parse(args)

	// Resolve the project root to an absolute path so error
	// messages and credential paths show something meaningful
	// regardless of where the operator invoked from.
	absProject, err := filepath.Abs(*projectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving --project=%q: %v\n", *projectDir, err)
		os.Exit(1)
	}

	manifest, err := bots.Load(absProject)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading %s/enju/bots.yaml: %v\n", absProject, err)
		os.Exit(1)
	}
	if manifest == nil || len(manifest.Bots) == 0 {
		fmt.Fprintf(os.Stderr, "No bots declared at %s/enju/bots.yaml — nothing to do.\n", absProject)
		return
	}

	resolvedOwnerCreds := resolveCredentialsPath(*credsPath)
	owner := loadCredentialsAt(*coordinator, resolvedOwnerCreds)
	if owner == nil || owner.Token == "" {
		fmt.Fprintf(os.Stderr, "No owner credentials found for %s at %s — run `enju mcp` once to register your own identity first.\n", *coordinator, resolvedOwnerCreds)
		os.Exit(1)
	}

	// Pre-loop summary so the operator sees what's about to
	// happen before any coord write fires. Especially valuable
	// on first-time setup against an example project (clones
	// + setup might silently register N citizens otherwise — a
	// brief line lets the operator Ctrl-C if the count or the
	// names look wrong). Not an interactive prompt — that would
	// break scripted setup and the example-project happy path.
	names := make([]string, 0, len(manifest.Bots))
	for i := range manifest.Bots {
		names = append(names, manifest.Bots[i].Name)
	}
	fmt.Fprintf(os.Stderr, "Setting up %d bot(s) declared in %s/enju/bots.yaml: %s\n", len(manifest.Bots), absProject, strings.Join(names, ", "))
	fmt.Fprintf(os.Stderr, "Coordinator: %s — owner: %s\n\n", *coordinator, owner.Username)

	// Tally counters for the trailing summary; feels small but
	// the user wants to see "what happened" at a glance,
	// especially for first-time setup against an example
	// project with several bots.
	var registered, skipped, failed int
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := range manifest.Bots {
		b := &manifest.Bots[i]
		// Idempotency: a credentials file at the declared path
		// means setup has already run for this bot. We don't
		// validate its contents — the daemon will surface a
		// clearer error if the token is bad than we would here.
		if _, err := os.Stat(b.Credentials); err == nil {
			fmt.Printf("  %-20s ✓ already set up — credentials at %s\n", b.Name, b.Credentials)
			skipped++
			continue
		}
		if *dryRun {
			fmt.Printf("  %-20s [dry-run] would register and stash at %s\n", b.Name, b.Credentials)
			registered++
			continue
		}

		token, username, err := registerBot(ctx, *coordinator, owner.Token, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-20s ✗ register failed: %v\n", b.Name, err)
			failed++
			continue
		}

		// Stash the bot's credentials in the same shape the
		// human's credentials.json uses, so the bot daemon can
		// reuse the existing fatclient identity-loading path
		// without a parallel format. The username field carries
		// whatever username the coord assigned (the manifest
		// requested b.Name; the coord may auto-suffix on
		// collision via generateUniqueUsername).
		if err := writeBotCredentials(b.Credentials, *coordinator, username, b.Name, token); err != nil {
			fmt.Fprintf(os.Stderr, "  %-20s ✗ token written but credentials file save failed: %v\n   IMPORTANT: stash this token NOW or the bot is unrecoverable: %s\n", b.Name, err, token)
			failed++
			continue
		}
		fmt.Printf("  %-20s ✓ registered as %s, credentials at %s\n", b.Name, username, b.Credentials)
		registered++
	}

	// Ensure enju/bots/ (per-bot worktree dir) is in the
	// project's gitignore. Worktrees aren't accidentally
	// added by `git add` (the .git pointer file makes git
	// treat them as separate repos), but `git status` would
	// otherwise list them as untracked dirs — noisy. Adding
	// to the managed block keeps the working tree clean.
	if !*dryRun {
		if changed, err := bots.EnsureGitignored(absProject); err != nil {
			fmt.Fprintf(os.Stderr, "  ! failed to update .gitignore for enju/bots/: %v\n", err)
		} else if changed {
			fmt.Println("  + .gitignore updated to ignore enju/bots/")
		}
	}

	fmt.Printf("\n%d registered, %d skipped, %d failed\n", registered, skipped, failed)
	if failed > 0 {
		os.Exit(2)
	}
}

// registerBot POSTs to /api/v1/citizens/me/bots and returns the
// freshly-issued token + the username the coord assigned. The
// owner's bearer authenticates the call; the coord parents the
// new bot citizen to the owner's id.
//
// We reach for net/http directly here rather than going through
// internal/fatclient/coord because that client is built around
// a long-lived session with auto-reregister semantics — overkill
// for a one-shot CLI registration call. A few hand-rolled HTTP
// lines is clearer than threading a Client through cmd/enju.
func registerBot(ctx context.Context, coordURL, ownerToken string, b *bots.Bot) (token, username string, err error) {
	body := map[string]string{
		"name": b.Name,
		// Manifest's name field doubles as the requested
		// username — the coord auto-disambiguates on collision
		// (returns the actually-assigned username in the
		// response), so even a recycled name lands cleanly.
		"username": b.Name,
	}
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", coordURL+"/api/v1/citizens/me/bots", bytes.NewReader(jsonBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("coord %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", fmt.Errorf("decoding response: %w", err)
	}
	if out.Token == "" {
		return "", "", fmt.Errorf("coord returned empty token")
	}
	return out.Token, out.Username, nil
}

// writeBotCredentials writes a credentials.json with 0600 mode
// at the bot's manifest path. Mirrors saveCredentialsAt's wire
// format (Coordinator/Username/Name/Token) so the bot daemon —
// which will reuse loadCredentialsAt — reads it without a
// schema branch on "is this a human or a bot."
//
// Refuses to overwrite an existing file: the idempotency check
// upstream already handled "skip if file exists," so reaching
// here with the file present indicates a TOCTOU race or a logic
// bug. Better to fail loudly than silently rotate a token.
func writeBotCredentials(path, coordinator, username, name, token string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("credentials file already exists at %s — refusing to overwrite", path)
	}
	parentDir := filepath.Dir(path)
	if err := os.MkdirAll(parentDir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDir, err)
	}
	// MkdirAll's mode is "for newly created directories only" —
	// if the parent already exists at 0755 (e.g. ~/.enju was
	// created by some earlier tool), the chmod above is a no-op
	// and the bot creds dir would be world-readable. Force the
	// tightening explicitly. Token files inside are 0600, but
	// directory traversal would still leak filenames (which are
	// bot names — could expose internal naming) without this.
	if err := os.Chmod(parentDir, 0700); err != nil {
		return fmt.Errorf("chmod %s to 0700: %w", parentDir, err)
	}
	creds := credentials{
		Coordinator: coordinator,
		Username:    username,
		Name:        name,
		Token:       token,
	}
	data, _ := json.MarshalIndent(creds, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// cmdBotRun launches the bot daemon — the long-running loop
// that polls the coord for tasks assigned to this bot, hands
// each prompt to the LLM, and submits the result.
//
// One process = one bot = one project. Run multiple `enju bot
// run` processes (different --bot, different cwd) for fleets.
//
// Walking-skeleton scope: action=review + action=vote only.
// Anything else surfaces an error from the runner; operators
// should leave non-supported tasks for humans (or wait for
// the git-aware bot path in Phase 2.4+).
func cmdBotRun(args []string) {
	fs := flag.NewFlagSet("bot run", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	botName := fs.String("bot", "", "Bot name from enju/bots.yaml (required)")
	projectDir := fs.String("project", ".", "Project directory containing enju/bots.yaml")
	projectID := fs.Int64("project-id", 0, "Optional project id to scope task discovery (0 = across every project the bot is a member of)")
	once := fs.Bool("once", false, "Run a single iteration then exit (for first-touch testing)")
	pollInterval := fs.Duration("poll-interval", 1*time.Second, "Floor sleep between empty polls (doubles up to --backoff-max)")
	backoffMax := fs.Duration("backoff-max", 30*time.Second, "Max sleep between empty polls — caps the exponential backoff")
	allowTools := fs.String("allow-tools", "", "Comma-separated MCP tool allowlist forwarded to any MCP host the daemon spawns. Defaults to the manifest's mcp_tools.allow when empty. v1 review/vote actions don't currently spawn MCP — the allowlist is declarative-only until action=contribute (Phase 2.4+).")
	fs.Parse(args)

	if *botName == "" {
		fmt.Fprintln(os.Stderr, "--bot=<name> is required (must match a bot in enju/bots.yaml)")
		os.Exit(1)
	}

	absProject, err := filepath.Abs(*projectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving --project=%q: %v\n", *projectDir, err)
		os.Exit(1)
	}
	manifest, err := bots.Load(absProject)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading manifest: %v\n", err)
		os.Exit(1)
	}
	if manifest == nil {
		fmt.Fprintf(os.Stderr, "no enju/bots.yaml found at %s — run `enju bot setup` first\n", absProject)
		os.Exit(1)
	}
	bot := manifest.ByName(*botName)
	if bot == nil {
		fmt.Fprintf(os.Stderr, "bot %q not found in %s/enju/bots.yaml\n", *botName, absProject)
		os.Exit(1)
	}

	// Load the bot's credentials. The setup step (enju bot setup)
	// already wrote them at bot.Credentials; if they're missing
	// the operator skipped setup or moved the file — surface
	// loudly with the recovery hint.
	creds := loadCredentialsAt(*coordinator, bot.Credentials)
	if creds == nil || creds.Token == "" {
		fmt.Fprintf(os.Stderr, "no credentials for bot %q at %s — run `enju bot setup` first (or check the manifest's credentials path matches your coord URL)\n", bot.Name, bot.Credentials)
		os.Exit(1)
	}

	// Read the bot's system prompt from the manifest path.
	// Optional — an empty system prompt is unusual but legal,
	// and we don't want a missing file to crash the loop on
	// startup. Just log + proceed with no system prompt.
	systemPrompt, err := os.ReadFile(filepath.Join(absProject, bot.SystemPrompt))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't read system prompt at %s: %v — continuing with empty system prompt\n", bot.SystemPrompt, err)
		systemPrompt = nil
	}

	// Pre-flight: confirm the LLM CLI is on PATH. Without
	// this check, the first task invocation would fail
	// mid-iteration with an exec lookup error AFTER claiming
	// the task — operator would have to release the claim
	// manually. Cheap to check upfront; saves a debug round.
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(os.Stderr, "claude CLI not found on PATH — install Claude Code first (https://claude.com/claude-code), or pass --allow-tools=... to skip if you've configured a different LLM backend (not yet supported, but the LookPath check would be moved at that point).")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	coordClient := coord.New(coord.Config{
		BaseURL:     *coordinator,
		Username:    creds.Username,
		CitizenName: creds.Name,
		AuthToken:   creds.Token,
		Logger:      logger,
	})
	runner := &bots.Runner{
		Bot:          bot,
		ProjectID:    *projectID,
		Coord:        &bots.HTTPCoordClient{C: coordClient},
		LLM:          &bots.ClaudeBackend{}, // TODO(phase2.4): pluggable backend per manifest
		Logger:       logger,
		PollInterval: *pollInterval,
		BackoffMax:   *backoffMax,
		SystemPrompt: string(systemPrompt),
	}

	fmt.Fprintf(os.Stderr, "Bot %q running against %s (model=%s, project_id=%d)\n",
		bot.Name, *coordinator, bot.Model, *projectID)
	// Resolve the tool allowlist: --allow-tools flag wins;
	// otherwise the manifest's mcp_tools.allow. Recording it
	// loudly so operators see the trust-model wiring even
	// though v1 actions don't currently spawn an MCP host
	// for the LLM (review/vote are text-only). When Phase 2.4
	// adds action=contribute the daemon will spawn `enju mcp
	// --allow-tools=...` for claude code's MCP toolbox; this
	// resolution path already feeds the right list.
	allowList := splitAllowTools(*allowTools)
	if len(allowList) == 0 && bot.MCPTools != nil {
		allowList = bot.MCPTools.Allow
	}
	if len(allowList) > 0 {
		fmt.Fprintf(os.Stderr, "Tool allowlist (declarative for v1 review/vote; pinned in MCP host for action=contribute): %v\n", allowList)
	}
	fmt.Fprintln(os.Stderr, "Walking-skeleton scope: action=review and action=vote only. Tasks with {{task.X.content}} upstream-content references will see literal placeholders (no git resolution yet — Phase 2.4+).")
	if *once {
		fmt.Fprintln(os.Stderr, "Single-iteration mode (--once): will exit after one claim+submit cycle (or no-work).")
	}

	// Signal handling + stdin-EOF: two ways the daemon's
	// owner can request a graceful shutdown.
	//
	//  - SIGINT/SIGTERM: standard Unix; covers operator Ctrl-C
	//    and `kill PID`. Windows handles SIGINT but NOT SIGTERM
	//    (no symbolic equivalent), so signal.NotifyContext
	//    falls back to os.Interrupt-only there.
	//  - stdin-EOF: the supervisor (Phase 4 fatclient MCP tool)
	//    closes the daemon's stdin pipe; the daemon notices
	//    EOF and triggers the same shutdown path. Cross-platform
	//    via stdlib only — no signal-tree gymnastics.
	//
	// Both feed the same context cancel so the runner's defer
	// ReleaseActiveClaim fires uniformly across exit paths.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Pre-flight: warn loudly if stdin is /dev/null or a
	// regular file. Both EOF immediately and would shut the
	// daemon down before it does any work — a confusing
	// silent-no-op for operators who try `nohup enju bot run
	// &` (which redirects stdin to /dev/null by default). The
	// warning tells them to use a long-lived stdin (pipe from
	// supervisor, interactive terminal, or `< /dev/zero` as
	// an escape hatch for service managers without a pipe).
	if st, err := os.Stdin.Stat(); err == nil {
		mode := st.Mode()
		// Heuristic: a regular file or /dev/null shows up as
		// "regular" (mode.IsRegular()) or with the 0 size +
		// no terminal/pipe bits. We flag both — false
		// positives (rare custom setups) just see a warning.
		if mode.IsRegular() || (mode&os.ModeCharDevice == 0 && mode&os.ModeNamedPipe == 0 && mode&os.ModeSocket == 0) {
			fmt.Fprintln(os.Stderr, "WARNING: stdin is not a TTY/pipe/socket — the daemon may shut down immediately on EOF. To run detached, pass `< /dev/zero` or use a supervisor that pipes stdin (Phase 4 fatclient supervisor handles this; nohup alone redirects stdin to /dev/null and triggers immediate shutdown).")
		}
	}

	go watchStdinEOF(os.Stdin, cancel)

	if *once {
		// --once skips Run() (and thus its deferred release).
		// Do the same release pass here so a Ctrl-C or LLM
		// failure during a single iteration doesn't leak the
		// claim for the reaper to clean up later.
		defer runner.ReleaseActiveClaim()
		err := runner.RunOnce(ctx)
		switch {
		case err == nil:
			// Claimed and submitted — exit 0.
		case errors.Is(err, bots.ErrNoWork):
			// Distinct from success so CI / scripted callers
			// can branch on it (e.g. systemd timer skips a
			// nudge if there's nothing pending).
			fmt.Fprintln(os.Stderr, "no work available; exiting (--once)")
			os.Exit(3)
		default:
			fmt.Fprintf(os.Stderr, "iteration error: %v\n", err)
			os.Exit(2)
		}
		return
	}

	if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "bot daemon exited: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "bot daemon stopped (signal or stdin EOF received)")
}

// splitAllowTools parses the --allow-tools comma-separated
// flag into a slice. Whitespace is trimmed, empties dropped.
// Mirrors the parsing in cmd/enju/main.go's cmdMCP so the bot
// daemon's flag and the MCP server's flag have identical
// semantics.
func splitAllowTools(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// watchStdinEOF reads os.Stdin into a discard buffer until EOF
// (or any read error), then calls the supplied cancel func to
// trigger graceful shutdown.
//
// Cross-platform shutdown trigger: a supervisor process (the
// fatclient MCP tool that started this daemon) closes its end
// of the stdin pipe; the daemon's read returns io.EOF; we
// cancel the context; the runner's deferred ReleaseActiveClaim
// fires; clean exit.
//
// Edge cases:
//   - Operator runs the daemon interactively (terminal stdin):
//     the goroutine blocks reading until the operator hits
//     Ctrl-D. That's intentional — Ctrl-D is also the canonical
//     "I'm done with this process" signal.
//   - Stdin is /dev/null (e.g. nohup): read returns EOF
//     immediately, daemon shuts down at startup. Operators who
//     want a detached daemon should set up a long-lived stdin
//     (e.g. `enju bot run < /dev/null` is the wrong answer; use
//     a pipe from a supervisor or a service manager).
//   - Stdin is a pipe but the writer never closes: read blocks;
//     shutdown via SIGINT/SIGTERM still works. Win-win.
func watchStdinEOF(stdin io.Reader, cancel context.CancelFunc) {
	buf := make([]byte, 256)
	for {
		_, err := stdin.Read(buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bot daemon: stdin EOF — initiating graceful shutdown")
			cancel()
			return
		}
		// Discard whatever bytes the supervisor sent. We
		// don't read commands over stdin (yet); this
		// goroutine exists solely to detect EOF.
	}
}
