package webui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// Artifacts surface — read-only:
//
//   GET /p/{pid}/artifacts                  — list page
//   GET /p/{pid}/artifacts/show/*           — content view
//   GET /p/{pid}/artifacts/history/*        — git log view
//
// All three are pure reads (coord HTTP for list/metadata, local
// git for content and log). No CSRF concerns. The wildcard
// captures the artifact's repo-relative path including slashes.

// artifactsListPage is the data shape for views/artifacts.html.
type artifactsListPage struct {
	pageData
	ProjectID int64
	Branch    string
	Prefix    string
	Items     []artifactRowView
}

// artifactRowView wraps service.ArtifactResponse with a
// derived TrackedLabel string so the template doesn't have to
// dereference *bool. Empty TrackedLabel means "coord didn't
// say" (legacy rows pre-Phase-tracked); template branches on
// the empty string.
type artifactRowView struct {
	service.ArtifactResponse
	TrackedLabel string
}

// artifactDetailPage is the data shape for views/artifact.html.
// Untyped Meta because GetArtifactContent returns the coord
// metadata as a generic map (path/commit/tracked/etc. plus the
// content under "content"). The template reads only a few keys.
type artifactDetailPage struct {
	pageData
	ProjectID int64
	Path      string
	Meta      map[string]interface{}
	Content   string
}

// artifactHistoryPage is the data shape for views/artifact-history.html.
type artifactHistoryPage struct {
	pageData
	ProjectID int64
	Path      string
	Entries   []map[string]interface{}
}

// handleArtifactsList renders /p/{pid}/artifacts. Optional
// ?branch= and ?prefix= filters; empty branch falls back to
// project default server-side.
func (s *Server) handleArtifactsList(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	opts := service.ListArtifactsOpts{
		Branch: strings.TrimSpace(r.URL.Query().Get("branch")),
		Prefix: strings.TrimSpace(r.URL.Query().Get("prefix")),
	}
	items, err := s.fc.ListArtifacts(r.Context(), pid, opts)
	if err != nil {
		s.logger.Error("ListArtifacts failed", "project_id", pid, "error", err)
		http.Error(w, "failed to list artifacts: "+err.Error(), http.StatusBadGateway)
		return
	}
	rows := make([]artifactRowView, 0, len(items))
	for _, a := range items {
		v := artifactRowView{ArtifactResponse: a}
		if a.Tracked != nil {
			if *a.Tracked {
				v.TrackedLabel = "tracked"
			} else {
				v.TrackedLabel = "untracked"
			}
		}
		rows = append(rows, v)
	}
	s.render(w, r, "artifacts.html", artifactsListPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Branch:    opts.Branch,
		Prefix:    opts.Prefix,
		Items:     rows,
	})
}

// handleArtifactView renders /p/{pid}/artifacts/show/{path}.
// Decodes the JSON metadata blob the existing
// Session.GetArtifactContent returns — content, commit_sha,
// last_writer, last_task_id, etc. — into a generic map and
// hands it to the template.
//
// The content can be large (multi-MB images, datasets); we
// don't try to detect binary here, just render as <pre>. A
// future polish step could content-sniff and show images
// inline / offer download for non-text.
func (s *Server) handleArtifactView(w http.ResponseWriter, r *http.Request) {
	pid, path, ok := parseArtifactRoute(w, r)
	if !ok {
		return
	}
	raw, err := s.fc.GetArtifactContent(r.Context(), pid, path)
	if err != nil {
		s.logger.Error("GetArtifactContent failed", "project_id", pid, "path", path, "error", err)
		http.Error(w, "failed to load artifact: "+err.Error(), http.StatusBadGateway)
		return
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(raw, &meta); err != nil {
		s.logger.Error("artifact metadata decode failed", "path", path, "error", err)
		http.Error(w, "failed to decode artifact metadata", http.StatusBadGateway)
		return
	}
	content, _ := meta["content"].(string)
	delete(meta, "content") // content rendered separately, don't dump it twice
	s.render(w, r, "artifact.html", artifactDetailPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Path:      path,
		Meta:      meta,
		Content:   content,
	})
}

// handleArtifactHistory renders /p/{pid}/artifacts/history/{path}.
// Decodes the existing Session.GetArtifactHistory response
// (an array of commit entries with annotations).
func (s *Server) handleArtifactHistory(w http.ResponseWriter, r *http.Request) {
	pid, path, ok := parseArtifactRoute(w, r)
	if !ok {
		return
	}
	raw, err := s.fc.GetArtifactHistory(r.Context(), pid, path)
	if err != nil {
		s.logger.Error("GetArtifactHistory failed", "project_id", pid, "path", path, "error", err)
		http.Error(w, "failed to load artifact history: "+err.Error(), http.StatusBadGateway)
		return
	}
	// History payload is `{"path": ..., "entries": [...]}`.
	var wrap map[string]interface{}
	_ = json.Unmarshal(raw, &wrap)
	var entries []map[string]interface{}
	if e, ok := wrap["entries"].([]interface{}); ok {
		for _, el := range e {
			if m, ok := el.(map[string]interface{}); ok {
				entries = append(entries, m)
			}
		}
	}
	s.render(w, r, "artifact-history.html", artifactHistoryPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Path:      path,
		Entries:   entries,
	})
}

// parseArtifactRoute pulls projectID + the wildcard-captured
// artifact path. 400 on bad input. Mirrors parseRunRoute /
// parseTaskRoute / parseIssueRoute boilerplate.
func parseArtifactRoute(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return 0, "", false
	}
	path := chi.URLParam(r, "*")
	if path == "" {
		http.Error(w, "artifact path is required", http.StatusBadRequest)
		return 0, "", false
	}
	return pid, path, true
}
