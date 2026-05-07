package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/enju-ai/enju/internal/fatclient/project"
	"github.com/enju-ai/enju/internal/webui"
)

// cmdUI is the `enju ui` subcommand entry point. Constructs a
// FatClient (identity → coord client → workspace) the same way
// `enju mcp` does, then mounts internal/webui's Handler() on a
// local HTTP listener.
//
// Identity resolution mirrors cmdMCP — load credentials.json,
// register if missing, persist on first run. Multi-citizen
// support via -credentials flag works the same.
//
// No local mode in v1: `enju ui` requires a running coordinator
// at -coordinator. Users wanting zero-config single-user can run
// `enju mcp --local` (which spins up the coord) and then point
// `enju ui` at the printed URL. Embedded-coord mode for `enju
// ui` is a follow-up if ergonomics demand it.
//
// === Auth / threat model ===
//
// `enju ui` is single-user, local-only. The threat model is:
//
//  1. The binary binds 127.0.0.1:PORT — never a public
//     interface. Anyone with shell access on this host already
//     has at least the privileges of the citizen the binary was
//     launched with; the UI doesn't widen that surface.
//  2. The bearer token sits in-process (loaded from
//     ~/.enju/credentials.json). The UI never serves it to the
//     browser — every coord call goes through FatClient,
//     which adds the Authorization header server-side.
//  3. CSRF is the only remaining risk: a malicious page on
//     evil.com could fetch() http://127.0.0.1:PORT/actions/...
//     and the browser would send the request from the user's
//     localhost session. The Origin-check middleware in
//     internal/webui/middleware.go rejects any state-changing
//     method (POST/PUT/DELETE) whose Origin header isn't one
//     of {http://127.0.0.1:PORT, http://localhost:PORT}.
//
// What this means for contributors:
//
//   - DO NOT bind to 0.0.0.0 or any public address. The
//     listener address is "127.0.0.1:PORT" and that's
//     load-bearing.
//   - DO NOT relax the Origin check for write paths. If a
//     legitimate same-origin caller is being rejected, the bug
//     is in the client (HTMX is misconfigured, fetch() is
//     missing credentials), not the middleware.
//   - DO NOT serve `enju ui` behind a reverse proxy that
//     rewrites Host/Origin without explicitly trusting the
//     proxy. Hosted-mode multi-user UI is its own iteration
//     with a real auth story; v1 doesn't try to be that.
func cmdUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	coordinator := fs.String("coordinator", defaultCoordinatorURL(), "Coordinator URL (defaults to value in ~/.enju/credentials.json, else http://localhost:8000)")
	name := fs.String("name", "", "Citizen display name (e.g. \"Tamer Gur\")")
	username := fs.String("username", "", "Citizen username (optional, auto-generated from name if omitted)")
	email := fs.String("email", "", "Citizen email (optional)")
	workspaceDir := fs.String("workspace", "", "Directory for per-project local clones (default ~/.enju/workspaces)")
	credsPath := fs.String("credentials", "", "Path to credentials.json (default ~/.enju/credentials.json)")
	port := fs.Int("port", 8080, "Port to bind the UI on (127.0.0.1 only)")
	dev := fs.Bool("dev", false, "Dev mode: re-parse templates per request, no-cache headers, debug overlay")
	fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Identity — mirror cmdMCP's resolution path. credentials.json
	// is the source of truth; CLI args are bootstrap-only.
	resolvedCredsPath := resolveCredentialsPath(*credsPath)
	creds := loadCredentialsAt(*coordinator, resolvedCredsPath)
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

	var token string
	if creds == nil {
		gotUsername, gotToken, err := registerCitizen(*coordinator, *name, *username, *email)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to register: %v\n", err)
			os.Exit(1)
		}
		*username = gotUsername
		token = gotToken
		saveCredentialsAt(*coordinator, *username, *name, *email, token, resolvedCredsPath)
		fmt.Fprintf(os.Stderr, "Registered as @%s (%s)\n", *username, *name)
	} else {
		token = creds.Token
	}

	ws, err := project.NewOpener(*workspaceDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create workspace: %v\n", err)
		os.Exit(1)
	}

	coordClient := coord.New(coord.Config{
		BaseURL:      *coordinator,
		Username:     *username,
		CitizenName:  *name,
		CitizenEmail: *email,
		AuthToken:    token,
		SaveCredentials: func(gotUsername, gotName, gotEmail, gotToken string) {
			saveCredentialsAt(*coordinator, gotUsername, gotName, gotEmail, gotToken, resolvedCredsPath)
		},
		Logger: logger,
	})
	fc := service.New(service.Config{
		Coord:           coordClient,
		WorkspaceRoot:   ws.RootDir(),
		Logger:          logger,
		ProjectRegistry: projectreg.Open(projectreg.DefaultPath()),
	})

	srv, err := webui.New(webui.Config{
		FC:     fc,
		Logger: logger,
		Dev:    *dev,
		Port:   *port,
		// DevViewsFS / DevStaticFS could be wired with
		// os.DirFS("internal/webui/views") + ".../static" for
		// live editing; deferred until we add the build-tag or
		// flag-driven dev path.
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to construct UI server: %v\n", err)
		os.Exit(1)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	fmt.Fprintf(os.Stderr, "Enju UI starting on http://%s (citizen @%s)\n", addr, *username)
	if *dev {
		fmt.Fprintln(os.Stderr, "Dev mode: templates re-parsed per request, no-cache headers")
	}
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		fmt.Fprintf(os.Stderr, "UI server error: %v\n", err)
		os.Exit(1)
	}
}
