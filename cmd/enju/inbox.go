package main

// `enju inbox` — thin wrapper over internal/inbox. Same
// projection logic the MCP tool runs, with a workspace-based
// git read instead of HTTP. Zero coordinator round-trips.

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/inbox"
)

func cmdInbox(args []string) {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	coordinator := fs.String("coordinator", defaultCoordinatorURL(), "Coordinator URL (defaults to value in ~/.enju/credentials.json; only used to look up identity, inbox reads local files)")
	credsPath := fs.String("credentials", "", "Path to credentials.json (default ~/.enju/credentials.json). Use a per-identity path when running for a non-default citizen.")
	workspaceRoot := fs.String("workspace", "", "Workspace root (default ~/.enju/workspaces/). The project clone is expected at <workspace>/{slug}-{id}/.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: enju inbox <project_id> [flags]

Show ready tasks assigned to you in the given project, with each
upstream task's latest submission inlined so you can read the
work without claiming first.

The inbox is derived entirely from local files: live.jsonl
provides the event stream and the project clone provides upstream
content via git. The project must already be cloned locally —
typically by running 'enju mcp' once. If you've only used the
CLI, the workspace won't exist; the command prints a helpful
error.

Flags:`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	projectID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || projectID <= 0 {
		fmt.Fprintf(os.Stderr, "invalid project_id %q (must be a positive integer)\n", fs.Arg(0))
		os.Exit(2)
	}

	resolvedCredsPath := resolveCredentialsPath(*credsPath)
	creds := loadCredentialsAt(*coordinator, resolvedCredsPath)
	if creds == nil || creds.Token == "" {
		fmt.Fprintf(os.Stderr, "no credentials found for coordinator %s at %s — run `enju mcp` once to register, or pass -coordinator/-credentials to point at the right setup.\n", *coordinator, resolvedCredsPath)
		os.Exit(1)
	}

	wsRoot := *workspaceRoot
	if wsRoot == "" {
		home, _ := os.UserHomeDir()
		wsRoot = filepath.Join(home, ".enju", "workspaces")
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		fmt.Fprintf(os.Stderr, "opening workspace %s: %v\n", wsRoot, err)
		os.Exit(1)
	}
	projectDir := ws.ProjectDir(projectID)
	view, err := ws.OpenView(projectID)
	if err != nil || projectDir == "" {
		fmt.Fprintf(os.Stderr, "project %d has no local clone at %s — run `enju mcp` once with this credentials file to materialize the clone.\n", projectID, wsRoot)
		os.Exit(1)
	}

	livePath := filepath.Join(projectDir, corelayout.EventsDir, "live.jsonl")
	rows, err := inbox.BuildInbox(livePath, creds.Username, &cliInboxDeps{view: view})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(inbox.FormatInbox(rows))
}

// cliInboxDeps adapts enjugit.View to inbox.Deps. The shared
// inbox core needs only a single method — git read at commit —
// so this is intentionally tiny.
type cliInboxDeps struct {
	view *enjugit.View
}

func (d *cliInboxDeps) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
	return d.view.ReadFileAtCommit(commitSHA, repoRelPath)
}
