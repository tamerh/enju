package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/api"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
	"github.com/enju-ai/enju/internal/fatclient/mcphandlers"
	"github.com/enju-ai/enju/internal/coordinator/scheduler"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "ui":
		cmdUI(os.Args[2:])
	case "wrap-task":
		cmdWrapTask(os.Args[2:])
	case "inbox":
		cmdInbox(os.Args[2:])
	case "review":
		cmdReview(os.Args[2:])
	case "version":
		fmt.Println("enju v0.1.0-dev")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Enju (槐) — Distributed Human-AI Collaborative Problem Solving

Usage:
 enju serve   Start the coordinator server
 enju mcp    Start the MCP server (for Claude Desktop/Code)
 enju ui     Start the web UI (browser, peer to enju mcp)
 enju inbox   Show tasks waiting on you in a project
 enju review  Submit a verdict on a claimed review task
 enju wrap-task Run a compute task's script + commit (internal)
 enju version  Print version

Run 'enju <command> -h' for command-specific help.`)
}

// --- serve ---

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "Path to enju.conf (YAML). Missing file is OK — built-in defaults apply.")
	port := fs.Int("port", 0, "Port to listen on (overrides config)")
	dbPath := fs.String("db", "", "Path to SQLite state database (overrides config)")
	// boot-time kill-switch. When false, the
	// EventStore opens but starts in disabled mode: Record()
	// is a no-op and reads return ErrEventStoreDisabled. The
	// runtime toggle (POST /api/v1/admin/events/enabled) can
	// flip it back on without restart. Useful for booting a
	// coordinator with a corrupted events.db to investigate
	// without firing more emissions, or for capacity-spike
	// triage where audit can wait.
	eventsEnabled := fs.Bool("events-enabled", true, "Enable the event store at boot (overrides config)")
	fs.Parse(args)

	cfg, err := LoadServerConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Apply CLI overrides only when the user explicitly set the flag.
	// flag.Visit walks set-flags only — unset flags keep their
	// config-supplied value.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Coordinator.Port = *port
		case "db":
			cfg.Data.StateDB = *dbPath
		case "events-enabled":
			b := *eventsEnabled
			cfg.Events.EmissionEnabled = &b
		}
	})

	logWriter, logCloser, err := resolveLogOutput(cfg.Logging.Output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging: %v\n", err)
		os.Exit(1)
	}
	if logCloser != nil {
		defer logCloser.Close()
	}
	// LevelVar lets SIGHUP-driven config reload mutate the log
	// level at runtime — slog.HandlerOptions{Level} is static
	// and would require rebuilding the handler.
	logLevel := new(slog.LevelVar)
	logLevel.Set(parseLogLevel(cfg.Logging.Level))
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: logLevel}))

	stateDBPath := expandPath(cfg.Data.StateDB)
	st, err := store.New(stateDBPath)
	if err != nil {
		logger.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// Events live in their own database alongside the state DB,
	// with their own connection pool and async writer goroutine.
	// Path is derived from the state DB unless explicitly set in
	// config so operators don't have to pass a second flag.
	eventsPath := cfg.Data.EventsDB
	if eventsPath == "" {
		eventsPath = deriveEventsDBPath(stateDBPath)
	} else {
		eventsPath = expandPath(eventsPath)
	}
	es, err := store.NewSQLiteEventStore(eventsPath, logger,
		store.WithQueueSize(cfg.Performance.EventQueueSize),
	)
	if err != nil {
		logger.Error("opening events database", "path", eventsPath, "error", err)
		os.Exit(1)
	}
	defer es.Close()
	emissionEnabled := cfg.Events.EmissionEnabled == nil || *cfg.Events.EmissionEnabled
	es.SetEnabled(emissionEnabled)
	st.AttachEventStore(es)
	if !emissionEnabled {
		logger.Warn("event store booted disabled — emissions and reads will no-op until toggled via admin endpoint")
	}
	store.SetEventDrainBudget(parseDurationOr(cfg.Performance.EventDrainBudget, 100*time.Millisecond))

	// Start task reaper at the operator-configured interval.
	reaper := scheduler.NewReaper(st, parseDurationOr(cfg.Performance.ReaperInterval, 60*time.Second), logger)
	reaper.Start()
	defer reaper.Stop()

	srv := api.NewServerWithOptions(st, logger, api.ServerOptions{
		HTTPRequestTimeout: parseDurationOr(cfg.Performance.HTTPRequestTimeout, 30*time.Second),
	})

	// SIGHUP reload: re-read enju.conf and apply the subset of
	// changes that are safe to mutate live (events kill-switch,
	// log level). Bind/port and DB paths require restart — we
	// log a warning if the operator changes them and SIGHUPs,
	// rather than silently ignoring or doing something dangerous
	// like rebinding sockets / closing DBs mid-flight.
	go reloadOnSIGHUP(*configPath, cfg, es, logLevel, logger)

	addr := fmt.Sprintf("%s:%d", cfg.Coordinator.Bind, cfg.Coordinator.Port)
	logger.Info("Enju coordinator starting",
		"bind", cfg.Coordinator.Bind,
		"port", cfg.Coordinator.Port,
		"db", stateDBPath,
		"events_db", eventsPath,
	)

	if err := http.ListenAndServe(addr, srv.Router()); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

// reloadOnSIGHUP listens for SIGHUP, re-reads the config file, and
// applies the subset of fields that are safe to mutate at runtime:
//
//   - events.emission_enabled → es.SetEnabled(...)
//   - logging.level           → logLevel.Set(...)
//
// Restart-only fields (bind, port, state_db, events_db, log output)
// log a warning if they changed in the new config — silent ignoring
// would be the worst kind of footgun.
//
// `current` is the live config pointer; reload mutates its fields
// in place so subsequent reloads see the latest applied state.
func reloadOnSIGHUP(path string, current *ServerConfig, es store.EventStore, logLevel *slog.LevelVar, logger *slog.Logger) {
	if path == "" {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	for range ch {
		fresh, err := LoadServerConfig(path)
		if err != nil {
			logger.Error("config reload failed — keeping previous values", "error", err)
			continue
		}
		applyConfigReload(current, fresh, es, logLevel, logger)
	}
}

// applyConfigReload diffs `fresh` against `current`, applies the
// runtime-safe fields, and warns about restart-only fields that
// changed. Mutates `current` in place to track latest applied state.
// Extracted from reloadOnSIGHUP so it's directly testable without
// sending real signals.
func applyConfigReload(current, fresh *ServerConfig, es store.EventStore, logLevel *slog.LevelVar, logger *slog.Logger) {
	warnRestartOnly(logger, "coordinator.bind", current.Coordinator.Bind, fresh.Coordinator.Bind)
	warnRestartOnly(logger, "coordinator.port", current.Coordinator.Port, fresh.Coordinator.Port)
	warnRestartOnly(logger, "data.state_db", current.Data.StateDB, fresh.Data.StateDB)
	warnRestartOnly(logger, "data.events_db", current.Data.EventsDB, fresh.Data.EventsDB)
	warnRestartOnly(logger, "logging.output", current.Logging.Output, fresh.Logging.Output)
	// Performance fields — most of the perf knobs aren't safely
	// mutable mid-process. event_queue_size is baked into a
	// channel; reaper_interval into a goroutine ticker;
	// http_request_timeout into the chi middleware chain. Warn
	// if changed; require restart to apply.
	warnRestartOnly(logger, "performance.event_queue_size", current.Performance.EventQueueSize, fresh.Performance.EventQueueSize)
	warnRestartOnly(logger, "performance.reaper_interval", current.Performance.ReaperInterval, fresh.Performance.ReaperInterval)
	warnRestartOnly(logger, "performance.http_request_timeout", current.Performance.HTTPRequestTimeout, fresh.Performance.HTTPRequestTimeout)

	if fresh.Logging.Level != current.Logging.Level {
		logLevel.Set(parseLogLevel(fresh.Logging.Level))
		logger.Info("config reload: log level changed", "from", current.Logging.Level, "to", fresh.Logging.Level)
		current.Logging.Level = fresh.Logging.Level
	}
	freshEnabled := fresh.Events.EmissionEnabled == nil || *fresh.Events.EmissionEnabled
	currentEnabled := current.Events.EmissionEnabled == nil || *current.Events.EmissionEnabled
	if freshEnabled != currentEnabled {
		es.SetEnabled(freshEnabled)
		logger.Warn("config reload: events kill-switch flipped", "from", currentEnabled, "to", freshEnabled)
		current.Events.EmissionEnabled = fresh.Events.EmissionEnabled
	}
	// event_drain_budget is the only perf field that's safely
	// hot-reloadable — it's read on each aggregation call from
	// an atomic, no goroutine ownership. Parse strictly here:
	// a typo like "100mss" would otherwise silently fall back
	// to the default while the log claims the new value was
	// applied. The operator who SIGHUPed needs an unambiguous
	// signal of "did this take or not."
	if fresh.Performance.EventDrainBudget != current.Performance.EventDrainBudget {
		newDur, parseErr := time.ParseDuration(fresh.Performance.EventDrainBudget)
		if parseErr != nil {
			logger.Warn("config reload: event_drain_budget unparseable, keeping previous value",
				"value", fresh.Performance.EventDrainBudget, "error", parseErr)
		} else {
			store.SetEventDrainBudget(newDur)
			logger.Info("config reload: event drain budget changed",
				"from", current.Performance.EventDrainBudget,
				"to", fresh.Performance.EventDrainBudget)
			current.Performance.EventDrainBudget = fresh.Performance.EventDrainBudget
		}
	}
	logger.Info("config reload complete")
}

func warnRestartOnly[T comparable](logger *slog.Logger, key string, old, new T) {
	if old != new {
		logger.Warn("config reload: field changed but requires restart — ignoring", "key", key, "current", old, "in_file", new)
	}
}

// --- mcp ---

func cmdMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	localMode := fs.Bool("local", false, "Run in local-only mode: embed the coordinator in this process (no separate enju serve needed)")
	localDB := fs.String("db", "", "SQLite path for local mode (default ~/.enju/local.db)")
	name := fs.String("name", "", "Citizen display name (e.g. \"Tamer Gur\")")
	username := fs.String("username", "", "Citizen username (optional, auto-generated from name if omitted)")
	email := fs.String("email", "", "Citizen email (optional)")
	model := fs.String("model", "", "LLM model name for contribution tracking (e.g. claude-opus-4, gpt-4o)")
	workspaceDir := fs.String("workspace", "", "Directory for per-project local clones (default ~/.enju/workspaces)")
	credsPath := fs.String("credentials", "", "Path to credentials.json (default ~/.enju/credentials.json). Use a per-identity path when running multiple MCP processes for different citizens on one host — see docs/multi-citizen.md § Running multiple citizens on one host.")
	// local-mode parity with `enju serve`. Useful
	// for testers reproducing "events disabled" behavior end-
	// to-end without hitting a runtime tool first. Default
	// true: local mode is normally a single-user dev path
	// where the kill-switch isn't load-bearing.
	localEventsEnabled := fs.Bool("events-enabled", true, "Local-mode only: enable the embedded coordinator's event store at boot")
	fs.Parse(args)

	resolvedCredsPath := resolveCredentialsPath(*credsPath)

	// Local-only mode: start an embedded coordinator in the
	// same process on a random port. The MCP client talks to
	// it over localhost — same code paths, no separate
	// `enju serve` process needed.
	if *localMode {
		dbPath := *localDB
		if dbPath == "" {
			home, _ := os.UserHomeDir()
			dbPath = filepath.Join(home, ".enju", "local.db")
			os.MkdirAll(filepath.Dir(dbPath), 0755)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		st, err := store.New(dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open local database: %v\n", err)
			os.Exit(1)
		}
		defer st.Close()

		eventsPath := deriveEventsDBPath(dbPath)
		es, err := store.NewSQLiteEventStore(eventsPath, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open events database: %v\n", err)
			os.Exit(1)
		}
		defer es.Close()
		es.SetEnabled(*localEventsEnabled)
		st.AttachEventStore(es)
		if !*localEventsEnabled {
			fmt.Fprintf(os.Stderr, "Local mode: event store booted disabled — emissions and reads no-op until toggled.\n")
		}

		reaper := scheduler.NewReaper(st, 60*time.Second, logger)
		reaper.Start()
		defer reaper.Stop()

		srv := api.NewServer(st, logger)

		// Find an available port.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to find a free port: %v\n", err)
			os.Exit(1)
		}
		localAddr := ln.Addr().String()
		go http.Serve(ln, srv.Router())
		*coordinator = "http://" + localAddr
		fmt.Fprintf(os.Stderr, "Local mode: embedded coordinator on %s (db: %s)\n", *coordinator, dbPath)
	}

	// For local mode, use a fixed sentinel as the coordinator
	// key in credentials.json so the identity persists across
	// sessions (the actual port changes every run).
	credsKey := *coordinator
	if *localMode {
		credsKey = "local"
	}

	// Load saved credentials. Persistent values beat CLI args —
	// ~/.enju/credentials.json is the source of truth for a user's
	// identity, and the CLI args exist mostly as bootstrap metadata
	// for the very first registration.
	creds := loadCredentialsAt(credsKey, resolvedCredsPath)
	if creds != nil {
		if creds.Username != "" {
			*username = creds.Username
		}
		if creds.Name != "" {
			*name = creds.Name
		}
		if creds.Email != "" {
			*email = creds.Email
		}
		fmt.Fprintf(os.Stderr, "Welcome back, %s (@%s)\n", creds.Name, creds.Username)
	}

	if *name == "" && *username == "" {
		fmt.Fprintln(os.Stderr, "At least one of -name or -username is required")
		fs.Usage()
		os.Exit(1)
	}

	// Register if we don't have a username yet. The server generates one
	// from the display name if we don't provide one.
	var token string
	if creds == nil {
		gotUsername, gotToken, err := registerCitizen(*coordinator, *name, *username, *email)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to register: %v\n", err)
			os.Exit(1)
		}
		*username = gotUsername
		token = gotToken
		saveCredentialsAt(credsKey, *username, *name, *email, token, resolvedCredsPath)
		fmt.Fprintf(os.Stderr, "Registered as @%s (%s)\n", *username, *name)
	} else {
		token = creds.Token
	}

	// Build a client-side git workspace. Used for iteration A.2's
	// fat-client write path when the project has a remote_url.
	// Self-hosted projects without a remote fall back to the
	// legacy coordinator-writes path; this workspace stays unused
	// for them but the creation itself is cheap and safe.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ws, err := workspace.NewWorkspace(*workspaceDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create MCP workspace: %v\n", err)
		fmt.Fprintf(os.Stderr, "Hint: the workspace directory (default ~/.enju/workspaces) must be writable and have free disk space. Check permissions with `ls -ld ~/.enju` and free space with `df -h ~`.\n")
		os.Exit(1)
	}

	// Tier 1 notification session — stays dormant at boot. When
	// the user calls enju_create_project or enju_init, mcpserver
	// fires notifySession.Switch and the polling loop activates
	// for that project. All notify state (cursor, event log,
	// rules) lives under the project's enju/ directory; nothing
	// in ~/.enju/.
	notifyCtx, cancelNotify := context.WithCancel(context.Background())
	defer cancelNotify()
	notifyOpts := &mcphandlers.NotifyOptions{ParentCtx: notifyCtx}

	s := mcphandlers.New(mcphandlers.Config{
		CoordinatorURL: *coordinator,
		Username:    *username,
		CitizenName:  *name,
		CitizenEmail:  *email,
		ModelName:   *model,
		AuthToken:   token,
		Workspace:   ws,
		Logger:     logger,
		SaveCredentials: func(gotUsername, gotName, gotEmail, gotToken string) {
			saveCredentialsAt(credsKey, gotUsername, gotName, gotEmail, gotToken, resolvedCredsPath)
		},
		Notify: notifyOpts,
	})

	fmt.Fprintf(os.Stderr, "MCP server starting (stdio mode)...\n")
	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "MCP server exited cleanly\n")
}


// --- wrap-task ---

// cmdWrapTask is the subprocess entry point used by the MCP
// server's compute task handler. It reads a Spec from disk,
// executes the script, commits the result, and writes a Result
// back to disk. Designed to be trivially re-hostable on compute
// nodes (SLURM, Kubernetes, …) later — the contract is env +
// files, not in-process calls.
//
// Not a user-facing command. The MCP handler invokes it via
// `os.Executable() wrap-task --spec … --output …`; a human
// running it by hand is fine for debugging but the flags will
// look opaque.
func cmdWrapTask(args []string) {
	os.Exit(compute.WrapMain(args, os.Stderr))
}

// --- helpers ---

// credentials is the client-side persistence of a citizen's handle.
// We store username (stable handle) and display name; the internal
// int id is not stored — if we ever need it we can look it up by
// username via the API.
type credentials struct {
	Coordinator string `json:"coordinator"`
	Username  string `json:"username"`
	Name    string `json:"name"`
	Email    string `json:"email,omitempty"`
	Token    string `json:"token,omitempty"` // auth token from registration
}

func credentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".enju", "credentials.json")
}

// deriveEventsDBPath turns a state-DB path like "/var/enju/state.db"
// into the sibling "/var/enju/events.db". Bare ":memory:" stays
// in-memory (used by some tests). The two databases live in the
// same directory so operators inspecting the deployment see them
// next to each other.
func deriveEventsDBPath(stateDBPath string) string {
	if stateDBPath == ":memory:" {
		return ":memory:"
	}
	dir, base := filepath.Split(stateDBPath)
	ext := filepath.Ext(base)
	stem := base[:len(base)-len(ext)]
	if stem == "" {
		stem = "state"
	}
	return filepath.Join(dir, stem+"-events"+ext)
}

// resolveCredentialsPath returns override when non-empty, else the
// default ~/.enju/credentials.json. Used by `enju mcp --credentials`
// so multiple bot/citizen MCP processes on one host can each carry
// their own identity without HOME isolation gymnastics. See
// docs/multi-citizen.md § Running multiple citizens on one host.
func resolveCredentialsPath(override string) string {
	if override != "" {
		return override
	}
	return credentialsPath()
}

func loadCredentials(coordinator string) *credentials {
	return loadCredentialsAt(coordinator, credentialsPath())
}

func loadCredentialsAt(coordinator, path string) *credentials {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var creds credentials
	if json.Unmarshal(data, &creds) != nil {
		return nil
	}
	if creds.Coordinator != coordinator || creds.Username == "" {
		return nil
	}
	return &creds
}

// saveCredentials writes the given identity into
// ~/.enju/credentials.json using a read-modify-write pass so
// unknown fields stay intact. Future versions may add optional
// keys (OAuth tokens, GitHub handle, etc.) and operators may
// hand-edit credentials.json — neither should be wiped just
// because auto re-register fires a save with a typed struct that
// doesn't know about those fields.
func saveCredentials(coordinator, username, name, email, token string) {
	saveCredentialsAt(coordinator, username, name, email, token, credentialsPath())
}

func saveCredentialsAt(coordinator, username, name, email, token, path string) {
	creds := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &creds) // tolerate missing/malformed
	}
	creds["coordinator"] = coordinator
	creds["username"] = username
	creds["name"] = name
	if email != "" {
		creds["email"] = email
	}
	if token != "" {
		creds["token"] = token
	}
	data, _ := json.MarshalIndent(creds, "", " ")
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0755)
	os.WriteFile(path, data, 0600)
}

// registerCitizen POSTs a registration request and returns the
// server-assigned username (generated from the name if the caller
// didn't pass one).
func registerCitizen(coordinatorURL, name, username, email string) (string, string, error) {
	reqBody := map[string]string{"name": name}
	if username != "" {
		reqBody["username"] = username
	}
	if email != "" {
		reqBody["email"] = email
	}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(coordinatorURL+"/api/v1/citizens/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}
	if errMsg, ok := result["error"].(string); ok {
		return "", "", fmt.Errorf("%s", errMsg)
	}
	got, _ := result["username"].(string)
	if got == "" {
		return "", "", fmt.Errorf("server did not return username")
	}
	token, _ := result["token"].(string)
	return got, token, nil
}
