package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

// --- Issues (living-workflow phase 3) ---

type fileIssueRequest struct {
	Title     string `json:"title"`
	Body     string `json:"body"`
	Severity   string `json:"severity"`
	FoundInRunSeq int  `json:"found_in_run_seq,omitempty"`
	FoundInTaskID string `json:"found_in_task_id,omitempty"`
}

// handleFileIssue creates a new issue under a project. Member-
// gated; the filer is the authenticated citizen. Emits an
// `issue_filed` contribution event so the issue appears in the
// project's event log. Body and severity are optional; status
// defaults to "open."
//
// Living-workflow phase 3 — see
// docs/living-workflow-design-notes.md § 6.
func (s *Server) handleFileIssue(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req fileIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.FileIssue(s.store, caller, service.FileIssueParams{
		ProjectID:     projectID,
		Title:         req.Title,
		Body:          req.Body,
		Severity:      req.Severity,
		FoundInRunSeq: req.FoundInRunSeq,
		FoundInTaskID: req.FoundInTaskID,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case err == service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusInternalServerError, "creating issue: "+err.Error())
		}
		return
	}

	// Living-workflow phase 4c — file-against-completed-run
	// gap. Re-evaluate completed runs in projects with
	// auto_triage so a newly-filed issue can re-open work
	// against a run that quiesced because no issue existed
	// yet. HTTP-side only for now: this hook touches the api
	// Server's DAG/parsed-run cache and isn't yet reachable
	// from the service layer (see DAG cache extraction TODO).
	// Native MCP file_issue skips this; a list_runs sweep on
	// auto_triage runs picks up the gap eventually.
	if runIDs, err := s.store.ListRunsWithAutoTriage(projectID); err == nil {
		for _, rid := range runIDs {
			s.evaluateRunStateAndMaybeTriage(rid)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleListIssues returns all issues in a project, newest-first.
// Optional query params: status (comma-separated, OR-matched),
// severity (comma-separated), limit (default 100, max 1000).
func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	f := store.IssueFilter{ProjectID: projectID}
	if st := r.URL.Query().Get("status"); st != "" {
		for _, s := range strings.Split(st, ",") {
			f.Status = append(f.Status, store.IssueStatus(s))
		}
	}
	if sv := r.URL.Query().Get("severity"); sv != "" {
		for _, s := range strings.Split(sv, ",") {
			f.Severity = append(f.Severity, store.IssueSeverity(s))
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = n
	}

	issues, err := s.store.ListIssues(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing issues: "+err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(issues))
	for _, it := range issues {
		out = append(out, s.issueToMap(&it))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetIssue returns one issue by its (project, seq) pair.
func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "issueSeq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid issue_seq")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	it, err := s.store.GetIssueBySeq(projectID, seq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if it == nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	writeJSON(w, http.StatusOK, s.issueToMap(it))
}

type triageIssueRequest struct {
	Severity string `json:"severity,omitempty"` // optional severity update
}

func (s *Server) handleTriageIssue(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	seq, _ := strconv.Atoi(chi.URLParam(r, "issueSeq"))
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req triageIssueRequest
	// Body is optional (severity-only update). Tolerate io.EOF.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	issue, err := service.TriageIssue(s.store, caller, projectID, seq, req.Severity)
	if err != nil {
		switch err {
		case service.ErrNotFound:
			writeError(w, http.StatusNotFound, "issue not found")
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

type closeIssueRequest struct {
	Status     string `json:"status"`      // "closed" | "wontfix"
	ClosedByTaskID string `json:"closed_by_task_id"` // optional
}

func (s *Server) handleCloseIssue(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	seq, _ := strconv.Atoi(chi.URLParam(r, "issueSeq"))
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req closeIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	issue, err := service.CloseIssue(s.store, caller, projectID, seq, req.Status, req.ClosedByTaskID)
	if err != nil {
		switch err {
		case service.ErrNotFound:
			writeError(w, http.StatusNotFound, "issue not found")
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

// issueToMap is the shared JSON shape for every issue endpoint.
// Keys mirror the YAML frontmatter from the design notes (id,
// title, status, severity, ...) so a future fat-client mirror
// can dump the map straight into ISSUE-<NNN>.md.
//
// Citizen ids are resolved to usernames so humans (and the
// markdown frontmatter) read names, not numbers — matches the
// vote/review submission rendering.
func (s *Server) issueToMap(it *store.IssueRecord) map[string]interface{} {
	m := map[string]interface{}{
		"id":     fmt.Sprintf("ISSUE-%03d", it.Seq),
		"db_id":   it.ID,
		"seq":    it.Seq,
		"project_id": it.ProjectID,
		"title":   it.Title,
		"body":    it.Body,
		"status":   it.Status,
		"severity":  it.Severity,
		"filed_by":  s.citizenUsername(it.FiledBy),
		"filed_at":  it.FiledAt.UTC().Format(time.RFC3339),
		"updated_at": it.UpdatedAt.UTC().Format(time.RFC3339),
	}
	// Surface the per-project run seq (#1, #2, ...) — the
	// citizen-facing identity. The internal DB id stays out
	// of the response shape so a future filesystem mirror
	// writes the human-meaningful number into ISSUE-NNN.md
	// frontmatter. Lookup is best-effort: if the run was
	// hard-deleted the field falls off silently rather than
	// blocking the issue render.
	if it.FoundInRunID > 0 {
		if run, err := s.store.GetRun(it.FoundInRunID); err == nil && run != nil {
			m["found_in_run_seq"] = run.Seq
		}
	}
	if it.FoundInTaskID != "" {
		m["found_in_task_id"] = it.FoundInTaskID
	}
	if it.TriagedBy > 0 {
		m["triaged_by"] = s.citizenUsername(it.TriagedBy)
	}
	if it.TriagedAt != nil {
		m["triaged_at"] = it.TriagedAt.UTC().Format(time.RFC3339)
	}
	if it.ClosedByTaskID != "" {
		m["closed_by_task_id"] = it.ClosedByTaskID
	}
	if it.ClosedAt != nil {
		m["closed_at"] = it.ClosedAt.UTC().Format(time.RFC3339)
	}
	return m
}
