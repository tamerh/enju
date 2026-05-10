package webui

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// projectPage is the data shape consumed by views/project.html.
// Embeds pageData for {{.Username}}; carries the project detail
// (name, description, members, etc.) and the run list shown
// inline as the page's primary content.
//
// Tabs (Inbox / Runs / Members / Settings) are deferred to the
// iteration that ships their own pages — until then this page
// is the project's single overview surface.
type projectPage struct {
	pageData
	Project *service.ProjectDetail
	Runs    []wire.Run
}

// handleProjectView renders /p/{projectID} — project overview
// with header + members + runs list. Two FatClient calls
// (GetProject, ListRuns) issued sequentially. Parallelizing
// is mechanical (errgroup) when latency justifies it.
//
// Bad project ID parses → 400. Coord 4xx (not a member, not
// found) surfaces as 502 today; refining to 404/403 with
// typed errors from FatClient is a follow-up.
func (s *Server) handleProjectView(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	proj, err := s.fc.GetProject(ctx, pid)
	if err != nil {
		s.logger.Error("GetProject failed", "project_id", pid, "error", err)
		http.Error(w, "failed to load project: "+err.Error(), http.StatusBadGateway)
		return
	}
	if proj == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	runs, err := s.fc.ListRuns(ctx, pid)
	if err != nil {
		s.logger.Warn("ListRuns failed; rendering with empty list",
			"project_id", pid, "error", err)
		runs = nil
	}
	// Newest first — most-recent runs are what users care
	// about when they land on the project page. Sorted
	// client-side so we don't depend on the coord's ordering
	// promise.
	sort.Slice(runs, func(i, j int) bool { return runs[i].Seq > runs[j].Seq })
	s.render(w, r, "project.html", projectPage{
		pageData: s.commonPageData(),
		Project:  proj,
		Runs:     runs,
	})
}
