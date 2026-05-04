package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/go-chi/chi/v5"
)

type addProjectMemberRequest struct {
	Username string `json:"username"`
	Role   string `json:"role,omitempty"` // optional; defaults to "member"
}

// projectMemberResponse aliases service.MemberResponse (which
// itself aliases wire.Member). Kept as a type alias so the
// handler call sites keep their tight names and the JSON wire
// shape stays anchored to a single source of truth.
type projectMemberResponse = service.MemberResponse

type setProjectMemberRoleRequest struct {
	Role string `json:"role"`
}

// handleListProjectMembers returns every member on the project,
// gated on caller membership. Response rows expose usernames (not
// citizen IDs) so all external identifiers match the rest of the
// API surface.
func (s *Server) handleListProjectMembers(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if projectID == 0 {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	rows, err := s.store.ListProjectMembers(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list members: "+err.Error())
		return
	}
	resp := make([]projectMemberResponse, 0, len(rows))
	for _, m := range rows {
		cz, _ := s.store.GetCitizen(m.CitizenID)
		username := ""
		name := ""
		if cz != nil {
			username = cz.Username
			name = cz.Name
		}
		resp = append(resp, projectMemberResponse{
			Username: username,
			Name:     name,
			Role:     string(m.Role),
			AddedAt:  m.AddedAt,
			AddedBy:  s.citizenUsername(m.AddedBy),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddProjectMember grants membership to a citizen. Any
// existing member can add — role-free delegation is the
// GitHub-style trust the user asked for.
func (s *Server) handleAddProjectMember(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req addProjectMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.AddProjectMember(s.store, caller, projectID, req.Username, req.Role)
	if err != nil {
		writeMembershipErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleRemoveProjectMember removes a citizen from the project.
// Resolves the path-param citizenID to a username and delegates
// to service.RemoveProjectMember.
func (s *Server) handleRemoveProjectMember(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	targetID, _ := strconv.ParseInt(chi.URLParam(r, "citizenID"), 10, 64)
	target, err := s.store.GetCitizen(targetID)
	if err != nil || target == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.RemoveProjectMember(s.store, caller, projectID, target.Username)
	if err != nil {
		writeMembershipErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRemoveProjectMemberByUsername resolves the username to a
// citizen ID and delegates to handleRemoveProjectMember. Thin
// convenience alias so the MCP layer doesn't have to round-trip
// through /citizens/by-username first.
func (s *Server) handleRemoveProjectMemberByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target := s.resolveCitizen(w, username)
	if target == nil {
		return
	}
	rctx := chi.RouteContext(r.Context())
	rctx.URLParams.Add("citizenID", strconv.FormatInt(target.ID, 10))
	s.handleRemoveProjectMember(w, r)
}

// handleSetProjectMemberRoleByUsername mirrors handleRemoveProjectMemberByUsername
// for the role-change endpoint — resolves username to citizen ID
// and hands off to the canonical handler.
func (s *Server) handleSetProjectMemberRoleByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target := s.resolveCitizen(w, username)
	if target == nil {
		return
	}
	rctx := chi.RouteContext(r.Context())
	rctx.URLParams.Add("citizenID", strconv.FormatInt(target.ID, 10))
	s.handleSetProjectMemberRole(w, r)
}

// handleSetProjectMemberRole promotes or demotes a member.
// Resolves the path-param citizenID to a username and delegates
// to service.SetProjectMemberRole.
func (s *Server) handleSetProjectMemberRole(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	targetID, _ := strconv.ParseInt(chi.URLParam(r, "citizenID"), 10, 64)
	target, err := s.store.GetCitizen(targetID)
	if err != nil || target == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setProjectMemberRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.SetProjectMemberRole(s.store, caller, projectID, target.Username, req.Role)
	if err != nil {
		writeMembershipErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
