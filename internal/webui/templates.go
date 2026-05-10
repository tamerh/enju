package webui

import (
	"fmt"
	"html/template"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
)

// templateSet wraps the parsed page templates plus a
// production/dev mode toggle. In production templates are
// parsed once at startup from the embedded FS; in dev mode
// every render() reparses from disk so edits show on save.
//
// One *template.Template per page (landing.html, project.html,
// …). Each page is built by Clone()ing a base tree (layout +
// partials) and Parse()ing the page file into the clone — so
// each page has its OWN set of {{define "main"}} /
// {{define "title"}} blocks without colliding with sibling
// pages. This is the canonical Go html/template inheritance
// pattern; a single shared namespace would let the last-parsed
// page silently overwrite earlier pages' overrides (and the
// failure mode is "wrong content rendered with no error").
type templateSet struct {
	mu    sync.RWMutex
	pages map[string]*template.Template

	dev  bool
	fsys fs.FS
}

// newTemplateSet builds a templateSet for the given mode.
func newTemplateSet(dev bool, fsys fs.FS) (*templateSet, error) {
	ts := &templateSet{dev: dev, fsys: fsys}
	if !dev {
		if err := ts.rebuild(); err != nil {
			return nil, err
		}
	}
	return ts, nil
}

// rebuild parses the views FS into one *template.Template per
// page. Layout files are parsed into a base tree; each page is
// then a Clone() + parse of its own file. Partials parsed at
// the layout layer become globally available.
func (t *templateSet) rebuild() error {
	matches, err := fs.Glob(t.fsys, "*.html")
	if err != nil {
		return fmt.Errorf("template glob: %w", err)
	}
	if len(matches) == 0 {
		// Nested layout (caller passed embedded FS without
		// sub-rooting at views/).
		matches, err = fs.Glob(t.fsys, "views/*.html")
		if err != nil {
			return fmt.Errorf("template glob (nested): %w", err)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no templates found in fs")
	}

	// Split into layouts and pages. Layouts (and future
	// partials/) parse into the base; pages parse into clones.
	var layouts, pages []string
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasPrefix(base, "layout") {
			layouts = append(layouts, m)
		} else {
			pages = append(pages, m)
		}
	}
	if len(layouts) == 0 {
		return fmt.Errorf("no layout templates found (expected layout*.html)")
	}

	base := template.New("")
	for _, name := range layouts {
		body, err := fs.ReadFile(t.fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := base.Parse(string(body)); err != nil {
			return fmt.Errorf("parse layout %s: %w", name, err)
		}
	}

	out := make(map[string]*template.Template, len(pages))
	for _, name := range pages {
		body, err := fs.ReadFile(t.fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		clone, err := base.Clone()
		if err != nil {
			return fmt.Errorf("clone base for %s: %w", name, err)
		}
		// Parse the page body into the cloned tree. Page-level
		// {{define "main"}} / {{define "title"}} land here and
		// override the layout's {{block ...}} defaults inside
		// THIS clone only — no cross-page leakage.
		if _, err := clone.Parse(string(body)); err != nil {
			return fmt.Errorf("parse page %s: %w", name, err)
		}
		// Boot-time guard: every page MUST define "main".
		// Without this, a page that omits {{define "main"}}
		// renders silently empty (the layout's {{block "main"}}
		// default is "(no content)"). Failing here turns a
		// silent footgun into a loud build/boot error.
		if clone.Lookup("main") == nil {
			return fmt.Errorf("page %s: missing {{define \"main\"}} block", name)
		}
		out[filepath.Base(name)] = clone
	}

	t.pages = out
	return nil
}

// lookup returns the parsed *template.Template for a page file
// (e.g. "landing.html"). Reparses in dev mode. The returned
// template carries a private clone of the layout tree, so
// ExecuteTemplate("layout", data) renders using the page's own
// overrides without seeing other pages' defines.
func (t *templateSet) lookup(name string) (*template.Template, error) {
	if t.dev {
		t.mu.Lock()
		defer t.mu.Unlock()
		if err := t.rebuild(); err != nil {
			return nil, err
		}
	} else {
		t.mu.RLock()
		defer t.mu.RUnlock()
	}
	tmpl, ok := t.pages[name]
	if !ok {
		return nil, fmt.Errorf("template %q not found", name)
	}
	return tmpl, nil
}
