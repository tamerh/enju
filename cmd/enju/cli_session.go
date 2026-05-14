package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// cliSession is the shared bootstrap for user-facing fat-client
// CLI commands (`go`, `status`, future tier-1 verbs). Pulls the
// credentials + workspace + coord client into one place so each
// subcommand stays focused on its own logic instead of replicating
// the wiring `cmdMCP` does inline.
//
// Lives separately from the MCP/bot wirings on purpose — those two
// already construct identity at process boot and never recover
// from a missing credentials file (they re-register on the spot).
// CLI verbs should refuse cleanly if credentials are absent, so
// the operator can fix it rather than silently registering a
// drive-by citizen.
type cliSession struct {
	FC    *service.FatClient
	URL   string
	Creds *credentials

	// Bot supervisor — lazily constructed by Supervisor()
	// because only the auto_bots code path needs it. Sharing
	// the same lazy-init shape as mcphandlers/client.go's
	// botSupervisor so the cross-session reconcile (pruning
	// stale auto_run_ids from a prior fatclient session) fires
	// the same way for both MCP and CLI callers.
	supervisorMu sync.Mutex
	supervisor   *bots.Supervisor
}

// Supervisor returns the lazily-constructed bot supervisor for
// this session. First call resolves ~/.enju/bots/{pids,logs} and
// kicks off a background reconcile against the coord; subsequent
// calls return the cached instance. Mirrors
// mcphandlers/client.go:botSupervisor so the two paths stay
// behaviorally identical.
func (s *cliSession) Supervisor() (*bots.Supervisor, error) {
	s.supervisorMu.Lock()
	defer s.supervisorMu.Unlock()
	if s.supervisor != nil {
		return s.supervisor, nil
	}
	sup, err := bots.NewSupervisor()
	if err != nil {
		return nil, err
	}
	s.supervisor = sup
	// Fire reconcile in a goroutine — coord round-trip would
	// otherwise block every auto-bots invocation on startup.
	// Stale refs that survive this pass are GC'd lazily by the
	// next terminal event the tailer observes.
	go func() {
		if rerr := sup.Reconcile(context.Background(), s.isRunTerminal); rerr != nil {
			slog.Default().Warn("supervisor reconcile failed", "error", rerr)
		}
	}()
	return sup, nil
}

// isRunTerminal mirrors mcphandlers/client.go:isRunTerminal —
// returns terminal=true when the coord reports a terminal run
// state OR when the coord doesn't know the run (HTTP 404 →
// coord DB was wiped). The bias toward "terminal on 404"
// matches the MCP path: a stale auto_run_id pointing at a
// vanished run serves no purpose.
func (s *cliSession) isRunTerminal(ctx context.Context, projectID, runSeq int64) (bool, error) {
	data, status, err := s.FC.Coord().GetStatus(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runSeq))
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return true, nil
	}
	if status >= 400 {
		return false, fmt.Errorf("coord run lookup: HTTP %d", status)
	}
	var resp map[string]any
	if jerr := json.Unmarshal(data, &resp); jerr != nil {
		return false, fmt.Errorf("decode run: %w", jerr)
	}
	state, _ := resp["state"].(string)
	switch state {
	case "completed", "failed", "terminated":
		return true, nil
	}
	return false, nil
}

// openCLISession resolves coord URL → credentials → workspace →
// FatClient. coordOverride==""  uses the stored default. Returns
// a typed error message via os.Exit on any missing prerequisite
// so callers don't repeat the same five-line error block.
func openCLISession(coordOverride string) *cliSession {
	url := coordOverride
	if url == "" {
		url = defaultCoordinatorURL()
	}
	creds := loadCredentials(url)
	if creds == nil {
		fmt.Fprintf(os.Stderr,
			"No credentials for coordinator %s.\n"+
				"  Run `enju mcp` once to register, or copy a credentials.json from another machine.\n",
			url)
		os.Exit(3)
	}
	// Post-NDW.6: resolve workspace rootDir + registry path from
	// the --registry default (~/.enju/projects.json). The tier-1
	// CLI verbs don't expose --registry of their own yet; they
	// run against the operator's default registry, same one
	// `enju mcp` writes when adopting projects.
	wsRoot, regPath, err := resolveCLIRegistry("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	coordClient := coord.New(coord.Config{
		BaseURL:     url,
		Username:    creds.Username,
		CitizenName: creds.Name,
		AuthToken:   creds.Token,
		Logger:      logger,
	})
	fc := service.New(service.Config{
		Coord:           coordClient,
		WorkspaceRoot:   wsRoot,
		Logger:          logger,
		LogName:         "cli",
		ProjectRegistry: projectreg.Open(regPath),
	})
	return &cliSession{FC: fc, URL: url, Creds: creds}
}
