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

	"github.com/enju-ai/enju/internal/api"
	enjuGit "github.com/enju-ai/enju/internal/git"
	"github.com/enju-ai/enju/internal/mcpserver"
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
	fmt.Println(`Enju (槐) — Distributed Human-AI Problem Solving

Usage:
  enju serve      Start the coordinator server
  enju mcp        Start the MCP server (for Claude Desktop/Code)
  enju submit     Submit a problem YAML to the coordinator
  enju status     Check problem status
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
	name := fs.String("name", "", "Participant name")
	participantID := fs.String("id", "", "Participant ID (from registration)")
	fs.Parse(args)

	if *name == "" && *participantID == "" {
		fmt.Fprintln(os.Stderr, "Either -name or -id is required")
		fs.Usage()
		os.Exit(1)
	}

	// Auto-register if no ID provided
	if *participantID == "" {
		id, err := registerParticipant(*coordinator, *name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to register: %v\n", err)
			os.Exit(1)
		}
		*participantID = id
		fmt.Fprintf(os.Stderr, "Registered as participant: %s\n", id)
	}

	s := mcpserver.New(mcpserver.Config{
		CoordinatorURL:  *coordinator,
		ParticipantID:   *participantID,
		ParticipantName: *name,
	})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
}

// --- submit ---

func cmdSubmit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: enju submit <problem.yaml> [-coordinator URL]")
		os.Exit(1)
	}

	yamlPath := fs.Arg(0)
	yamlData, err := os.ReadFile(yamlPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	body, _ := json.Marshal(map[string]string{
		"yaml": string(yamlData),
	})

	resp, err := http.Post(*coordinator+"/api/v1/problems", "application/json", bytes.NewReader(body))
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
		// List all problems
		resp, err := http.Get(*coordinator + "/api/v1/problems")
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

	// Get specific problem + tasks
	problemID := fs.Arg(0)
	resp, err := http.Get(*coordinator + "/api/v1/problems/" + problemID + "/tasks")
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

func registerParticipant(coordinatorURL, name string) (string, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := http.Post(coordinatorURL+"/api/v1/participants/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}
