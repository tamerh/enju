package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/go-chi/chi/v5"
)

// artifactResponse aliases service.ArtifactResponse so any
// remaining literal-struct call sites in this package keep
// working while the canonical shape lives in service.
type artifactResponse = service.ArtifactResponse

// citizenUsername looks up the username for an internal citizen ID.
// Returns the empty string if the citizen isn't found (e.g. id is 0).
// This is the centralized point for translating backstage IDs into
// user-facing handles.
func (s *Server) citizenUsername(id int64) string {
	if id == 0 {
		return ""
	}
	c, _ := s.store.GetCitizen(id)
	if c == nil {
		return ""
	}
	return c.Username
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.ListArtifacts(s.store, caller, service.ArtifactListParams{
		ProjectID: projectID,
		Branch:    r.URL.Query().Get("branch"),
		Prefix:    r.URL.Query().Get("prefix"),
	})
	if err != nil {
		switch err {
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusInternalServerError, "failed to list artifacts")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetArtifact returns the artifacts index metadata for one
// artifact path: who wrote it, in what task, at what commit SHA.
// File content reading has moved to the MCP client side, which
// reads directly from its local clone at the commit SHA this
// endpoint returns.
func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	// chi's wildcard captures everything after "artifacts/" — that IS
	// the user-facing artifact path.
	path := chi.URLParam(r, "*")
	if err := validateArtifactPath(path); err != nil {
		writeError(w, http.StatusBadRequest, "invalid artifact path: "+err.Error())
		return
	}

	// Branch filter defaults to the project's configured
	// default — single-branch projects get the expected
	// behavior, multi-branch projects can query with
	// ?branch=<name>.
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		if p, _ := s.store.GetProject(projectID); p != nil {
			branch = p.DefaultBranch
		}
	}
	meta, err := s.store.GetArtifact(projectID, branch, path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read artifact index")
		return
	}
	if meta == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":     path,
		"last_writer": s.citizenUsername(meta.LastWriter),
		"last_task_id": meta.LastTaskID,
		"last_run_id": meta.LastRunID,
		"commit_sha":  meta.CommitSHA,
		"tracked":   meta.Tracked,
		"updated_at":  meta.UpdatedAt.Format(time.RFC3339),
	})
}
