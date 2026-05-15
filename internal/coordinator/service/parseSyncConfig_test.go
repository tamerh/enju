package service

// Tests for parseSyncConfig — the single validation chokepoint that
// resolves sync mode from (SyncModeOverride, YAMLData) to the
// (mode, remote) pair applyRunCompletion acts on. The precedence
// ladder is: operator override (validated) > YAML sync: block
// (validated) > defaults ("merge", "origin").

import (
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

func TestParseSyncConfig_Defaults(t *testing.T) {
	mode, remote := parseSyncConfig(&store.RunRecord{})
	if mode != "merge" {
		t.Errorf("default mode: got %q, want %q", mode, "merge")
	}
	if remote != "origin" {
		t.Errorf("default remote: got %q, want %q", remote, "origin")
	}
}

func TestParseSyncConfig_NilRecord(t *testing.T) {
	mode, remote := parseSyncConfig(nil)
	if mode != "merge" || remote != "origin" {
		t.Errorf("nil record: got (%q, %q), want (\"merge\", \"origin\")", mode, remote)
	}
}

func TestParseSyncConfig_OverrideValidPush(t *testing.T) {
	mode, remote := parseSyncConfig(&store.RunRecord{SyncModeOverride: "push"})
	if mode != "push" {
		t.Errorf("override=push: got mode %q, want %q", mode, "push")
	}
	if remote != "origin" {
		t.Errorf("override=push: got remote %q, want %q", remote, "origin")
	}
}

func TestParseSyncConfig_OverrideValidNone(t *testing.T) {
	mode, _ := parseSyncConfig(&store.RunRecord{SyncModeOverride: "none"})
	if mode != "none" {
		t.Errorf("override=none: got %q, want %q", mode, "none")
	}
}

// TestParseSyncConfig_OverrideUnknownFallsThrough — a typo in the
// override (MCP call, direct DB edit) is ignored; the YAML value
// is used instead. Prevents silent push→merge downgrade.
func TestParseSyncConfig_OverrideUnknownFallsThrough(t *testing.T) {
	yaml := "sync:\n  mode: none\n"
	mode, _ := parseSyncConfig(&store.RunRecord{
		SyncModeOverride: "psh", // typo
		YAMLData:         yaml,
	})
	if mode != "none" {
		t.Errorf("unknown override should fall through to YAML: got %q, want %q", mode, "none")
	}
}

func TestParseSyncConfig_YAMLModeNone(t *testing.T) {
	yaml := "sync:\n  mode: none\n"
	mode, _ := parseSyncConfig(&store.RunRecord{YAMLData: yaml})
	if mode != "none" {
		t.Errorf("YAML mode=none: got %q, want %q", mode, "none")
	}
}

// TestParseSyncConfig_YAMLModeGarbage — an invalid mode that somehow
// reached yaml_data (direct DB edit, pre-validation schema) falls
// back to "merge", not the garbage string.
func TestParseSyncConfig_YAMLModeGarbage(t *testing.T) {
	yaml := "sync:\n  mode: fast-forward\n"
	mode, _ := parseSyncConfig(&store.RunRecord{YAMLData: yaml})
	if mode != "merge" {
		t.Errorf("YAML garbage mode: got %q, want %q", mode, "merge")
	}
}

// TestParseSyncConfig_OverrideTakesPrecedenceOverYAML — a valid
// override beats a valid YAML mode.
func TestParseSyncConfig_OverrideTakesPrecedenceOverYAML(t *testing.T) {
	yaml := "sync:\n  mode: none\n"
	mode, _ := parseSyncConfig(&store.RunRecord{
		SyncModeOverride: "push",
		YAMLData:         yaml,
	})
	if mode != "push" {
		t.Errorf("override should beat YAML: got %q, want %q", mode, "push")
	}
}

// TestParseSyncConfig_YAMLRemoteHonored — sync.remote in the YAML
// is returned as the remote even when no override is set.
func TestParseSyncConfig_YAMLRemoteHonored(t *testing.T) {
	yaml := "sync:\n  mode: push\n  remote: upstream\n"
	_, remote := parseSyncConfig(&store.RunRecord{YAMLData: yaml})
	if remote != "upstream" {
		t.Errorf("YAML remote: got %q, want %q", remote, "upstream")
	}
}

// TestParseSyncConfig_OverrideDoesNotChangeRemote — --sync overrides
// the mode only; sync.remote from the YAML is still honored.
// The operator who declared remote: upstream and runs --sync=push
// should push to upstream, not silently to origin.
func TestParseSyncConfig_OverrideDoesNotChangeRemote(t *testing.T) {
	yaml := "sync:\n  mode: merge\n  remote: upstream\n"
	mode, remote := parseSyncConfig(&store.RunRecord{
		SyncModeOverride: "push",
		YAMLData:         yaml,
	})
	if mode != "push" {
		t.Errorf("mode: got %q, want %q", mode, "push")
	}
	if remote != "upstream" {
		t.Errorf("remote: got %q, want %q (override should not reset remote)", remote, "upstream")
	}
}
