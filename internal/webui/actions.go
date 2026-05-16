package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// Write actions wired here:
//
//   POST /p/{projectID}/t/{taskID}/claim    — claim the task
//   POST /p/{projectID}/t/{taskID}/release  — give it back
//   POST /p/{projectID}/t/{taskID}/review   — submit a verdict
//
// All three are CSRF-gated by requireSameOriginForWrites at the
// router layer. Handlers re-render the task page (or just the
// task page's "main" block on HX-Request) so the form region
// updates in place.

// handleClaim is POST /p/{projectID}/t/{taskID}/claim.
// Re-renders the task detail page after the claim lands so the
// state badge flips ready→claimed and the verdict form (if
// review) appears.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	pid, taskID, ok := parseTaskRoute(w, r)
	if !ok {
		return
	}
	res, err := s.fc.ClaimTask(r.Context(), service.ClaimParams{
		TaskID:         taskID,
		IncludeContext: false, // we re-fetch meta below; no need for the heavy descriptor
	})
	if err != nil {
		s.logger.Error("ClaimTask failed", "task_id", taskID, "error", err)
		s.renderTaskActionError(w, r, pid, taskID, "claim failed: "+err.Error())
		return
	}
	_ = res // res.Data is the raw coord JSON; we don't surface it inline
	s.renderTaskAfterAction(w, r, pid, taskID)
}

// handleRelease is POST /p/{projectID}/t/{taskID}/release.
// Hands the task back to the queue. Re-renders the task page —
// state badge flips claimed→ready, the verdict form disappears.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	pid, taskID, ok := parseTaskRoute(w, r)
	if !ok {
		return
	}
	if err := s.fc.ReleaseTask(r.Context(), taskID); err != nil {
		s.logger.Error("ReleaseTask failed", "task_id", taskID, "error", err)
		s.renderTaskActionError(w, r, pid, taskID, "release failed: "+err.Error())
		return
	}
	s.renderTaskAfterAction(w, r, pid, taskID)
}

// handleFailTask is POST /p/{projectID}/t/{taskID}/fail. Drives
// the task to terminal `failed` (mirror of enju_fail_task).
// Reason is required — the MCP tool requires it and it's shown
// to every citizen in run status, so we enforce it server-side
// rather than trust the form's `required` attribute (HTMX can
// bypass native validation). On success the page re-renders:
// state flips to `failed`, the action forms disappear.
func (s *Server) handleFailTask(w http.ResponseWriter, r *http.Request) {
	pid, taskID, ok := parseTaskRoute(w, r)
	if !ok {
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.renderTaskActionError(w, r, pid, taskID,
			"a reason is required to fail a task (it's shown to all citizens in run status)")
		return
	}
	if err := s.fc.FailTask(r.Context(), taskID, reason); err != nil {
		s.logger.Error("FailTask failed", "task_id", taskID, "error", err)
		s.renderTaskActionError(w, r, pid, taskID, "fail failed: "+err.Error())
		return
	}
	s.renderTaskAfterAction(w, r, pid, taskID)
}

// handleSubmit is POST /p/{projectID}/t/{taskID}/submit.
// Generic submit endpoint for action:answer and action:vote.
//
//   - answer: form carries `content` (the prose answer)
//   - vote:   form carries `option` (id from the declared
//             options list) + optional `content` (commentary)
//
// Action:review uses /review (handleReview) instead — it has
// its own dedicated path because the verdict semantics
// (approve / request_changes / reject / comment) differ from
// the answer/vote flow and we want the URL to reflect that.
//
// Action:compute is not exposed here. Compute task execution
// goes through enju_execute_task today; UI integration is its
// own discussion.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	pid, taskID, ok := parseTaskRoute(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	content := r.FormValue("content")
	option := strings.TrimSpace(r.FormValue("option"))

	meta, err := s.fc.FetchTaskMeta(r.Context(), taskID)
	if err != nil || meta == nil {
		s.renderActionError(w, r, "task lookup failed before submit")
		return
	}

	// Action-specific validation. Server-side defense; the
	// form templates already restrict input shape, but a
	// hand-crafted POST shouldn't silently submit garbage.
	switch meta.Action {
	case "answer":
		if strings.TrimSpace(content) == "" {
			s.renderTaskActionError(w, r, pid, taskID, "answer content is required")
			return
		}
	case "vote":
		if option == "" {
			s.renderTaskActionError(w, r, pid, taskID, "option is required for vote tasks")
			return
		}
	default:
		s.renderTaskActionError(w, r, pid, taskID,
			"submit endpoint handles answer and vote only; got action="+meta.Action)
		return
	}

	res := s.fc.SubmitTaskResult(r.Context(), service.SubmitParams{
		TaskID:  taskID,
		Meta:    meta,
		Content: content,
		Option:  option,
	})
	if res != nil && res.ErrorMessage != "" {
		s.logger.Warn("submit returned error",
			"task_id", taskID, "action", meta.Action, "error", res.ErrorMessage)
		s.renderTaskActionError(w, r, pid, taskID, "submit failed: "+res.ErrorMessage)
		return
	}
	s.renderTaskAfterAction(w, r, pid, taskID)
}

// handleReview is POST /p/{projectID}/t/{taskID}/review.
// Submits a verdict (decision + prose) for an action:review
// task. Pre-fetches TaskMeta because SubmitTaskResult needs it
// (Meta carries iteration branch, action type, validation
// schema). On validation failure, re-renders with an error
// banner and the form preserved.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	pid, taskID, ok := parseTaskRoute(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	decision := strings.TrimSpace(r.FormValue("decision"))
	content := r.FormValue("content")
	if decision == "" {
		s.renderTaskActionError(w, r, pid, taskID, "decision is required (approve, request_changes, reject, or comment)")
		return
	}

	meta, err := s.fc.FetchTaskMeta(r.Context(), taskID)
	if err != nil || meta == nil {
		s.renderActionError(w, r, "task lookup failed before submit")
		return
	}
	res := s.fc.SubmitTaskResult(r.Context(), service.SubmitParams{
		TaskID:   taskID,
		Meta:     meta,
		Content:  content,
		Decision: decision,
	})
	if res != nil && res.ErrorMessage != "" {
		s.logger.Warn("review submit returned error",
			"task_id", taskID, "decision", decision, "error", res.ErrorMessage)
		s.renderTaskActionError(w, r, pid, taskID, "submit failed: "+res.ErrorMessage)
		return
	}
	s.renderTaskAfterAction(w, r, pid, taskID)
}

// parseTaskRoute is the boilerplate shared by all three action
// handlers: pull project id + task id from chi URL params,
// 400 on bad input. Returns (pid, taskID, true) on success.
func parseTaskRoute(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return 0, "", false
	}
	taskID := chi.URLParam(r, "taskID")
	if taskID == "" {
		http.Error(w, "missing task id", http.StatusBadRequest)
		return 0, "", false
	}
	return pid, taskID, true
}

// renderTaskActionError re-fetches the task and re-renders
// the task page with a banner showing the error message.
// Same shape as renderTaskAfterAction but with ActionError
// populated so the form region stays intact.
//
// Falls back to the lightweight renderActionError if the task
// can't be re-fetched (banner-only response).
func (s *Server) renderTaskActionError(w http.ResponseWriter, r *http.Request, pid int64, taskID, msg string) {
	meta, err := s.fc.FetchTaskMeta(r.Context(), taskID)
	if err != nil || meta == nil {
		s.renderActionError(w, r, msg)
		return
	}
	result, _, _ := s.fc.ReadTaskResult(r.Context(), taskID)
	iterations := s.loadIterationViews(r, meta)
	s.render(w, r, "task.html", taskPage{
		pageData:    s.commonPageData(),
		ProjectID:   pid,
		Task:        meta,
		DependsOn:   splitDeps(meta.DependsOn),
		VoteOptions: parseVoteOptions(meta.VoteOptionsJSON, s.logger),
		ClaimedByMe: meta.State == "claimed" && meta.ClaimedBy == s.fc.Username(),
		Result:      result,
		Iterations:  iterations,
		ActionError: msg,
	})
}

// renderTaskAfterAction re-fetches the task and re-renders the
// task page (or just the main block on HX-Request). Used after
// every successful write so the user sees the new state without
// a manual refresh.
func (s *Server) renderTaskAfterAction(w http.ResponseWriter, r *http.Request, pid int64, taskID string) {
	meta, err := s.fc.FetchTaskMeta(r.Context(), taskID)
	if err != nil {
		s.logger.Warn("post-action FetchTaskMeta failed; rendering minimal page",
			"task_id", taskID, "error", err)
		http.Redirect(w, r, "/p/"+strconv.FormatInt(pid, 10)+"/t/"+taskID, http.StatusSeeOther)
		return
	}
	result, _, rerr := s.fc.ReadTaskResult(r.Context(), taskID)
	if rerr != nil {
		s.logger.Warn("ReadTaskResult failed after action; rendering without result body",
			"task_id", taskID, "error", rerr)
	}
	iterations := s.loadIterationViews(r, meta)
	s.render(w, r, "task.html", taskPage{
		pageData:    s.commonPageData(),
		ProjectID:   pid,
		Task:        meta,
		DependsOn:   splitDeps(meta.DependsOn),
		VoteOptions: parseVoteOptions(meta.VoteOptionsJSON, s.logger),
		ClaimedByMe: meta.State == "claimed" && meta.ClaimedBy == s.fc.Username(),
		Result:      result,
		Iterations:  iterations,
	})
}

// renderActionError surfaces a validation / submit failure as a
// banner-style HTML response. HTMX consumers see the banner
// fragment swap into the task page's main block; non-HTMX
// callers see a styled error page (uses the same template).
//
// Uses HTTP 200 with the banner inside the response body —
// HTMX swaps on 200 by default, and the user-visible "this
// failed" signal is the banner copy, not the status code.
// Real 4xx (bad URL params) stays as http.Error.
func (s *Server) renderActionError(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Keep it tiny — a div with the message. The task page's
	// own re-render is the natural success path; for failure we
	// just want the user to see what went wrong without losing
	// what's on screen.
	if r.Header.Get("HX-Request") == "true" {
		_, _ = w.Write([]byte(`<div class="banner banner-error">` + htmlEscape(msg) + `</div>`))
		return
	}
	_, _ = w.Write([]byte(`<!doctype html><html><body><div class="banner banner-error">` + htmlEscape(msg) + `</div></body></html>`))
}

// htmlEscape is a tiny escape for error messages we render
// unescaped via byte concatenation. We don't trust msg to be
// safe — it includes server-side error strings that might
// contain user-controlled fragments.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}
