package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// Templates surface — read (list + describe) and write (create
// run from template). Templates live in the project repo at
// enju/templates/<bundle>/. A bundle has a manifest enju.yaml
// plus optional scripts/data/.
//
//   GET  /p/{projectID}/templates       — list bundles
//   GET  /p/{projectID}/templates/*     — describe one (path is the
//                                         repo-relative path, slashes
//                                         intact via chi wildcard)
//   POST /p/{projectID}/templates/*/run — create a run; form carries
//                                         params + optional branch
//
// The detail page hosts the create-run form — the user picks
// values for declared params and submits, the handler calls
// Session.CreateRunFromTemplate which prepares the bundle,
// posts to coord, and freezes a snapshot. Success → redirect
// to /p/{pid}/r/{seq}.

// templatesListPage is the data shape for views/templates.html.
type templatesListPage struct {
	pageData
	ProjectID int64
	Templates []service.TemplateSummary
}

// templateDetailPage is the data shape for views/template.html.
// Error / Submitted carry create-run failure state so the form
// repopulates rather than getting wiped (same pattern as the
// issues list page's file-form failure path).
type templateDetailPage struct {
	pageData
	ProjectID    int64
	Path         string
	Template     *service.LoadedTemplate
	Error        string
	SubmittedBranch string
	SubmittedParams map[string]string
}

// handleTemplatesList renders /p/{pid}/templates.
func (s *Server) handleTemplatesList(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	tmpls, err := s.fc.ListTemplates(r.Context(), pid)
	if err != nil {
		s.logger.Error("ListTemplates failed", "project_id", pid, "error", err)
		http.Error(w, "failed to list templates: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.render(w, r, "templates.html", templatesListPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Templates: tmpls,
	})
}

// handleTemplateDetail renders /p/{pid}/templates/{path...}.
// The wildcard captures the repo-relative path
// (e.g. "enju/templates/llm_eval"). DescribeTemplate accepts
// either the bundle dir or the manifest path.
func (s *Server) handleTemplateDetail(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	path := chi.URLParam(r, "*")
	if path == "" {
		http.Error(w, "template path is required", http.StatusBadRequest)
		return
	}
	loaded, err := s.fc.DescribeTemplate(r.Context(), pid, path)
	if err != nil {
		s.logger.Error("DescribeTemplate failed", "project_id", pid, "path", path, "error", err)
		http.Error(w, "failed to load template: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.render(w, r, "template.html", templateDetailPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Path:      path,
		Template:  loaded,
	})
}

// handleCreateRunFromTemplate is POST
// /p/{pid}/templates/{path...}/run. Form carries:
//
//   branch   — optional run branch override
//   p_<name> — one entry per declared param (e.g. p_disease=tp53)
//
// Multi-value params (list<string>) come through as comma-
// separated strings; we split + trim. On success redirects to
// /p/{pid}/r/{seq}; on failure re-renders the detail page with
// the form repopulated.
func (s *Server) handleCreateRunFromTemplate(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	path := chi.URLParam(r, "*")
	if path == "" {
		http.Error(w, "template path is required", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	branch := strings.TrimSpace(r.FormValue("branch"))

	// Re-fetch the template to know its declared params shape.
	// Required to (a) coerce list<string> values from the
	// comma-separated input, (b) repopulate on error.
	loaded, lerr := s.fc.DescribeTemplate(r.Context(), pid, path)
	if lerr != nil {
		s.logger.Error("DescribeTemplate failed during create-run", "project_id", pid, "path", path, "error", lerr)
		http.Error(w, "failed to load template: "+lerr.Error(), http.StatusBadGateway)
		return
	}

	submitted := make(map[string]string)
	params := make(map[string]interface{})
	if loaded != nil {
		for _, p := range loaded.Summary.Params {
			raw := r.FormValue("p_" + p.Name)
			submitted[p.Name] = raw
			if raw == "" {
				continue
			}
			params[p.Name] = coerceParamValue(p.Type, raw)
		}
	}

	authorName, authorEmail := s.fc.CommitAuthor(r.Context())
	res, err := s.fc.CreateRunFromTemplate(r.Context(), pid, path, params, branch, authorName, authorEmail)
	if err != nil {
		s.logger.Error("CreateRunFromTemplate failed", "project_id", pid, "path", path, "error", err)
		s.render(w, r, "template.html", templateDetailPage{
			pageData:        s.commonPageData(),
			ProjectID:       pid,
			Path:            path,
			Template:        loaded,
			Error:           "create run failed: " + err.Error(),
			SubmittedBranch: branch,
			SubmittedParams: submitted,
		})
		return
	}

	seq := service.RunSeqFromCreateResponse(res.CoordResponse)
	if seq == 0 {
		// Couldn't decode seq; fall back to runs list.
		target := "/p/" + strconv.FormatInt(pid, 10) + "/runs"
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", target)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	target := "/p/" + strconv.FormatInt(pid, 10) + "/r/" + strconv.Itoa(seq)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// coerceParamValue converts the raw form-string value into the
// shape the coord's ParseWithParams expects for the declared
// param type. Strict-mode: unknown types pass through as
// string (coord rejects with a typed error).
//
// list<string>: split on commas, trim per-item; empty items
// dropped. Best-effort — param values are user input and we
// don't try to parse JSON-array strings or anything fancy here.
func coerceParamValue(declaredType, raw string) interface{} {
	switch declaredType {
	case "int":
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			return n
		}
		return raw
	case "bool":
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "yes", "1":
			return true
		case "false", "no", "0":
			return false
		}
		return raw
	case "list<string>":
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	default: // string / unknown
		return raw
	}
}
