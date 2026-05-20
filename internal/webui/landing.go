package webui

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
	Rows         []projectRow
	ProjectCount int
	Materialized []service.MaterializedProject
	Error        string
	Submitted    service.CreateProjectParams
	// Archived switches landing.html to the archived-only view:
	// heading + title change, the create-project form and the
	// local-clone count are suppressed, and the footer link
	// points back to the active list instead of forward to the
	// archived one. Rows then holds the archived set.
	Archived bool
}

// projectRow is the table-row view of a project: the raw
// wire.Project plus a precomputed display string for the "last
// activity" column. Sort key is also derived here so the
// template stays branch-free.
type projectRow struct {
	wire.Project
	// ActivityAt is max(LastActivityAt, CreatedAt) — the wire
	// contract says LastActivityAt is zero on older coords / for
	// projects with no activity since the column was added, and
	// callers must floor to CreatedAt.
	ActivityAt time.Time
	// ActivityRel is the short human label ("3m ago", "2d ago",
	// "Apr 2"); ActivityISO is the full timestamp for the
	// title= tooltip. Both honor ActivityAt's floored value.
	ActivityRel string
	ActivityISO string
}

// buildProjectRows sorts the projects by last activity (newest
// first) and precomputes the display fields for the table.
func buildProjectRows(projects []wire.Project, now time.Time) []projectRow {
	rows := make([]projectRow, len(projects))
	for i, p := range projects {
		at := p.LastActivityAt
		if at.IsZero() || p.CreatedAt.After(at) {
			at = p.CreatedAt
		}
		rows[i] = projectRow{
			Project:     p,
			ActivityAt:  at,
			ActivityRel: relativeTime(now, at),
			ActivityISO: at.Format(time.RFC3339),
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].ActivityAt.After(rows[j].ActivityAt)
	})
	return rows
}

// relativeTime renders a compact "how long ago" label. Granular
// for fresh events (the operator-useful range), coarsening to
// date-only past a week. Zero time → "—".
func relativeTime(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return t.Format("Jan 2")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
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
		Rows:         buildProjectRows(projects, time.Now()),
		ProjectCount: len(projects),
		Materialized: materialized,
	})
}

// handleArchivedProjects is GET /archived — the archived-only
// roster. Reuses landing.html in Archived mode. Pure read; no
// local-clone walk (archived projects aren't part of the
// everyday "what's on disk" view).
func (s *Server) handleArchivedProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.fc.ListArchivedProjects(r.Context())
	if err != nil {
		s.logger.Error("ListArchivedProjects failed", "error", err)
		s.writeFetchError(w, "archived projects", err)
		return
	}
	s.render(w, r, "landing.html", landingPage{
		pageData:     s.commonPageData(),
		Rows:         buildProjectRows(projects, time.Now()),
		ProjectCount: len(projects),
		Archived:     true,
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
		Rows:         buildProjectRows(projects, time.Now()),
		ProjectCount: len(projects),
		Materialized: materialized,
		Error:        msg,
		Submitted:    submitted,
	})
}
