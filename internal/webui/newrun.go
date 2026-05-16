package webui

import (
	"net/http"
	"strconv"
	"strings"

	enjuyaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// newRunPage backs views/new-run.html. SubmittedYAML /
// SubmittedBranch repopulate the form on a validation or create
// failure so a long paste isn't lost. At most one of Error is
// set; Warnings are non-fatal yaml.Parse advisories surfaced
// alongside a validation failure (or shown when the user asked
// to validate only — see handleNewRun).
type newRunPage struct {
	pageData
	ProjectID       int64
	SubmittedYAML   string
	SubmittedBranch string
	Error           string
	Warnings        []string
	// Validated is true after a successful parse with no create
	// attempted (the "Validate" button) — drives a green "looks
	// good" confirmation without leaving the page.
	Validated bool
}

// handleNewRunForm renders GET /p/{projectID}/new-run — an
// empty paste-a-workflow form. This is the UI's authoring
// entry point: inline YAML, validated locally before it ever
// reaches the coordinator (mirror of enju_create_run yaml=
// mode). Parameterized workflows still go through templates;
// this is the straightforward "I have an enju.yaml, run it"
// path.
func (s *Server) handleNewRunForm(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	s.render(w, r, "new-run.html", newRunPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
	})
}

// handleNewRun is POST /p/{projectID}/new-run. The form carries
// `yaml` (required), an optional `branch`, and an `action` that
// is either "validate" (parse only, stay on the page) or
// "create" (parse, then create the run and redirect to it).
//
// Validation always runs first via yaml.Parse — the same parser
// the coordinator uses — so authoring mistakes surface as a
// precise local error instead of an opaque coord rejection.
func (s *Server) handleNewRun(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	yamlContent := r.FormValue("yaml")
	branch := strings.TrimSpace(r.FormValue("branch"))
	createRequested := r.FormValue("action") == "create"

	base := newRunPage{
		pageData:        s.commonPageData(),
		ProjectID:       pid,
		SubmittedYAML:   yamlContent,
		SubmittedBranch: branch,
	}

	if strings.TrimSpace(yamlContent) == "" {
		base.Error = "paste a workflow YAML definition first"
		s.render(w, r, "new-run.html", base)
		return
	}

	// Validate with the coordinator's own parser. Parse errors
	// are authoring mistakes — surface them precisely and keep
	// the paste on screen.
	parsed, perr := enjuyaml.Parse([]byte(yamlContent))
	if perr != nil {
		base.Error = "invalid workflow YAML: " + perr.Error()
		s.render(w, r, "new-run.html", base)
		return
	}
	if parsed != nil {
		base.Warnings = parsed.Warnings
	}

	if !createRequested {
		// Validate-only: confirm in place.
		base.Validated = true
		s.render(w, r, "new-run.html", base)
		return
	}

	authorName, authorEmail := s.fc.CommitAuthor(r.Context())
	res, cerr := s.fc.CreateRunFromYAML(r.Context(), pid, yamlContent, nil, branch, authorName, authorEmail)
	if cerr != nil {
		s.logger.Error("CreateRunFromYAML failed", "project_id", pid, "error", cerr)
		base.Error = "create run failed: " + cerr.Error()
		s.render(w, r, "new-run.html", base)
		return
	}

	// Land the user on the run they just created (same redirect
	// shape as create-run-from-template).
	target := "/p/" + strconv.FormatInt(pid, 10) + "/runs"
	if seq := service.RunSeqFromCreateResponse(res.CoordResponse); seq != 0 {
		target = "/p/" + strconv.FormatInt(pid, 10) + "/r/" + strconv.Itoa(seq)
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
