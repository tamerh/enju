package mcpgit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/engine"
	yamlv3 "gopkg.in/yaml.v3"
)

// ProjectConfig is the shape of the optional per-project config
// file at engine.ProjectConfigPath (enju/conf.yaml). Every field
// is optional; absent file or absent keys fall through to the
// built-in defaults.
//
// Designed to be additive: new keys can land here without a
// migration step because old conf files (missing the new key)
// still load cleanly under yaml.Unmarshal's default behavior.
//
// Scope intent: project-level integration knobs — "where does
// this repo keep its templates" today, potentially "default
// branch override" or "policy knobs" later. NOT a place for
// per-run or per-task config.
type ProjectConfig struct {
	// Templates lists the repo-relative directories the template
	// loader should scan. Order is honored; the first match for
	// a given bundle wins. When empty or the file is absent, the
	// loader falls back to engine.DefaultTemplatesDir.
	Templates []string `yaml:"templates"`
}

// LoadProjectConfig reads enju/conf.yaml from the workspace. A
// missing file is a normal state and returns (nil, nil) — the
// conf is optional and the caller is expected to apply built-in
// defaults. Returns an error only for real I/O / parse failures
// so a malformed conf surfaces loudly instead of silently
// reverting to defaults (which would make misconfiguration
// invisible).
func (p *Project) LoadProjectConfig() (*ProjectConfig, error) {
	path := filepath.Join(p.workDir, engine.ProjectConfigPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", engine.ProjectConfigPath, err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var cfg ProjectConfig
	if err := yamlv3.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", engine.ProjectConfigPath, err)
	}
	// Normalize Templates entries: trim whitespace, strip trailing
	// slashes so `enju/templates/` and `enju/templates` match
	// downstream path comparisons without string games at every
	// call site. Reject empty-after-trim entries loudly rather
	// than silently dropping them — a blank line in the list is
	// almost certainly a template-author mistake.
	for i, t := range cfg.Templates {
		trimmed := strings.TrimSpace(t)
		trimmed = strings.TrimRight(trimmed, "/")
		if trimmed == "" {
			return nil, fmt.Errorf("%s: templates[%d] is empty", engine.ProjectConfigPath, i)
		}
		if filepath.IsAbs(trimmed) || strings.Contains(trimmed, "..") {
			return nil, fmt.Errorf("%s: templates[%d] %q must be a repo-relative path without ..", engine.ProjectConfigPath, i, t)
		}
		cfg.Templates[i] = filepath.ToSlash(trimmed)
	}
	return &cfg, nil
}

// templateRoots returns the ordered list of directories the
// template loader should scan. Pulls from enju/conf.yaml's
// `templates:` list if present; otherwise falls back to the
// built-in default. Always returns at least one entry so
// downstream scans don't need a special-case "no roots" path.
func (p *Project) templateRoots() ([]string, error) {
	cfg, err := p.LoadProjectConfig()
	if err != nil {
		return nil, err
	}
	if cfg != nil && len(cfg.Templates) > 0 {
		return cfg.Templates, nil
	}
	return []string{engine.DefaultTemplatesDir}, nil
}
