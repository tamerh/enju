package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/api"
	enjuGit "github.com/enju-ai/enju/internal/git"
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
	case "submit":
		cmdSubmit(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
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
  enju submit     Submit a run YAML to the coordinator
  enju status     Check run status
  enju version    Print version

Run 'enju <command> -h' for command-specific help.`)
}

// --- serve ---

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 8000, "Port to listen on")
	dbPath := fs.String("db", "enju.db", "Path to SQLite database")
	gitDir := fs.String("git-dir", "", "Base directory for per-project git repos")
	fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *gitDir == "" {
		home, _ := os.UserHomeDir()
		*gitDir = filepath.Join(home, ".enju", "results")
	}

	if err := os.MkdirAll(*gitDir, 0755); err != nil {
		logger.Error("creating git base directory", "path", *gitDir, "error", err)
		os.Exit(1)
	}

	st, err := store.New(*dbPath)
	if err != nil {
		logger.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	registry, err := enjuGit.NewRegistry(*gitDir, logger)
	if err != nil {
		logger.Error("initializing git registry", "error", err)
		os.Exit(1)
	}

	// Startup health check — warn if any project's repo is missing.
	if projects, perr := st.ListProjects(); perr == nil {
		ids := make([]int64, 0, len(projects))
		for _, p := range projects {
			ids = append(ids, p.ID)
		}
		registry.HealthCheck(ids)
	}

	// Start task reaper
	reaper := scheduler.NewReaper(st, 60*time.Second, logger)
	reaper.Start()
	defer reaper.Stop()

	srv := api.NewServer(st, registry, logger)

	addr := fmt.Sprintf(":%d", *port)
	logger.Info("Enju coordinator starting",
		"port", *port,
		"db", *dbPath,
		"git_dir", *gitDir,
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

	s := mcpserver.New(mcpserver.Config{
		CoordinatorURL: *coordinator,
		Username:       *username,
		CitizenName:    *name,
	})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
}

// --- submit ---

func cmdSubmit(args []string) {
	// Extract the YAML file path from args (can be anywhere in the arg list)
	var yamlPath string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i])
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
		} else {
			yamlPath = args[i]
		}
	}

	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	fs.Parse(flagArgs)

	if yamlPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: enju submit <run.yaml> [-coordinator URL]")
		os.Exit(1)
	}
	yamlData, err := os.ReadFile(yamlPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	body, _ := json.Marshal(map[string]string{
		"yaml": string(yamlData),
	})

	resp, err := http.Post(*coordinator+"/api/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error submitting: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var pretty bytes.Buffer
	json.Indent(&pretty, data, "", "  ")
	fmt.Println(pretty.String())
}

// --- status ---

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	fs.Parse(args)

	if fs.NArg() < 1 {
		// List all runs
		resp, err := http.Get(*coordinator + "/api/v1/runs")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var pretty bytes.Buffer
		json.Indent(&pretty, data, "", "  ")
		fmt.Println(pretty.String())
		return
	}

	// Get specific run + tasks
	runID := fs.Arg(0)
	resp, err := http.Get(*coordinator + "/api/v1/runs/" + runID + "/tasks")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var pretty bytes.Buffer
	json.Indent(&pretty, data, "", "  ")
	fmt.Println(pretty.String())
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
