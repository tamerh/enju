package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// Workflows surface — read (list + describe) and write (create
// run from workflow). Workflows are any *.yaml file in the project
// repo on the default branch (root-level enju.yaml is the common
// case; nested workflows/<name>/ is also fine).
//
//   GET  /p/{projectID}/workflows        — list paths
//   GET  /p/{projectID}/workflows/show/* — describe one (path is
//                                          the repo-relative path,
//                                          slashes intact via chi
//                                          wildcard)
//   POST /p/{projectID}/workflows/run/*  — create a run; form carries
//                                          params + optional branch
//
// List is path-only — picking which YAML is "actually" a workflow
// is the operator's call. The detail page parses the file and
// hosts the create-run form (declared params → form fields). On
// submit the handler calls Session.CreateRunFromTemplate which
// snapshots the bundle, posts to coord, and freezes the per-run
// copy. Success → redirect to /p/{pid}/r/{seq}.

// workflowsListPage is the data shape for views/workflows.html.
type workflowsListPage struct {
	pageData
	ProjectID int64
	Workflows []service.WorkflowSummary
}

// workflowDetailPage is the data shape for views/workflow.html.
// Error / Submitted carry create-run failure state so the form
// repopulates rather than getting wiped (same pattern as the
// issues list page's file-form failure path).
type workflowDetailPage struct {
	pageData
	ProjectID       int64
	Path            string
	Workflow        *service.LoadedWorkflow
	Error           string
	SubmittedBranch string
	SubmittedParams map[string]string
}

// handleWorkflowsList renders /p/{pid}/workflows.
func (s *Server) handleWorkflowsList(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	wfs, err := s.fc.ListWorkflows(r.Context(), pid)
	if err != nil {
		s.logger.Error("ListWorkflows failed", "project_id", pid, "error", err)
		http.Error(w, "failed to list workflows: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.render(w, r, "workflows.html", workflowsListPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Workflows: wfs,
	})
}

// handleWorkflowDetail renders /p/{pid}/workflows/show/{path...}.
// The wildcard captures the repo-relative path (e.g.
// "workflows/gwas/enju.yaml" or just "enju.yaml"). DescribeWorkflow
// parses the file for declared params.
func (s *Server) handleWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	path := chi.URLParam(r, "*")
	if path == "" {
		http.Error(w, "workflow path is required", http.StatusBadRequest)
		return
	}
	loaded, err := s.fc.DescribeWorkflow(r.Context(), pid, path)
	if err != nil {
		s.logger.Error("DescribeWorkflow failed", "project_id", pid, "path", path, "error", err)
		http.Error(w, "failed to load workflow: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.render(w, r, "workflow.html", workflowDetailPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Path:      path,
		Workflow:  loaded,
	})
}

// handleCreateRunFromWorkflow is POST
// /p/{pid}/workflows/run/{path...}. Form carries:
//
//   branch   — optional run branch override
//   p_<name> — one entry per declared param (e.g. p_disease=tp53)
//
// Multi-value params (list<string>) come through as comma-
// separated strings; we split + trim. On success redirects to
// /p/{pid}/r/{seq}; on failure re-renders the detail page with
// the form repopulated.
func (s *Server) handleCreateRunFromWorkflow(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	path := chi.URLParam(r, "*")
	if path == "" {
		http.Error(w, "workflow path is required", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	branch := strings.TrimSpace(r.FormValue("branch"))

	// Re-fetch the workflow to know its declared params shape.
	// Required to (a) coerce list<string> values from the
	// comma-separated input, (b) repopulate on error.
	loaded, lerr := s.fc.DescribeWorkflow(r.Context(), pid, path)
	if lerr != nil {
		s.logger.Error("DescribeWorkflow failed during create-run", "project_id", pid, "path", path, "error", lerr)
		http.Error(w, "failed to load workflow: "+lerr.Error(), http.StatusBadGateway)
		return
	}

	submitted := make(map[string]string)
	params := make(map[string]interface{})
	if loaded != nil {
		for _, p := range loaded.Details.Params {
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
		s.render(w, r, "workflow.html", workflowDetailPage{
			pageData:        s.commonPageData(),
			ProjectID:       pid,
			Path:            path,
			Workflow:        loaded,
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
