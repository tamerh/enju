package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

type createProjectRequest struct {
	Name    string `json:"name"`
	Description string `json:"description,omitempty"`
	RemoteURL  string `json:"remote_url,omitempty"`
	// DefaultBranch is the git branch new runs land on by
	// default. Optional — falls back to "main" when unset or
	// empty. Orgs that want Enju activity to stay off their
	// repo's main branch set this to e.g. "enju/work" at
	// create-project time. Validated against the same loose
	// git-ref grammar as branch= on create_run.
	DefaultBranch string `json:"default_branch,omitempty"`
}

type setProjectRemoteRequest struct {
	RemoteURL string `json:"remote_url"`
}

// projectResponse aliases service.ProjectResponse so this
// package can keep its existing literal-struct call sites
// (handleCreateProject, handleGetProject) working unchanged
// while the canonical shape lives in the shared service layer.
type projectResponse = service.ProjectResponse

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	caller := citizenFromRequest(r)
	resp, err := service.CreateProject(s.store, caller, service.CreateProjectParams{
		Name:     req.Name,
		Description:  req.Description,
		RemoteURL:   req.RemoteURL,
		DefaultBranch: req.DefaultBranch,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, service.ErrForbidden):
			writeError(w, http.StatusUnauthorized, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleSetProjectRemote updates the project's remote URL in the DB.
// Reconfiguring the MCP client's local clone happens on the
// client side; this endpoint just persists the new URL.
func (s *Server) handleSetProjectRemote(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	caller := citizenFromRequest(r)
	var req setProjectRemoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.SetProjectRemoteURL(s.store, caller, projectID, req.RemoteURL)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrForbidden), errors.Is(err, service.ErrNotMember):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSetProjectDefaultBranch updates a project's
// default_branch column. Owner-only: the default branch is
// where new runs land when no explicit branch is specified, so
// flipping it is the sort of project-wide configuration change
// that should sit with the admin tier.
func (s *Server) handleSetProjectDefaultBranch(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.SetProjectDefaultBranch(s.store, caller, projectID, req.Branch)
	if err != nil {
		writeMembershipErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.ListProjects(s.store, caller)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list projects")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// toProjectResponse delegates to the shared service helper.
// Kept as an in-package wrapper so the existing call sites
// (handleCreateProject, handleGetProject) read the same way
// they always did.
func toProjectResponse(p store.ProjectRecord, runCount int) projectResponse {
	return service.ToProjectResponse(p, runCount)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	p, err := s.store.GetProject(projectID)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	runs, _ := s.store.ListRunsByProject(p.ID)
	writeJSON(w, http.StatusOK, toProjectResponse(*p, len(runs)))
}

// handleProjectRemoteStatus / handleProjectSync were deleted during
// the iteration A orchestrator rewrite. The coordinator no longer
// owns a clone to compare or push from — the MCP client runs these
// diagnostics against its own local clone via workspace. The MCP tool
// names are unchanged; see internal/mcpserver/server.go for the new
// implementations.

func (s *Server) handleListProjectRuns(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.ListRunsByProject(s.store, caller, projectID)
	if err != nil {
		switch err {
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusInternalServerError, "failed to list runs")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
