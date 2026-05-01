package notify

// User config loader for project-scoped notify.yaml. v1 schema is
// dead simple: one knob, `disable_defaults`, to opt out of any of
// the 9 built-in Layer 1 defaults. Custom user-defined rules are
// post-launch — for v1 the user surface is "you have 9 built-in
// notification types, you can turn any of them off."

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// UserConfig is the on-disk shape of {project_clone}/enju/notify.yaml.
// Missing file → zero value (no defaults disabled). Malformed file
// → error so a typo surfaces immediately rather than silently
// dropping the user's intent.
type UserConfig struct {
	// DisableDefaults turns off built-in Layer 1 defaults by
	// name. The literal "all" disables every default. Names match
	// the Rule.Name values in compiledDefaults().
	DisableDefaults []string `yaml:"disable_defaults"`
}

// LoadUserConfig reads enju/notify.yaml (or whatever path the
// caller passes) and returns the parsed config. Missing file is
// fine — returns zero config + nil error. Malformed YAML returns
// an error so the caller can log and fall back to "no opts."
func LoadUserConfig(path string) (UserConfig, error) {
	if path == "" {
		return UserConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return UserConfig{}, nil
		}
		return UserConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg UserConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return UserConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}
