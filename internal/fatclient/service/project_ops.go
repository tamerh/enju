package service

// Per-project workspace orchestration that handlers call into.
// Covers the non-trivial bodies of enju_create_project,
// enju_project_remote_status, enju_project_sync,
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

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
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
	if s.enjugit == nil {
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
		wf, err := s.enjugit.ForProject(projectID, remoteURL, pName)
		if err != nil {
			continue
		}
		t := wf.LastPushAt()
		if t.IsZero() {
			t = wf.HeadCommitTime()
		}
		if !t.IsZero() {
			p["last_push_at"] = t.Format(time.RFC3339)
			changed = true
		}
		if e := wf.LastPushError(); e != "" {
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
// Path: the project's working tree IS this directory
// (filesystem-validated below — must be absolute, no symlinks,
// no nested git repos under populated dirs without `force`).
// Post-NDW.2 path-anchoring is mandatory in practice: omitting
// Path creates the coord project record but leaves no local
// clone for the fat-client to operate on (every subsequent
// workspace op errors ErrProjectNotRegistered until the operator
// re-runs with path=<abs/dir>).
//
// Smart-detect: when Path points at an existing folder,
// CreateProject inspects it and dispatches:
//   - empty / nonexistent → git init + seed + register
//   - populated, no .git  → git init + commit existing files +
//     register (Force gates the populated-unrelated-repo
//     safety check)
//   - .git, no origin     → register the dir as-is
//   - .git with origin    → register only; user's remote stays
//
// RemoteURL: when non-empty AND Path is empty, registered as
// origin metadata on the coord project record only. Mutually
// exclusive with Path — Path adopts an existing tree;
// RemoteURL by itself doesn't materialize a clone (the
// silent-clone behavior is gone post-NDW.5).
//
// Force: bypasses the populated-unrelated-repo safety check
// (a `.git` directory with commits and no Enju marker). Default
// behavior refuses such adoptions to avoid the LLM-typoed-path
// footgun.
type CreateProjectParams struct {
	Name          string
	Description   string
	DefaultBranch string
	Path          string
	RemoteURL     string
	Force         bool
}

// CreateProjectResult bundles the coord response with the
// resolved project ID for the caller's redirect target.
type CreateProjectResult struct {
	CoordResponse []byte
	ProjectID     int64
	// InitWarning carries a non-fatal local-state warning the
	// MCP handler should surface to the operator. Most common
	// case: local clone materialization hit a transient error.
	// The coord project was created, but the local clone wasn't
	// fully materialized; the operator needs to retry or fix
	// the on-disk inconsistency before the project is usable.
	InitWarning string
}

// CreateProject creates a project on the coordinator and
// eagerly materializes the local clone at the operator's chosen
// Path. Post-NDW.5 there is no "default workspace location"
// fallback — operators always supply Path to make the project
// usable locally.
//
// Eager init is best-effort: if the project record is created
// on the coord but local init fails, we still return success
// with a logged warning. Subsequent tool calls retry.
func (s *FatClient) CreateProject(ctx context.Context, params CreateProjectParams) (*CreateProjectResult, error) {
	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if params.Path != "" && params.RemoteURL != "" {
		return nil, fmt.Errorf("path and remote_url are mutually exclusive — path adopts an existing folder (or creates one); remote_url clones the URL into the default workspace location")
	}
	var target *AdoptionTarget
	if params.Path != "" {
		var verr error
		target, verr = validateAndInspectPath(params.Path, params.Force, s.alreadyAdoptedByEnju)
		if verr != nil {
			return nil, verr
		}
	}
	body := map[string]string{
		"name":        params.Name,
		"description": params.Description,
	}
	// Coord registration: when an existing repo already has an
	// origin, surface that to coord so it can render
	// last_push_status etc. Cases 1-4 leave coord-side empty;
	// case 5 (.git + origin) records the real URL. Caller-
	// supplied params.RemoteURL takes precedence (the clone-fresh
	// path).
	switch {
	case params.RemoteURL != "":
		body["remote_url"] = params.RemoteURL
	case target != nil && target.OriginURL != "":
		body["remote_url"] = target.OriginURL
	}
	switch {
	case params.DefaultBranch != "":
		body["default_branch"] = params.DefaultBranch
	case target != nil && target.DefaultBranch != "":
		body["default_branch"] = target.DefaultBranch
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
	var initWarning string
	if pid > 0 && s.enjugit != nil && params.Path != "" {
		if ierr := s.EagerInitProjectClone(ctx, pid, params.Path, target); ierr != nil {
			s.logger.Warn("eager workspace init failed (will retry on first task)",
				"project_id", pid, "path", params.Path, "error", ierr)
			initWarning = ierr.Error()
		}
	}
	return &CreateProjectResult{CoordResponse: data, ProjectID: pid, InitWarning: initWarning}, nil
}

// AdoptionTarget classifies the on-disk state of a directory the
// caller wants to adopt as an Enju project. Populated by
// InspectAdoptionTarget; consumed by CreateProject + the
// materialize helpers to dispatch the right branch of the
// adopt-or-init logic.
type AdoptionTarget struct {
	// Path is the absolute directory path the caller passed.
	Path string

	// Existed is true when the path existed before InspectAdoptionTarget
	// ran (the inspector mkdir's missing paths).
	Existed bool

	// HasFiles is true when the directory contains any entries
	// other than `.git/`. False on fresh git inits and empty dirs.
	HasFiles bool

	// HasGit is true when `.git` is present (directory or file —
	// gogit treats them equivalently for PlainOpen).
	HasGit bool

	// OriginURL is the configured `origin` remote URL when present,
	// or "" when no origin is configured (or no .git at all).
	OriginURL string

	// DefaultBranch is HEAD's short-ref name when the existing repo
	// has commits, e.g. "main" or "trunk". Empty for fresh git
	// inits and non-git directories.
	DefaultBranch string
}

// alreadyAdoptedByEnju reports whether `path` is already a
// known project in the fat-client's registry. Used by the
// adoption safety gate to skip the "populated unrelated repo"
// refusal on idempotent re-create.
func (s *FatClient) alreadyAdoptedByEnju(path string) bool {
	if s.projectRegistry == nil {
		return false
	}
	entries, err := s.projectRegistry.List()
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.LocalPath == path {
			return true
		}
	}
	return false
}

// validateAndInspectPath validates the path and classifies its
// adoption state. Refuses symlinks and non-directories outright.
// For directories that already contain a `.git` with commits but
// no Enju metadata, refuses unless force=true (the LLM-typoed-
// path footgun) OR alreadyAdopted reports the path was previously
// adopted by this fat-client (idempotent re-create on a path enju
// already manages — registry knows about it).
//
// Side effect: creates the directory when it doesn't exist, so
// the subsequent materialize step has somewhere to land.
func validateAndInspectPath(path string, force bool, alreadyAdopted func(string) bool) (*AdoptionTarget, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path must be absolute, got %q", path)
	}
	target := &AdoptionTarget{Path: path}
	info, lstatErr := os.Lstat(path)
	switch {
	case lstatErr == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path %q is a symlink — pass a real directory path. If you intended the link target, resolve it with `readlink -f` and pass that instead", path)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("path %q exists but is not a directory", path)
		}
		target.Existed = true
	case os.IsNotExist(lstatErr):
		// Will be created by MkdirAll below.
	default:
		return nil, fmt.Errorf("checking path %q: %w", path, lstatErr)
	}
	if mkErr := os.MkdirAll(path, 0755); mkErr != nil {
		return nil, fmt.Errorf("creating %q: %w", path, mkErr)
	}
	if target.Existed {
		if err := inspectExistingDir(path, target); err != nil {
			return nil, err
		}
	}
	// Footgun gate: existing `.git` with commits, no Enju marker.
	// Force=true is the operator's explicit override; an already-
	// adopted path is implicitly safe (idempotent re-create).
	if target.HasGit && !force && (alreadyAdopted == nil || !alreadyAdopted(path)) {
		if reason := DetectPopulatedUnrelatedRepo(path); reason != "" {
			return nil, fmt.Errorf("%s. To adopt this directory anyway, re-invoke enju_create_project with force=true", reason)
		}
	}
	return target, nil
}

// inspectExistingDir fills in HasFiles, HasGit, OriginURL,
// DefaultBranch on target by reading the directory entries and
// (if a .git is present) opening the repo for origin + HEAD.
func inspectExistingDir(path string, target *AdoptionTarget) error {
	entries, readErr := os.ReadDir(path)
	if readErr != nil {
		return fmt.Errorf("reading %q: %w", path, readErr)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			target.HasGit = true
			continue
		}
		target.HasFiles = true
	}
	if !target.HasGit {
		return nil
	}
	info, openErr := enjugit.InspectGitDir(path)
	if openErr != nil {
		// `.git` exists but is unreadable — surface the underlying
		// error rather than silently treating it as no-git.
		return fmt.Errorf("opening repo at %q: %w", path, openErr)
	}
	target.OriginURL = info.OriginURL
	target.DefaultBranch = info.DefaultBranch
	return nil
}

// EagerInitProjectClone materializes the local working tree for
// a freshly-created project at the operator's chosen path. The
// project's working tree IS that directory; its `.git/` IS the
// single git store enju writes objects + refs into via plumbing.
//
// Dispatch by inspected target state:
//
//   - empty / nonexistent: ForProject opens via InitLocal (seed
//     README + enju/templates/.gitkeep + initial commit).
//   - populated, no .git: git init + add+commit existing files +
//     write enju/ scaffold, then ForProject opens.
//   - .git present, no origin: ForProject opens. No origin gets
//     auto-wired — solo single-machine projects stay local; the
//     operator wires a real remote later via
//     enju_set_project_remote to opt into sharing.
//   - .git + origin: ForProject opens. Existing origin is left
//     intact (user's github/gitlab clone keeps its remote).
//
// path is required — `enju_create_project` validates this at the
// MCP handler boundary, so a zero-value here is a programming
// error.
//
// target may be nil for legacy callers (back-compat); when nil,
// the inspection runs implicitly via the existing-empty-dir path.
//
// Errors are returned but treated as warnings by callers (the
// project record is registered; the next tool call will retry
// the init).
func (s *FatClient) EagerInitProjectClone(ctx context.Context, projectID int64, path string, target *AdoptionTarget) error {
	if s.enjugit == nil {
		return nil
	}
	if path == "" {
		return fmt.Errorf("EagerInitProjectClone: path is required (programming error: handler should reject empty path)")
	}
	// Case 3 prep: populated dir with no .git. Run git init +
	// add+commit existing files BEFORE ForProject opens the
	// directory, so the existing user files end up on the initial
	// commit instead of being shadowed by InitLocal's seed.
	if target != nil && target.HasFiles && !target.HasGit {
		if _, err := s.initGitWithExistingFiles(path); err != nil {
			return fmt.Errorf("initializing git in populated dir %q: %w", path, err)
		}
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
	_, err := s.enjugit.ForProject(projectID, "")
	return err
}

// initGitWithExistingFiles runs `git init` then stages every
// existing file in the directory and lands them on a single
// initial commit. Used by case 3 (populated folder, no .git) so
// the user's existing files become the project's first commit
// instead of being silently overwritten by InitLocal's seed.
//
// Phase 8 dropped the auto-scaffold of enju/templates/.gitkeep —
// templates live wherever the user puts them; enju imposes zero
// required directory structure on user content.
func (s *FatClient) initGitWithExistingFiles(dirPath string) (string, error) {
	return enjugit.InitLocalAdoptExisting(dirPath, "")
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
	if !enjugit.HeadResolvesToCommit(dirPath) {
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


// RemoteStatusReport assembles the response payload for
// enju_project_remote_status. Opens the local clone, runs
// CompareToRemote, and returns the full field set the formatter
// expects (status, local/remote heads, ahead/behind counts,
// optional last_push_*, optional remote_error).
func (s *FatClient) RemoteStatusReport(ctx context.Context, projectID int64) (map[string]interface{}, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("remote status is only available in MCP client mode")
	}
	wf, remoteURL, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, err
	}
	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
	}
	cmp, err := wf.CompareDefaultBranch("")
	if err != nil {
		return nil, fmt.Errorf("comparing to remote: %w", err)
	}
	// For path-mode projects, coord's remote_url is empty but git
	// has origin pointing at the managed bare. Surface the git
	// origin so the operator sees where pushes are landing.
	if gitOrigin := wf.RemoteURL(); gitOrigin != "" && gitOrigin != remoteURL {
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
	t := wf.LastPushAt()
	if t.IsZero() {
		t = wf.HeadCommitTime()
	}
	if !t.IsZero() {
		resp["last_push_at"] = t.Format(time.RFC3339)
	}
	if e := wf.LastPushError(); e != "" {
		resp["last_push_error"] = e
	}
	return resp, nil
}

// SyncProjectToRemote runs the orchestration body of
// enju_project_sync: open the clone, lock it, preflight via
// CompareToRemote (refuse diverged state without force), push.
// Returns the response payload the formatter renders.
func (s *FatClient) SyncProjectToRemote(ctx context.Context, projectID int64, force bool) (map[string]interface{}, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("project sync is only available in MCP client mode")
	}
	wf, remoteURL, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if remoteURL == "" {
		// SyncProjectToRemote pushes from the operator's clone to
		// the coord-known remote URL. Path-mode projects have a
		// local managed bare (origin in git), but no coord-side
		// remote_url to sync to. Operator should use
		// `enju_set_project_remote` first to wire a real remote,
		// then run sync.
		return nil, fmt.Errorf("project has no remote URL configured on the coordinator — set one with `enju_set_project_remote` before running sync")
	}

	resp := map[string]interface{}{
		"project_id": projectID,
		"remote_url": remoteURL,
		"force":      force,
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

	pushErr := wf.PushAllRefs(force)
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
//  2. Push every local branch to it so the new remote receives
//     all the work that accumulated locally before origin was
//     wired up.
//  3. Reset scan cursors for every local branch to the sentinel
//     that forces full-history rescans on next reconcile, so the
//     scanner re-emits historical trailers and the artifact index
//     catches up.
//
// Returns a warning (non-empty when the push step failed; remote
// is set, but seeding failed). Empty workspace returns "".
func (s *FatClient) MirrorRemoteAfterSet(projectID int64, remoteURL string) (warning string) {
	if s.enjugit == nil {
		return ""
	}
	wf, err := s.enjugit.ForProject(projectID, remoteURL)
	if err != nil {
		return ""
	}
	_ = wf.SetRemote(remoteURL)

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
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := enjugit.LoadCursors(stateDir, projectID)
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
	if s.enjugit == nil {
		return false, nil
	}
	hadClone = s.enjugit.HasLocalClone(projectID)
	if err := s.enjugit.LeaveProject(projectID); err != nil {
		return hadClone, fmt.Errorf("removing local clone: %w", err)
	}
	s.UnregisterProject(projectID)
	return hadClone, nil
}

