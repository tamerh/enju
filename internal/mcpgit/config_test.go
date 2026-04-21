package mcpgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadProjectConfigMissingFile — a project without an
// enju/conf.yaml must load cleanly with a nil config. The file
// is optional; absence is the expected zero-config state.
func TestLoadProjectConfigMissingFile(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	cfg, err := proj.LoadProjectConfig()
	if err != nil {
		t.Fatalf("LoadProjectConfig on empty project: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config for missing file, got %+v", cfg)
	}
}

// TestLoadProjectConfigWithTemplates — a valid conf with a
// templates: list surfaces in the returned ProjectConfig and
// normalizes trailing slashes.
func TestLoadProjectConfigWithTemplates(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	confDir := filepath.Join(proj.WorkDir(), "enju")
	_ = os.MkdirAll(confDir, 0o755)
	confBody := []byte(`templates:
  - config/enju/
  - workflows/enju
`)
	if err := os.WriteFile(filepath.Join(confDir, "conf.yaml"), confBody, 0o644); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	cfg, err := proj.LoadProjectConfig()
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	want := []string{"config/enju", "workflows/enju"}
	if len(cfg.Templates) != len(want) {
		t.Fatalf("templates: got %+v, want %+v", cfg.Templates, want)
	}
	for i, w := range want {
		if cfg.Templates[i] != w {
			t.Errorf("templates[%d] = %q, want %q", i, cfg.Templates[i], w)
		}
	}
}

// TestLoadProjectConfigRejectsAbsoluteOrEscape — paths that
// escape the workspace (absolute, .. traversal) fail loudly at
// load time so a typo doesn't degrade silently to defaults.
func TestLoadProjectConfigRejectsAbsoluteOrEscape(t *testing.T) {
	cases := map[string]string{
		"absolute path": "templates:\n  - /etc/passwd\n",
		"parent escape": "templates:\n  - ../secrets\n",
		"empty entry":   "templates:\n  - \"\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			bare := initBareRemote(t)
			seedRemoteWithInitialCommit(t, bare)
			ws, _ := NewWorkspace(t.TempDir(), nullLogger())
			proj, _ := ws.ForProject(1, bare)

			confDir := filepath.Join(proj.WorkDir(), "enju")
			_ = os.MkdirAll(confDir, 0o755)
			_ = os.WriteFile(filepath.Join(confDir, "conf.yaml"), []byte(body), 0o644)

			_, err := proj.LoadProjectConfig()
			if err == nil {
				t.Fatalf("expected error for %s, got nil", name)
			}
		})
	}
}

// TestLoadProjectConfigMalformedYAML — a broken conf file
// surfaces the YAML error instead of silently reverting to
// defaults. Visibility beats leniency for configuration.
func TestLoadProjectConfigMalformedYAML(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	confDir := filepath.Join(proj.WorkDir(), "enju")
	_ = os.MkdirAll(confDir, 0o755)
	_ = os.WriteFile(filepath.Join(confDir, "conf.yaml"), []byte("templates: [unclosed\n"), 0o644)

	_, err := proj.LoadProjectConfig()
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing") {
		t.Errorf("expected parse error message, got: %v", err)
	}
}

// TestListTemplatesUsesConfiguredRoots — a conf pointing at a
// non-default directory drives ListTemplates to scan that
// directory instead of enju/templates/. This is the whole
// monorepo-friendly point of the override.
func TestListTemplatesUsesConfiguredRoots(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	// Drop a bundle under a custom path.
	bundleDir := filepath.Join(proj.WorkDir(), "config", "enju", "hello")
	_ = os.MkdirAll(bundleDir, 0o755)
	_ = os.WriteFile(filepath.Join(bundleDir, "enju.yaml"), []byte(`name: "hello"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "x"
`), 0o644)

	// Without the conf, discovery skips the custom dir.
	templates, err := proj.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates (no conf): %v", err)
	}
	if len(templates) != 0 {
		t.Errorf("expected 0 templates without conf, got %d", len(templates))
	}

	// Write a conf pointing at the custom dir. Discovery now
	// surfaces the bundle.
	confDir := filepath.Join(proj.WorkDir(), "enju")
	_ = os.MkdirAll(confDir, 0o755)
	_ = os.WriteFile(filepath.Join(confDir, "conf.yaml"), []byte("templates:\n  - config/enju\n"), 0o644)

	templates, err = proj.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates (with conf): %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected 1 template via conf, got %d: %+v", len(templates), templates)
	}
	if templates[0].Path != "config/enju/hello/enju.yaml" {
		t.Errorf("path: got %q", templates[0].Path)
	}

	// LoadTemplate honors the same conf-driven root.
	loaded, err := proj.LoadTemplate("config/enju/hello")
	if err != nil {
		t.Fatalf("LoadTemplate via conf: %v", err)
	}
	if loaded.BundleDir != "config/enju/hello" {
		t.Errorf("BundleDir: got %q", loaded.BundleDir)
	}
}
