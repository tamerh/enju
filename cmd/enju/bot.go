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

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
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
 enju bot setup --workflow path/to/workflow.yaml
            Register every bot declared inline in the workflow
            YAML's bots: section against the coordinator and
            stash each bot's auth token at the manifest's
            credentials path. Idempotent — bots whose credentials
            file already exists are skipped.

 enju bot run --workflow path/to/workflow.yaml --bot <name>
            Run the bot daemon — polls the coordinator for ready
            tasks assigned to the bot, invokes the handler, and
            submits the result.

Run 'enju bot <command> -h' for command-specific help.`)
}

// cmdBotSetup registers every bot declared inline in the workflow
// YAML's bots: section that doesn't already have a stashed
// credentials file. Owner identity comes from the operator's default
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
	coordinator := fs.String("coordinator", defaultCoordinatorURL(), "Coordinator URL (defaults to value in ~/.enju/credentials.json, else http://localhost:8000)")
	credsPath := fs.String("credentials", "", "Path to OWNER credentials.json (default ~/.enju/credentials.json). Used only to authenticate the registration calls — bot tokens land at each bot's manifest-declared path.")
	workflowPath := fs.String("workflow", "", "Path to the workflow YAML whose inline bots: section declares this fleet (required)")
	projectIDFlag := fs.Int64("project-id", 0, "Project id to add bots to as members. 0 = no auto-add; operator must call enju_add_project_member manually.")
	dryRun := fs.Bool("dry-run", false, "Print what would happen without registering or writing files")
	fs.Parse(args)

	if *workflowPath == "" {
		fmt.Fprintln(os.Stderr, "--workflow=<path/to/workflow.yaml> is required")
		os.Exit(1)
	}
	absWorkflow, err := filepath.Abs(*workflowPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving --workflow=%q: %v\n", *workflowPath, err)
		os.Exit(1)
	}
	// Existence check first so a typo'd path surfaces as
	// "file not found" instead of misleading the operator into
	// thinking the issue is the surrounding git repo.
	if _, err := os.Stat(absWorkflow); err != nil {
		fmt.Fprintf(os.Stderr, "workflow YAML not found at %q: %v\n", absWorkflow, err)
		os.Exit(1)
	}
	// Project root = the git repo root containing the workflow
	// YAML. Walking up from the workflow file is the natural
	// way to find it without a separate flag.
	absProject, err := findGitRoot(filepath.Dir(absWorkflow))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	manifest, err := bots.LoadFromWorkflow(absWorkflow)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading workflow %s: %v\n", absWorkflow, err)
		os.Exit(1)
	}
	if manifest == nil || len(manifest.Bots) == 0 {
		fmt.Fprintf(os.Stderr, "No bots declared inline in %s — nothing to do.\n", absWorkflow)
		return
	}

	resolvedOwnerCreds := resolveCredentialsPath(*credsPath)
	owner := loadCredentialsAt(*coordinator, resolvedOwnerCreds)
	if owner == nil || owner.Token == "" {
		fmt.Fprintf(os.Stderr, "No owner credentials found for %s at %s — run `enju mcp` once to register your own identity first.\n", *coordinator, resolvedOwnerCreds)
		os.Exit(1)
	}

	// Project ID for membership auto-add: caller-supplied only.
	// Inline-bots workflows don't carry a project_id field; the
	// operator passes --project-id=N when they want bots added
	// to a project's membership at setup time. 0 = skip.
	effectiveProjectID := *projectIDFlag

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
	fmt.Fprintf(os.Stderr, "Setting up %d bot(s) declared inline in %s: %s\n", len(manifest.Bots), absWorkflow, strings.Join(names, ", "))
	fmt.Fprintf(os.Stderr, "Coordinator: %s — owner: %s\n", *coordinator, owner.Username)
	if effectiveProjectID > 0 {
		fmt.Fprintf(os.Stderr, "Project membership: bots will be added to project #%d\n\n", effectiveProjectID)
	} else {
		fmt.Fprintln(os.Stderr, "Project membership: skipped (pass --project-id=N to auto-add)")
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
		if *dryRun {
			if _, err := os.Stat(b.Credentials); err == nil {
				fmt.Printf("  %-20s ✓ already set up — credentials at %s\n", b.Name, b.Credentials)
				skipped++
			} else {
				fmt.Printf("  %-20s [dry-run] would register and stash at %s\n", b.Name, b.Credentials)
				registered++
			}
			continue
		}
		res, err := setupBotIfNeeded(ctx, *coordinator, owner, b, effectiveProjectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-20s ✗ %v\n", b.Name, err)
			failed++
			continue
		}
		switch res.Status {
		case "registered":
			fmt.Printf("  %-20s ✓ registered as %s, credentials at %s\n", b.Name, res.Username, b.Credentials)
			registered++
		case "already-set-up":
			fmt.Printf("  %-20s ✓ already set up — credentials at %s\n", b.Name, b.Credentials)
			skipped++
		}
		if effectiveProjectID > 0 && res.Username != "" {
			if res.AddedToPr {
				fmt.Printf("  %-20s ✓ ensured member of project #%d\n", b.Name, effectiveProjectID)
			} else {
				fmt.Fprintf(os.Stderr, "  %-20s ⚠ couldn't add to project %d (manual: enju_add_project_member project_id=%d username=%s)\n",
					b.Name, effectiveProjectID, effectiveProjectID, res.Username)
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

// botSetupResult summarizes setupBotIfNeeded's outcome so callers
// can render a one-line status without re-running the existence
// check. Status mirrors the cmdBotSetup tally vocabulary so
// `enju bot run`'s self-heal output stays visually consistent
// with `enju bot setup`.
type botSetupResult struct {
	Status    string // "registered", "already-set-up", or empty on no-op
	Username  string // coord-assigned username (may differ from b.Name on collision)
	AddedToPr bool   // true when project membership was newly added (vs already-member)
}

// setupBotIfNeeded performs the per-bot work `enju bot setup`
// does — register against the coord if no credentials file
// exists at b.Credentials, write the credentials file, and
// optionally add the bot to a project. Idempotent: a present
// credentials file short-circuits to "already-set-up" without
// touching the coord. Used by both `enju bot setup` (looped
// over every manifest entry) and `enju bot run` (lazy self-
// heal for the one bot the operator named).
//
// owner.Token authenticates the registration. effectiveProjectID
// = 0 means "skip the membership step" — the operator can add
// later via enju_add_project_member.
func setupBotIfNeeded(ctx context.Context, coordinator string, owner *credentials, b *bots.Bot, effectiveProjectID int64) (botSetupResult, error) {
	if _, err := os.Stat(b.Credentials); err == nil {
		// Idempotent path: existing credentials file means setup
		// already happened. We don't probe the coord — if the
		// token's stale the daemon's first auth attempt surfaces
		// a clearer error than a setup-time liveness check.
		res := botSetupResult{Status: "already-set-up"}
		existing := loadCredentialsAt(coordinator, b.Credentials)
		if existing != nil {
			res.Username = existing.Username
		}
		// Re-run membership add only when explicitly scoped to a
		// project — fixes the legacy "registered before auto-add
		// existed" state without mutating coord state on every
		// bot run.
		if effectiveProjectID > 0 && res.Username != "" {
			if err := addBotToProject(ctx, coordinator, owner.Token, effectiveProjectID, res.Username); err == nil {
				res.AddedToPr = true
			}
		}
		return res, nil
	}

	token, username, err := registerBot(ctx, coordinator, owner.Token, b)
	if err != nil {
		return botSetupResult{}, fmt.Errorf("register: %w", err)
	}
	if err := writeBotCredentials(b.Credentials, coordinator, username, b.Name, token); err != nil {
		return botSetupResult{}, fmt.Errorf("token issued by coord but couldn't be written to %s: %w (token: %s — stash this NOW or the bot is unrecoverable)", b.Credentials, err, token)
	}
	res := botSetupResult{Status: "registered", Username: username}
	if effectiveProjectID > 0 {
		if err := addBotToProject(ctx, coordinator, owner.Token, effectiveProjectID, username); err == nil {
			res.AddedToPr = true
		}
	}
	return res, nil
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

// ensureBotMembership best-effort adds botUsername to projectID
// using owner's token. Called on every `enju bot run` startup —
// not just on first registration — because a previously-set-up
// bot may be starting against a different project than setup
// ran for, or the manifest's project_id may have been 0 at
// setup time. addBotToProject is idempotent (treats already-
// a-member as success), so re-running is free.
//
// Failures don't abort startup: the daemon will surface a
// clearer "not a member" error on its first poll, and we'd
// rather see a bot try and fail loudly than block its launch
// over a transient coord blip. The stderr line gives the
// operator the bot+project pair so they know which manual
// add to run if the auto-step keeps failing.
//
// `stderr` is parameterized for testability — production passes
// os.Stderr; tests capture into a bytes.Buffer to assert the
// failure-message shape.
func ensureBotMembership(ctx context.Context, coord, ownerToken string, projectID int64, botUsername string, stderr io.Writer) {
	if err := addBotToProject(ctx, coord, ownerToken, projectID, botUsername); err != nil {
		fmt.Fprintf(stderr, "self-heal: couldn't ensure bot %q is a member of project %d: %v\n   the daemon will surface the failure on its first poll if this isn't resolved\n",
			botUsername, projectID, err)
	}
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

// defaultWorkspaceRoot is the same path `enju bot run` uses for
// its --workspace-dir default, kept identical so the bare-clone
// resolution + the daemon's clone resolution agree on the
// managed-clone root.
func defaultWorkspaceRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".enju", "workspaces"), nil
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

// findGitRoot walks up from start to find the directory
// containing a `.git/` entry — that's the project root. Errors
// when no .git/ is found anywhere on the way up to the
// filesystem root. Used to locate the project a workflow YAML
// belongs to without requiring a separate --project flag.
func findGitRoot(start string) (string, error) {
	cur := start
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf(
				"workflow YAML at %q is not inside a git repository — "+
					"every run must pin to a commit for audit. Initialize git at the project root "+
					"(git init && git add . && git commit -m \"initial\") or move the workflow into an existing repo",
				start)
		}
		cur = parent
	}
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
	coordinator := fs.String("coordinator", defaultCoordinatorURL(), "Coordinator URL (defaults to value in ~/.enju/credentials.json, else http://localhost:8000)")
	botName := fs.String("bot", "", "Bot name from the workflow's inline bots: section (required)")
	workflowPath := fs.String("workflow", "", "Path to the workflow YAML whose inline bots: section declares this fleet (required)")
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
		fmt.Fprintln(os.Stderr, "--bot=<name> is required (must match a bot declared inline in the workflow YAML)")
		os.Exit(1)
	}
	if *workflowPath == "" {
		fmt.Fprintln(os.Stderr, "--workflow=<path/to/workflow.yaml> is required")
		os.Exit(1)
	}

	// Hard dependency: enju shells out to system `git` for
	// rebase-on-non-FF and merge-commit-on-conflict paths.
	// Catch a missing binary at startup rather than mid-submit
	// where the error is buried in a hook log.
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "`git` not found on PATH — install git (https://git-scm.com/downloads) before running enju bots.")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	absWorkflow, err := filepath.Abs(*workflowPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving --workflow=%q: %v\n", *workflowPath, err)
		os.Exit(1)
	}
	if _, err := os.Stat(absWorkflow); err != nil {
		fmt.Fprintf(os.Stderr, "workflow YAML not found at %q: %v\n", absWorkflow, err)
		os.Exit(1)
	}
	absProject, err := findGitRoot(filepath.Dir(absWorkflow))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	manifest, err := bots.LoadFromWorkflow(absWorkflow)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading workflow %s: %v\n", absWorkflow, err)
		os.Exit(1)
	}
	if manifest == nil {
		fmt.Fprintf(os.Stderr, "no bots declared inline in %s\n", absWorkflow)
		os.Exit(1)
	}
	bot := manifest.ByName(*botName)
	if bot == nil {
		fmt.Fprintf(os.Stderr, "bot %q not found in %s\n", *botName, absWorkflow)
		os.Exit(1)
	}

	// Self-heal: if the bot has no credentials file yet, register
	// it on the spot using the operator's owner credentials. Saves
	// the operator one explicit `enju bot setup` round-trip — for
	// solo projects with one operator + a few bots that's the
	// dominant case. Owner credentials default to
	// ~/.enju/credentials.json; an explicit --owner-credentials
	// flag overrides for hosts running multiple operator
	// identities.
	//
	// TP53 Bug 4 fix: gate self-heal on FILE-level presence, not
	// on loadCredentialsAt's coordinator-aware return value. When
	// a parseable creds file with token already lives at
	// bot.Credentials, registration is wrong even if the file
	// names a different coordinator URL — registering anyway
	// produces "self-heal failed: register: coord 409: username
	// already taken" noise on every startup. Coord-URL mismatch
	// is surfaced as its own error instead.
	creds := loadCredentialsAt(*coordinator, bot.Credentials)
	if (creds == nil || creds.Token == "") && !peekCredentialsFile(bot.Credentials) {
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		owner := loadCredentialsAt(*coordinator, resolveCredentialsPath(""))
		if owner == nil || owner.Token == "" {
			setupCancel()
			fmt.Fprintf(os.Stderr, "no credentials for bot %q at %s, and no owner credentials at %s for %s either.\n",
				bot.Name, bot.Credentials, resolveCredentialsPath(""), *coordinator)
			fmt.Fprintln(os.Stderr, "Register your own identity first by running `enju mcp` once, then re-run `enju bot run --bot=...`.")
			os.Exit(1)
		}
		// Membership: --project-id flag scopes the bot to one
		// project at setup time. 0 = skip membership add; daemon
		// polls every project the bot already belongs to.
		effectiveProjectID := *projectID
		res, setupErr := setupBotIfNeeded(setupCtx, *coordinator, owner, bot, effectiveProjectID)
		setupCancel()
		if setupErr != nil {
			fmt.Fprintf(os.Stderr, "self-heal failed: %v\n", setupErr)
			os.Exit(1)
		}
		switch res.Status {
		case "registered":
			fmt.Fprintf(os.Stderr, "self-heal: registered bot %q as %s, credentials at %s\n", bot.Name, res.Username, bot.Credentials)
			if effectiveProjectID > 0 && res.AddedToPr {
				fmt.Fprintf(os.Stderr, "self-heal: added to project #%d as member\n", effectiveProjectID)
			}
		case "already-set-up":
			// Defensive: setupBotIfNeeded saw a creds file even
			// though our outer gates missed it — race where
			// another process landed creds between our checks.
			fmt.Fprintf(os.Stderr, "self-heal: credentials file appeared at %s — re-loading\n", bot.Credentials)
		}
		creds = loadCredentialsAt(*coordinator, bot.Credentials)
		if creds == nil || creds.Token == "" {
			fmt.Fprintf(os.Stderr, "self-heal succeeded but credentials still unloadable at %s — coordinator URL mismatch?\n", bot.Credentials)
			os.Exit(1)
		}
	}
	// If we reached here with a still-nil creds but a parseable
	// file on disk, the gate above suppressed self-heal — meaning
	// the file's coordinator URL doesn't match the one we're
	// running against. Surface that as a clear error rather than
	// silently falling through.
	if creds == nil || creds.Token == "" {
		fmt.Fprintf(os.Stderr, "bot %q credentials at %s exist but don't match coordinator %s — check the coordinator URL in the file or pass --coordinator\n",
			bot.Name, bot.Credentials, *coordinator)
		os.Exit(1)
	}

	// Ensure this bot is a member of the project the operator
	// scoped us to. Idempotent — addBotToProject treats "already
	// a member" as success — so the run path can fire on every
	// start without coord chatter.
	//
	// Membership matters even when bot creds already exist:
	// a bot may have been registered against project A (where
	// setup ran) and is now starting against project B. The
	// user-reported failure mode was the daemon polling for
	// tasks, hitting "not a member of this project"
	// indefinitely, with the operator forced to call
	// enju_add_project_member by hand for every bot.
	if *projectID > 0 {
		// Membership additions require an existing member with
		// permission to add (typically the project owner). Use
		// the owner's credentials to make the API call.
		owner := loadCredentialsAt(*coordinator, resolveCredentialsPath(""))
		if owner != nil && owner.Token != "" {
			ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 15*time.Second)
			ensureBotMembership(ensureCtx, *coordinator, owner.Token, *projectID, creds.Username, os.Stderr)
			ensureCancel()
		}
		// No owner creds → skip silently. The daemon either
		// works against an existing membership (real-remote
		// project where collaborators were added out-of-band)
		// or the operator will see the "not a member" loop and
		// run `enju mcp` to register an owner identity.
	}

	// Maintain the managed gitignore block so a re-run of `enju
	// bot run` after a fresh manifest-edit doesn't leave bot
	// worktrees showing in `git status`. Same idempotency story
	// as ensureBotPushTarget — silent no-op when the block
	// already covers everything.
	if changed, err := bots.EnsureGitignored(absProject); err == nil && changed {
		fmt.Fprintln(os.Stderr, "self-heal: .gitignore updated to ignore .enju/")
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

	wsRoot, err := resolveWorkspaceRoot(*workspaceDir)
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
		WorkspaceRoot:   wsRoot,
		ModelName:       bot.Model,
		Logger:          logger,
		LogName:         "bot-" + creds.Username,
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
