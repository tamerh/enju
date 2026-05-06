package service

// Project-metadata + workspace-clone helpers shared across tool
// handlers. Every fat-client flow that touches a project's local
// clone goes through these: fetch the project's metadata from the
// coordinator, then open / configure the local workspace clone so
// pulls + pushes target the right ref.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// ErrNoCloneSource indicates a project has neither a remote_url
// nor a registered adopted-folder path, so the bot daemon (or
// `enju bot setup`) cannot materialize a clone or push target.
// This is a permanent deployment-config error — retrying won't
// help. Surfaced as a sentinel so the daemon's poll loop can
// exit cleanly instead of looping forever on the same failure.
var ErrNoCloneSource = errors.New("project has no clone source (no remote_url and no registered adopted path)")

// isManagedWorkspaceClone returns true if path lives under the
// workspace's managed root (`~/.enju/workspaces/...`). Used to
// distinguish two registry entry shapes that share the same
// LocalPath field:
//
//   - `enju_init --path=`'s adopted external folder — a genuine
//     clone source (operator's own working tree, outside our
//     managed roots).
//   - `enju_create_project`'s workspace path — the internally-
//     managed clone we just initialized. Treating this as a
//     clone source would self-reference: ForceManagedClone's
//     destination is also `ws.projectDir(id)`, identical to the
//     source, leading to PlainClone errors that fall through to
//     the empty-remote bootstrap path and a corrupted workspace
//     with origin pointing at itself.
//
// The discriminator: is the path under `ws.RootDir()`? If yes,
// it's a managed internal clone, NOT a valid clone source for
// the bot daemon.
func (s *FatClient) isManagedWorkspaceClone(path string) bool {
	if s.workspace == nil || path == "" {
		return false
	}
	rootDir := s.workspace.RootDir()
	if rootDir == "" {
		return false
	}
	rel, err := filepath.Rel(rootDir, path)
	if err != nil {
		return false
	}
	// rel is "." (path == rootDir), ".." or "../..." (path is
	// outside root), or "subfolder/..." (path is under root).
	// Only the last is "managed clone."
	if rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// FetchProjectMeta reads a project's metadata from the coordinator.
// Used by the client-side project_remote_status / project_sync /
// get_artifact / get_artifact_history / set_project_remote handlers
// that need the project's remote_url to open the local clone.
func (s *FatClient) FetchProjectMeta(ctx context.Context, projectID int64) (remoteURL string, err error) {
	remoteURL, _, err = s.FetchProjectMetaFull(ctx, projectID)
	return
}

// FetchProjectMetaFull is like FetchProjectMeta but also returns
// the human-readable project name for workspace directory naming.
func (s *FatClient) FetchProjectMetaFull(ctx context.Context, projectID int64) (remoteURL, name string, err error) {
	remoteURL, name, _, err = s.FetchProjectMetaExpanded(ctx, projectID)
	return
}

// ResolveProjectWorkspace returns the absolute path to the
// project's local clone, materializing it (clone or init) if
// not yet present. Honors externalDirs (an `enju_init`'d
// adopted dir wins over the managed `~/.enju/workspaces/`
// path), which is the right call for HUMAN flows: the
// operator's edits in their adopted tree are visible to MCP /
// webui directly. Bot flows MUST NOT use this — see
// ResolveBotWorkspace below.
func (s *FatClient) ResolveProjectWorkspace(ctx context.Context, projectID int64) (string, error) {
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	if proj == nil {
		return "", fmt.Errorf("no workspace project for project_id=%d", projectID)
	}
	return proj.WorkDir(), nil
}

// ResolveBotWorkspace returns the absolute path to a managed
// clone of the project at `~/.enju/workspaces/<slug>-<id>/`,
// materializing it if absent. Distinct from
// ResolveProjectWorkspace because the bot daemon MUST operate
// in a working tree separate from the operator's adopted dir
// (when the project was created via `enju_init --path=`).
//
// Why bots can't share the operator's tree:
//
//  1. **Branch switches collide with operator state.** The bot
//     checks out a topic branch per claim; if the operator has
//     uncommitted edits in tracked files, git refuses with
//     "worktree contains unstaged changes" and the bot loops
//     on the failed checkout.
//  2. **Bot writes pollute operator status.** Each completed
//     develop task leaves files committed on its topic branch;
//     after a checkout back to the run branch (or another
//     topic) those files appear as untracked in the operator's
//     `git status`, conflating bot residue with the operator's
//     real working state.
//  3. **claude -p edits land in the wrong tree.** The Handler's
//     cwd is whatever ResolveProjectWorkspace returned. When
//     that's the operator's adopted dir, `Edit`/`Write` tools
//     modify files the operator may also be editing.
//
// Resolution:
//
//  1. Look up the project's remote_url via the coord. If
//     non-empty, that's the clone source.
//  2. If remote_url is empty, fall back to the projectRegistry's
//     adopted LocalPath as the clone source — the bot's
//     managed clone has the operator's tree as its `origin`,
//     suitable for pull/push via git's local protocol.
//  3. If neither is configured, error — bot can't materialize
//     a clone.
//  4. Call workspace.ForceManagedClone, which always uses the
//     `~/.enju/workspaces/<slug>-<id>/` path regardless of any
//     externalDirs registration.
func (s *FatClient) ResolveBotWorkspace(ctx context.Context, projectID int64) (string, error) {
	if s.workspace == nil {
		return "", fmt.Errorf("no workspace configured")
	}
	remoteURL, projName, _, err := s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return "", err
	}
	// Empty remote → fall back to the registry's adopted path
	// (if any). This handles the `enju_init --path=` case where
	// the operator's tree IS the only source of truth and the
	// project has no real remote.
	//
	// Filter: skip the entry if its LocalPath is the managed
	// workspace clone itself (the `enju_create_project` case).
	// Using that path as a clone source would be self-referential
	// (source == destination == ws.projectDir(id)), which corrupts
	// the workspace via the openOrClone bootstrap-empty-remote
	// fallback. See isManagedWorkspaceClone for the rationale.
	if remoteURL == "" && s.projectRegistry != nil {
		entry, err := s.projectRegistry.Get(projectID)
		if err == nil && entry != nil && entry.LocalPath != "" && !s.isManagedWorkspaceClone(entry.LocalPath) {
			remoteURL = entry.LocalPath
		}
	}
	if remoteURL == "" {
		return "", fmt.Errorf(
			"%w: project %d — for `enju_create_project` projects "+
				"with no remote, bots are not yet supported (the "+
				"workspace clone IS the only working tree). Set a "+
				"real remote with `enju_set_project_remote`, or "+
				"adopt an external folder with `enju_init --path=` "+
				"and start the bot against that project instead",
			ErrNoCloneSource, projectID)
	}
	proj, err := s.workspace.ForceManagedClone(projectID, remoteURL, projName)
	if err != nil {
		return "", err
	}
	return proj.WorkDir(), nil
}

// EnsureBotPushTarget makes sure the project has a non-working-
// tree git destination the bot daemon can push to. Called by
// `enju bot setup` so the moment a project opts into bots, it
// gets a proper bare push target. Idempotent — re-runs are
// safe.
//
// Decision tree on the project's current remote_url:
//
//   - **Real remote (https://, git@, ssh://):** github / gitlab
//     / etc. is already a bare. No-op. Returns the existing
//     remote unchanged.
//   - **Local path (operator's adopted dir, from `enju_init
//     --path=`):** call workspace.PromoteWorkingTreeToBare to
//     bare-clone that path to `~/.enju/repos/{id}.git/`, then
//     PUT the new bare path to the coord's project record so
//     future fatclient processes (webui, other operators)
//     route through the bare.
//   - **Empty:** fall back to the projectRegistry's adopted
//     LocalPath as the source. Same promote + PUT flow.
//   - **Empty AND no registry entry:** error — there's no
//     working tree to mirror, so nothing for the bot to push
//     to. Operator needs to set a real remote
//     (`enju_set_project_remote`) or re-run `enju_init`.
//
// Why this lives at "bot setup" time rather than at
// `enju_init`: Option B (commit d8e97b6, tasks #162-164)
// removed auto-bare from `enju_init` because once the
// scanner gained a `refs/heads/<branch>` fallback (when no
// `refs/remotes/origin/<branch>` exists), the bare became
// redundant for single-citizen flows. In solo mode nobody
// pushes — submit wrappers commit straight to the working
// tree's local heads, the scanner reads local heads, done.
// The earlier auto-bare was a workaround for TP53 Bug 1
// (async tasks stalling on local-only projects); the
// scanner fallback is the real fix.
//
// Bots break that property: the daemon runs in a SEPARATE
// managed clone under `~/.enju/workspaces/<id>/`, makes
// commits there, and must push them somewhere the
// scanner can see. Pushing into the operator's working
// tree fails on whatever branch is currently checked out
// (and topic-branch + FF-merge flows get fragile). A bare
// has no working tree, so pushes never contend.
//
// Conclusion: solo flows stay bare-free (Option B's win
// preserved); the bare appears the moment the operator
// opts into bots, exactly when it starts earning its
// keep.
func (s *FatClient) EnsureBotPushTarget(ctx context.Context, projectID int64) (bareURL string, created bool, err error) {
	if s.workspace == nil {
		return "", false, fmt.Errorf("no workspace configured")
	}
	remoteURL, _, _, ferr := s.FetchProjectMetaExpanded(ctx, projectID)
	if ferr != nil {
		return "", false, ferr
	}

	// Real remote (https/git/ssh) — github plays the bare role.
	// IsLocalWorkingTree returns true only for filesystem paths
	// pointing at a real git working tree; everything else
	// (network URLs, missing paths) returns false here too, so
	// we narrow to "non-empty AND local working tree" before
	// promoting.
	if remoteURL != "" && !workspace.IsLocalWorkingTree(remoteURL) {
		return remoteURL, false, nil
	}

	// Source for the bare-clone: prefer the existing remote_url
	// (when it points at a local working tree), else fall back
	// to the registry's adopted path.
	//
	// Filter the registry entry the same way ResolveBotWorkspace
	// does: a managed-workspace LocalPath (from
	// `enju_create_project`) is NOT a valid promote source — the
	// bot's managed clone would land at the same path, defeating
	// the operator/bot isolation that ForceManagedClone provides.
	// Surface ErrNoCloneSource so the operator knows to either
	// use a real remote or `enju_init --path=` instead.
	source := remoteURL
	if source == "" && s.projectRegistry != nil {
		entry, gerr := s.projectRegistry.Get(projectID)
		if gerr == nil && entry != nil && !s.isManagedWorkspaceClone(entry.LocalPath) {
			source = entry.LocalPath
		}
	}
	if source == "" {
		return "", false, fmt.Errorf(
			"%w: project %d — for `enju_create_project` projects "+
				"with no remote, bots are not yet supported (the "+
				"workspace clone IS the only working tree, so a "+
				"separate bot clone would collide with the operator's). "+
				"Set a real remote with `enju_set_project_remote`, or "+
				"adopt an external folder with `enju_init --path=`",
			ErrNoCloneSource, projectID)
	}
	if !workspace.IsLocalWorkingTree(source) {
		return "", false, fmt.Errorf(
			"clone source %q for project %d is not a git working tree; "+
				"cannot promote to a bare",
			source, projectID)
	}

	home, hErr := os.UserHomeDir()
	if hErr != nil {
		return "", false, fmt.Errorf("resolving home dir for bare path: %w", hErr)
	}
	barePath := filepath.Join(home, ".enju", "repos", fmt.Sprintf("%d.git", projectID))

	// Note whether this is a fresh promote or a no-op so the
	// caller can render different UX ("✓ created" vs "✓ already
	// in place"). Detect by the bare's HEAD presence, mirroring
	// PromoteWorkingTreeToBare's idempotency check.
	wasFresh := true
	if _, statErr := os.Stat(filepath.Join(barePath, "HEAD")); statErr == nil {
		wasFresh = false
	}

	if err := workspace.PromoteWorkingTreeToBare(source, barePath); err != nil {
		return "", false, fmt.Errorf("promoting %q to bare at %q: %w", source, barePath, err)
	}

	// Update the coord's project record so OTHER fatclient
	// processes (webui on this machine, future bot daemons,
	// other adopted-dir consumers) route through the bare too.
	// Without this PUT, the coord would still hand out the
	// operator's path on FetchProjectMetaExpanded — a fresh
	// daemon would think the bare doesn't exist and fall back
	// to the operator's tree on its first claim.
	body := map[string]string{"remote_url": barePath}
	if _, putErr := s.coord.Put(ctx, fmt.Sprintf("/api/v1/projects/%d/remote", projectID), body); putErr != nil {
		// Bare exists, working tree's origin is correct; just
		// the coord didn't get the update. Don't roll back —
		// the operator can re-run setup and the idempotent
		// PromoteWorkingTreeToBare + PUT will retry.
		return barePath, wasFresh, fmt.Errorf("bare created at %q but updating coord remote_url failed: %w", barePath, putErr)
	}
	return barePath, wasFresh, nil
}

// OpenProject fetches project metadata, opens the workspace
// clone, AND wires the project's default_branch into the
// Project so Pull/Push fallback paths target the right ref.
// Every call site that pairs FetchProjectMetaFull +
// workspace.ForProject should use this helper instead.
func (s *FatClient) OpenProject(ctx context.Context, projectID int64) (proj *workspace.Project, remoteURL, projName, defaultBranch string, err error) {
	if s.workspace == nil {
		return nil, "", "", "", fmt.Errorf("no workspace configured")
	}
	remoteURL, projName, defaultBranch, err = s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return nil, "", "", "", err
	}
	proj, err = s.workspace.ForProject(projectID, remoteURL, projName)
	if err != nil {
		return nil, remoteURL, projName, defaultBranch, err
	}
	proj.SetDefaultBranch(defaultBranch)
	return proj, remoteURL, projName, defaultBranch, nil
}

// FetchProjectMetaExpanded returns remote_url + name +
// default_branch. Called from paths that need the branch name
// to configure the workspace (submit / claim / execute) so
// Pull/Push target the right ref.
func (s *FatClient) FetchProjectMetaExpanded(ctx context.Context, projectID int64) (remoteURL, name, defaultBranch string, err error) {
	data, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d", projectID))
	if err != nil {
		return "", "", "", err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", "", fmt.Errorf("parsing project: %w", err)
	}
	if errMsg, ok := raw["error"].(string); ok {
		return "", "", "", fmt.Errorf("%s", errMsg)
	}
	if v, ok := raw["remote_url"].(string); ok {
		remoteURL = v
	}
	if v, ok := raw["name"].(string); ok {
		name = v
	}
	if v, ok := raw["default_branch"].(string); ok {
		defaultBranch = v
	}
	return remoteURL, name, defaultBranch, nil
}
