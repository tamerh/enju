package webui

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// eventsPage is the data shape for views/events.html, used for
// both /p/{pid}/events (scoped) and /events (global). When
// ScopeProjectID is 0, the page is global and rows carry their
// own ProjectID/ProjectName for the per-row project tag.
type eventsPage struct {
	pageData
	ScopeProjectID int64
	Events         []eventRowView
	Limit          int
	ForMe          bool
}

// eventRowView wraps service.EventRow with project context for
// the global view. Templates read flat dot fields.
type eventRowView struct {
	service.EventRow
	ProjectID   int64
	ProjectName string
}

// handleEvents renders /p/{projectID}/events — paginated
// projection of the per-project event log. Default limit 50;
// caller can pass ?limit=N up to 1000. ?for_me=true filters
// post-fetch to events where the calling citizen is named
// (citizen or assign_to). Mirrors the mcphandlers pattern;
// the limitations documented in the MCP tool description
// apply equally here (events on tasks I authored but didn't
// claim, project-wide events without a citizen, won't surface).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 50, 1000)
	forMe := r.URL.Query().Get("for_me") == "true"

	rows, err := s.fc.ListEvents(r.Context(), pid, service.ListEventsOpts{Limit: limit})
	if err != nil {
		s.logger.Error("ListEvents failed", "project_id", pid, "error", err)
		http.Error(w, "failed to list events: "+err.Error(), http.StatusBadGateway)
		return
	}
	if forMe {
		rows = filterEventsForMe(rows, s.fc.Username())
	}
	views := make([]eventRowView, 0, len(rows))
	for _, e := range rows {
		views = append(views, eventRowView{EventRow: e, ProjectID: pid})
	}
	s.render(w, r, "events.html", eventsPage{
		pageData:       s.commonPageData(),
		ScopeProjectID: pid,
		Events:         views,
		Limit:          limit,
		ForMe:          forMe,
	})
}

// handleGlobalEvents renders /events — cross-project event log.
// Walks ListProjects + ListEvents per project, merges, sorts
// newest-first by timestamp, caps at limit.
//
// Each project has its own monotone seq counter so cross-project
// seq compare is meaningless. Timestamp ordering is the right
// fold for a feed view.
func (s *Server) handleGlobalEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := s.fc.Username()
	limit := parseLimit(r.URL.Query().Get("limit"), 50, 1000)
	forMe := r.URL.Query().Get("for_me") == "true"

	projects, err := s.fc.ListProjects(ctx)
	if err != nil {
		s.logger.Error("ListProjects failed", "error", err)
		http.Error(w, "failed to list projects: "+err.Error(), http.StatusBadGateway)
		return
	}

	var merged []eventRowView
	for _, p := range projects {
		rows, err := s.fc.ListEvents(ctx, p.ID, service.ListEventsOpts{Limit: limit})
		if err != nil {
			s.logger.Warn("ListEvents failed for project; skipping",
				"project_id", p.ID, "error", err)
			continue
		}
		if forMe {
			rows = filterEventsForMe(rows, username)
		}
		for _, e := range rows {
			merged = append(merged, eventRowView{
				EventRow: e, ProjectID: p.ID, ProjectName: p.Name,
			})
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Timestamp.After(merged[j].Timestamp)
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	s.render(w, r, "events.html", eventsPage{
		pageData:       s.commonPageData(),
		ScopeProjectID: 0,
		Events:         merged,
		Limit:          limit,
		ForMe:          forMe,
	})
}

// handleNotificationsRedirect maps the legacy
// /p/{pid}/notifications URL onto /p/{pid}/events?for_me=true.
// Notifications were consolidated into events with a filter; the
// redirect keeps shared bookmarks and old chat URLs working.
func (s *Server) handleNotificationsRedirect(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "projectID")
	http.Redirect(w, r, "/p/"+pid+"/events?for_me=true", http.StatusFound)
}

// filterEventsForMe keeps events where username appears as the
// actor (citizen) or as an assignee (assign_to, hoisted out of
// metadata at coord emit-time). Same predicate the MCP
// recent_events tool applies; documented limitations match.
func filterEventsForMe(events []service.EventRow, username string) []service.EventRow {
	if username == "" {
		return events
	}
	out := events[:0]
	for _, e := range events {
		if e.Citizen == username || e.AssignTo == username {
			out = append(out, e)
		}
	}
	return out
}

// parseLimit parses a ?limit= query value, applying default and
// max ceilings. Invalid input falls back to def silently — the
// limit is a UX knob, not a contract worth 400-erroring over.
func parseLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
