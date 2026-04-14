package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/enju-ai/enju/internal/api"
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
	name := fs.String("name", "", "Citizen display name (e.g. \"Tamer Gur\")")
	username := fs.String("username", "", "Citizen username (optional, auto-generated from name if omitted)")
	email := fs.String("email", "", "Citizen email (optional)")
	workspaceDir := fs.String("workspace", "", "Directory for per-project local clones (default ~/.enju/workspaces)")
	fs.Parse(args)

	// Try loading saved credentials — bound to a (coordinator, username) pair.
	creds := loadCredentials(*coordinator)
	if creds != nil && *username == "" {
		*username = creds.Username
		if *name == "" {
			*name = creds.Name
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
	if creds == nil {
		gotUsername, err := registerCitizen(*coordinator, *name, *username, *email)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to register: %v\n", err)
			os.Exit(1)
		}
		*username = gotUsername
		saveCredentials(*coordinator, *username, *name)
		fmt.Fprintf(os.Stderr, "Registered as @%s (%s)\n", *username, *name)
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
		os.Exit(1)
	}

	s := mcpserver.New(mcpserver.Config{
		CoordinatorURL: *coordinator,
		Username:       *username,
		CitizenName:    *name,
		Workspace:      ws,
		Logger:         logger,
	})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
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
}

func credentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".enju", "credentials.json")
}

func loadCredentials(coordinator string) *credentials {
	data, err := os.ReadFile(credentialsPath())
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

func saveCredentials(coordinator, username, name string) {
	creds := credentials{
		Coordinator: coordinator,
		Username:    username,
		Name:        name,
	}
	data, _ := json.MarshalIndent(creds, "", "  ")
	dir := filepath.Dir(credentialsPath())
	os.MkdirAll(dir, 0755)
	os.WriteFile(credentialsPath(), data, 0600)
}

// registerCitizen POSTs a registration request and returns the
// server-assigned username (generated from the name if the caller
// didn't pass one).
func registerCitizen(coordinatorURL, name, username, email string) (string, error) {
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
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if errMsg, ok := result["error"].(string); ok {
		return "", fmt.Errorf("%s", errMsg)
	}
	got, _ := result["username"].(string)
	if got == "" {
		return "", fmt.Errorf("server did not return username")
	}
	return got, nil
}
