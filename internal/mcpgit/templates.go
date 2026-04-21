package mcpgit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/engine"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// Template discovery and instantiation on the fat-client side.
// A template is a DIRECTORY containing an `enju.yaml` manifest
// at its root:
//
//   enju/templates/
//     gwas-analysis/
//       enju.yaml           ← the run definition (required)
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
// Loose `.yaml` files directly under a templates root are not
// recognized — they'd be ambiguous about whether they own the
// surrounding directory. Templates must live in their own
// folder with an `enju.yaml` inside.
//
// Discovery path: engine.DefaultTemplatesDir (enju/templates/)
// by default, overridable via enju/conf.yaml's `templates:` list
// for monorepos with existing config/ or workflows/ conventions.

// TemplateSummary is the lightweight shape returned by
// ListTemplates — enough for an LLM to pick a template from a
// menu without having to parse the full YAML of each one.
// When a template file fails to parse, the summary still shows
// up in the list with ParseError populated; the caller can see
// the path + the reason without having to drill in via
// describe_template to discover why it's missing.
type TemplateSummary struct {
	Path        string         `json:"path"`                   // repo-relative, e.g. "enju/templates/gwas/enju.yaml"
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

// ListTemplates scans every configured template root for
// directory-shaped bundles and returns a summary for each one.
// Roots come from enju/conf.yaml's `templates:` list if present,
// otherwise fall back to engine.DefaultTemplatesDir. Empty or
// missing roots are a normal state, not an error — just
// contribute zero entries.
//
// Directories without an enju.yaml are skipped silently
// (scratch folders, README-only dirs, etc). Bundles whose
// enju.yaml fails to parse are surfaced with ParseError
// populated — a visible "unparseable" menu entry beats a
// silent drop that makes the author think the scan missed
// their template.
//
// Loose `.yaml` files directly under a templates root are NOT
// discovered. If any are found, they're surfaced as a single
// migration-hint entry in the result so the author knows to
// move them.
func (p *Project) ListTemplates() ([]TemplateSummary, error) {
	roots, err := p.templateRoots()
	if err != nil {
		return nil, err
	}
	var out []TemplateSummary
	seen := make(map[string]bool)
	for _, root := range roots {
		items, err := p.scanTemplateRoot(root)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if seen[it.Path] {
				continue // same bundle reachable from overlapping roots
			}
			seen[it.Path] = true
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// scanTemplateRoot walks one template root and returns the
// bundles it finds. Split out so ListTemplates can fan over
// multiple configured roots.
func (p *Project) scanTemplateRoot(root string) ([]TemplateSummary, error) {
	absRoot := filepath.Join(p.workDir, root)
	entries, err := os.ReadDir(absRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading templates directory %s: %w", root, err)
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
				legacyRel := filepath.ToSlash(filepath.Join(root, name))
				base := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
				out = append(out, TemplateSummary{
					Path: legacyRel,
					ParseError: fmt.Sprintf(
						"legacy single-file template layout — move %s to %s/%s/%s",
						legacyRel, root, base, engine.BundleManifestName),
				})
			}
			continue
		}
		// Directory entry → check for the bundle manifest at its root.
		manifest := filepath.Join(absRoot, name, engine.BundleManifestName)
		if _, statErr := os.Stat(manifest); statErr != nil {
			// Missing manifest → not a template bundle.
			// Skip silently; directories might exist for
			// other purposes (shared scratch, etc).
			continue
		}
		rel := filepath.ToSlash(filepath.Join(root, name, engine.BundleManifestName))
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
	Path      string // e.g. "enju/templates/gwas/enju.yaml"
	BundleDir string // e.g. "enju/templates/gwas"
	Raw       []byte
	Summary   TemplateSummary
}

// ReadBundleFiles walks the bundle directory and returns
// every regular file as a FileWrite, with repo-relative paths
// rebased to a target directory (typically the per-run
// snapshot location produced by engine.RunTemplateSnapshotDir).
// Used by handleCreateRun to commit the bundle into the run's
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
//   - its directory path (e.g. "enju/templates/gwas-analysis")
//   - the full path to its manifest (e.g.
//     "enju/templates/gwas-analysis/enju.yaml")
//
// Both forms resolve to the same bundle; the loader picks up
// the enju.yaml at the directory root. The resolved YAML path
// is what shows up in the returned LoadedTemplate.Path;
// BundleDir carries the surrounding directory so callers doing
// snapshot-on-instantiate can enumerate all the files in the
// bundle.
func (p *Project) LoadTemplate(repoRelPath string) (*LoadedTemplate, error) {
	// Block path escapes — user-controlled input even though
	// it's read from the local workspace, and a `../` could
	// let a caller pull files from outside the configured
	// templates roots.
	clean := filepath.ToSlash(filepath.Clean(repoRelPath))
	if strings.Contains(clean, "../") || clean != repoRelPath {
		return nil, fmt.Errorf("template path %q contains disallowed path components", repoRelPath)
	}
	if err := p.assertUnderTemplatesRoot(repoRelPath); err != nil {
		return nil, err
	}

	bundleDir, manifestPath, err := p.resolveBundlePaths(repoRelPath)
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

// assertUnderTemplatesRoot confirms the caller-supplied path
// lives under at least one of the configured templates roots.
// Cheap containment check — the individual file reads still
// validate existence — but it keeps the error message
// consistent ("must live under <configured roots>") when a
// caller types a typo'd path.
func (p *Project) assertUnderTemplatesRoot(repoRelPath string) error {
	roots, err := p.templateRoots()
	if err != nil {
		return err
	}
	for _, r := range roots {
		if repoRelPath == r || strings.HasPrefix(repoRelPath, r+"/") {
			return nil
		}
	}
	return fmt.Errorf("template path %q must live under one of: %s", repoRelPath, strings.Join(roots, ", "))
}

// resolveBundlePaths maps a caller-supplied template reference
// to the (bundleDir, manifestPath) pair the loader uses, both
// as repo-relative slash-paths. Accepts:
//
//   - "<root>/NAME"                → dir form
//   - "<root>/NAME/enju.yaml"      → manifest form
//   - anything else ending in .yaml/.yml → legacy single-file,
//                                          rejected with migration hint
func (p *Project) resolveBundlePaths(repoRelPath string) (bundleDir, manifestPath string, err error) {
	pth := strings.TrimSuffix(repoRelPath, "/")
	// Manifest form: ends in /<BundleManifestName>.
	if strings.HasSuffix(pth, "/"+engine.BundleManifestName) {
		bundleDir = strings.TrimSuffix(pth, "/"+engine.BundleManifestName)
		// Disallow manifest sitting directly in the templates
		// root (bundleDir would equal the root itself).
		roots, rerr := p.templateRoots()
		if rerr != nil {
			return "", "", rerr
		}
		for _, r := range roots {
			if bundleDir == r {
				return "", "", fmt.Errorf("template manifest must live inside a bundle subdirectory, e.g. %s/NAME/%s", r, engine.BundleManifestName)
			}
		}
		return bundleDir, pth, nil
	}
	// Dir form: must be a directory with the manifest inside.
	if strings.HasSuffix(pth, ".yaml") || strings.HasSuffix(pth, ".yml") {
		// Legacy single-file reference — emit a migration hint.
		base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(pth), ".yaml"), ".yml")
		parentDir := filepath.ToSlash(filepath.Dir(pth))
		return "", "", fmt.Errorf(
			"legacy single-file template path %q — templates are now directory bundles. "+
				"Move %s to %s/%s/%s and reference it as %s/%s (or the full manifest path)",
			repoRelPath, repoRelPath, parentDir, base, engine.BundleManifestName, parentDir, base)
	}
	info, statErr := os.Stat(filepath.Join(p.workDir, pth))
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", "", fmt.Errorf("template %q not found in workspace — check `enju_list_templates` for available recipes", repoRelPath)
		}
		return "", "", fmt.Errorf("stat template %s: %w", repoRelPath, statErr)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("template path %q is not a directory; templates are directory bundles with %s at their root", repoRelPath, engine.BundleManifestName)
	}
	return pth, pth + "/" + engine.BundleManifestName, nil
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
