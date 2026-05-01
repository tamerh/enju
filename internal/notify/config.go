package notify

// User-level config loader — Layer 3 of the three-layer rule
// model in docs/notifications.md. Lives at ~/.enju/notify.yaml.
// The format is intentionally narrow for v1: rules + opt-out of
// Layer 1 defaults. Phase 4e+ may add allow_shell_rules_from_
// projects (Layer 2 trust list) and rate-limit overrides; the
// schema is forward-compatible.

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// UserConfig is the on-disk shape of ~/.enju/notify.yaml. All
// fields are optional. A missing file is fine (returns zero
// value); a malformed file is a hard error so the user notices
// the typo instead of silently losing rules.
type UserConfig struct {
	// DisableDefaults turns off built-in Layer 1 defaults by
	// name. The literal "all" disables every default.
	DisableDefaults []string `yaml:"disable_defaults"`

	// Custom is the list of user-defined rules. Each is
	// composed with Layer 1 defaults (additive — defaults
	// still fire unless disabled).
	Custom []userRule `yaml:"custom"`
}

// userRule is the YAML wire shape of one rule. We keep this
// separate from notify.Rule so the YAML schema can evolve
// independently of the in-memory matcher (e.g., adding `if:`
// predicate expressions later without churning Rule).
type userRule struct {
	Name    string         `yaml:"name"`
	On      userPredicate  `yaml:"on"`
	Kind    string         `yaml:"kind"`
	Message string         `yaml:"message"`
	Do      string         `yaml:"do"`
}

type userPredicate struct {
	EventType string `yaml:"event_type"`
	Subtype   string `yaml:"subtype"`
	TaskID    string `yaml:"task_id"`
	Citizen   string `yaml:"citizen"`
}

// LoadUserConfig reads ~/.enju/notify.yaml (or whatever path
// the caller passes) and returns the parsed config + a list
// of human-readable warnings about parsed-but-unusable rules
// (typo'd kind, missing kind, etc). Callers should log the
// warnings so a `kind: dektop` typo surfaces at startup rather
// than on the first event.
//
// Missing file → zero config + nil warnings + nil error.
// Malformed YAML → empty config + error so the caller can log
// and continue with just Layer 1 defaults.
func LoadUserConfig(path string) (UserConfig, []string, error) {
	if path == "" {
		return UserConfig{}, nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return UserConfig{}, nil, nil
		}
		return UserConfig{}, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg UserConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return UserConfig{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, cfg.validate(), nil
}

// validate returns non-fatal issues with the parsed config.
// Hard errors (malformed YAML) are surfaced by LoadUserConfig
// itself; this list is for "the rule parsed but won't fire" —
// the kind of thing a user wants to see at startup so they can
// fix their typo.
func (uc UserConfig) validate() []string {
	var warnings []string
	knownKinds := map[string]bool{"desktop": true, "shell": true, "slack": true}
	for i, r := range uc.Custom {
		label := r.Name
		if label == "" {
			label = fmt.Sprintf("rule[%d]", i)
		}
		if r.Kind == "" {
			warnings = append(warnings, fmt.Sprintf("%s: missing `kind:` — rule will be ignored", label))
			continue
		}
		if !knownKinds[r.Kind] {
			warnings = append(warnings, fmt.Sprintf("%s: unknown kind %q (known: desktop, shell, slack) — rule will fail to dispatch", label, r.Kind))
		}
	}
	return warnings
}

// ToRules converts the parsed YAML rules into the in-memory
// Rule shape the matcher consumes. Drops rules with empty Kind
// (would be impossible to dispatch) and logs nothing — caller
// can validate at load time if it wants.
func (uc UserConfig) ToRules() []Rule {
	out := make([]Rule, 0, len(uc.Custom))
	for _, ur := range uc.Custom {
		if ur.Kind == "" {
			continue
		}
		out = append(out, Rule{
			Name: ur.Name,
			When: Predicate{
				EventType: ur.On.EventType,
				Subtype:   ur.On.Subtype,
				TaskID:    ur.On.TaskID,
				Citizen:   ur.On.Citizen,
			},
			Kind:    ur.Kind,
			Message: ur.Message,
			Do:      ur.Do,
		})
	}
	return out
}
