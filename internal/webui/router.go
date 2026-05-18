package webui

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// router builds the chi router with middleware and route
// registrations. Kept as a method on *Server so handlers can
// reach s.fc / s.logger / s.dev without globals.
//
// Skeleton stage: landing + health + static are wired. Real
// project / run / task routes land as their handlers are
// written. Each route added MUST have a corresponding template
// (or partial) and a smoke-test entry in server_test.go.
func (s *Server) router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(s.logRequest)
	r.Use(s.requireSameOriginForWrites)

	r.Get("/health", s.handleHealth)
	r.Get("/", s.handleLanding)
	r.Post("/projects", s.handleCreateProject)
	r.Get("/me", s.handleMe)
	r.Post("/me/profile", s.handleUpdateProfile)
	r.Post("/me/agents", s.handleRegisterAgent)
	r.Get("/p/{projectID}", s.handleProjectView)
	r.Get("/p/{projectID}/settings", s.handleProjectSettings)
	r.Get("/p/{projectID}/r/{runSeq}", s.handleRunView)
	r.Get("/p/{projectID}/r/{runSeq}/export.md", s.handleExportRun)
	r.Get("/p/{projectID}/t/{taskID}", s.handleTaskView)
	r.Get("/p/{projectID}/inbox", s.handleProjectInbox)
	r.Get("/inbox", s.handleGlobalInbox)
	r.Get("/p/{projectID}/events", s.handleEvents)
	r.Get("/events", s.handleGlobalEvents)
	r.Get("/p/{projectID}/notifications", s.handleNotificationsRedirect)

	// Issues — per-project list + detail (read), file/triage/
	// close (write, CSRF-gated).
	r.Get("/p/{projectID}/issues", s.handleIssuesList)
	r.Get("/p/{projectID}/issues/{issueSeq}", s.handleIssueView)
	r.Post("/p/{projectID}/issues", s.handleFileIssue)
	r.Post("/p/{projectID}/issues/{issueSeq}/triage", s.handleTriageIssue)
	r.Post("/p/{projectID}/issues/{issueSeq}/close", s.handleCloseIssue)

	// Templates — list, describe, create-run-from. Wildcard
	// captures the repo-relative path (which contains /).
	r.Get("/p/{projectID}/templates", s.handleTemplatesList)
	r.Get("/p/{projectID}/templates/show/*", s.handleTemplateDetail)
	r.Post("/p/{projectID}/templates/run/*", s.handleCreateRunFromTemplate)

	// New run from inline YAML — the paste-a-workflow authoring
	// path. GET renders the form (read); POST validates and
	// optionally creates (write, CSRF-gated).
	r.Get("/p/{projectID}/new-run", s.handleNewRunForm)
	r.Post("/p/{projectID}/new-run", s.handleNewRun)

	// Artifacts — list, content view, history (read-only).
	r.Get("/p/{projectID}/artifacts", s.handleArtifactsList)
	r.Get("/p/{projectID}/artifacts/show/*", s.handleArtifactView)
	r.Get("/p/{projectID}/artifacts/history/*", s.handleArtifactHistory)

	// Write actions — gated by requireSameOriginForWrites at the
	// middleware layer.
	r.Post("/p/{projectID}/t/{taskID}/claim", s.handleClaim)
	r.Post("/p/{projectID}/t/{taskID}/release", s.handleRelease)
	r.Post("/p/{projectID}/t/{taskID}/review", s.handleReview)
	r.Post("/p/{projectID}/t/{taskID}/submit", s.handleSubmit)
	r.Post("/p/{projectID}/t/{taskID}/fail", s.handleFailTask)
	r.Post("/p/{projectID}/t/{taskID}/execute", s.handleExecuteComputeTask)

	// Run-level write actions
	r.Post("/p/{projectID}/r/{runSeq}/pause", s.handlePauseRun)
	r.Post("/p/{projectID}/r/{runSeq}/resume", s.handleResumeRun)
	r.Post("/p/{projectID}/r/{runSeq}/terminate", s.handleTerminateRun)
	r.Post("/p/{projectID}/r/{runSeq}/execute", s.handleExecuteRun)

	// Project-level write actions
	r.Post("/p/{projectID}/sync", s.handleProjectSync)
	r.Post("/p/{projectID}/members", s.handleAddProjectMember)
	r.Post("/p/{projectID}/members/{username}/remove", s.handleRemoveProjectMember)
	r.Post("/p/{projectID}/members/{username}/role", s.handleSetProjectMemberRole)
	r.Post("/p/{projectID}/default-branch", s.handleSetProjectDefaultBranch)
	r.Post("/p/{projectID}/remote", s.handleSetProjectRemote)
	r.Post("/p/{projectID}/leave", s.handleLeaveProject)

	// Static asset serving. The path prefix is stripped by
	// chi.StripPrefix so /static/app.css maps to app.css inside
	// the FS (which is sub-rooted at static/ in production).
	r.Handle("/static/*", http.StripPrefix("/static/", s.staticHandler(s.staticFS)))

	return r
}

// handleHealth is the liveness probe. Returns 200 + "ok". No
// FatClient touch, no template render — purely "the binary is
// up and the router is wired."
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
