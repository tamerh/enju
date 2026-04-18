package mcpgit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// Phase H.1 + template-bundles pass — Template discovery and
// instantiation on the fat client side. A template is a
// DIRECTORY under `enju_templates/` with a `template.yaml` at
// its root:
//
//   enju_templates/
//     gwas-analysis/
//       template.yaml       ← the run definition (required)
//       scripts/            ← bundled scripts referenced by compute tasks
//       examples/           ← sample outputs, ignored by the loader
//       README.md           ← author docs, ignored by the loader
//
// Everything else in the bundle — scripts, data files, docs,
// examples — travels with the template when it's instantiated
// into a run (see the snapshot-on-instantiate flow driven from
// handleCreateRun). This makes templates self-contained and
// makes runs reproducible: a live template edit after a run
// was created can't retroactively change that run's behavior
// because the run owns a frozen copy.
//
// Loose `.yaml` files directly under `enju_templates/` are not
// recognized — they'd be ambiguous about whether they own the
// surrounding directory. Templates must live in their own
// folder; if an author wants to migrate an existing single-file
// template, they move `enju_templates/foo.yaml` to
// `enju_templates/foo/template.yaml`.

// TemplateSummary is the lightweight shape returned by
// ListTemplates — enough for an LLM to pick a template from a
// menu without having to parse the full YAML of each one.
// When a template file fails to parse, the summary still shows
// up in the list with ParseError populated; the caller can see
// the path + the reason without having to drill in via
// describe_template to discover why it's missing. Hiding
// unparseable templates (the old behavior) was a silent-skip
// UX failure — users thought the tool didn't scan their
// directory, when actually their file just failed to decode.
type TemplateSummary struct {
	Path        string         `json:"path"`                   // repo-relative, e.g. "enju_templates/gwas.yaml"
	Name        string         `json:"name,omitempty"`         // from `name:` field
	Description string         `json:"description,omitempty"`  // from `description:` field
	Params      []ParamSummary `json:"params,omitempty"`       // short param summary
	ParseError  string         `json:"parse_error,omitempty"` // set when the template YAML failed to decode/validate
}

// ParamSummary is the per-param shape embedded in a
// TemplateSummary. It's a compressed view of the YAML
// ParamDef — just the fields the LLM needs when deciding
// whether a template fits a user's request.
type ParamSummary struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Required    bool        `json:"required,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Description string      `json:"description,omitempty"`
}

// templateBundleYAML is the canonical entry-point filename
// at the root of every template directory. Named by role
// rather than by enclosing directory (like docker-compose.yaml
// or pyproject.toml) so authors can rename the bundle folder
// without also renaming the YAML.
const templateBundleYAML = "template.yaml"

// ListTemplates scans `enju_templates/` for directory-shaped
// template bundles and returns a summary for each one. A
// bundle is any subdirectory containing `template.yaml` at
// its root. Empty or missing `enju_templates/` is a normal
// state, not an error — returns (nil, nil).
//
// Directories without a `template.yaml` are skipped silently
// (scratch folders, README-only dirs, etc). Bundles whose
// template.yaml fails to parse are surfaced with ParseError
// populated — a visible "unparseable" menu entry beats a
// silent drop that makes the author think the scan missed
// their template.
//
// Loose `.yaml` files directly under `enju_templates/` are
// NOT discovered. If any are found, they're surfaced as a
// single migration-hint entry in the result so the author
// knows to move them; this keeps the "I had foo.yaml, where
// did it go?" experience debuggable.
func (p *Project) ListTemplates() ([]TemplateSummary, error) {
	templatesDir := filepath.Join(p.workDir, "enju_templates")
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading templates directory: %w", err)
	}
	var out []TemplateSummary
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			// Catch the legacy single-file shape and emit
			// exactly one actionable migration hint per
			// offending file — silently skipping would leave
			// the author with an empty menu and no clue why.
			if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
				legacyRel := filepath.ToSlash(filepath.Join("enju_templates", name))
				out = append(out, TemplateSummary{
					Path: legacyRel,
					ParseError: fmt.Sprintf(
						"legacy single-file template layout — move %s to enju_templates/%s/template.yaml",
						legacyRel, strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")),
				})
			}
			continue
		}
		// Directory entry → check for template.yaml at its root.
		manifest := filepath.Join(templatesDir, name, templateBundleYAML)
		if _, statErr := os.Stat(manifest); statErr != nil {
			// Missing manifest → not a template bundle.
			// Skip silently; directories might exist for
			// other purposes (shared scratch, etc).
			continue
		}
		rel := filepath.ToSlash(filepath.Join("enju_templates", name, templateBundleYAML))
		summary, err := p.templateSummary(rel)
		if err != nil {
			out = append(out, TemplateSummary{
				Path:       rel,
				ParseError: err.Error(),
			})
			continue
		}
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// templateSummary reads one template file and returns its
// compressed view. Used by ListTemplates and as a building
// block for LoadTemplate.
func (p *Project) templateSummary(repoRelPath string) (*TemplateSummary, error) {
	data, err := os.ReadFile(filepath.Join(p.workDir, repoRelPath))
	if err != nil {
		return nil, fmt.Errorf("reading template %s: %w", repoRelPath, err)
	}
	parsed, err := enjuYaml.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", repoRelPath, err)
	}
	return &TemplateSummary{
		Path:        repoRelPath,
		Name:        parsed.Run.Name,
		Description: parsed.Run.Description,
		Params:      paramSummaries(parsed.Run.Params),
	}, nil
}

func paramSummaries(ps []enjuYaml.ParamDef) []ParamSummary {
	out := make([]ParamSummary, 0, len(ps))
	for _, pp := range ps {
		out = append(out, ParamSummary{
			Name:        pp.Name,
			Type:        pp.Type,
			Required:    pp.Required,
			Default:     pp.Default,
			Description: pp.Description,
		})
	}
	return out
}

// LoadedTemplate is the full parsed view of a template
// bundle, returned by LoadTemplate. Path is the
// repo-relative path of the manifest YAML; BundleDir is the
// enclosing directory, used by the snapshot-on-instantiate
// flow to enumerate every file to copy.
type LoadedTemplate struct {
	Path      string // e.g. "enju_templates/gwas/template.yaml"
	BundleDir string // e.g. "enju_templates/gwas"
	Raw       []byte
	Summary   TemplateSummary
}

// ReadBundleFiles walks the bundle directory and returns
// every regular file as a FileWrite, with repo-relative paths
// rebased to a target directory (typically the per-run
// snapshot location like `.enju/runs/3/template/`). Used by
// handleCreateRun to commit the bundle into the run's
// snapshot area at instantiation time, locking the recipe +
// its scripts to the moment the run was created.
//
// Symlinks, special files, and hidden-dir entries (`.git/`)
// are skipped. Size-guard: if the bundle exceeds 10 MB total,
// return an error — templates aren't the place for large
// data blobs, and a runaway snapshot would bloat every
// subsequent run commit.
func (p *Project) ReadBundleFiles(bundleDir, targetDir string) ([]FileWrite, error) {
	root := filepath.Join(p.workDir, bundleDir)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat bundle %s: %w", bundleDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle path %q is not a directory", bundleDir)
	}
	const maxBundleBytes = 10 * 1024 * 1024
	var totalBytes int64
	var files []FileWrite
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// Skip hidden dirs — .git (nested submodule checkouts)
			// would pull a huge history into the snapshot.
			if strings.HasPrefix(info.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks, devices, etc.
		}
		totalBytes += info.Size()
		if totalBytes > maxBundleBytes {
			return fmt.Errorf("bundle %q exceeds %d-byte size limit; templates shouldn't carry large data blobs", bundleDir, maxBundleBytes)
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("reading bundle file %s: %w", path, rerr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return fmt.Errorf("computing bundle-relative path for %s: %w", path, rerr)
		}
		// Preserve the source file's mode. Scripts with +x
		// in the live bundle must stay executable in the
		// snapshot — otherwise the executor resolves `script:`
		// to a non-executable file and the task fails with
		// "permission denied" on run. Use at-least-0644 as a
		// floor so a weirdly-permissioned source (0600 from a
		// restrictive umask) still becomes readable by git.
		mode := info.Mode().Perm()
		if mode&0o444 == 0 {
			mode |= 0o644
		}
		files = append(files, FileWrite{
			RepoRelPath: filepath.ToSlash(filepath.Join(targetDir, rel)),
			Content:     body,
			Mode:        mode,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// LoadTemplate reads a template bundle by either:
//   - its directory path (e.g. "enju_templates/gwas-analysis")
//   - the full path to its manifest (e.g.
//     "enju_templates/gwas-analysis/template.yaml")
//
// Both forms resolve to the same bundle; the loader picks up
// the `template.yaml` at the directory root. The resolved
// YAML path is what shows up in the returned
// LoadedTemplate.Path; BundleDir carries the surrounding
// directory so callers doing snapshot-on-instantiate can
// enumerate all the files in the bundle.
func (p *Project) LoadTemplate(repoRelPath string) (*LoadedTemplate, error) {
	if !strings.HasPrefix(repoRelPath, "enju_templates/") {
		return nil, fmt.Errorf("template path %q must live under enju_templates/", repoRelPath)
	}
	// Block path escapes — user-controlled input even though
	// it's read from the local workspace, and a `../` could
	// let a caller pull files from outside the templates dir.
	clean := filepath.ToSlash(filepath.Clean(repoRelPath))
	if strings.Contains(clean, "../") || clean != repoRelPath {
		return nil, fmt.Errorf("template path %q contains disallowed path components", repoRelPath)
	}

	// Normalize: accept either the bundle dir or the full
	// template.yaml path. Loose single-file paths like
	// `enju_templates/foo.yaml` are the legacy shape we no
	// longer support — reject with a migration hint.
	bundleDir, manifestPath, err := resolveBundlePaths(p.workDir, repoRelPath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filepath.Join(p.workDir, manifestPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("template %q not found in workspace — check `enju_list_templates` for available recipes", repoRelPath)
		}
		return nil, fmt.Errorf("reading template %s: %w", manifestPath, err)
	}
	parsed, err := enjuYaml.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", manifestPath, err)
	}
	return &LoadedTemplate{
		Path:      manifestPath,
		BundleDir: bundleDir,
		Raw:       data,
		Summary: TemplateSummary{
			Path:        manifestPath,
			Name:        parsed.Run.Name,
			Description: parsed.Run.Description,
			Params:      paramSummaries(parsed.Run.Params),
		},
	}, nil
}

// resolveBundlePaths maps a caller-supplied template reference
// to the (bundleDir, manifestPath) pair the loader uses, both
// as repo-relative slash-paths. Accepts:
//
//   - "enju_templates/NAME"                  → dir form
//   - "enju_templates/NAME/template.yaml"    → manifest form
//   - anything else ending in .yaml/.yml     → legacy single-file,
//                                              rejected with migration hint
func resolveBundlePaths(workDir, repoRelPath string) (bundleDir, manifestPath string, err error) {
	p := strings.TrimSuffix(repoRelPath, "/")
	// Manifest form: ends in /template.yaml under a bundle dir.
	if strings.HasSuffix(p, "/"+templateBundleYAML) {
		bundleDir = strings.TrimSuffix(p, "/"+templateBundleYAML)
		if bundleDir == "enju_templates" {
			return "", "", fmt.Errorf("template manifest must live inside a bundle subdirectory, e.g. enju_templates/NAME/%s", templateBundleYAML)
		}
		return bundleDir, p, nil
	}
	// Dir form: must be a directory with template.yaml inside.
	if strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") {
		// Legacy single-file reference — emit a migration hint.
		base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(p), ".yaml"), ".yml")
		return "", "", fmt.Errorf(
			"legacy single-file template path %q — templates are now directory bundles. "+
				"Move %s to enju_templates/%s/%s and reference it as enju_templates/%s (or the full manifest path)",
			repoRelPath, repoRelPath, base, templateBundleYAML, base)
	}
	info, statErr := os.Stat(filepath.Join(workDir, p))
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", "", fmt.Errorf("template %q not found in workspace — check `enju_list_templates` for available recipes", repoRelPath)
		}
		return "", "", fmt.Errorf("stat template %s: %w", repoRelPath, statErr)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("template path %q is not a directory; templates are directory bundles with %s at their root", repoRelPath, templateBundleYAML)
	}
	return p, p + "/" + templateBundleYAML, nil
}

// InstantiateTemplate loads a template, substitutes the
// supplied param values, and returns the fully-resolved run
// ready for the normal submit path. Errors from
// ParseWithParams (missing required params, type mismatches,
// unknown param names) bubble up with their natural-language
// phrasing so the LLM can forward them to the user.
func (p *Project) InstantiateTemplate(repoRelPath string, params map[string]interface{}) (*enjuYaml.ParsedRun, []byte, error) {
	loaded, err := p.LoadTemplate(repoRelPath)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := enjuYaml.ParseWithParams(loaded.Raw, params)
	if err != nil {
		return nil, nil, err
	}
	return parsed, loaded.Raw, nil
}

// ValidateTemplateParams runs the ParseWithParams path without
// producing a run — useful as a dry-run from the LLM side
// before the user commits to submission. Returns nil if the
// param set is valid; returns the natural-language error
// otherwise.
func (p *Project) ValidateTemplateParams(repoRelPath string, params map[string]interface{}) error {
	_, _, err := p.InstantiateTemplate(repoRelPath, params)
	return err
}
