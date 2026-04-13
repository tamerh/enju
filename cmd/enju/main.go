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
  enju submit     Submit a project YAML to the coordinator
  enju status     Check project status
  enju version    Print version

Run 'enju <command> -h' for command-specific help.`)
}

// --- serve ---

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 8000, "Port to listen on")
	dbPath := fs.String("db", "enju.db", "Path to SQLite database")
	gitDir := fs.String("git-dir", "", "Path to local git working directory for results")
	repoURL := fs.String("repo", "", "Git remote URL for results")
	fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *gitDir == "" {
		home, _ := os.UserHomeDir()
		*gitDir = filepath.Join(home, ".enju", "results")
	}

	if err := os.MkdirAll(*gitDir, 0755); err != nil {
		logger.Error("creating git directory", "path", *gitDir, "error", err)
		os.Exit(1)
	}

	st, err := store.New(*dbPath)
	if err != nil {
		logger.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	gitWriter, err := enjuGit.NewWriter(*gitDir, *repoURL, logger)
	if err != nil {
		logger.Error("initializing git writer", "error", err)
		os.Exit(1)
	}

	// Start task reaper
	reaper := scheduler.NewReaper(st, 60*time.Second, logger)
	reaper.Start()
	defer reaper.Stop()

	srv := api.NewServer(st, gitWriter, logger)

	addr := fmt.Sprintf(":%d", *port)
	logger.Info("Enju coordinator starting",
		"port", *port,
		"db", *dbPath,
		"git_dir", *gitDir,
		"repo", *repoURL,
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
	name := fs.String("name", "", "Citizen name")
	email := fs.String("email", "", "Citizen email (optional)")
	citizenID := fs.String("id", "", "Citizen ID (from registration)")
	fs.Parse(args)

	// Try loading saved credentials
	creds := loadCredentials(*coordinator)
	if creds != nil && *citizenID == "" {
		*citizenID = creds.CitizenID
		if *name == "" {
			*name = creds.Name
		}
		fmt.Fprintf(os.Stderr, "Welcome back, %s (citizen %s)\n", creds.Name, creds.CitizenID)
	}

	if *name == "" && *citizenID == "" {
		fmt.Fprintln(os.Stderr, "Either -name or -id is required")
		fs.Usage()
		os.Exit(1)
	}

	// Register if no ID
	if *citizenID == "" {
		id, err := registerCitizen(*coordinator, *name, *email)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to register: %v\n", err)
			os.Exit(1)
		}
		*citizenID = id
		saveCredentials(*coordinator, id, *name)
		fmt.Fprintf(os.Stderr, "Registered as citizen: %s (%s)\n", *name, id)
	}

	s := mcpserver.New(mcpserver.Config{
		CoordinatorURL: *coordinator,
		CitizenID:      *citizenID,
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
		fmt.Fprintln(os.Stderr, "Usage: enju submit <project.yaml> [-coordinator URL]")
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

	resp, err := http.Post(*coordinator+"/api/v1/projects", "application/json", bytes.NewReader(body))
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
		// List all projects
		resp, err := http.Get(*coordinator + "/api/v1/projects")
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

	// Get specific project + tasks
	projectID := fs.Arg(0)
	resp, err := http.Get(*coordinator + "/api/v1/projects/" + projectID + "/tasks")
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

type credentials struct {
	Coordinator string `json:"coordinator"`
	CitizenID   string `json:"citizen_id"`
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
	if creds.Coordinator != coordinator {
		return nil
	}
	return &creds
}

func saveCredentials(coordinator, citizenID, name string) {
	creds := credentials{
		Coordinator: coordinator,
		CitizenID:   citizenID,
		Name:        name,
	}
	data, _ := json.MarshalIndent(creds, "", "  ")
	dir := filepath.Dir(credentialsPath())
	os.MkdirAll(dir, 0755)
	os.WriteFile(credentialsPath(), data, 0600)
}

func registerCitizen(coordinatorURL, name, email string) (string, error) {
	reqBody := map[string]string{"name": name}
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
	id, _ := result["id"].(string)
	return id, nil
}
