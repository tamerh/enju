package mcpgit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// Phase H.1 — Template discovery and instantiation on the fat
// client side. Templates are just regular run YAML files that
// happen to live under `templates/` at the root of a project's
// git clone. Any file in that directory is considered a
// template; `.yaml` / `.yml` extensions only.
//
// There is no separate "template" file shape. A file is a
// template when it's placed under `templates/` with a `params:`
// block declaring what the caller must supply. The same file
// can be submitted directly as a run (by passing param values
// inline) or picked up by the LLM from the templates directory
// and instantiated on behalf of a user. Location signals intent,
// not schema.

// TemplateSummary is the lightweight shape returned by
// ListTemplates — enough for an LLM to pick a template from a
// menu without having to parse the full YAML of each one.
type TemplateSummary struct {
	Path        string         `json:"path"`                  // repo-relative, e.g. "templates/gwas.yaml"
	Name        string         `json:"name,omitempty"`        // from `name:` field
	Description string         `json:"description,omitempty"` // from `description:` field
	Params      []ParamSummary `json:"params,omitempty"`      // short param summary
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

// ListTemplates scans the project's `templates/` directory and
// returns a summary for every *.yaml / *.yml file it finds. An
// empty or missing templates/ directory returns an empty slice,
// not an error — "no templates yet" is a normal state.
//
// Files that fail to parse as run YAML are skipped silently in
// the summary (callers can drill in via LoadTemplate for a
// specific file to see the parse error). Rationale: one bad
// template shouldn't crash the menu.
func (p *Project) ListTemplates() ([]TemplateSummary, error) {
	templatesDir := filepath.Join(p.workDir, "templates")
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading templates directory: %w", err)
	}
	var out []TemplateSummary
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("templates", name))
		summary, err := p.templateSummary(rel)
		if err != nil {
			// Skip unreadable / unparseable templates but keep
			// going so the menu still shows everything else.
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

// LoadedTemplate is the full parsed view of a template file,
// returned by LoadTemplate. Carries the raw YAML bytes so the
// caller can submit them unchanged (with a params map) to
// ParseWithParams — i.e. the loader does not mutate the file.
type LoadedTemplate struct {
	Path    string
	Raw     []byte
	Summary TemplateSummary
}

// LoadTemplate reads a single template by its repo-relative
// path (e.g. "templates/gwas.yaml"), parses it to extract the
// summary, and returns both the raw bytes and the summary. The
// raw bytes are what the caller hands to
// yaml.ParseWithParams at instantiation time.
func (p *Project) LoadTemplate(repoRelPath string) (*LoadedTemplate, error) {
	if !strings.HasPrefix(repoRelPath, "templates/") {
		return nil, fmt.Errorf("template path %q must live under templates/", repoRelPath)
	}
	// Block path escapes — this is user-controlled input even
	// though it's read from the local workspace, and a relative
	// `../` could let a caller pull in files from outside the
	// templates directory.
	clean := filepath.ToSlash(filepath.Clean(repoRelPath))
	if strings.Contains(clean, "../") || clean != repoRelPath {
		return nil, fmt.Errorf("template path %q contains disallowed path components", repoRelPath)
	}
	data, err := os.ReadFile(filepath.Join(p.workDir, repoRelPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("template %q not found in workspace — check `enju_list_templates` for available recipes", repoRelPath)
		}
		return nil, fmt.Errorf("reading template %s: %w", repoRelPath, err)
	}
	parsed, err := enjuYaml.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", repoRelPath, err)
	}
	return &LoadedTemplate{
		Path: repoRelPath,
		Raw:  data,
		Summary: TemplateSummary{
			Path:        repoRelPath,
			Name:        parsed.Run.Name,
			Description: parsed.Run.Description,
			Params:      paramSummaries(parsed.Run.Params),
		},
	}, nil
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
