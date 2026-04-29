package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/enju-ai/enju/internal/api"
	"github.com/enju-ai/enju/internal/compute"
	"github.com/enju-ai/enju/internal/mcpgit"
	"github.com/enju-ai/enju/internal/mcpserver"
	"github.com/enju-ai/enju/internal/scheduler"
	"github.com/enju-ai/enju/internal/store"
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
	case "wrap-task":
		cmdWrapTask(os.Args[2:])
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
  enju serve      Start the coordinator server
  enju mcp        Start the MCP server (for Claude Desktop/Code)
  enju wrap-task  Run a compute task's script + commit (internal)
  enju version    Print version

Run 'enju <command> -h' for command-specific help.`)
}

// --- serve ---

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 8000, "Port to listen on")
	dbPath := fs.String("db", "enju.db", "Path to SQLite database")
	fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.New(*dbPath)
	if err != nil {
		logger.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// Start task reaper.
	reaper := scheduler.NewReaper(st, 60*time.Second, logger)
	reaper.Start()
	defer reaper.Stop()

	srv := api.NewServer(st, logger)

	addr := fmt.Sprintf(":%d", *port)
	logger.Info("Enju coordinator starting",
		"port", *port,
		"db", *dbPath,
	)

	if err := http.ListenAndServe(addr, srv.Router()); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
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
	ws, err := mcpgit.NewWorkspace(*workspaceDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create MCP workspace: %v\n", err)
		fmt.Fprintf(os.Stderr, "Hint: the workspace directory (default ~/.enju/workspaces) must be writable and have free disk space. Check permissions with `ls -ld ~/.enju` and free space with `df -h ~`.\n")
		os.Exit(1)
	}

	s := mcpserver.New(mcpserver.Config{
		CoordinatorURL: *coordinator,
		Username:       *username,
		CitizenName:    *name,
		CitizenEmail:   *email,
		ModelName:      *model,
		AuthToken:      token,
		Workspace:      ws,
		Logger:         logger,
		SaveCredentials: func(gotUsername, gotName, gotEmail, gotToken string) {
			saveCredentialsAt(credsKey, gotUsername, gotName, gotEmail, gotToken, resolvedCredsPath)
		},
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
	Username    string `json:"username"`
	Name        string `json:"name"`
	Email       string `json:"email,omitempty"`
	Token       string `json:"token,omitempty"` // auth token from registration
}

func credentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".enju", "credentials.json")
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
	data, _ := json.MarshalIndent(creds, "", "  ")
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
