package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// projectPage is the data shape consumed by views/project.html.
// Embeds pageData for {{.Username}}; carries the project detail
// (name, description, members, etc.) and the run list shown
// inline as the page's primary content.
//
// Tabs (Inbox / Runs / Members / Settings) are deferred to the
// iteration that ships their own pages — until then this page
// is the project's single overview surface.
type projectPage struct {
	pageData
	Project *service.ProjectDetail
	Runs    []wire.Run
	// Notice / NoticeError surface the outcome of any
	// project-page write (sync, membership change, …) inline on
	// the re-rendered page. Notice is the success one-liner
	// (e.g. the format.ProjectSyncResult text the CLI prints);
	// NoticeError is a hard failure. At most one is non-empty.
	Notice      string
	NoticeError string
	// IsOwner is true when the calling citizen holds the owner
	// role on this project. Gates the membership-management
	// controls in the template — non-owners still see the
	// roster, just no add/remove/promote/demote (the coord
	// would reject them anyway; hiding avoids a misleading UI).
	IsOwner bool
}

// handleProjectView renders /p/{projectID} — project overview
// with header + members + runs list. Two FatClient calls
// (GetProject, ListRuns) issued sequentially. Parallelizing
// is mechanical (errgroup) when latency justifies it.
//
// Bad project ID parses → 400. Coord 4xx (not a member, not
// found) surfaces as 502 today; refining to 404/403 with
// typed errors from FatClient is a follow-up.
func (s *Server) handleProjectView(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	s.renderProjectPage(w, r, pid, "", "")
}

// renderProjectPage does the GetProject + ListRuns fetch and
// renders project.html. Shared by handleProjectView (no banner)
// and the project-page write handlers (sync, membership), which
// pass their outcome as notice / noticeErr. On a hard fetch
// error it writes the error response itself and returns —
// callers must not write after calling it.
func (s *Server) renderProjectPage(w http.ResponseWriter, r *http.Request, pid int64, notice, noticeErr string) {
	ctx := r.Context()
	proj, err := s.fc.GetProject(ctx, pid)
	if err != nil {
		s.logger.Error("GetProject failed", "project_id", pid, "error", err)
		http.Error(w, "failed to load project: "+err.Error(), http.StatusBadGateway)
		return
	}
	if proj == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	runs, err := s.fc.ListRuns(ctx, pid)
	if err != nil {
		s.logger.Warn("ListRuns failed; rendering with empty list",
			"project_id", pid, "error", err)
		runs = nil
	}
	// Newest first — most-recent runs are what users care
	// about when they land on the project page. Sorted
	// client-side so we don't depend on the coord's ordering
	// promise.
	sort.Slice(runs, func(i, j int) bool { return runs[i].Seq > runs[j].Seq })

	// Owner gate for the membership-management controls. The
	// coord is the real authority (it rejects non-owner writes);
	// this just keeps the UI honest about what the viewer can do.
	me := s.fc.Username()
	isOwner := false
	for _, m := range proj.Members {
		if m.Username == me && m.Role == "owner" {
			isOwner = true
			break
		}
	}
	s.render(w, r, "project.html", projectPage{
		pageData:    s.commonPageData(),
		Project:     proj,
		Runs:        runs,
		Notice:      notice,
		NoticeError: noticeErr,
		IsOwner:     isOwner,
	})
}

// handleProjectSync is POST /p/{projectID}/sync (mirror of
// enju_project_sync): push local HEAD to the coord-known
// remote. `force` (checkbox) does a destructive force-push when
// histories diverged; default is the safe fast-forward-only
// push. The result map is rendered through the same
// format.ProjectSyncResult the CLI uses, then the project page
// re-renders with that one-liner as a banner. A hard error
// (notably "no remote configured" — common post-Phase-8 where
// no-origin is first-class) becomes a friendly notice-error
// banner rather than a 5xx.
func (s *Server) handleProjectSync(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	force := r.FormValue("force") == "true" || r.FormValue("force") == "on"
	resp, serr := s.fc.SyncProjectToRemote(r.Context(), pid, force)
	if serr != nil {
		s.logger.Info("SyncProjectToRemote returned error",
			"project_id", pid, "force", force, "error", serr)
		s.renderProjectPage(w, r, pid, "", serr.Error())
		return
	}
	data, _ := json.Marshal(resp)
	s.renderProjectPage(w, r, pid, format.ProjectSyncResult(data), "")
}

// projectIDOrBadRequest parses {projectID}; on a bad value it
// writes 400 and returns ok=false. Shared by the membership
// write handlers, which all start the same way.
func (s *Server) projectIDOrBadRequest(w http.ResponseWriter, r *http.Request) (int64, bool) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return 0, false
	}
	return pid, true
}

// handleAddProjectMember is POST /p/{projectID}/members (mirror
// of enju_add_project_member). Form: username (required), role
// (optional — coord defaults to "member"). Owner-only is
// enforced coord-side; a non-owner caller gets the coord error
// surfaced as a notice banner, not a 5xx.
func (s *Server) handleAddProjectMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		s.renderProjectPage(w, r, pid, "", "username is required to add a member")
		return
	}
	role := strings.TrimSpace(r.FormValue("role"))
	if err := s.fc.AddProjectMember(r.Context(), pid, username, role); err != nil {
		s.logger.Info("AddProjectMember failed", "project_id", pid, "username", username, "error", err)
		s.renderProjectPage(w, r, pid, "", "add member failed: "+err.Error())
		return
	}
	shown := role
	if shown == "" {
		shown = "member"
	}
	s.renderProjectPage(w, r, pid,
		fmt.Sprintf("✓ Added @%s to the project as %s", username, shown), "")
}

// handleRemoveProjectMember is POST
// /p/{projectID}/members/{username}/remove (mirror of
// enju_remove_project_member). Owner-only coord-side.
func (s *Server) handleRemoveProjectMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	username := chi.URLParam(r, "username")
	if username == "" {
		s.renderProjectPage(w, r, pid, "", "username is required")
		return
	}
	if err := s.fc.RemoveProjectMember(r.Context(), pid, username); err != nil {
		s.logger.Info("RemoveProjectMember failed", "project_id", pid, "username", username, "error", err)
		s.renderProjectPage(w, r, pid, "", "remove member failed: "+err.Error())
		return
	}
	s.renderProjectPage(w, r, pid, fmt.Sprintf("✓ Removed @%s from the project", username), "")
}

// handleSetProjectMemberRole is POST
// /p/{projectID}/members/{username}/role (mirror of
// enju_promote_member / enju_demote_owner). Form: role —
// "owner" promotes, "member" demotes. Coord reports a no-op
// when the member already holds that role.
func (s *Server) handleSetProjectMemberRole(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	username := chi.URLParam(r, "username")
	role := strings.TrimSpace(r.FormValue("role"))
	if role != "owner" && role != "member" {
		s.renderProjectPage(w, r, pid, "", `role must be "owner" or "member"`)
		return
	}
	changed, err := s.fc.SetProjectMemberRole(r.Context(), pid, username, role)
	if err != nil {
		s.logger.Info("SetProjectMemberRole failed", "project_id", pid, "username", username, "role", role, "error", err)
		s.renderProjectPage(w, r, pid, "", "role change failed: "+err.Error())
		return
	}
	if !changed {
		s.renderProjectPage(w, r, pid,
			fmt.Sprintf("• @%s is already %s — no change", username, role), "")
		return
	}
	verb := "promoted to owner"
	if role == "member" {
		verb = "demoted to member"
	}
	s.renderProjectPage(w, r, pid, fmt.Sprintf("✓ @%s %s", username, verb), "")
}
