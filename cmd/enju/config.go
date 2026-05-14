package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerConfig is the on-disk shape for `enju serve` configuration.
// Everything is optional: a missing file (or any unset key) falls
// back to defaults equivalent to the pre-config CLI behavior.
//
// Precedence: CLI flag (when explicitly set) > config file > default.
// Existing flags continue to work; users not using a config file
// see no behavior change.
type ServerConfig struct {
	Coordinator CoordinatorConfig `yaml:"coordinator"`
	Data        DataConfig        `yaml:"data"`
	Events      EventsConfig      `yaml:"events"`
	Logging     LoggingConfig     `yaml:"logging"`
	Performance PerformanceConfig `yaml:"performance"`
}

type CoordinatorConfig struct {
	Bind string `yaml:"bind"`
	Port int    `yaml:"port"`
}

type DataConfig struct {
	StateDB  string `yaml:"state_db"`
	EventsDB string `yaml:"events_db"` // empty = derive from state_db
}

type EventsConfig struct {
	EmissionEnabled *bool `yaml:"emission_enabled"` // pointer so "absent" is distinguishable from "false"
}

type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Output string `yaml:"output"` // "stdout" | "stderr" | file path
}

// PerformanceConfig groups the runtime-tuning knobs operators
// reach for when they hit a workload that doesn't match the
// "small lab" defaults — high-throughput hosted multi-tenant,
// HPC compute clusters, slow-link environments, etc. Durations
// are strings (Go's time.ParseDuration syntax: "100ms", "30s",
// "2m") so the YAML stays readable.
type PerformanceConfig struct {
	EventQueueSize     int    `yaml:"event_queue_size"`     // bounded async event-store buffer; full = drop
	EventDrainBudget   string `yaml:"event_drain_budget"`   // duration: read-after-write wait window for aggregations
	ReaperInterval     string `yaml:"reaper_interval"`      // duration: how often the claim reaper sweeps expired claims
	HTTPRequestTimeout string `yaml:"http_request_timeout"` // duration: coordinator HTTP middleware cap
}

// defaultServerConfig returns the built-in defaults — what `enju serve`
// runs as without any config file or flags.
func defaultServerConfig() *ServerConfig {
	enabled := true
	return &ServerConfig{
		Coordinator: CoordinatorConfig{
			Bind: "",         // empty = bind all interfaces (preserves pre-config behavior)
			Port: 8000,
		},
		Data: DataConfig{
			StateDB:  "enju.db",
			EventsDB: "",
		},
		Events: EventsConfig{
			EmissionEnabled: &enabled,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Output: "stdout",
		},
		Performance: PerformanceConfig{
			EventQueueSize:     1000,
			EventDrainBudget:   "100ms",
			ReaperInterval:     "60s",
			HTTPRequestTimeout: "30s",
		},
	}
}

// LoadServerConfig reads the config file at path and overlays its
// values on top of defaults. A missing file is not an error — it
// returns defaults. Unset YAML keys are also defaulted.
func LoadServerConfig(path string) (*ServerConfig, error) {
	cfg := defaultServerConfig()
	if path == "" {
		return cfg, nil
	}
	expanded := expandPath(path)
	data, err := os.ReadFile(expanded)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", expanded, err)
	}
	var loaded ServerConfig
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", expanded, err)
	}
	mergeServerConfig(cfg, &loaded)
	return cfg, nil
}

// mergeServerConfig overlays src onto dst — only non-zero fields in
// src override dst. Keeps the "absent key inherits the default"
// invariant.
func mergeServerConfig(dst, src *ServerConfig) {
	if src.Coordinator.Bind != "" {
		dst.Coordinator.Bind = src.Coordinator.Bind
	}
	if src.Coordinator.Port != 0 {
		dst.Coordinator.Port = src.Coordinator.Port
	}
	if src.Data.StateDB != "" {
		dst.Data.StateDB = src.Data.StateDB
	}
	if src.Data.EventsDB != "" {
		dst.Data.EventsDB = src.Data.EventsDB
	}
	if src.Events.EmissionEnabled != nil {
		dst.Events.EmissionEnabled = src.Events.EmissionEnabled
	}
	if src.Logging.Level != "" {
		dst.Logging.Level = src.Logging.Level
	}
	if src.Logging.Output != "" {
		dst.Logging.Output = src.Logging.Output
	}
	if src.Performance.EventQueueSize != 0 {
		dst.Performance.EventQueueSize = src.Performance.EventQueueSize
	}
	if src.Performance.EventDrainBudget != "" {
		dst.Performance.EventDrainBudget = src.Performance.EventDrainBudget
	}
	if src.Performance.ReaperInterval != "" {
		dst.Performance.ReaperInterval = src.Performance.ReaperInterval
	}
	if src.Performance.HTTPRequestTimeout != "" {
		dst.Performance.HTTPRequestTimeout = src.Performance.HTTPRequestTimeout
	}
}

// parseDurationOr returns the parsed duration, or the fallback if
// the string is empty or unparseable. Used for performance.* fields
// where a config typo shouldn't keep the coordinator from booting.
func parseDurationOr(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// expandPath turns a leading "~" into the user's home directory.
// Used so config files can reference "~/.enju/state.db" portably.
func expandPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// resolveCLIWorkspace turns the (workspaceFlag, registryFlag) pair
// the CLI entry points expose into the absolute workspace rootDir
// and the absolute registry path to open. Encapsulates the
// deprecation handshake:
//
//   - --registry default = projectreg.DefaultPath() (~/.enju/projects.json).
//   - --workspace passed → emit a deprecation warning + use it as
//     the workspace rootDir AND derive the registry path as
//     <workspace>/projects.json unless --registry was also given
//     explicitly (which then wins).
//   - --workspace empty → derive the workspace rootDir from
//     filepath.Dir(registryPath) so housekeeping state (scratch,
//     logs that aren't project-internal) keeps a sibling home
//     next to the registry file. The workspace rootDir is
//     vestigial post-NDW.5 — clones don't live there — but
//     NewWorkspace still requires a non-empty value.
//
// stderr accepts the destination for the deprecation warning so
// tests can capture (or silence) it. Pass os.Stderr in production.
func resolveCLIWorkspace(workspaceFlag, registryFlag string, stderr io.Writer) (workspaceRoot, registryPath string, err error) {
	regPath := registryFlag
	if workspaceFlag != "" {
		fmt.Fprintln(stderr, "warning: --workspace is deprecated post-NDW.6 — projects are path-anchored via projectreg (use --registry to point at projects.json directly). The flag will be removed in a future release.")
		if regPath == "" {
			regPath = filepath.Join(workspaceFlag, "projects.json")
		}
	}
	if regPath == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", "", fmt.Errorf("resolveCLIWorkspace: resolving home dir for default --registry: %w", herr)
		}
		regPath = filepath.Join(home, ".enju", "projects.json")
	}
	wsRoot := workspaceFlag
	if wsRoot == "" {
		wsRoot = filepath.Dir(regPath)
	}
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		return "", "", fmt.Errorf("creating workspace root %s: %w", wsRoot, err)
	}
	return wsRoot, regPath, nil
}

// defaultConfigPath returns the conventional location for the server
// config. Used as the default value of -config; absence is fine.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".enju", "enju.conf")
}

// parseLogLevel maps the textual level into slog. Unknown values
// fall back to info with no error — config typos shouldn't keep
// the coordinator from booting.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// resolveLogOutput opens the writer named by the config. "stdout"
// and "stderr" are special-cased; anything else is treated as a file
// path opened for append. The returned closer is nil for the std
// streams — the caller must not close them.
func resolveLogOutput(s string) (io.Writer, io.Closer, error) {
	switch strings.ToLower(s) {
	case "", "stdout":
		return os.Stdout, nil, nil
	case "stderr":
		return os.Stderr, nil, nil
	default:
		f, err := os.OpenFile(expandPath(s), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", s, err)
		}
		return f, f, nil
	}
}
