package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadUserConfigMissingFile pins the "fresh install" path:
// no ~/.enju/notify.yaml → zero config, no error. This is what
// keeps the default install pleasant — Layer 1 defaults still
// fire because the loader returns successfully with empty rules.
func TestLoadUserConfigMissingFile(t *testing.T) {
	cfg, warnings, err := LoadUserConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Errorf("missing file should not error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("missing file should yield no warnings, got %v", warnings)
	}
	if len(cfg.Custom) != 0 || len(cfg.DisableDefaults) != 0 {
		t.Errorf("missing file should yield empty config, got %+v", cfg)
	}

	// Empty path → also no-op.
	cfg, warnings, err = LoadUserConfig("")
	if err != nil {
		t.Errorf("empty path should not error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("empty path should yield no warnings, got %v", warnings)
	}
	if len(cfg.Custom) != 0 {
		t.Errorf("empty path should yield empty config, got %+v", cfg)
	}
}

// TestLoadUserConfigParsesRules pins the YAML schema. If a user
// writes a rule with predicate + kind + message, it round-trips
// to a Rule the matcher accepts.
func TestLoadUserConfigParsesRules(t *testing.T) {
	yaml := `
disable_defaults:
  - my_task_failed

custom:
  - name: slack-completions
    on:
      event_type: task_completed
      citizen: "{{me}}"
    kind: slack
    message: "Task {{task_id}} done"
  - name: shell-on-issue
    on:
      event_type: issue_filed
    kind: shell
    do: "./scripts/page.sh {{task_id}}"
`
	path := filepath.Join(t.TempDir(), "notify.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("valid config should produce no warnings, got %v", warnings)
	}
	if len(cfg.DisableDefaults) != 1 || cfg.DisableDefaults[0] != "my_task_failed" {
		t.Errorf("disable_defaults: got %v, want [my_task_failed]", cfg.DisableDefaults)
	}
	if len(cfg.Custom) != 2 {
		t.Fatalf("custom rules: got %d, want 2", len(cfg.Custom))
	}

	rules := cfg.ToRules()
	if len(rules) != 2 {
		t.Fatalf("ToRules: got %d, want 2", len(rules))
	}
	if rules[0].Name != "slack-completions" || rules[0].Kind != "slack" {
		t.Errorf("rule 0: got name=%q kind=%q, want slack-completions/slack", rules[0].Name, rules[0].Kind)
	}
	if rules[0].When.Citizen != "{{me}}" {
		t.Errorf("rule 0 citizen: got %q, want {{me}}", rules[0].When.Citizen)
	}
	if rules[1].Do != "./scripts/page.sh {{task_id}}" {
		t.Errorf("rule 1 Do: got %q", rules[1].Do)
	}
}

// TestLoadUserConfigMalformedErrors pins that bad YAML surfaces
// as an error instead of silently dropping rules. A typo'd rule
// the user thinks is active but actually isn't would be a quiet
// trust failure — better to refuse to load.
func TestLoadUserConfigMalformedErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("custom:\n  - this is not\n     a valid: : :"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadUserConfig(path)
	if err == nil {
		t.Error("expected parse error on malformed YAML")
	}
	if err != nil && !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure, got: %v", err)
	}
}

// TestLoadUserConfigSurfacesWarnings pins the load-time
// validation: rules that parse but won't dispatch (missing
// kind, typo'd kind) come back as human-readable warnings so
// the caller can log them at startup. Without this, a
// `kind: dektop` typo would silently disappear at the matcher
// layer and only surface when an event hit the rule.
func TestLoadUserConfigSurfacesWarnings(t *testing.T) {
	yaml := `
custom:
  - name: typo-kind
    on: { event_type: task_completed }
    kind: dektop  # typo for desktop
  - name: missing-kind
    on: { event_type: task_failed }
  - name: valid
    on: { event_type: task_completed }
    kind: desktop
`
	path := filepath.Join(t.TempDir(), "notify.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings (typo-kind + missing-kind), got %d: %v", len(warnings), warnings)
	}
	// Sanity: the warning for typo-kind should mention the bad kind value.
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "dektop") {
		t.Errorf("warning should name the unknown kind value 'dektop', got: %v", warnings)
	}
	if !strings.Contains(joined, "missing") {
		t.Errorf("warning should call out missing kind, got: %v", warnings)
	}
}

// TestToRulesDropsKindlessRules pins that a rule without a
// `kind:` field gets silently dropped at load time — it's
// undispatchable, so passing it to the matcher would only waste
// cycles. Caller can validate at YAML-load time if it wants.
func TestToRulesDropsKindlessRules(t *testing.T) {
	cfg := UserConfig{
		Custom: []userRule{
			{Name: "valid", Kind: "desktop"},
			{Name: "no-kind"}, // kindless → dropped
			{Name: "valid-2", Kind: "shell", Do: "echo hi"},
		},
	}
	rules := cfg.ToRules()
	if len(rules) != 2 {
		t.Errorf("expected 2 valid rules after dropping kindless, got %d", len(rules))
	}
	for _, r := range rules {
		if r.Kind == "" {
			t.Errorf("kindless rule should not be returned: %+v", r)
		}
	}
}
