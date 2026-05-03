package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ctxKey is a private type for context keys so package-external
// callers can't collide. Only the authenticated-citizen key is
// stored this way today.
type ctxKey int

const (
	ctxKeyCitizen ctxKey = iota
)

// citizenFromRequest returns the authenticated citizen that
// authMiddleware stashed into the request context, or nil when
// the request arrived without a valid Bearer token (soft-auth
// backwards-compat path). Handlers that need to know who is
// asking — read gating, ownership checks, creator auto-add —
// call this; handlers that don't care skip it.
func citizenFromRequest(r *http.Request) *store.CitizenRecord {
	if v, ok := r.Context().Value(ctxKeyCitizen).(*store.CitizenRecord); ok {
		return v
	}
	return nil
}

// authenticateCitizen extracts the Bearer token from the
// Authorization header and verifies it matches a registered
// citizen. Returns the citizen record on success, or writes
// an HTTP error and returns nil on failure. Write endpoints
// call this; read endpoints don't need it.
//
// If no Authorization header is present, the request is
// allowed through (backwards compatibility for the
// transition period). Once all clients send tokens, this
// can be tightened to reject unauthenticated writes.
func (s *Server) authenticateCitizen(w http.ResponseWriter, r *http.Request) *store.CitizenRecord {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil // no auth — allowed for now
	}
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "invalid Authorization header — expected 'Bearer <token>'")
		return nil
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	citizen, err := s.store.GetCitizenByToken(token)
	if err != nil || citizen == nil {
		writeError(w, http.StatusUnauthorized, "invalid or expired token — re-register with enju mcp")
		return nil
	}
	return citizen
}

// requireProjectMembershipForTask gates a task-scoped endpoint by
// resolving the task's run → project, then deferring to
// requireProjectMembership. Convenience wrapper so each task
// handler doesn't re-implement the lookup.
func (s *Server) requireProjectMembershipForTask(w http.ResponseWriter, r *http.Request, taskID string) (*store.ProjectMemberRecord, bool) {
	task, err := s.store.GetTask(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task lookup failed: "+err.Error())
		return nil, false
	}
	if task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return nil, false
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		writeError(w, http.StatusInternalServerError, "run lookup failed")
		return nil, false
	}
	return s.requireProjectMembership(w, r, run.ProjectID)
}

// checkProjectMembershipForTask is the write-free variant of
// requireProjectMembershipForTask: returns the caller's
// membership row or an error message without touching the
// response writer. Used by batch endpoints (e.g.
// /tasks/reconcile) that need to emit per-entry membership
// errors as part of a larger JSON response envelope, not as
// standalone HTTP errors. Mixing both forms in one request
// would corrupt the response (headers already written, then a
// second writeJSON) — see reconcileOne.
func (s *Server) checkProjectMembershipForTask(r *http.Request, taskID string) (*store.ProjectMemberRecord, string) {
	task, err := s.store.GetTask(taskID)
	if err != nil {
		return nil, "task lookup failed: " + err.Error()
	}
	if task == nil {
		return nil, "task not found"
	}
	run, err := s.store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, "run lookup failed"
	}
	return s.checkProjectMembership(r, run.ProjectID)
}

// checkProjectMembership is the write-free variant of
// requireProjectMembership. Same gating rules, but returns
// (memb, errMsg) instead of writing to w. Empty errMsg with
// nil memb means "legacy pre-membership project, caller is
// accepted" — matches the membership-bypass branch in
// requireProjectMembership.
func (s *Server) checkProjectMembership(r *http.Request, projectID int64) (*store.ProjectMemberRecord, string) {
	caller := citizenFromRequest(r)
	if caller == nil {
		return nil, "authentication required"
	}
	total, err := s.store.CountProjectMembers(projectID)
	if err != nil {
		return nil, "membership lookup failed: " + err.Error()
	}
	if total == 0 {
		// Legacy project — same bypass as requireProjectMembership.
		return nil, ""
	}
	memb, err := s.store.GetProjectMember(projectID, caller.ID)
	if err != nil {
		return nil, "membership lookup failed: " + err.Error()
	}
	if memb == nil {
		return nil, fmt.Sprintf("not a member of project %d", projectID)
	}
	return memb, ""
}

// requireProjectMembership returns the caller's membership row
// for a project, writing the appropriate error and returning
// false when gating blocks the request.
//
// Since authMiddleware hard-enforces Bearer tokens, the caller is
// always non-nil by the time this runs. The only gray area is
// pre-membership legacy projects with zero rows in
// project_members — those remain open so databases migrated from
// the pre-Phase-J schema don't lose access overnight. Every new
// project seeds its creator as owner so it never lands there.
func (s *Server) requireProjectMembership(w http.ResponseWriter, r *http.Request, projectID int64) (*store.ProjectMemberRecord, bool) {
	caller := citizenFromRequest(r)
	if caller == nil {
		// Belt-and-suspenders: authMiddleware should have
		// already rejected this. Treat as 401 if we ever
		// somehow reach here.
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	total, err := s.store.CountProjectMembers(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "membership lookup failed: "+err.Error())
		return nil, false
	}
	if total == 0 {
		// Pre-membership legacy project — no rows means "not
		// migrated yet", not "empty." Keep open so reading
		// the DB before any member is seeded still works.
		return nil, true
	}
	m, err := s.store.GetProjectMember(projectID, caller.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "membership lookup failed: "+err.Error())
		return nil, false
	}
	if m == nil {
		writeError(w, http.StatusForbidden, fmt.Sprintf("not a member of project %d — ask an existing member to add you", projectID))
		return nil, false
	}
	return m, true
}
