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
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/project"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
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
func (s *FatClient) DecorateProjectListWithPushStatus(data []byte) []byte {
	if s.project == nil {
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
		if !s.enjugit.HasLocalClone(projectID) {
			continue
		}
		pName, _ := p["name"].(string)
		proj, err := s.project.ForProject(projectID, remoteURL, pName)
		if err != nil {
			continue
		}
		gc := proj.GitClone()
		t := gc.LastPushAt()
		if t.IsZero() {
			t = gc.HeadCommitTime()
		}
		if !t.IsZero() {
			p["last_push_at"] = t.Format(time.RFC3339)
			changed = true
		}
		if e := gc.LastPushError(); e != "" {
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

// CreateProjectParams bundles the inputs for creating a fresh
// project from in-process consumers (web UI). Mirrors the
// surface enju_create_project exposes: required name, optional
// description / default_branch / path.
//
// Path: when non-empty, the project's working tree IS this
// directory (filesystem-validated below — must be absolute,
// must be empty or non-existent, no symlinks). When empty the
// workspace lands at the standard ~/.enju/workspaces/{slug}-{id}/.
//
// RemoteURL: when non-empty, configured as `origin` on the
// fresh clone; every task-result commit is pushed there
// (GitHub/GitLab/Gitea/self-hosted). Mutually exclusive with
// Path — path-mode seeds a fresh local tree without a remote;
// remote-url-mode pulls from the URL.
type CreateProjectParams struct {
	Name          string
	Description   string
	DefaultBranch string
	Path          string
	RemoteURL     string
}

// CreateProjectResult bundles the coord response with the
// resolved project ID for the caller's redirect target.
type CreateProjectResult struct {
	CoordResponse []byte
	ProjectID     int64
}

// CreateProject creates a project on the coordinator and
// eagerly materializes the local workspace clone. Mirrors
// mcphandlers.handleCreateProject: optional Path makes the
// project's working tree be the user's chosen directory
// (filesystem-validated below); empty Path falls back to the
// standard ~/.enju/workspaces/ layout.
//
// Eager init is best-effort: if the project record is created
// on the coord but workspace init fails, we still return
// success with a logged warning. Subsequent tool calls retry.
func (s *FatClient) CreateProject(ctx context.Context, params CreateProjectParams) (*CreateProjectResult, error) {
	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if params.Path != "" && params.RemoteURL != "" {
		return nil, fmt.Errorf("path and remote_url are mutually exclusive — path seeds a fresh local working tree (no remote); remote_url clones the URL into the default workspace location")
	}
	if params.Path != "" {
		if err := validateCreateProjectPath(params.Path); err != nil {
			return nil, err
		}
	}
	body := map[string]string{
		"name":        params.Name,
		"description": params.Description,
	}
	if params.DefaultBranch != "" {
		body["default_branch"] = params.DefaultBranch
	}
	if params.RemoteURL != "" {
		body["remote_url"] = params.RemoteURL
	}
	data, err := s.coord.Post(ctx, "/api/v1/projects", body)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var result map[string]interface{}
	_ = json.Unmarshal(data, &result)
	pid := int64(0)
	if idF, ok := result["id"].(float64); ok {
		pid = int64(idF)
	}
	if pid > 0 && s.project != nil {
		if ierr := s.EagerInitProjectClone(ctx, pid, params.Path); ierr != nil {
			s.logger.Warn("eager workspace init failed (will retry on first task)",
				"project_id", pid, "path", params.Path, "error", ierr)
		}
	}
	return &CreateProjectResult{CoordResponse: data, ProjectID: pid}, nil
}

// validateCreateProjectPath enforces the same rules
// mcphandlers.handleCreateProject does:
//
//   - absolute path required
//   - no symlinks (Lstat — silently following would dual-root
//     the working tree if the link target is empty, or be a
//     confusing "not empty" error if populated)
//   - must be empty or non-existent (populated dirs go through
//     enju_init, not create_project)
//   - parent directories created on the empty path so the
//     fat-client's EagerInitProjectClone has somewhere to land
func validateCreateProjectPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path must be absolute, got %q", path)
	}
	info, lstatErr := os.Lstat(path)
	switch {
	case lstatErr == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q is a symlink — pass a real directory path. If you intended the link target, resolve it with `readlink -f` and pass that instead", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("path %q exists but is not a directory", path)
		}
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return fmt.Errorf("reading %q: %w", path, readErr)
		}
		if len(entries) > 0 {
			return fmt.Errorf("path %q exists and is not empty — create_project requires an empty or non-existent directory. To adopt a populated folder, use enju_init via MCP", path)
		}
	case os.IsNotExist(lstatErr):
		// Will be created by MkdirAll below.
	default:
		return fmt.Errorf("checking path %q: %w", path, lstatErr)
	}
	if mkErr := os.MkdirAll(path, 0755); mkErr != nil {
		return fmt.Errorf("creating %q: %w", path, mkErr)
	}
	return nil
}

// EagerInitProjectClone materializes the local working tree for
// a freshly-created project at the operator's chosen path. The
// project's working tree IS that directory: register it as
// external, then ForProject opens it (git-init's if needed).
//
// path is required — `enju_create_project` validates this at the
// MCP handler boundary, so a zero-value here is a programming
// error.
//
// Errors are returned but treated as warnings by callers (the
// project record is registered; the next tool call will retry
// the init).
func (s *FatClient) EagerInitProjectClone(ctx context.Context, projectID int64, path string) error {
	if s.project == nil {
		return nil
	}
	if path == "" {
		return fmt.Errorf("EagerInitProjectClone: path is required (programming error: handler should reject empty path)")
	}
	// Register in projectreg FIRST so the subsequent ForProject
	// call's registry lookup resolves to `path`. ForProject
	// consults the registry directly for path resolution;
	// without this ordering it would fall through to the
	// "clone-from-remote" branch with empty remoteURL.
	s.RegisterProject(projectreg.Entry{
		ID:        projectID,
		LocalPath: path,
	})
	_, err := s.project.ForProject(projectID, "")
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
func (s *FatClient) InitDirAsProject(dirPath string) (adoptedBranch string, err error) {
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
// after the coordinator has registered the project: writes the
// path to projectreg (durable), then opens it once via
// ForProject (which queries the registry) to verify the clone
// works.
func (s *FatClient) RegisterAdoptedDir(projectID int64, dirPath string) error {
	if s.project == nil {
		return nil
	}
	// Registry write FIRST — ForProject's path lookup consults
	// the registry, so the adoption must be durable before the
	// open call.
	s.RegisterProject(projectreg.Entry{
		ID:        projectID,
		LocalPath: dirPath,
	})
	_, err := s.project.ForProject(projectID, "")
	return err
}

// RemoteStatusReport assembles the response payload for
// enju_project_remote_status. Opens the local clone, runs
// CompareToRemote, and returns the full field set the formatter
// expects (status, local/remote heads, ahead/behind counts,
// optional last_push_*, optional remote_error). Returns the
// no-remote payload directly when the project has no remote URL.
func (s *FatClient) RemoteStatusReport(ctx context.Context, projectID int64) (map[string]interface{}, error) {
	if s.project == nil {
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
		resp["status"] = string(enjugit.AggregateNoRemote)
		return resp, nil
	}
	wf, _, _, _, werr := s.OpenWorkflow(ctx, projectID)
	if werr != nil {
		return nil, fmt.Errorf("opening workflow: %w", werr)
	}
	cmp, err := wf.CompareDefaultBranch("")
	if err != nil {
		return nil, fmt.Errorf("comparing to remote: %w", err)
	}
	// For init'd projects, surface both the workspace path and
	// the actual git origin URL when they differ.
	if gitOrigin := proj.GitClone().RemoteURL(); gitOrigin != "" && gitOrigin != remoteURL {
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
	gc := proj.GitClone()
	t := gc.LastPushAt()
	if t.IsZero() {
		t = gc.HeadCommitTime()
	}
	if !t.IsZero() {
		resp["last_push_at"] = t.Format(time.RFC3339)
	}
	if e := gc.LastPushError(); e != "" {
		resp["last_push_error"] = e
	}
	return resp, nil
}

// SyncProjectToRemote runs the orchestration body of
// enju_project_sync: open the clone, lock it, preflight via
// CompareToRemote (refuse diverged state without force), push.
// Returns the response payload the formatter renders.
func (s *FatClient) SyncProjectToRemote(ctx context.Context, projectID int64, force bool) (map[string]interface{}, error) {
	if s.project == nil {
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

	wf, _, _, _, werr := s.OpenWorkflow(ctx, projectID)
	if werr != nil {
		return nil, fmt.Errorf("opening workflow: %w", werr)
	}
	cmp, cmpErr := wf.CompareDefaultBranch("")
	if cmpErr == nil && cmp != nil {
		resp["status"] = string(cmp.Status)
		resp["local_head"] = cmp.LocalHead
		resp["remote_head"] = cmp.RemoteHead
		resp["ahead_by"] = cmp.AheadBy
		resp["behind_by"] = cmp.BehindBy

		switch cmp.Status {
		case enjugit.AggregateInSync:
			resp["result"] = "noop"
			resp["message"] = "already in sync"
			return resp, nil
		case enjugit.AggregateBehind:
			resp["result"] = "noop"
			resp["message"] = fmt.Sprintf("local is behind remote by %d commit(s); nothing to push — fetch+merge to catch up", cmp.BehindBy)
			return resp, nil
		case enjugit.AggregateDiverged, enjugit.AggregateUnrelated:
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

	pushErr := proj.GitClone().PushAllRefs(force)
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
func (s *FatClient) MirrorRemoteAfterSet(projectID int64, remoteURL string) string {
	if s.enjugit == nil {
		return ""
	}
	wf, err := s.enjugit.ForProject(projectID, remoteURL)
	if err != nil {
		return ""
	}
	_ = wf.SetRemote(remoteURL)

	var warning string
	if pushErr := wf.PushAllRefs(false); pushErr != nil {
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
	if branches, lerr := wf.LocalBranches(); lerr == nil {
		stateDir := s.StateDir()
		cursorMu := project.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := project.LoadCursors(stateDir, projectID)
		for _, b := range branches {
			cursors.Set(b, enjugit.RescanSentinelSHA)
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
func (s *FatClient) LocalLeaveProject(projectID int64) (hadClone bool, err error) {
	if s.project == nil {
		return false, nil
	}
	hadClone = s.enjugit.HasLocalClone(projectID)
	s.project.EvictProjectCache(projectID)
	if err := s.enjugit.LeaveProject(projectID); err != nil {
		return hadClone, fmt.Errorf("removing local clone: %w", err)
	}
	s.UnregisterProject(projectID)
	return hadClone, nil
}

