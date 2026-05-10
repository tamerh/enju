package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// Issues surface (per-project for v1, no cross-project view).
//
//   GET  /p/{projectID}/issues             — list page (with file form)
//   GET  /p/{projectID}/issues/{issueSeq}  — detail page
//   POST /p/{projectID}/issues             — file new (CSRF-gated)
//   POST /p/{projectID}/issues/{issueSeq}/triage  — re-severity
//   POST /p/{projectID}/issues/{issueSeq}/close   — close / wontfix
//
// Read pages are GET (not CSRF-checked); writes inherit the
// existing same-origin middleware.

// issuesListPage is the data shape for views/issues.html.
//
// Error + Submitted carry the file-form failure state when the
// user's POST to /issues was rejected. On success the user is
// redirected to the new detail page so this struct is never
// rendered with both — Error is empty on the GET render too.
// On failure we re-render the list page with:
//   - Error set so the banner shows + form auto-expands
//   - Submitted populated so the form repopulates with what
//     they typed (don't lose the body they wrote)
type issuesListPage struct {
	pageData
	ProjectID int64
	Issues    []service.IssueResponse
	Filter    string
	Error     string
	Submitted service.FileIssueParams
}

// issueDetailPage is the data shape for views/issue.html.
type issueDetailPage struct {
	pageData
	ProjectID int64
	Issue     *service.IssueResponse
}

// handleIssuesList renders /p/{pid}/issues. Optional ?status=
// filter narrows the list. Default = all (no filter).
func (s *Server) handleIssuesList(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	filter := r.URL.Query().Get("status")
	opts := service.IssueListOpts{Limit: 200}
	if filter != "" {
		opts.Status = filter
	}
	issues, err := s.fc.ListIssues(r.Context(), pid, opts)
	if err != nil {
		s.logger.Error("ListIssues failed", "project_id", pid, "error", err)
		http.Error(w, "failed to list issues: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.render(w, r, "issues.html", issuesListPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Issues:    issues,
		Filter:    filter,
	})
}

// handleIssueView renders /p/{pid}/issues/{seq}.
func (s *Server) handleIssueView(w http.ResponseWriter, r *http.Request) {
	pid, seq, ok := parseIssueRoute(w, r)
	if !ok {
		return
	}
	issue, err := s.fc.GetIssue(r.Context(), pid, seq)
	if err != nil {
		s.logger.Error("GetIssue failed", "project_id", pid, "seq", seq, "error", err)
		http.Error(w, "failed to load issue: "+err.Error(), http.StatusBadGateway)
		return
	}
	if issue == nil {
		http.Error(w, "issue not found", http.StatusNotFound)
		return
	}
	s.render(w, r, "issue.html", issueDetailPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Issue:     issue,
	})
}

// handleFileIssue is POST /p/{pid}/issues. Form fields:
//
//   title (required), body, severity, found_in_run_seq, found_in_task_id
//
// On success redirects to the new issue's detail page.
// On failure (validation OR coord rejection) re-renders the
// list page with the error banner and the form repopulated so
// the user doesn't lose what they typed.
func (s *Server) handleFileIssue(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	params := service.FileIssueParams{
		Title:         strings.TrimSpace(r.FormValue("title")),
		Body:          r.FormValue("body"),
		Severity:      strings.TrimSpace(r.FormValue("severity")),
		FoundInTaskID: strings.TrimSpace(r.FormValue("found_in_task_id")),
	}
	if rs := strings.TrimSpace(r.FormValue("found_in_run_seq")); rs != "" {
		if n, err := strconv.Atoi(rs); err == nil && n > 0 {
			params.FoundInRunSeq = n
		}
	}

	if params.Title == "" {
		s.renderFileIssueError(w, r, pid, params, "title is required")
		return
	}
	res, err := s.fc.FileIssue(r.Context(), pid, params)
	if err != nil {
		s.logger.Error("FileIssue failed", "project_id", pid, "error", err)
		s.renderFileIssueError(w, r, pid, params, "file failed: "+err.Error())
		return
	}
	target := "/p/" + strconv.FormatInt(pid, 10) + "/issues/" + strconv.Itoa(res.Seq)
	// HX-Request: tell HTMX to push the new URL via response
	// header so the user lands on the detail page; otherwise
	// 303.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// renderFileIssueError re-renders the issues list page with the
// file form pre-expanded, the error banner showing, and the
// form fields repopulated with what the user typed. This is
// the failure path of handleFileIssue — we render the same
// surface the user came from, just enriched with the error +
// their previous input, instead of dumping a bare banner that
// destroys context.
//
// Best-effort fetches: ListIssues failure here just renders an
// empty list — the form + error banner are what matter.
func (s *Server) renderFileIssueError(w http.ResponseWriter, r *http.Request, pid int64, submitted service.FileIssueParams, msg string) {
	issues, err := s.fc.ListIssues(r.Context(), pid, service.IssueListOpts{Limit: 200})
	if err != nil {
		s.logger.Warn("ListIssues failed during file-error render; showing empty",
			"project_id", pid, "error", err)
		issues = nil
	}
	s.render(w, r, "issues.html", issuesListPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Issues:    issues,
		Error:     msg,
		Submitted: submitted,
	})
}

// handleTriageIssue is POST /p/{pid}/issues/{seq}/triage.
// Form: severity (optional). Re-renders detail page on success.
func (s *Server) handleTriageIssue(w http.ResponseWriter, r *http.Request) {
	pid, seq, ok := parseIssueRoute(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	severity := strings.TrimSpace(r.FormValue("severity"))
	issue, err := s.fc.TriageIssue(r.Context(), pid, seq, severity)
	if err != nil {
		s.logger.Error("TriageIssue failed", "project_id", pid, "seq", seq, "error", err)
		s.renderActionError(w, r, "triage failed: "+err.Error())
		return
	}
	s.render(w, r, "issue.html", issueDetailPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Issue:     issue,
	})
}

// handleCloseIssue is POST /p/{pid}/issues/{seq}/close. Form:
// status (default closed; wontfix accepted), closed_by_task_id
// (optional). Re-renders detail page.
func (s *Server) handleCloseIssue(w http.ResponseWriter, r *http.Request) {
	pid, seq, ok := parseIssueRoute(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	status := strings.TrimSpace(r.FormValue("status"))
	closedByTaskID := strings.TrimSpace(r.FormValue("closed_by_task_id"))
	issue, err := s.fc.CloseIssue(r.Context(), pid, seq, status, closedByTaskID)
	if err != nil {
		s.logger.Error("CloseIssue failed", "project_id", pid, "seq", seq, "error", err)
		s.renderActionError(w, r, "close failed: "+err.Error())
		return
	}
	s.render(w, r, "issue.html", issueDetailPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Issue:     issue,
	})
}

// parseIssueRoute pulls projectID + issueSeq from chi URL params.
// Mirrors parseRunRoute. 400 on bad input.
func parseIssueRoute(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return 0, 0, false
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "issueSeq"))
	if err != nil || seq <= 0 {
		http.Error(w, "invalid issue seq", http.StatusBadRequest)
		return 0, 0, false
	}
	return pid, seq, true
}
