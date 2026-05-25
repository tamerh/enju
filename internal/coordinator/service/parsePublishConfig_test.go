package service

// Tests for parsePublishConfig — the single validation chokepoint that
// resolves publish policy from (SyncModeOverride, YAMLData) to the
// (mode, remote) pair applyRunCompletion acts on. The precedence
// ladder is: operator override (validated, mode only) > YAML publish:
// block (validated) > defaults ("local", "origin"). (SyncModeOverride
// is the retained wire/column name for the publish-mode override.)

import (
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

func TestParsePublishConfig_Defaults(t *testing.T) {
	mode, remote := parsePublishConfig(&store.RunRecord{})
	if mode != "local" {
		t.Errorf("default mode: got %q, want %q", mode, "local")
	}
	if remote != "origin" {
		t.Errorf("default remote: got %q, want %q", remote, "origin")
	}
}

func TestParsePublishConfig_NilRecord(t *testing.T) {
	mode, remote := parsePublishConfig(nil)
	if mode != "local" || remote != "origin" {
		t.Errorf("nil record: got (%q, %q), want (\"local\", \"origin\")", mode, remote)
	}
}

func TestParsePublishConfig_OverrideValidPush(t *testing.T) {
	mode, remote := parsePublishConfig(&store.RunRecord{SyncModeOverride: "push"})
	if mode != "push" {
		t.Errorf("override=push: got mode %q, want %q", mode, "push")
	}
	if remote != "origin" {
		t.Errorf("override=push: got remote %q, want %q", remote, "origin")
	}
}

func TestParsePublishConfig_OverrideValidNone(t *testing.T) {
	mode, _ := parsePublishConfig(&store.RunRecord{SyncModeOverride: "none"})
	if mode != "none" {
		t.Errorf("override=none: got %q, want %q", mode, "none")
	}
}

// TestParsePublishConfig_OverrideUnknownFallsThrough — a typo in the
// override (MCP call, direct DB edit) is ignored; the YAML value
// is used instead. Prevents silent push→local downgrade.
func TestParsePublishConfig_OverrideUnknownFallsThrough(t *testing.T) {
	yaml := "publish:\n  mode: none\n"
	mode, _ := parsePublishConfig(&store.RunRecord{
		SyncModeOverride: "psh", // typo
		YAMLData:         yaml,
	})
	if mode != "none" {
		t.Errorf("unknown override should fall through to YAML: got %q, want %q", mode, "none")
	}
}

func TestParsePublishConfig_YAMLModeNone(t *testing.T) {
	yaml := "publish:\n  mode: none\n"
	mode, _ := parsePublishConfig(&store.RunRecord{YAMLData: yaml})
	if mode != "none" {
		t.Errorf("YAML mode=none: got %q, want %q", mode, "none")
	}
}

// TestParsePublishConfig_YAMLModeGarbage — an invalid mode that somehow
// reached yaml_data (direct DB edit, pre-validation schema) falls
// back to "local", not the garbage string.
func TestParsePublishConfig_YAMLModeGarbage(t *testing.T) {
	yaml := "publish:\n  mode: fast-forward\n"
	mode, _ := parsePublishConfig(&store.RunRecord{YAMLData: yaml})
	if mode != "local" {
		t.Errorf("YAML garbage mode: got %q, want %q", mode, "local")
	}
}

// TestParsePublishConfig_OverrideTakesPrecedenceOverYAML — a valid
// override beats a valid YAML mode.
func TestParsePublishConfig_OverrideTakesPrecedenceOverYAML(t *testing.T) {
	yaml := "publish:\n  mode: none\n"
	mode, _ := parsePublishConfig(&store.RunRecord{
		SyncModeOverride: "push",
		YAMLData:         yaml,
	})
	if mode != "push" {
		t.Errorf("override should beat YAML: got %q, want %q", mode, "push")
	}
}

// TestParsePublishConfig_YAMLRemoteHonored — publish.remote in the YAML
// is returned as the remote even when no override is set.
func TestParsePublishConfig_YAMLRemoteHonored(t *testing.T) {
	yaml := "publish:\n  mode: push\n  remote: upstream\n"
	_, remote := parsePublishConfig(&store.RunRecord{YAMLData: yaml})
	if remote != "upstream" {
		t.Errorf("YAML remote: got %q, want %q", remote, "upstream")
	}
}

// TestParsePublishConfig_OverrideDoesNotChangeRemote — --publish overrides
// the mode only; publish.remote from the YAML is still honored.
// The operator who declared remote: upstream and runs --publish=push
// should push to upstream, not silently to origin.
func TestParsePublishConfig_OverrideDoesNotChangeRemote(t *testing.T) {
	yaml := "publish:\n  mode: local\n  remote: upstream\n"
	mode, remote := parsePublishConfig(&store.RunRecord{
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
