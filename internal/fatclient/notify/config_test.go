package notify

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadUserConfigDisableAllScalar pins that `disable_defaults: all`
// (bare scalar form) suppresses every default. The list form
// `disable_defaults: [all]` already worked; the bare scalar form
// is what the docs imply via `# or: "all"`.
func TestLoadUserConfigDisableAllScalar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notify.yaml")
	if err := os.WriteFile(path, []byte("disable_defaults: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("LoadUserConfig with bare scalar 'all' should parse, got error: %v", err)
	}
	rules := EffectiveDefaults(cfg.DisableDefaults)
	if len(rules) != 0 {
		t.Errorf("disable_defaults: all should suppress every default; got %d rules still active", len(rules))
	}
}

// TestLoadUserConfigDisableAllList pins the list form still works.
func TestLoadUserConfigDisableAllList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notify.yaml")
	if err := os.WriteFile(path, []byte("disable_defaults: [all]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadUserConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	rules := EffectiveDefaults(cfg.DisableDefaults)
	if len(rules) != 0 {
		t.Errorf("disable_defaults: [all] should suppress every default; got %d rules still active", len(rules))
	}
}

// TestLoadUserConfigDisableSpecific pins the named-list form (regression).
func TestLoadUserConfigDisableSpecific(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notify.yaml")
	if err := os.WriteFile(path, []byte("disable_defaults: [branch_merged, run_paused]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadUserConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	rules := EffectiveDefaults(cfg.DisableDefaults)
	for _, r := range rules {
		if r.Name == "branch_merged" || r.Name == "run_paused" {
			t.Errorf("rule %q should be suppressed", r.Name)
		}
	}
	if len(rules) != 8 {
		t.Errorf("expected 8 rules after disabling 2 of 10, got %d", len(rules))
	}
}
