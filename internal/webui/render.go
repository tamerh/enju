package webui

import (
	"net/http"
)

// pageData carries the common header fields every page renders.
// Page-specific structs embed it so templates can access both
// the common fields ({{.Username}}) and the page payload
// ({{.Projects}}) with flat dot syntax — no .Data.X tax for
// template authors.
//
// TODO(common-chrome): when the layout grows shared header
// chrome (active-project name, breadcrumbs synthesized
// server-side, refresh-button state) those fields land here so
// every embedding page gets them for free.
type pageData struct {
	Username string
	// AssetVer is the static-asset content hash, appended as
	// ?v= to app.css/app.js in the layout so the immutable-1y
	// browser cache busts automatically on a rebuild.
	AssetVer string
}

// commonPageData returns a populated pageData for the current
// caller. Handlers embed the result in their page struct:
//
//	type landingPage struct {
//	    pageData
//	    Projects []wire.Project
//	}
//
//	data := landingPage{
//	    pageData: s.commonPageData(),
//	    Projects: projects,
//	}
//	s.render(w, r, "landing.html", data)
func (s *Server) commonPageData() pageData {
	return pageData{Username: s.fc.Username(), AssetVer: s.assetVer}
}

// renderFull renders the full layout shell with the page's
// "main" block embedded inside it. Used when HX-Request is
// absent — the user navigated via address bar, link click,
// or refresh.
//
// All templates parse into one tree, so any lookup returns the
// full namespace: ExecuteTemplate("layout", ...) finds the
// page's overrides of {{define "main"}} and {{define "title"}}
// automatically.
func (s *Server) renderFull(w http.ResponseWriter, page string, data any) {
	tmpl, err := s.tmpl.lookup(page)
	if err != nil {
		s.logger.Error("page template lookup failed", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.dev {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("execute layout failed", "page", page, "error", err)
	}
}

// renderPartial renders just the page's "main" block (the
// HTMX-swapped content). Used when HX-Request is present —
// the layout shell is already in the DOM.
func (s *Server) renderPartial(w http.ResponseWriter, page string, data any) {
	tmpl, err := s.tmpl.lookup(page)
	if err != nil {
		s.logger.Error("page template lookup failed", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.dev {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if err := tmpl.ExecuteTemplate(w, "main", data); err != nil {
		s.logger.Error("execute partial failed", "page", page, "error", err)
	}
}

// render dispatches to renderFull or renderPartial based on the
// HX-Request header. When HTMX-driven, the partial is enough;
// otherwise we render the full page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	if r.Header.Get("HX-Request") == "true" {
		s.renderPartial(w, page, data)
		return
	}
	s.renderFull(w, page, data)
}
