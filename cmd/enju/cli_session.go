package main

import (
	"fmt"
	"log/slog"
	"os"

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
