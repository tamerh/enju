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

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// ErrNoCloneSource indicates a project has neither a remote_url
// nor a registered adopted-folder path, so the bot daemon (or
// `enju bot setup`) cannot materialize a clone or push target.
// This is a permanent deployment-config error — retrying won't
// help. Surfaced as a sentinel so the daemon's poll loop can
// exit cleanly instead of looping forever on the same failure.
var ErrNoCloneSource = errors.New("project has no clone source (no remote_url and no registered adopted path)")

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

// ResolveBotWorkspace returns the absolute path to the bot's
// managed clone, materializing it if absent. Distinct from
// ResolveProjectWorkspace (operator-facing) because the bot
// MUST operate in a working tree separate from the operator's:
//
//  1. **Branch switches collide with operator state.** The bot
//     checks out a topic branch per claim; if the operator has
//     uncommitted edits, git refuses with "worktree contains
//     unstaged changes" and the bot loops on failed checkout.
//  2. **Bot writes pollute operator status.** Files left over
//     from a topic branch appear as untracked in the operator's
//     `git status` after a checkout back to the run branch.
//  3. **claude -p edits land in the wrong tree.** The Handler's
//     cwd is whatever this function returns; if it were the
//     operator's tree, Edit/Write tools would touch files the
//     operator may also be editing.
//
// Layout (post-Phase-C): the clone lives at
// `<projectHome>/enju/.clone/`. Source for the clone:
//
//   - **Real remote (https/git/ssh):** clone directly from the
//     coord's remote_url. The bot pushes back to the same
//     remote as the operator — sharing happens via the network.
//   - **Local-only project:** clone from the bare at
//     `<projectHome>/enju/.bare.git/`. The bare is created by
//     `enju bot setup` (see EnsureBotPushTarget). If it doesn't
//     exist, surface a clear "run setup first" error.
//
// Project home comes from the projectreg registry — Phase A
// guarantees every project has an explicit, registered home.
// No remote_url, no fallback shenanigans.
func (s *FatClient) ResolveBotWorkspace(ctx context.Context, projectID int64) (string, error) {
	if s.workspace == nil {
		return "", fmt.Errorf("no workspace configured")
	}
	remoteURL, _, _, err := s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return "", err
	}

	// Determine the project home: registry is authoritative
	// (Phase A made `path=` required for both create_project and
	// init, both of which register the home). The coord's
	// remote_url is for the SHARED remote (github/gitlab) and
	// no longer doubles as a local path.
	var home string
	if s.projectRegistry != nil {
		entry, gerr := s.projectRegistry.Get(projectID)
		if gerr == nil && entry != nil {
			home = entry.LocalPath
		}
	}
	if home == "" {
		return "", fmt.Errorf(
			"%w: project %d — no registered home path. Register "+
				"the project with `enju_init --path=` or "+
				"`enju_create_project path=`",
			ErrNoCloneSource, projectID)
	}

	// Compute the bot's clone path and resolve the source URL.
	clonePath := filepath.Join(home, corelayout.BotCloneDir)

	// Source: real remote wins (push/pull travels the network),
	// else the per-project bare from `enju bot setup`.
	var source string
	if remoteURL != "" && !workspace.IsLocalWorkingTree(remoteURL) {
		// Network URL (https://, git@, ssh://). Clone direct.
		source = remoteURL
	} else {
		barePath := filepath.Join(home, corelayout.BotPushTargetDir)
		if _, statErr := os.Stat(filepath.Join(barePath, "HEAD")); statErr != nil {
			return "", fmt.Errorf(
				"project %d has no push target — run `enju bot setup` "+
					"to create the bare at %q (or set a real remote "+
					"with `enju_set_project_remote`): %w",
				projectID, barePath, statErr)
		}
		source = barePath
	}

	proj, err := s.workspace.OpenBotCloneAt(projectID, clonePath, source)
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
// Layout: the bare lives at `<projectHome>/enju/.bare.git/`.
// Gitignored, so it doesn't propagate via `git clone` to other
// machines — each operator's `enju bot setup` creates their own
// bare locally. This matches the "one project = one folder"
// model: everything Enju touches is inside the project. No
// `~/.enju/repos/`, no machine-shared state via the coord.
//
// Decision tree on the project's current remote_url:
//
//   - **Real remote (https://, git@, ssh://):** github / gitlab
//     / etc. is already a bare. No-op. Returns the existing
//     remote unchanged. The bot pushes there directly via the
//     project's `origin`.
//   - **Local path (project home):** call
//     workspace.PromoteWorkingTreeToBare to bare-clone the home
//     into `<home>/enju/.bare.git/`, rewire the home's `origin`
//     to that bare. Returns the bare path.
//   - **Empty:** fall back to the projectRegistry's home path
//     as the source. Same promote flow.
//   - **No source:** error.
//
// No coord PUT: the bare path is local-per-machine and
// derivable from the project home anyway. Other fatclient
// processes on the same machine compute the same path; the
// coord doesn't need to know.
//
// Why this lives at "bot setup" time rather than at
// `enju_init`: Option B (commit d8e97b6) removed auto-bare
// from `enju_init` because once the scanner gained a
// `refs/heads/<branch>` fallback (when no
// `refs/remotes/origin/<branch>` exists), the bare became
// redundant for single-citizen flows. In solo mode nobody
// pushes — submit wrappers commit straight to the working
// tree's local heads, the scanner reads local heads, done.
//
// Bots break that property: the daemon runs in a SEPARATE
// managed clone, makes commits there, and must push them
// somewhere the scanner can see. Pushing into the operator's
// working tree fails on whatever branch is currently checked
// out (and topic-branch + FF-merge flows get fragile). A bare
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

	// Source for the bare-clone is the project's home path:
	// prefer remote_url (when it points at a local working
	// tree), else fall back to the registry's home path.
	source := remoteURL
	if source == "" && s.projectRegistry != nil {
		entry, gerr := s.projectRegistry.Get(projectID)
		if gerr == nil && entry != nil {
			source = entry.LocalPath
		}
	}
	if source == "" {
		return "", false, fmt.Errorf(
			"%w: project %d — set a real remote with "+
				"`enju_set_project_remote`, or register a project home "+
				"with `enju_init --path=` / `enju_create_project path=`",
			ErrNoCloneSource, projectID)
	}
	if !workspace.IsLocalWorkingTree(source) {
		return "", false, fmt.Errorf(
			"clone source %q for project %d is not a git working tree; "+
				"cannot promote to a bare",
			source, projectID)
	}

	// Bare lives inside the project home, at the convention
	// path `<home>/enju/.bare.git/` (corelayout.BotPushTargetDir).
	// Gitignored via the managed block so it stays out of the
	// operator's commits.
	barePath := filepath.Join(source, corelayout.BotPushTargetDir)

	// Note whether this is a fresh promote or a no-op so the
	// caller can render different UX ("created" vs "ready").
	// Detect by the bare's HEAD presence, mirroring
	// PromoteWorkingTreeToBare's idempotency check.
	wasFresh := true
	if _, statErr := os.Stat(filepath.Join(barePath, "HEAD")); statErr == nil {
		wasFresh = false
	}

	if err := workspace.PromoteWorkingTreeToBare(source, barePath); err != nil {
		return "", false, fmt.Errorf("promoting %q to bare at %q: %w", source, barePath, err)
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
