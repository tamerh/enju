package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/store"
)

func TestLoadServerConfigMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "does-not-exist.conf"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	defaults := defaultServerConfig()
	if cfg.Coordinator.Port != defaults.Coordinator.Port {
		t.Errorf("port = %d, want default %d", cfg.Coordinator.Port, defaults.Coordinator.Port)
	}
	if cfg.Data.StateDB != defaults.Data.StateDB {
		t.Errorf("state_db = %q, want default %q", cfg.Data.StateDB, defaults.Data.StateDB)
	}
	if cfg.Events.EmissionEnabled == nil || !*cfg.Events.EmissionEnabled {
		t.Error("emission_enabled should default to true")
	}
}

func TestLoadServerConfigOverlaysOnDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enju.conf")
	if err := os.WriteFile(path, []byte(`
coordinator:
  bind: "127.0.0.1"
  port: 9999
data:
  state_db: "/var/enju/state.db"
events:
  emission_enabled: false
logging:
  level: "debug"
  output: "stderr"
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Coordinator.Bind != "127.0.0.1" {
		t.Errorf("bind = %q, want 127.0.0.1", cfg.Coordinator.Bind)
	}
	if cfg.Coordinator.Port != 9999 {
		t.Errorf("port = %d, want 9999", cfg.Coordinator.Port)
	}
	if cfg.Data.StateDB != "/var/enju/state.db" {
		t.Errorf("state_db = %q", cfg.Data.StateDB)
	}
	if cfg.Events.EmissionEnabled == nil || *cfg.Events.EmissionEnabled {
		t.Error("emission_enabled should be false")
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("level = %q", cfg.Logging.Level)
	}
	if cfg.Logging.Output != "stderr" {
		t.Errorf("output = %q", cfg.Logging.Output)
	}
}

func TestLoadServerConfigPartialOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enju.conf")
	if err := os.WriteFile(path, []byte(`
coordinator:
  port: 9000
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := defaultServerConfig()
	if cfg.Coordinator.Port != 9000 {
		t.Errorf("port = %d, want 9000", cfg.Coordinator.Port)
	}
	if cfg.Data.StateDB != defaults.Data.StateDB {
		t.Errorf("unset state_db should keep default %q, got %q", defaults.Data.StateDB, cfg.Data.StateDB)
	}
	if cfg.Logging.Level != defaults.Logging.Level {
		t.Errorf("unset level should keep default %q, got %q", defaults.Logging.Level, cfg.Logging.Level)
	}
	if cfg.Events.EmissionEnabled == nil || !*cfg.Events.EmissionEnabled {
		t.Error("unset emission_enabled should keep default true")
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := map[string]string{
		"":              "",
		"/abs/path":     "/abs/path",
		"relative/path": "relative/path",
		"~":             home,
		"~/.enju/x":     filepath.Join(home, ".enju", "x"),
	}
	for input, want := range cases {
		if got := expandPath(input); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadServerConfigMalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enju.conf")
	if err := os.WriteFile(path, []byte("coordinator: { port: not-a-number }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServerConfig(path); err == nil {
		t.Fatal("expected parse error for malformed YAML")
	}
}

// --- SIGHUP-driven reload tests ---

// fakeEventStore satisfies store.EventStore. Only SetEnabled is
// exercised by applyConfigReload; the rest are inert. Used so the
// reload tests don't need a real SQLite-backed events.db.
type fakeEventStore struct {
	enabled  bool
	setCalls int
}

func (f *fakeEventStore) Record(store.Event)     {}
func (f *fakeEventStore) Enabled() bool          { return f.enabled }
func (f *fakeEventStore) SetEnabled(b bool)      { f.enabled = b; f.setCalls++ }
func (f *fakeEventStore) Stats() store.Stats     { return store.Stats{} }
func (f *fakeEventStore) Close() error           { return nil }
func (f *fakeEventStore) WaitForDrain(time.Duration) {}
func (f *fakeEventStore) QueryByRun(context.Context, int64, time.Time, int) ([]store.Event, error) {
	return nil, nil
}
func (f *fakeEventStore) QueryByCitizen(context.Context, int64, int) ([]store.Event, error) {
	return nil, nil
}
func (f *fakeEventStore) Query(context.Context, store.EventQuery) ([]store.Event, error) {
	return nil, nil
}
func (f *fakeEventStore) CountByCitizenAndType(context.Context, int64) (map[string]map[string]int, error) {
	return nil, nil
}
func (f *fakeEventStore) SumTokensForCitizen(context.Context, int64) (int64, error)             { return 0, nil }
func (f *fakeEventStore) CountDistinctProjectsForCitizen(context.Context, int64) (int, error)   { return 0, nil }
func (f *fakeEventStore) CountContributionEvents(context.Context, int64) (int, error)           { return 0, nil }
func (f *fakeEventStore) CountProjectsThisMonth(context.Context, int64, time.Time) (int, error) { return 0, nil }
func (f *fakeEventStore) LatestMetadataForTask(context.Context, string, string) (string, error) { return "", nil }
func (f *fakeEventStore) DistinctTaskIDsForCitizenAndType(context.Context, int64, string) ([]string, error) {
	return nil, nil
}
func (f *fakeEventStore) GapsInProject(context.Context, int64) ([]int64, error) { return nil, nil }

func TestApplyConfigReloadFlipsEventsKillSwitch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	logLevel := new(slog.LevelVar)

	enabled, disabled := true, false
	current := &ServerConfig{Events: EventsConfig{EmissionEnabled: &enabled}}
	fresh := &ServerConfig{Events: EventsConfig{EmissionEnabled: &disabled}}

	es := &fakeEventStore{enabled: true}
	applyConfigReload(current, fresh, es, logLevel, logger)

	if es.enabled {
		t.Error("expected event store disabled after reload")
	}
	if es.setCalls != 1 {
		t.Errorf("SetEnabled called %d times, want 1", es.setCalls)
	}
	if current.Events.EmissionEnabled == nil || *current.Events.EmissionEnabled {
		t.Error("current cfg should track applied state (disabled)")
	}
}

func TestApplyConfigReloadChangesLogLevel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)

	current := &ServerConfig{Logging: LoggingConfig{Level: "info"}}
	fresh := &ServerConfig{Logging: LoggingConfig{Level: "debug"}}

	applyConfigReload(current, fresh, &fakeEventStore{}, logLevel, logger)

	if logLevel.Level() != slog.LevelDebug {
		t.Errorf("log level = %v, want debug", logLevel.Level())
	}
	if current.Logging.Level != "debug" {
		t.Errorf("current cfg should track applied level, got %q", current.Logging.Level)
	}
}

func TestApplyConfigReloadIgnoresRestartOnlyFields(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	logLevel := new(slog.LevelVar)

	current := &ServerConfig{
		Coordinator: CoordinatorConfig{Bind: "", Port: 8000},
		Data:        DataConfig{StateDB: "enju.db"},
	}
	fresh := &ServerConfig{
		Coordinator: CoordinatorConfig{Bind: "127.0.0.1", Port: 9000},
		Data:        DataConfig{StateDB: "/var/enju/state.db"},
	}

	applyConfigReload(current, fresh, &fakeEventStore{}, logLevel, logger)

	if current.Coordinator.Port == 9000 {
		t.Error("port should not be applied at runtime — requires restart")
	}
	if current.Data.StateDB == "/var/enju/state.db" {
		t.Error("state_db should not be applied at runtime — requires restart")
	}
	out := buf.String()
	for _, key := range []string{"coordinator.bind", "coordinator.port", "data.state_db"} {
		if !strings.Contains(out, key) {
			t.Errorf("expected restart-only warning for %q in log output, got: %s", key, out)
		}
	}
}
