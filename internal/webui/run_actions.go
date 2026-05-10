package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Run-level write actions wired here:
//
//   POST /p/{projectID}/r/{runSeq}/pause      — pause a live run
//   POST /p/{projectID}/r/{runSeq}/resume     — resume a paused run
//   POST /p/{projectID}/r/{runSeq}/terminate  — irreversibly abort a run
//
// All three are CSRF-gated by requireSameOriginForWrites at the
// router layer. After each action the handler re-renders the
// run page so the state badge + button row reflect new state.

// handlePauseRun is POST /p/{projectID}/r/{runSeq}/pause.
// Idempotent server-side; coord refuses on terminal runs and
// returns an error which we surface as the action banner.
func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	pid, seq, ok := parseRunRoute(w, r)
	if !ok {
		return
	}
	if err := s.fc.PauseRun(r.Context(), pid, seq); err != nil {
		s.logger.Error("PauseRun failed", "project_id", pid, "run_seq", seq, "error", err)
		s.renderActionError(w, r, "pause failed: "+err.Error())
		return
	}
	s.renderRunAfterAction(w, r, pid, seq)
}

// handleResumeRun is POST /p/{projectID}/r/{runSeq}/resume.
// Lifts a paused run back to active or idle depending on
// whether ready work exists. Coord makes the call.
func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	pid, seq, ok := parseRunRoute(w, r)
	if !ok {
		return
	}
	if err := s.fc.ResumeRun(r.Context(), pid, seq); err != nil {
		s.logger.Error("ResumeRun failed", "project_id", pid, "run_seq", seq, "error", err)
		s.renderActionError(w, r, "resume failed: "+err.Error())
		return
	}
	s.renderRunAfterAction(w, r, pid, seq)
}

// handleTerminateRun is POST /p/{projectID}/r/{runSeq}/terminate.
// Irreversible — cascade-skips every non-terminal task, abandons
// every open claim, transitions the run to `terminated`. Form
// carries an optional `reason` field; coord caps it server-side.
//
// Confirmation lives in the UI form template (terminate button
// reveals an inline reason+confirm form). The handler doesn't
// re-prompt — by the time we're here the user has already
// confirmed.
func (s *Server) handleTerminateRun(w http.ResponseWriter, r *http.Request) {
	pid, seq, ok := parseRunRoute(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if err := s.fc.TerminateRun(r.Context(), pid, seq, reason); err != nil {
		s.logger.Error("TerminateRun failed",
			"project_id", pid, "run_seq", seq, "error", err)
		s.renderActionError(w, r, "terminate failed: "+err.Error())
		return
	}
	s.renderRunAfterAction(w, r, pid, seq)
}

// parseRunRoute pulls projectID + runSeq from chi URL params.
// Mirrors parseTaskRoute. 400 on bad input.
func parseRunRoute(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return 0, 0, false
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "runSeq"))
	if err != nil || seq <= 0 {
		http.Error(w, "invalid run seq", http.StatusBadRequest)
		return 0, 0, false
	}
	return pid, seq, true
}

// renderRunAfterAction re-fetches run detail and re-renders the
// run page (full or partial via HX-Request). Same pattern as
// renderTaskAfterAction. On fetch failure, falls back to a 303
// redirect at the run URL so the user lands on a fresh page
// rather than a stale one.
func (s *Server) renderRunAfterAction(w http.ResponseWriter, r *http.Request, pid int64, seq int) {
	run, err := s.fc.GetRun(r.Context(), pid, seq)
	if err != nil || run == nil {
		s.logger.Warn("post-action GetRun failed; redirecting",
			"project_id", pid, "run_seq", seq, "error", err)
		target := "/p/" + strconv.FormatInt(pid, 10) + "/r/" + strconv.Itoa(seq)
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	s.render(w, r, "run.html", runPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Run:       run,
	})
}
