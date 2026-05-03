package service

// Per-project workspace orchestration that handlers call into.
// Covers the non-trivial bodies of enju_create_project,
// enju_init, enju_project_remote_status, enju_project_sync,
// enju_set_project_remote and enju_leave_project — the parts
// that touch git + filesystem locally and would otherwise need
// to live in the MCP handler files.
//
// Each method takes already-parsed inputs and returns
// already-structured outputs. Handlers stay responsible for
// MCP arg parsing, coord HTTP calls (project create / register
// / leave), and final text formatting; this file owns the
// "what do we do locally on disk + how do we project workspace
// state into the response" middle.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// DecorateProjectListWithPushStatus reads the coordinator's JSON
// project list and injects per-project last_push_at / last_push_error
// fields pulled from local clones that already exist on disk. Skips
// any project whose clone hasn't been materialized yet — keeps
// list_projects cheap (no fresh clones triggered as a side effect
// of a listing call). Returns the input bytes verbatim if the
// workspace is unset, the body doesn't unmarshal, or no
// decoration applied.
func (s *Session) DecorateProjectListWithPushStatus(data []byte) []byte {
	if s.workspace == nil {
		return data
	}
	var projects []map[string]interface{}
	if err := json.Unmarshal(data, &projects); err != nil {
		return data
	}
	changed := false
	for _, p := range projects {
		remoteURL, _ := p["remote_url"].(string)
		if remoteURL == "" {
			continue
		}
		var projectID int64
		if v, ok := p["id"].(float64); ok {
			projectID = int64(v)
		}
		if projectID == 0 {
			continue
		}
		if !s.workspace.HasLocalClone(projectID) {
			continue
		}
		pName, _ := p["name"].(string)
		proj, err := s.workspace.ForProject(projectID, remoteURL, pName)
		if err != nil {
			continue
		}
		if t := proj.LastPushAt(); !t.IsZero() {
			p["last_push_at"] = t.Format(time.RFC3339)
			changed = true
		}
		if e := proj.LastPushError(); e != "" {
			p["last_push_error"] = e
			changed = true
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(projects)
	if err != nil {
		return data
	}
	return out
}

// EagerInitProjectClone materializes the local workspace clone for
// a freshly-created project. Two paths:
//
//   - customPath set: the project's working tree IS the user's
//     chosen directory. Register it as external, then ForProject
//     opens it (git-init's if needed). Skips the default
//     ~/.enju/workspaces/ slug + the remote-clone path.
//   - customPath empty: pulls the coordinator's remote_url + name
//     and either clones (when a remote is set) or seeds a fresh
//     local working tree under ~/.enju/workspaces/<slug>-<id>/.
//
// Errors are returned but treated as warnings by callers (the
// project record is registered; the next tool call will retry the
// init/clone).
func (s *Session) EagerInitProjectClone(ctx context.Context, projectID int64, customPath string) error {
	if s.workspace == nil {
		return nil
	}
	if customPath != "" {
		s.workspace.RegisterExternalDir(projectID, customPath)
		_, err := s.workspace.ForProject(projectID, "")
		return err
	}
	remote, projName, _, err := s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return err
	}
	_, err = s.workspace.ForProject(projectID, remote, projName)
	return err
}

// DetectPopulatedUnrelatedRepo returns a non-empty refusal reason
// if dirPath looks like an unrelated user git repo that the
// caller almost certainly didn't mean to adopt: it has commits
// AND no Enju marker (enju/ subdirectory or enju/conf.yaml).
//
// The check is deliberately narrow — no heuristics about commit
// counts, file counts, or "looks like a code repo." Only two
// existence checks: HEAD resolves to a commit, and no enju
// marker is present. Fresh `git init` (no commits) passes.
// Previously-adopted Enju projects pass (their scaffold IS the
// marker).
//
// Returns "" if it's safe to proceed; non-empty string is the
// refusal reason for the caller to splice into a curative error.
func DetectPopulatedUnrelatedRepo(dirPath string) string {
	repo, err := gogit.PlainOpen(dirPath)
	if err != nil {
		return ""
	}
	if _, err := repo.Head(); err != nil {
		return ""
	}
	// Has commits. Check for Enju markers — either the scaffold
	// directory or a project conf file. Either signals "this
	// directory is already an Enju project, adoption is a re-
	// adoption (idempotent)."
	//
	// Type discrimination matters: in the enju repo itself, the
	// compiled binary is named `enju` (a regular file at repo
	// root), which would false-positive-match an "enju path
	// exists" check. Require the directory marker to be a
	// directory, and the YAML markers to be regular files.
	markers := []struct {
		rel   string
		isDir bool
	}{
		{"enju", true},
		{"enju/conf.yaml", false},
		{"enju.yaml", false},
	}
	for _, m := range markers {
		info, err := os.Stat(filepath.Join(dirPath, m.rel))
		if err != nil {
			continue
		}
		if info.IsDir() == m.isDir {
			return ""
		}
	}
	return fmt.Sprintf(
		"path %q is a populated git repo with no Enju metadata — refusing to adopt it as an Enju project to avoid accidentally writing into the wrong directory (common when the calling LLM is running inside a different project than the one being adopted)",
		dirPath,
	)
}

// InitDirAsProject performs the local git scaffolding for
// enju_init: open or create the repo, write the enju/ +
// enju/templates/ scaffold if missing, commit the scaffold,
// ensure at least one commit exists. Returns the adopted branch
// name (HEAD's short ref) so the caller can pass it as
// default_branch when registering with the coordinator. Empty
// adoptedBranch means HEAD couldn't be resolved (rare; falls
// back to coordinator default).
func (s *Session) InitDirAsProject(dirPath string) (adoptedBranch string, err error) {
	repo, openErr := gogit.PlainOpen(dirPath)
	if openErr != nil {
		var initErr error
		repo, initErr = gogit.PlainInitWithOptions(dirPath, &gogit.PlainInitOptions{
			InitOptions: gogit.InitOptions{
				DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
			},
		})
		if initErr != nil {
			return "", fmt.Errorf("git init failed: %w", initErr)
		}
		s.logger.Info("initialized git in existing folder", "path", dirPath)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("getting worktree: %w", err)
	}
	scaffoldWritten := false
	enjuDir := filepath.Join(dirPath, "enju")
	if _, err := os.Stat(enjuDir); os.IsNotExist(err) {
		os.MkdirAll(enjuDir, 0755)
		scaffoldWritten = true
	}
	templatesDir := filepath.Join(dirPath, corelayout.DefaultTemplatesDir)
	if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
		os.MkdirAll(templatesDir, 0755)
		os.WriteFile(filepath.Join(templatesDir, ".gitkeep"), []byte(""), 0644)
		scaffoldWritten = true
	}

	if scaffoldWritten {
		if err := wt.AddGlob("."); err != nil {
			s.logger.Warn("staging scaffold", "error", err)
		}
		status, _ := wt.Status()
		if !status.IsClean() {
			sig := &object.Signature{
				Name:  "Enju",
				Email: "enju@localhost",
				When:  time.Now(),
			}
			if _, commitErr := wt.Commit("Initialize Enju orchestration", &gogit.CommitOptions{
				Author:    sig,
				Committer: sig,
			}); commitErr != nil {
				s.logger.Warn("scaffold commit", "error", commitErr)
			} else {
				s.logger.Info("committed Enju scaffold", "path", dirPath)
			}
		}
	}

	// Ensure the repo has at least one commit (needed for clone).
	if _, err := repo.Head(); err != nil {
		wt.AddGlob(".")
		sig := &object.Signature{
			Name:  "Enju",
			Email: "enju@localhost",
			When:  time.Now(),
		}
		wt.Commit("initial commit", &gogit.CommitOptions{
			Author:    sig,
			Committer: sig,
		})
	}

	if head, herr := repo.Head(); herr == nil && head.Name().IsBranch() {
		adoptedBranch = head.Name().Short()
	}
	return adoptedBranch, nil
}

// RegisterAdoptedDir wires an existing on-disk directory into
// the workspace as the project's working tree. Used by enju_init
// after the coordinator has registered the project: marks the
// directory as external (so ForProject opens it directly instead
// of cloning), then opens it once to verify it works.
func (s *Session) RegisterAdoptedDir(projectID int64, dirPath string) error {
	if s.workspace == nil {
		return nil
	}
	s.workspace.RegisterExternalDir(projectID, dirPath)
	_, err := s.workspace.ForProject(projectID, "")
	return err
}

// RemoteStatusReport assembles the response payload for
// enju_project_remote_status. Opens the local clone, runs
// CompareToRemote, and returns the full field set the formatter
// expects (status, local/remote heads, ahead/behind counts,
// optional last_push_*, optional remote_error). Returns the
// no-remote payload directly when the project has no remote URL.
func (s *Session) RemoteStatusReport(ctx context.Context, projectID int64) (map[string]interface{}, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("remote status is only available in MCP client mode")
	}
	proj, remoteURL, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
	}
	if remoteURL == "" {
		resp["status"] = string(workspace.RemoteNoRemote)
		return resp, nil
	}
	cmp, err := proj.CompareToRemote()
	if err != nil {
		return nil, fmt.Errorf("comparing to remote: %w", err)
	}
	// For init'd projects, surface both the workspace path and
	// the actual git origin URL when they differ.
	if gitOrigin := proj.GitOriginURL(); gitOrigin != "" && gitOrigin != remoteURL {
		resp["workspace"] = remoteURL
		resp["remote_url"] = gitOrigin
	}
	resp["status"] = string(cmp.Status)
	resp["local_head"] = cmp.LocalHead
	resp["remote_head"] = cmp.RemoteHead
	resp["ahead_by"] = cmp.AheadBy
	resp["behind_by"] = cmp.BehindBy
	if cmp.Unreachable != "" {
		resp["remote_error"] = cmp.Unreachable
	}
	if t := proj.LastPushAt(); !t.IsZero() {
		resp["last_push_at"] = t.Format(time.RFC3339)
	}
	if e := proj.LastPushError(); e != "" {
		resp["last_push_error"] = e
	}
	return resp, nil
}

// SyncProjectToRemote runs the orchestration body of
// enju_project_sync: open the clone, lock it, preflight via
// CompareToRemote (refuse diverged state without force), push.
// Returns the response payload the formatter renders.
func (s *Session) SyncProjectToRemote(ctx context.Context, projectID int64, force bool) (map[string]interface{}, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("project sync is only available in MCP client mode")
	}
	proj, remoteURL, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if remoteURL == "" {
		return nil, fmt.Errorf("project has no remote configured")
	}
	proj.Lock()
	defer proj.Unlock()

	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
		"force":      force,
	}

	cmp, cmpErr := proj.CompareToRemote()
	if cmpErr == nil && cmp != nil {
		resp["status"] = string(cmp.Status)
		resp["local_head"] = cmp.LocalHead
		resp["remote_head"] = cmp.RemoteHead
		resp["ahead_by"] = cmp.AheadBy
		resp["behind_by"] = cmp.BehindBy

		switch cmp.Status {
		case workspace.RemoteInSync:
			resp["result"] = "noop"
			resp["message"] = "already in sync"
			return resp, nil
		case workspace.RemoteBehind:
			resp["result"] = "noop"
			resp["message"] = fmt.Sprintf("local is behind remote by %d commit(s); nothing to push — fetch+merge to catch up", cmp.BehindBy)
			return resp, nil
		case workspace.RemoteDiverged, workspace.RemoteUnrelated:
			if !force {
				resp["result"] = "refused"
				resp["message"] = fmt.Sprintf(
					"remote has diverged (local ahead by %d, behind by %d) — refuse to push without force=true; re-run with force=true to overwrite remote, or reconcile manually",
					cmp.AheadBy, cmp.BehindBy,
				)
				return resp, nil
			}
		}
	}

	var pushErr error
	if force {
		pushErr = proj.PushForce()
	} else {
		pushErr = proj.Push()
	}
	if pushErr != nil {
		resp["result"] = "failed"
		resp["error"] = pushErr.Error()
		return resp, nil
	}
	if force {
		resp["result"] = "force_pushed"
	} else {
		resp["result"] = "pushed"
	}
	return resp, nil
}

// MirrorRemoteAfterSet propagates a freshly-set remote URL into
// the existing local clone:
//
//  1. Update the on-disk origin URL so future pushes/fetches hit
//     the right place.
//  2. Push every local branch to it so the new bare contains all
//     the work that accumulated while the project was originless
//     (typical late-add scenario: project ran async compute with
//     no remote, commits stranded on local refs/heads/*).
//  3. Reset scan cursors for every local branch to the sentinel
//     that forces full-history rescans on next reconcile, so the
//     scanner re-emits historical trailers and the artifact index
//     catches up.
//
// Returns a warning message when push fails (non-fatal — remote
// is set, but seeding failed). Empty workspace is a no-op.
func (s *Session) MirrorRemoteAfterSet(projectID int64, remoteURL string) string {
	if s.workspace == nil {
		return ""
	}
	proj, err := s.workspace.ForProject(projectID, remoteURL)
	if err != nil {
		return ""
	}
	proj.Lock()
	defer proj.Unlock()

	_ = proj.SetRemote(remoteURL)

	var warning string
	if pushErr := proj.PushAllLocalBranches(); pushErr != nil {
		warning = fmt.Sprintf("\n⚠ Pushing local branches to new remote failed: %v", pushErr)
		s.logger.Warn("set_project_remote: push to new remote failed",
			"project_id", projectID, "remote", remoteURL, "error", pushErr)
	}

	// Cursor reset runs even on push failure: a partial push
	// (some branches landed) still wants retroactive scans on
	// those branches, and resetting branches whose remote refs
	// don't yet exist is harmless (the scanner returns empty
	// when the remote ref is missing, leaving the sentinel in
	// place for the next attempt).
	if branches, lerr := proj.LocalBranches(); lerr == nil {
		stateDir := s.StateDir()
		cursorMu := workspace.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := workspace.LoadCursors(stateDir, projectID)
		for _, b := range branches {
			cursors.Set(b, workspace.RescanSentinelSHA)
		}
		if serr := cursors.Save(); serr != nil {
			s.logger.Warn("set_project_remote: cursor reset save failed",
				"project_id", projectID, "error", serr)
		}
		cursorMu.Unlock()
	}
	return warning
}

// LocalLeaveProject wipes the project's local clone (best-effort)
// and reports whether one existed beforehand. Caller decides
// what to do with the membership row on the coordinator side.
func (s *Session) LocalLeaveProject(projectID int64) (hadClone bool, err error) {
	if s.workspace == nil {
		return false, nil
	}
	hadClone = s.workspace.HasLocalClone(projectID)
	if err := s.workspace.LeaveProject(projectID); err != nil {
		return hadClone, fmt.Errorf("removing local clone: %w", err)
	}
	return hadClone, nil
}

