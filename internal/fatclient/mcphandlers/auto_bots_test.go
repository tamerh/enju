package mcphandlers

import (
	"os"
	"testing"
	"time"
)

func TestAutoBotsReadyTimeout_Default(t *testing.T) {
	// Clear any inherited env so the test sees the production
	// default (30s) regardless of the shell that started it.
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "")
	got := autoBotsReadyTimeout()
	if want := 30 * time.Second; got != want {
		t.Errorf("default timeout: want %s, got %s", want, got)
	}
}

func TestAutoBotsReadyTimeout_EnvOverride(t *testing.T) {
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "5s")
	if got := autoBotsReadyTimeout(); got != 5*time.Second {
		t.Errorf("env=5s: want 5s, got %s", got)
	}
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "2m")
	if got := autoBotsReadyTimeout(); got != 2*time.Minute {
		t.Errorf("env=2m: want 2m, got %s", got)
	}
}

func TestAutoBotsReadyTimeout_BadValueFallsBackToDefault(t *testing.T) {
	// A malformed duration shouldn't crash create_run; falls back
	// to the safe default so the operator just sees the standard
	// 30s wait instead of a hard failure at the wait site.
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "not-a-duration")
	if got := autoBotsReadyTimeout(); got != 30*time.Second {
		t.Errorf("bad value: want 30s default, got %s", got)
	}
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "-5s")
	if got := autoBotsReadyTimeout(); got != 30*time.Second {
		t.Errorf("negative value: want 30s default, got %s", got)
	}
}

// TestEnjuAutoBotsTimeoutEnvVarName guards against an accidental
// rename of the env var — the documented name is part of the
// public surface (operators set it in their shell rc).
func TestEnjuAutoBotsTimeoutEnvVarName(t *testing.T) {
	// Direct os.Setenv via t.Setenv guarantees cleanup; this
	// also serves as living documentation for the var name.
	const want = "ENJU_AUTO_BOTS_TIMEOUT"
	t.Setenv(want, "42s")
	if got := os.Getenv(want); got != "42s" {
		t.Fatalf("env round-trip via %q failed", want)
	}
	if got := autoBotsReadyTimeout(); got != 42*time.Second {
		t.Errorf("autoBotsReadyTimeout didn't honor %s=42s: %s", want, got)
	}
}
