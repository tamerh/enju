package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// landingPage is the data shape consumed by views/landing.html.
// Embeds pageData so layout-level fields ({{.Username}}) and
// page-level fields ({{.Projects}}) both read with flat dots.
//
// Error / Submitted carry create-project failure state so the
// inline form repopulates rather than getting wiped. Same
// pattern as the issues list page's file-form failure path.
type landingPage struct {
	pageData
	Projects     []wire.Project
	Materialized []service.MaterializedProject
	Error        string
	Submitted    service.CreateProjectParams
}

// handleLanding is the cross-project landing page (GET /). For
// v1 it lists every project the caller is a member of, plus
// notes which already have a materialized local clone. Future
// fields (inbox count per project, last-touched, recent
// activity) hang off this same shape.
//
// Both calls fan out — ListProjects hits coord, ListMaterialized
// is a local file walk. They're independent; we issue them
// sequentially today but parallelizing is mechanical (errgroup)
// once latency justifies the complexity.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projects, err := s.fc.ListProjects(ctx)
	if err != nil {
		s.logger.Error("ListProjects failed", "error", err)
		http.Error(w, "failed to list projects: "+err.Error(), http.StatusBadGateway)
		return
	}
	materialized, err := s.fc.ListMaterializedProjects()
	if err != nil {
		s.logger.Warn("ListMaterializedProjects failed; rendering with empty list", "error", err)
		materialized = nil
	}
	s.render(w, r, "landing.html", landingPage{
		pageData:     s.commonPageData(),
		Projects:     projects,
		Materialized: materialized,
	})
}

// handleCreateProject is POST /projects. Form fields:
//
//   name (required), description, default_branch
//
// On success redirects to /p/{newID} so the user lands on
// their just-created project. On failure re-renders the
// landing page with the inline form auto-expanded, error
// banner, and form values preserved.
//
// Custom path / remote URL flows are not in the v1 UI surface
// — power users wanting those keep using MCP.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	params := service.CreateProjectParams{
		Name:          strings.TrimSpace(r.FormValue("name")),
		Description:   r.FormValue("description"),
		DefaultBranch: strings.TrimSpace(r.FormValue("default_branch")),
		Path:          strings.TrimSpace(r.FormValue("path")),
		RemoteURL:     strings.TrimSpace(r.FormValue("remote_url")),
	}
	if params.Name == "" {
		s.renderCreateProjectError(w, r, params, "name is required")
		return
	}
	res, err := s.fc.CreateProject(r.Context(), params)
	if err != nil {
		s.logger.Error("CreateProject failed", "name", params.Name, "error", err)
		s.renderCreateProjectError(w, r, params, "create failed: "+err.Error())
		return
	}
	if res == nil || res.ProjectID == 0 {
		s.renderCreateProjectError(w, r, params, "create returned no project id")
		return
	}
	target := "/p/" + strconv.FormatInt(res.ProjectID, 10)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// renderCreateProjectError re-renders the landing page with
// the inline create-project form pre-expanded, the error
// banner showing, and the form fields repopulated. Same
// failure-path pattern as the file-issue form on the issues
// list page.
func (s *Server) renderCreateProjectError(w http.ResponseWriter, r *http.Request, submitted service.CreateProjectParams, msg string) {
	projects, err := s.fc.ListProjects(r.Context())
	if err != nil {
		s.logger.Warn("ListProjects failed during create-error render; showing empty list",
			"error", err)
		projects = nil
	}
	materialized, _ := s.fc.ListMaterializedProjects()
	s.render(w, r, "landing.html", landingPage{
		pageData:     s.commonPageData(),
		Projects:     projects,
		Materialized: materialized,
		Error:        msg,
		Submitted:    submitted,
	})
}
