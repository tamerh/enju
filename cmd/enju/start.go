package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// startRecord is persisted to ~/.enju/enju-start.json so cmdStop
// can find and terminate the processes started by cmdStart.
type startRecord struct {
	CoordPID  int    `json:"coord_pid"`
	UIPID     int    `json:"ui_pid,omitempty"`
	CoordPort int    `json:"coord_port"`
	UIPort    int    `json:"ui_port,omitempty"`
	StartedAt string `json:"started_at"`
}

func startRecordPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".enju", "enju-start.json")
}

// cmdStart is the primary way to run Enju locally. By default it forks
// the coordinator and web UI to the background and returns immediately.
//
// --foreground runs the coordinator in-process (blocking), with no UI,
// for deployment scenarios where a process manager (systemd, Docker)
// owns the lifecycle. In this mode it is equivalent to `enju serve`
// with the same flags; `enju serve` is still available as a power-user
// alias but is no longer documented in the main help.
//
// UI startup in background mode is skipped when no credentials exist —
// the UI needs an authenticated citizen. Register first via
// `enju mcp --name "..."` or `enju go`, then restart `enju start`.
func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	port := fs.Int("port", 8333, "Coordinator port")
	uiPort := fs.Int("ui-port", 8484, "UI port (background mode only)")
	dbDir := fs.String("db-dir", "", "Directory for coordinator databases (default ~/.enju/db)")
	noUI := fs.Bool("no-ui", false, "Skip the web UI (background mode only)")
	foreground := fs.Bool("foreground", false, "Run coordinator in foreground instead of background (no UI; for deployment)")
	configPath := fs.String("config", defaultConfigPath(), "Config file path (foreground mode only)")
	fs.Parse(args)

	resolvedDBDir := *dbDir
	if resolvedDBDir == "" {
		home, _ := os.UserHomeDir()
		resolvedDBDir = filepath.Join(home, ".enju", "db")
	}
	dbPath := filepath.Join(resolvedDBDir, "enju.db")

	if *foreground {
		// Run coordinator directly in this process — blocking until signal.
		// Construct the same args cmdServe would receive so all its logic
		// (config file, SIGHUP reload, event store, reaper) applies normally.
		serveArgs := []string{
			fmt.Sprintf("--port=%d", *port),
			fmt.Sprintf("--db=%s", dbPath),
			fmt.Sprintf("--config=%s", *configPath),
		}
		cmdServe(serveArgs)
		return
	}

	// Background mode.
	pidPath := startRecordPath()
	if rec := loadStartRecord(pidPath); rec != nil && isProcessAlive(rec.CoordPID) {
		fmt.Fprintf(os.Stderr, "Enju is already running (coordinator PID %d on port %d).\n", rec.CoordPID, rec.CoordPort)
		fmt.Fprintln(os.Stderr, "Run 'enju stop' first.")
		os.Exit(1)
	}
	os.Remove(pidPath)

	coordURL := fmt.Sprintf("http://localhost:%d", *port)
	credsPath := resolveCredentialsPath("")

	// Fail fast before starting anything if there are no credentials and no
	// git config to auto-register from. Starting the coordinator in this state
	// leaves it running but unusable — the user would have to stop it anyway.
	if loadCredentialsAt(coordURL, credsPath) == nil {
		name := gitGlobalConfig("user.name")
		email := gitGlobalConfig("user.email")
		if !hasFullIdentity(name, email) {
			fmt.Fprintln(os.Stderr, "Cannot start: no credentials found and git config user.name/user.email are both required.")
			fmt.Fprintln(os.Stderr, "  git config --global user.name  \"Your Name\"")
			fmt.Fprintln(os.Stderr, "  git config --global user.email \"you@example.com\"")
			fmt.Fprintln(os.Stderr, "  Then run: enju start")
			os.Exit(1)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start: resolve executable: %v\n", err)
		os.Exit(1)
	}

	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".enju", "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "start: create logs dir: %v\n", err)
		os.Exit(1)
	}

	// Fork coordinator via --foreground so the child runs the same code path.
	coordArgs := []string{"start", "--foreground",
		fmt.Sprintf("--port=%d", *port),
		fmt.Sprintf("--db-dir=%s", resolvedDBDir),
		fmt.Sprintf("--config=%s", *configPath),
	}
	coordLog := filepath.Join(logsDir, "coord.log")
	coordPID, err := forkBackground(exe, coordArgs, coordLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start: coordinator: %v\n", err)
		os.Exit(1)
	}

	if !waitForHTTP(coordURL+"/health", 5*time.Second) {
		fmt.Fprintf(os.Stderr, "start: coordinator (PID %d) did not come up within 5s — check logs: %s\n", coordPID, coordLog)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Coordinator started  PID %-7d  %s\n", coordPID, coordURL)
	fmt.Fprintf(os.Stderr, "  logs → %s\n", coordLog)

	// Auto-register from git global config if no credentials exist yet.
	// This makes `enju start` zero-friction on first run — the user
	// never has to run a separate registration step.
	registered := false
	if creds := loadCredentialsAt(coordURL, credsPath); creds == nil {
		registered = autoRegister(coordURL, credsPath)
	} else {
		registered = true
	}

	rec := &startRecord{
		CoordPID:  coordPID,
		CoordPort: *port,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if !*noUI {
		creds := loadCredentialsAt(coordURL, credsPath)
		if creds != nil && creds.Token != "" {
			uiLog := filepath.Join(logsDir, "ui.log")
			uiArgs := []string{"ui",
				fmt.Sprintf("--port=%d", *uiPort),
				fmt.Sprintf("--coordinator=%s", coordURL),
			}
			uiPID, uiErr := forkBackground(exe, uiArgs, uiLog)
			if uiErr != nil {
				fmt.Fprintf(os.Stderr, "start: UI failed to start: %v (run 'enju ui' manually)\n", uiErr)
			} else {
				fmt.Fprintf(os.Stderr, "Web UI started       PID %-7d  http://localhost:%d\n", uiPID, *uiPort)
				fmt.Fprintf(os.Stderr, "  logs → %s\n", uiLog)
				rec.UIPID = uiPID
				rec.UIPort = *uiPort
			}
		} else if registered {
			// registered=true but no token means registration returned credentials
			// without a token — shouldn't happen, but surface it clearly.
			fmt.Fprintln(os.Stderr, "Web UI skipped — could not load credentials after registration.")
			fmt.Fprintf(os.Stderr, "  Run manually: enju ui --coordinator %s\n", coordURL)
		}
		// If !registered, autoRegister already printed the reason — no duplicate.
	}

	if err := saveStartRecord(pidPath, rec); err != nil {
		fmt.Fprintf(os.Stderr, "start: warning: couldn't write pid record: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "\nRun 'enju stop' to shut down.")
}

// cmdStop reads the pid record written by cmdStart and sends SIGTERM
// to each recorded process, waiting up to 5 seconds for clean exit
// before escalating to SIGKILL.
func cmdStop(args []string) {
	_ = args
	pidPath := startRecordPath()
	rec := loadStartRecord(pidPath)
	if rec == nil {
		fmt.Fprintln(os.Stderr, "No enju-start record found — nothing to stop.")
		fmt.Fprintln(os.Stderr, "(Processes still running? find them with: pgrep -a enju)")
		os.Exit(1)
	}

	if rec.UIPID > 0 {
		stopProcess("Web UI", rec.UIPID)
	}
	stopProcess("Coordinator", rec.CoordPID)
	os.Remove(pidPath)
	fmt.Fprintln(os.Stderr, "Enju stopped.")
}

func stopProcess(label string, pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil || !isProcessAlive(pid) {
		fmt.Fprintf(os.Stderr, "%s (PID %d): already gone\n", label, pid)
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			fmt.Fprintf(os.Stderr, "%s stopped (PID %d)\n", label, pid)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	fmt.Fprintf(os.Stderr, "%s force-killed (PID %d)\n", label, pid)
}

// forkBackground spawns exe with args as a detached background process,
// appending stdout+stderr to logPath. Returns the child PID.
func forkBackground(exe string, args []string, logPath string) (int, error) {
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// waitForHTTP polls url until it returns 2xx or the deadline passes.
func waitForHTTP(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// isProcessAlive returns true if the process exists and is reachable via signal 0.
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func loadStartRecord(path string) *startRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rec startRecord
	if json.Unmarshal(data, &rec) != nil || rec.CoordPID == 0 {
		return nil
	}
	return &rec
}

func saveStartRecord(path string, rec *startRecord) error {
	data, _ := json.MarshalIndent(rec, "", "  ")
	return os.WriteFile(path, data, 0600)
}
