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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
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
	// Stop / status / logs are MCP tools (enju_bot_stop,
	// enju_bot_status, enju_bot_logs) rather than CLI
	// subcommands — operators manage the running fleet through
	// the same MCP host they use for every other coord
	// interaction. CLI subcommand mirrors aren't planned for
	// v1; if a CLI-only operator workflow appears the dispatcher
	// is the place to add them.
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
	projectIDFlag := fs.Int64("project-id", 0, "Project id to add bots to as members (overrides manifest's project_id). 0 = no auto-add; operator must call enju_add_project_member manually.")
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

	// Resolve effective project id for membership auto-add. Flag
	// wins so an operator can override the committed manifest
	// without editing it; manifest is the convenient default for
	// projects with a single stable coord.
	effectiveProjectID := *projectIDFlag
	if effectiveProjectID == 0 {
		effectiveProjectID = manifest.ProjectID
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
	fmt.Fprintf(os.Stderr, "Coordinator: %s — owner: %s\n", *coordinator, owner.Username)
	if effectiveProjectID > 0 {
		fmt.Fprintf(os.Stderr, "Project membership: bots will be added to project #%d\n\n", effectiveProjectID)
	} else {
		fmt.Fprintln(os.Stderr, "Project membership: skipped (pass --project-id=N or set project_id in manifest to auto-add)")
		fmt.Fprintln(os.Stderr)
	}

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
		//
		// We still run the membership add for already-registered
		// bots when --project-id is set: the bot might have been
		// registered before the auto-add path existed, or the
		// previous setup ran without a project id. Membership is
		// idempotent on the coord side, so re-running is safe and
		// fixes the legacy "registered but not a project member"
		// state.
		if _, err := os.Stat(b.Credentials); err == nil {
			fmt.Printf("  %-20s ✓ already set up — credentials at %s\n", b.Name, b.Credentials)
			skipped++
			if effectiveProjectID > 0 && !*dryRun {
				existing := loadCredentialsAt(*coordinator, b.Credentials)
				if existing != nil && existing.Username != "" {
					if err := addBotToProject(ctx, *coordinator, owner.Token, effectiveProjectID, existing.Username); err != nil {
						fmt.Fprintf(os.Stderr, "  %-20s ⚠ couldn't add to project %d: %v\n   add manually: enju_add_project_member project_id=%d username=%s\n",
							b.Name, effectiveProjectID, err, effectiveProjectID, existing.Username)
					} else {
						fmt.Printf("  %-20s ✓ ensured member of project #%d\n", b.Name, effectiveProjectID)
					}
				}
			}
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

		// Auto-add to project membership when configured. Without
		// this, the freshly-registered bot is a citizen but has no
		// access to its project's runs — `enju bot run` would
		// poll, get 403s on /projects/{id}/runs, and never claim
		// anything. The owner's token authorizes the add (operator
		// who ran `enju bot setup` must be a project member; the
		// coord enforces that on the POST).
		//
		// "already a member" is treated as success — the operator
		// re-running setup after a partial registration shouldn't
		// see a failure when the membership step succeeded the
		// first time around.
		if effectiveProjectID > 0 {
			if err := addBotToProject(ctx, *coordinator, owner.Token, effectiveProjectID, username); err != nil {
				fmt.Fprintf(os.Stderr, "  %-20s ⚠ registered but couldn't add to project %d: %v\n   add manually: enju_add_project_member project_id=%d username=%s\n",
					b.Name, effectiveProjectID, err, effectiveProjectID, username)
			} else {
				fmt.Printf("  %-20s ✓ added to project #%d as member\n", b.Name, effectiveProjectID)
			}
		}
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

// addBotToProject POSTs to /api/v1/projects/{id}/members to add
// the freshly-registered bot as a project member. The owner's
// bearer authenticates the call; the coord enforces that the
// caller is itself a member with permission to add (typically
// owner role for new members).
//
// Idempotency: if the bot is already a member, the coord's
// "already a member" / 409-ish response is treated as success
// rather than error. The exact wording is keyed off substrings
// from the coord's add-member handler; a future structured-
// error-code refactor coord-side would make this less brittle.
//
// We re-use net/http directly here for the same reason
// registerBot does — one-shot CLI calls don't need the long-
// lived auto-reregister coord.Client machinery.
func addBotToProject(ctx context.Context, coordURL, ownerToken string, projectID int64, botUsername string) error {
	body := map[string]string{"username": botUsername, "role": "member"}
	jsonBody, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/api/v1/projects/%d/members", coordURL, projectID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// 2xx = added or already-member happy paths.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Treat already-a-member as success regardless of status code.
	// The coord's add-member handler can surface this as either
	// 409 with `{"error": "already a member"}` or 200 with the
	// existing membership row; both shapes mean "done."
	if strings.Contains(strings.ToLower(string(respBody)), "already") {
		return nil
	}
	return fmt.Errorf("coord %d: %s", resp.StatusCode, string(respBody))
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

// cmdBotRun launches the bot daemon: a long-running loop that
// finds tasks assigned to this bot, runs them through the bot's
// Handler (Claude CLI by default, but pluggable per manifest),
// and submits results.
//
// Architectural note: the daemon is a peer consumer of
// service.FatClient — same shape as `enju ui` — so all five
// task actions (answer / contribute / compute / review / vote)
// just work. The daemon adds no orchestration of its own beyond
// "find → claim → handler → submit." Pre-Phase-7 code
// reimplemented a tiny subset of fat-client functionality and
// could only handle 2/5 actions; that code was thrown out and
// rewritten on top of the same FatClient the web UI uses.
//
// One process = one bot. ProjectID > 0 scopes to a single
// project; ProjectID == 0 polls every project the bot is a
// member of. Multi-bot fleets run multiple processes (one per
// manifest entry) — the supervisor MCP tools (enju_bot_start
// / start_all) are the recommended way to orchestrate.
func cmdBotRun(args []string) {
	fs := flag.NewFlagSet("bot run", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	botName := fs.String("bot", "", "Bot name from enju/bots.yaml (required)")
	projectDir := fs.String("project", ".", "Project directory containing enju/bots.yaml")
	projectID := fs.Int64("project-id", 0, "Project id to scope task discovery (0 = every project the bot is a member of)")
	workspaceDir := fs.String("workspace", "", "Directory for per-project local clones (default ~/.enju/workspaces)")
	once := fs.Bool("once", false, "Run a single iteration then exit (for first-touch testing)")
	pollInterval := fs.Duration("poll-interval", 1*time.Second, "Floor sleep between empty polls (doubles up to --backoff-max)")
	backoffMax := fs.Duration("backoff-max", 30*time.Second, "Max sleep between empty polls — caps the exponential backoff")
	// --allow-tools is accepted for parity with the supervisor's
	// argv (which always passes it). The daemon doesn't act on
	// it directly; the manifest's mcp_tools.allow is the
	// authoritative allowlist consumed by the Handler at
	// construction time.
	_ = fs.String("allow-tools", "", "Reserved — manifest mcp_tools.allow is authoritative")
	fs.Parse(args)

	if *botName == "" {
		fmt.Fprintln(os.Stderr, "--bot=<name> is required (must match a bot in enju/bots.yaml)")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

	creds := loadCredentialsAt(*coordinator, bot.Credentials)
	if creds == nil || creds.Token == "" {
		fmt.Fprintf(os.Stderr, "no credentials for bot %q at %s — run `enju bot setup` first\n", bot.Name, bot.Credentials)
		os.Exit(1)
	}

	// Read the bot's system prompt. Empty is legal (some handler
	// types don't use one); a missing file is a soft warning so
	// a typo'd path doesn't crash the daemon at startup.
	var systemPrompt string
	if bot.SystemPrompt != "" {
		data, err := os.ReadFile(filepath.Join(absProject, bot.SystemPrompt))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't read system prompt at %s: %v — continuing with empty system prompt\n", bot.SystemPrompt, err)
		} else {
			systemPrompt = string(data)
		}
	}

	handler, err := bots.NewHandler(bot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build handler: %v\n", err)
		os.Exit(1)
	}

	ws, err := workspace.NewWorkspace(*workspaceDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build workspace: %v\n", err)
		os.Exit(1)
	}
	coordClient := coord.New(coord.Config{
		BaseURL:     *coordinator,
		Username:    creds.Username,
		CitizenName: creds.Name,
		AuthToken:   creds.Token,
		Logger:      logger,
	})
	fc := service.New(service.Config{
		Coord:           coordClient,
		Workspace:       ws,
		ModelName:       bot.Model,
		Logger:          logger,
		ProjectRegistry: projectreg.Open(projectreg.DefaultPath()),
	})

	daemon, err := bots.New(bots.Config{
		FC:           fc,
		Handler:      handler,
		Bot:          bot,
		SystemPrompt: systemPrompt,
		ProjectID:    *projectID,
		PollFloor:    *pollInterval,
		BackoffMax:   *backoffMax,
		Logger:       logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build daemon: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Bot %q running against %s (model=%s, project_id=%d, handler=%s)\n",
		bot.Name, *coordinator, bot.Model, *projectID, bot.Handler)

	// Two graceful-shutdown triggers feeding the same cancel:
	//   - SIGINT/SIGTERM (operator Ctrl-C, `kill PID`)
	//   - stdin-EOF (supervisor closed our stdin pipe)
	// Both fire the daemon's deferred ReleaseActiveClaim so a
	// shutdown mid-iteration doesn't leak the claim.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if st, err := os.Stdin.Stat(); err == nil {
		mode := st.Mode()
		if mode.IsRegular() || (mode&os.ModeCharDevice == 0 && mode&os.ModeNamedPipe == 0 && mode&os.ModeSocket == 0) {
			fmt.Fprintln(os.Stderr, "WARNING: stdin is not a TTY/pipe/socket — the daemon may shut down immediately on EOF. To run detached, pass `< /dev/zero` or use a supervisor that pipes stdin.")
		}
	}
	go watchStdinEOF(os.Stdin, cancel)

	if *once {
		_, err := daemon.RunOnce(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "iteration error: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if err := daemon.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
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
