package webui

import (
	"net/http"
	"strconv"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// inboxPage is the data shape for views/inbox.html, used for
// both the global (`/inbox`) and per-project (`/p/{pid}/inbox`)
// surfaces. ScopeProjectID is 0 for global and the project id
// for per-project — the template branches on it to decide
// whether to show the project tag per row (redundant when
// scoped) and which crumb trail to render.
//
// CloneIssues is the list of projects we couldn't enumerate
// (no clone yet, BuildInbox error). Surfaced so the global
// view can tell the user "skipped X projects" rather than
// silently returning fewer rows.
type inboxPage struct {
	pageData
	ScopeProjectID int64
	CloneOK        bool
	Rows           []inboxRowView
	CloneIssues    []inboxCloneIssue
}

// inboxCloneIssue records one project the global inbox handler
// skipped (no local clone, or BuildInbox failed). Lets the UI
// surface "X projects need to be materialized first" rather
// than silently dropping items.
type inboxCloneIssue struct {
	ProjectID   int64
	ProjectName string
	Reason      string
}

// inboxRowView is the per-row shape we hand the template. We
// re-shape inbox.InboxRow into a flat struct so the template
// doesn't need to know about the fatclient/inbox package types.
// ProjectID + ProjectName are populated for global rows;
// templates ignore them when ScopeProjectID matches.
type inboxRowView struct {
	TaskID      string
	Action      string
	ProjectID   int64
	ProjectName string
	Upstream    []inboxUpstreamView
}

type inboxUpstreamView struct {
	TaskID    string
	Action    string
	CommitSHA string // already truncated to 12 chars
	Content   string
}

// handleProjectInbox renders /p/{projectID}/inbox — the
// assignee's waiting work for one project, with each upstream's
// most recent submission inlined.
//
// ProjectClonePresent=false (project has metadata but no local
// clone yet) renders a friendly "materialize first" message
// rather than treating it as an error.
func (s *Server) handleProjectInbox(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	res, err := s.fc.BuildInbox(r.Context(), pid, s.fc.Username())
	if err != nil {
		s.logger.Error("BuildInbox failed", "project_id", pid, "error", err)
		http.Error(w, "failed to build inbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.render(w, r, "inbox.html", inboxPage{
		pageData:       s.commonPageData(),
		ScopeProjectID: pid,
		CloneOK:        res.ProjectClonePresent,
		Rows:           shapeInboxRows(res, pid, ""),
	})
}

// handleGlobalInbox renders /inbox — cross-project list. Walks
// every project the caller is a member of, calls BuildInbox per
// project, merges results.
//
// Cost: one coord call (ListProjects) + N local file walks
// (BuildInbox per project). All local after the first hop, so
// even 20 projects render in well under a second.
//
// Per-project failures (no clone, BuildInbox error) are
// recorded in CloneIssues and surfaced to the user as "skipped
// X projects" rather than silently dropped or fataled.
func (s *Server) handleGlobalInbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := s.fc.Username()
	projects, err := s.fc.ListProjects(ctx)
	if err != nil {
		s.logger.Error("ListProjects failed", "error", err)
		http.Error(w, "failed to list projects: "+err.Error(), http.StatusBadGateway)
		return
	}
	var rows []inboxRowView
	var issues []inboxCloneIssue
	for _, p := range projects {
		res, err := s.fc.BuildInbox(ctx, p.ID, username)
		if err != nil {
			s.logger.Warn("BuildInbox failed for project; skipping",
				"project_id", p.ID, "error", err)
			issues = append(issues, inboxCloneIssue{
				ProjectID: p.ID, ProjectName: p.Name,
				Reason: "build failed",
			})
			continue
		}
		if !res.ProjectClonePresent {
			issues = append(issues, inboxCloneIssue{
				ProjectID: p.ID, ProjectName: p.Name,
				Reason: "no local clone yet",
			})
			continue
		}
		rows = append(rows, shapeInboxRows(res, p.ID, p.Name)...)
	}
	s.render(w, r, "inbox.html", inboxPage{
		pageData:       s.commonPageData(),
		ScopeProjectID: 0,
		CloneOK:        true,
		Rows:           rows,
		CloneIssues:    issues,
	})
}

// shapeInboxRows turns the rows on a *service.InboxResult into
// flat view structs templates consume. Takes the result struct
// (rather than the raw rows slice) so this file never has to
// name the internal-tree types — keeps Rule 5 happy: webui's
// production imports stay {common, fatclient/service}.
//
// pid + pname populate per-row project context for the global
// inbox; pname=="" means scoped (template hides the redundant
// project tag).
func shapeInboxRows(res *service.InboxResult, pid int64, pname string) []inboxRowView {
	if res == nil {
		return nil
	}
	out := make([]inboxRowView, 0, len(res.Rows))
	for _, row := range res.Rows {
		v := inboxRowView{
			TaskID:      row.TaskID,
			Action:      row.Action,
			ProjectID:   pid,
			ProjectName: pname,
		}
		for _, u := range row.Upstream {
			short := u.CommitSHA
			if len(short) > 12 {
				short = short[:12]
			}
			v.Upstream = append(v.Upstream, inboxUpstreamView{
				TaskID:    u.TaskID,
				Action:    u.Action,
				CommitSHA: short,
				Content:   u.Content,
			})
		}
		out = append(out, v)
	}
	return out
}
