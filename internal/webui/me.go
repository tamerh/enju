package webui

import (
	"net/http"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// "Me" surface — single page combining the calling citizen's
// dashboard (active claims + recent completions) + contribution
// stats + an inline edit-profile form. Mirrors:
//
//   enju_my_dashboard       — dashboard view
//   enju_my_profile         — contribution counters
//   enju_update_profile     — edit name/email
//
//   GET  /me           — render the page
//   POST /me/profile   — update profile (CSRF-gated)

// mePage is the data shape consumed by views/me.html.
//
// Either Dashboard or Contributions can be nil (best-effort
// fetches; we don't fail the whole page on a single endpoint
// error). Saved is the just-updated profile, set after a
// successful PUT so the page can confirm the change inline.
// Error / Submitted thread the failure path of the profile
// form (banner + repopulate).
type mePage struct {
	pageData
	Dashboard     *service.DashboardResponse
	Contributions *service.ContributionsResponse
	Saved         *service.CitizenResponse
	Error         string
	Submitted     service.UpdateProfileParams
	// Agents is the caller's agent roster (best-effort, like
	// Dashboard/Contributions). NewAgent is set ONLY on the
	// turn an agent was just registered — it carries the
	// one-time token, surfaced once and never re-fetched.
	// AgentError / SubmittedAgent thread the register-form
	// failure path (banner + repopulate).
	Agents         []service.AgentSummary
	NewAgent       *service.RegisterAgentResult
	AgentError     string
	SubmittedAgent service.RegisterAgentParams
}

// handleMe renders /me.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	s.renderMe(w, r, mePage{pageData: s.commonPageData()})
}

// handleUpdateProfile is POST /me/profile. Form fields:
// name, email. Both optional — empty values are sent as
// empty strings (i.e. clearing).
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	email := strings.TrimSpace(r.FormValue("email"))
	params := service.UpdateProfileParams{Name: &name, Email: &email}

	saved, err := s.fc.UpdateProfile(r.Context(), params)
	if err != nil {
		s.logger.Error("UpdateProfile failed", "error", err)
		s.renderMe(w, r, mePage{
			pageData:  s.commonPageData(),
			Error:     "update failed: " + err.Error(),
			Submitted: params,
		})
		return
	}
	s.renderMe(w, r, mePage{pageData: s.commonPageData(), Saved: saved})
}

// renderMe assembles the page state by fetching dashboard +
// contributions, then renders. Both fetches are best-effort:
// a single endpoint failure leaves its slot nil rather than
// erroring out the page (so the user can still edit profile
// even if the contributions endpoint is wedged).
//
// pre carries any pre-set fields (Saved, Error, Submitted) so
// callers can pass through write-action context to the render.
func (s *Server) renderMe(w http.ResponseWriter, r *http.Request, pre mePage) {
	username := s.fc.Username()
	dash, derr := s.fc.GetDashboard(r.Context())
	if derr != nil {
		s.logger.Warn("GetDashboard failed; rendering without dashboard",
			"error", derr)
	}
	contrib, cerr := s.fc.GetContributions(r.Context(), username)
	if cerr != nil {
		s.logger.Warn("GetContributions failed; rendering without contributions",
			"error", cerr)
	}
	// Agent roster — best-effort, same contract: a wedged
	// endpoint leaves the slot empty, not a dead page. Don't
	// clobber a roster the caller already supplied (none today,
	// but keeps the pre-pass-through contract honest).
	if pre.Agents == nil {
		agents, aerr := s.fc.ListMyAgents(r.Context())
		if aerr != nil {
			s.logger.Warn("ListMyAgents failed; rendering without agent roster",
				"error", aerr)
		} else {
			pre.Agents = agents
		}
	}
	pre.Dashboard = dash
	pre.Contributions = contrib
	s.render(w, r, "me.html", pre)
}

// handleRegisterAgent is POST /me/agents. Form: name (required),
// optional username / role / token-label. On success the page
// re-renders with the new agent's one-time token shown once
// (NewAgent) — there is no recovery path, so the reveal is
// prominent and the token is never re-fetched. On failure the
// form repopulates with the submitted values + an error banner.
func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	params := service.RegisterAgentParams{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Username: strings.TrimSpace(r.FormValue("username")),
		Role:     strings.TrimSpace(r.FormValue("role")),
		Label:    strings.TrimSpace(r.FormValue("label")),
	}
	if params.Name == "" {
		s.renderMe(w, r, mePage{
			pageData:       s.commonPageData(),
			AgentError:     "an agent name is required",
			SubmittedAgent: params,
		})
		return
	}
	res, err := s.fc.RegisterAgent(r.Context(), params)
	if err != nil {
		s.logger.Error("RegisterAgent failed", "error", err)
		s.renderMe(w, r, mePage{
			pageData:       s.commonPageData(),
			AgentError:     "register agent failed: " + err.Error(),
			SubmittedAgent: params,
		})
		return
	}
	s.renderMe(w, r, mePage{pageData: s.commonPageData(), NewAgent: res})
}
