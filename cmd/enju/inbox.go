package main

// `enju inbox` — terminal counterpart to the enju_inbox MCP
// tool. Calls the coordinator's /projects/{id}/inbox endpoint
// and renders the response with the same formatter the MCP
// tool uses (mcpserver.FormatInbox), so both surfaces stay
// textually identical.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/enju-ai/enju/internal/mcpserver"
)

func cmdInbox(args []string) {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	coordinator := fs.String("coordinator", "http://localhost:8000", "Coordinator URL")
	credsPath := fs.String("credentials", "", "Path to credentials.json (default ~/.enju/credentials.json). Use a per-identity path when running for a non-default citizen.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: enju inbox <project_id> [flags]

Show ready tasks assigned to you in the given project, with each
upstream task's latest submission inlined so you can read the
work without claiming first.

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

	rows, err := fetchInbox(*coordinator, creds.Token, projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(mcpserver.FormatInbox(rows))
}

// fetchInbox calls GET /api/v1/projects/{id}/inbox with the
// bearer token and decodes the response. Surfaces the
// coordinator's error message verbatim on a non-2xx status so
// the user sees the real cause (auth, membership, etc.) instead
// of a generic transport error.
func fetchInbox(coordinator, token string, projectID int64) ([]mcpserver.InboxRow, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%d/inbox", coordinator, projectID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling coordinator: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// Coordinator returns {"error": "..."} on 4xx/5xx;
		// surface that message rather than the raw decode error.
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errBody.Error)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var rows []mcpserver.InboxRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decoding inbox response: %w", err)
	}
	return rows, nil
}
