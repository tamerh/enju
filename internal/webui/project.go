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

// projectPage backs views/project.html — the OVERVIEW surface.
// Runs-first (the reason you visit a project); a compact
// read-only members strip for context; no admin controls. All
// project administration moved to the Settings page so the
// everyday view stays uncluttered (the everyday-loop-vs-admin
// split, applied to layout).
type projectPage struct {
	pageData
	Project *service.ProjectDetail
	Runs    []wire.Run
}

// projectSettingsPage backs views/project-settings.html — the
// ADMIN surface (members management, general config, remote
// maintenance, danger zone). Owner-gated controls; non-owners
// see read-only state. Notice / NoticeError carry the outcome
// of a settings write inline on the re-rendered page (the
// project write handlers all re-render here now, not the
// overview).
type projectSettingsPage struct {
	pageData
	Project      *service.ProjectDetail
	Notice       string
	NoticeError  string
	IsOwner      bool
	RemoteStatus string
}

// handleProjectView renders /p/{projectID} — the overview:
// runs + a read-only members strip. Pure read, no write
// actions live here anymore.
func (s *Server) handleProjectView(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	s.renderProjectOverview(w, r, pid)
}

// handleProjectSettings renders GET /p/{projectID}/settings —
// the admin surface. Also the re-render target for every
// project write handler (membership, branch, remote, sync,
// leave-keep) so their outcome banners land where the forms are.
func (s *Server) handleProjectSettings(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	s.renderProjectSettings(w, r, pid, "", "")
}

// loadProjectAndOwner fetches the project and computes whether
// the caller is an owner — shared by both renderers. On a hard
// fetch error it writes the response itself and returns
// ok=false (callers must not write after).
func (s *Server) loadProjectAndOwner(w http.ResponseWriter, r *http.Request, pid int64) (proj *service.ProjectDetail, isOwner, ok bool) {
	proj, err := s.fc.GetProject(r.Context(), pid)
	if err != nil {
		s.logger.Error("GetProject failed", "project_id", pid, "error", err)
		s.writeFetchError(w, "project", err)
		return nil, false, false
	}
	if proj == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return nil, false, false
	}
	me := s.fc.Username()
	for _, m := range proj.Members {
		if m.Username == me && m.Role == "owner" {
			isOwner = true
			break
		}
	}
	return proj, isOwner, true
}

// renderProjectOverview renders the runs-first overview.
func (s *Server) renderProjectOverview(w http.ResponseWriter, r *http.Request, pid int64) {
	proj, _, ok := s.loadProjectAndOwner(w, r, pid)
	if !ok {
		return
	}
	runs, err := s.fc.ListRuns(r.Context(), pid)
	if err != nil {
		s.logger.Warn("ListRuns failed; rendering with empty list",
			"project_id", pid, "error", err)
		runs = nil
	}
	// Newest first — most-recent runs are what users care about
	// when they land here. Sorted client-side so we don't depend
	// on the coord's ordering promise.
	sort.Slice(runs, func(i, j int) bool { return runs[i].Seq > runs[j].Seq })
	s.render(w, r, "project.html", projectPage{
		pageData: s.commonPageData(),
		Project:  proj,
		Runs:     runs,
	})
}

// renderProjectSettings renders the admin surface. notice /
// noticeErr carry a just-performed write's outcome. On a hard
// fetch error it writes the response itself and returns.
func (s *Server) renderProjectSettings(w http.ResponseWriter, r *http.Request, pid int64, notice, noticeErr string) {
	proj, isOwner, ok := s.loadProjectAndOwner(w, r, pid)
	if !ok {
		return
	}
	// Remote status is a read-only local-vs-remote comparison.
	// Best-effort, same contract as the untracked-artifact
	// panel: only meaningful with a remote + a local workspace;
	// any error just omits the line rather than failing the page.
	var remoteStatus string
	if proj.RemoteURL != "" {
		if rpt, rerr := s.fc.RemoteStatusReport(r.Context(), pid); rerr != nil {
			s.logger.Info("RemoteStatusReport unavailable; omitting line",
				"project_id", pid, "error", rerr)
		} else if data, merr := json.Marshal(rpt); merr == nil {
			remoteStatus = format.ProjectRemoteStatus(data)
		}
	}
	s.render(w, r, "project-settings.html", projectSettingsPage{
		pageData:     s.commonPageData(),
		Project:      proj,
		Notice:       notice,
		NoticeError:  noticeErr,
		IsOwner:      isOwner,
		RemoteStatus: remoteStatus,
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
		s.renderProjectSettings(w, r, pid, "", serr.Error())
		return
	}
	data, _ := json.Marshal(resp)
	s.renderProjectSettings(w, r, pid, format.ProjectSyncResult(data), "")
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
		s.renderProjectSettings(w, r, pid, "", "username is required to add a member")
		return
	}
	role := strings.TrimSpace(r.FormValue("role"))
	if err := s.fc.AddProjectMember(r.Context(), pid, username, role); err != nil {
		s.logger.Info("AddProjectMember failed", "project_id", pid, "username", username, "error", err)
		s.renderProjectSettings(w, r, pid, "", "add member failed: "+err.Error())
		return
	}
	shown := role
	if shown == "" {
		shown = "member"
	}
	s.renderProjectSettings(w, r, pid,
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
		s.renderProjectSettings(w, r, pid, "", "username is required")
		return
	}
	if err := s.fc.RemoveProjectMember(r.Context(), pid, username); err != nil {
		s.logger.Info("RemoveProjectMember failed", "project_id", pid, "username", username, "error", err)
		s.renderProjectSettings(w, r, pid, "", "remove member failed: "+err.Error())
		return
	}
	s.renderProjectSettings(w, r, pid, fmt.Sprintf("✓ Removed @%s from the project", username), "")
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
		s.renderProjectSettings(w, r, pid, "", `role must be "owner" or "member"`)
		return
	}
	changed, err := s.fc.SetProjectMemberRole(r.Context(), pid, username, role)
	if err != nil {
		s.logger.Info("SetProjectMemberRole failed", "project_id", pid, "username", username, "role", role, "error", err)
		s.renderProjectSettings(w, r, pid, "", "role change failed: "+err.Error())
		return
	}
	if !changed {
		s.renderProjectSettings(w, r, pid,
			fmt.Sprintf("• @%s is already %s — no change", username, role), "")
		return
	}
	verb := "promoted to owner"
	if role == "member" {
		verb = "demoted to member"
	}
	s.renderProjectSettings(w, r, pid, fmt.Sprintf("✓ @%s %s", username, verb), "")
}

// handleSetProjectDefaultBranch is POST
// /p/{projectID}/default-branch (mirror of
// enju_set_project_default_branch). The service composes the
// coord update with a local materialize; a non-empty warning is
// appended to the success banner (coord update still landed).
func (s *Server) handleSetProjectDefaultBranch(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	branch := strings.TrimSpace(r.FormValue("branch"))
	if branch == "" {
		s.renderProjectSettings(w, r, pid, "", "branch is required")
		return
	}
	warning, err := s.fc.SetProjectDefaultBranch(r.Context(), pid, branch)
	if err != nil {
		s.logger.Info("SetProjectDefaultBranch failed", "project_id", pid, "branch", branch, "error", err)
		s.renderProjectSettings(w, r, pid, "", "set default branch failed: "+err.Error())
		return
	}
	msg := fmt.Sprintf("✓ Default branch set to %q", branch)
	if warning != "" {
		msg += " — ⚠ " + warning
	}
	s.renderProjectSettings(w, r, pid, msg, "")
}

// handleSetProjectRemote is POST /p/{projectID}/remote (mirror
// of enju_set_project_remote). Empty remote_url is refused by
// the service (clearing a remote bifurcates multi-machine
// teams). A non-empty warning from the local-mirror seed step
// is appended to the success banner.
func (s *Server) handleSetProjectRemote(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	remoteURL := strings.TrimSpace(r.FormValue("remote_url"))
	if remoteURL == "" {
		s.renderProjectSettings(w, r, pid, "", "remote_url is required")
		return
	}
	warning, err := s.fc.SetProjectRemote(r.Context(), pid, remoteURL)
	if err != nil {
		s.logger.Info("SetProjectRemote failed", "project_id", pid, "error", err)
		s.renderProjectSettings(w, r, pid, "", "set remote failed: "+err.Error())
		return
	}
	msg := fmt.Sprintf("✓ Remote set to %s", remoteURL)
	if warning != "" {
		msg += " — ⚠ " + warning
	}
	s.renderProjectSettings(w, r, pid, msg, "")
}

// handleArchiveProject / handleRestoreProject are
// POST /p/{projectID}/archive | /restore (mirror of
// enju_archive_project / enju_restore_project). Owner-gating
// and the non-terminal-run precondition are coord-enforced; a
// refusal or an idempotent no-op is bannered on the re-rendered
// settings page (proj.Archived flips so the Danger-zone block
// swaps Archive↔Restore).
func (s *Server) handleArchiveProject(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, true)
}

func (s *Server) handleRestoreProject(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, false)
}

func (s *Server) setArchived(w http.ResponseWriter, r *http.Request, archive bool) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	res, err := s.fc.SetProjectArchived(r.Context(), pid, archive)
	if err != nil {
		verb := "archive"
		if !archive {
			verb = "restore"
		}
		s.logger.Info("SetProjectArchived failed", "project_id", pid, "archive", archive, "error", err)
		s.renderProjectSettings(w, r, pid, "", verb+" failed: "+err.Error())
		return
	}
	var notice string
	switch res.Status {
	case "archived":
		notice = "✓ Project archived — hidden from the default project list; restore it here anytime."
	case "restored":
		notice = "✓ Project restored — back in the default project list."
	case "already_archived":
		notice = "• Project is already archived (no change)."
	case "already_restored":
		notice = "• Project is not archived (no change)."
	default:
		notice = "✓ Done."
	}
	s.renderProjectSettings(w, r, pid, notice, "")
}

// handleLeaveProject is POST /p/{projectID}/leave (mirror of
// enju_leave_project). Destructive: removes the caller's
// membership and wipes the local clone. `keep_membership`
// (checkbox) wipes the clone only. A full leave makes the
// project inaccessible to this user, so on success we redirect
// to the landing page rather than re-render a project the
// caller can no longer see; keep-membership stays on the page
// with a notice. A sole-owner refusal (or any error) re-renders
// the project with the error banner.
func (s *Server) handleLeaveProject(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.projectIDOrBadRequest(w, r)
	if !ok {
		return
	}
	keep := r.FormValue("keep_membership") == "true" || r.FormValue("keep_membership") == "on"
	summary, err := s.fc.LeaveProject(r.Context(), pid, keep)
	if err != nil {
		s.logger.Info("LeaveProject failed", "project_id", pid, "keep_membership", keep, "error", err)
		s.renderProjectSettings(w, r, pid, "", "leave project failed: "+err.Error())
		return
	}
	if keep {
		// Membership intact — the project is still viewable.
		s.renderProjectSettings(w, r, pid, "✓ "+summary, "")
		return
	}
	// Membership gone: the project page would 4xx for this
	// user now. Redirect to the landing list. Mirror the
	// HX-Redirect / 303 pattern used by file-issue + create.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
