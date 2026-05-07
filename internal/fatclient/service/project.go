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
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/project"
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
// Layout: the clone lives at `<projectHome>/enju/.clone/`.
// Source for the clone:
//
//   - **Real remote (https/git/ssh):** clone directly from the
//     coord's remote_url. The bot pushes back to the same
//     remote as the operator — sharing happens via the network.
//   - **Local-only project:** clone from the bare at
//     `<projectHome>/enju/.bare.git/`. The bare is created by
//     `enju bot setup` (see EnsureBotPushTarget). If it doesn't
//     exist, surface a clear "run setup first" error.
//
// Project home comes from the projectreg registry. Every project
// is registered with an explicit path at create_project + init
// time (both require `path=`), so the lookup is unambiguous —
// no remote_url-as-path fallback, no filesystem walk.
func (s *FatClient) ResolveBotWorkspace(ctx context.Context, projectID int64, botUsername string) (string, error) {
	if s.project == nil {
		return "", fmt.Errorf("no workspace configured")
	}
	if botUsername == "" {
		return "", fmt.Errorf("bot username is required")
	}
	remoteURL, _, _, err := s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return "", err
	}

	// Determine the project home: registry is authoritative.
	// Both create_project and init require `path=`, so every
	// project ends up with an entry. The coord's remote_url is
	// reserved for the SHARED remote (github/gitlab) — it does
	// not double as a local path.
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

	// Compute this bot's clone path. Per-bot per-project so two
	// bots running on the same machine for the same project have
	// their own working trees and don't collide on branch
	// switches or in-flight scratch files. The path safety
	// guards inside BotCloneDirFor reject malformed usernames so
	// a hostile manifest can't escape into the project tree.
	relClone, cerr := corelayout.BotCloneDirFor(botUsername)
	if cerr != nil {
		return "", fmt.Errorf("invalid bot username %q: %w", botUsername, cerr)
	}
	clonePath := filepath.Join(home, relClone)

	// Source: real remote wins (push/pull travels the network),
	// else the per-project bare from `enju bot setup`.
	var source string
	if remoteURL != "" && !project.IsLocalWorkingTree(remoteURL) {
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

	proj, err := s.project.OpenBotCloneAt(projectID, clonePath, source)
	if err != nil {
		return "", err
	}

	// Stash the resolved clone keyed by projectID so subsequent
	// OpenProject calls inside this FatClient (claim, submit,
	// reset) route to the bot clone instead of the operator-side
	// ForProject lookup. One FatClient = one citizen, so a
	// project-keyed map is sufficient — there's no scenario in
	// which the same FatClient resolves multiple bot identities
	// for the same project.
	s.botClonesMu.Lock()
	if s.botClones == nil {
		s.botClones = make(map[int64]*project.Clone)
	}
	s.botClones[projectID] = proj
	// Mirror into the enjugit stash so OpenWorkflow routes to the
	// same per-bot tree. Falls back silently if enjugit isn't
	// configured (test fixtures with project-only setup).
	if s.enjugit != nil {
		wf, werr := s.enjugit.OpenBotCloneAt(projectID, clonePath, source)
		if werr == nil {
			if s.botWorkflows == nil {
				s.botWorkflows = make(map[int64]*enjugit.Workflow)
			}
			s.botWorkflows[projectID] = wf
		} else {
			s.logger.Warn("enjugit bot clone open failed; new-API call sites for this project unavailable",
				"project_id", projectID, "error", werr)
		}
	}
	s.botClonesMu.Unlock()

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
//     project.PromoteWorkingTreeToBare to bare-clone the home
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
	if s.project == nil {
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
	if remoteURL != "" && !project.IsLocalWorkingTree(remoteURL) {
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
	if !project.IsLocalWorkingTree(source) {
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

	if err := project.PromoteWorkingTreeToBare(source, barePath); err != nil {
		return "", false, fmt.Errorf("promoting %q to bare at %q: %w", source, barePath, err)
	}
	return barePath, wasFresh, nil
}

// OpenProject fetches project metadata, opens the workspace
// clone, AND wires the project's default_branch into the
// Project so Pull/Push fallback paths target the right ref.
// Every call site that pairs FetchProjectMetaFull +
// project.ForProject should use this helper instead.
func (s *FatClient) OpenProject(ctx context.Context, projectID int64) (proj *project.Clone, remoteURL, projName, defaultBranch string, err error) {
	if s.project == nil {
		return nil, "", "", "", fmt.Errorf("no workspace configured")
	}
	remoteURL, projName, defaultBranch, err = s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return nil, "", "", "", err
	}

	// Bot path: if ResolveBotWorkspace stashed a per-bot clone
	// for this project, use it. Routes claim / submit / reset to
	// the bot's own working tree at <project>/enju/bots/<bot>/clone/
	// instead of the operator's adopted dir. Without this lookup,
	// a daemon's submit would fall through to ForProject and
	// land on the operator's tree — the legacy "shared bot
	// clone" failure mode.
	s.botClonesMu.Lock()
	cached := s.botClones[projectID]
	s.botClonesMu.Unlock()
	if cached != nil {
		cached.SetDefaultBranch(defaultBranch)
		return cached, remoteURL, projName, defaultBranch, nil
	}

	proj, err = s.project.ForProject(projectID, remoteURL, projName)
	if err != nil {
		return nil, remoteURL, projName, defaultBranch, err
	}
	proj.SetDefaultBranch(defaultBranch)
	return proj, remoteURL, projName, defaultBranch, nil
}

// OpenWorkflow is the enjugit-side analog of OpenProject:
// fetches project metadata, opens (or reuses the bot stash for)
// the workspace clone, and pre-configures the Workflow with
// the project's default branch so subsequent template / submit
// verbs target the right ref. Every call site that pairs
// FetchProjectMetaExpanded + enjugit.ForProject should use this
// helper instead.
//
// Returns ErrNoWorkspace when no enjugit workspace is configured
// (test fixture without local fs). Errors from the underlying
// open propagate as-is.
func (s *FatClient) OpenWorkflow(ctx context.Context, projectID int64) (wf *enjugit.Workflow, remoteURL, projName, defaultBranch string, err error) {
	if s.enjugit == nil {
		return nil, "", "", "", fmt.Errorf("no workspace configured")
	}
	remoteURL, projName, defaultBranch, err = s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return nil, "", "", "", err
	}

	// Bot path: ResolveBotWorkspace stashed a per-bot Workflow
	// for this project — route to it. Mirrors OpenProject's
	// botClones lookup.
	s.botClonesMu.Lock()
	cached := s.botWorkflows[projectID]
	s.botClonesMu.Unlock()
	if cached != nil {
		cached.SetDefaultBranch(defaultBranch)
		return cached, remoteURL, projName, defaultBranch, nil
	}

	wf, err = s.enjugit.ForProject(projectID, remoteURL, projName)
	if err != nil {
		return nil, remoteURL, projName, defaultBranch, err
	}
	wf.SetDefaultBranch(defaultBranch)
	// Band-aid for #381 dual-handle bug: while project.Clone and
	// enjugit.Workflow both touch the same on-disk dir, some code
	// path inside the project package (claim/pull) intermittently
	// wipes the [remote "origin"] section from .git/config. The
	// cached enjugit Workflow's in-memory remoteURL stays correct
	// but go-git's Fetch reads .git/config every call and fails
	// with "remote not found". EnsureOrigin idempotently restores
	// the section when missing. Drop this call once Phase 11
	// retires the project package and the wipe source goes away.
	if remoteURL != "" {
		if eerr := wf.EnsureOrigin(remoteURL); eerr != nil {
			s.logger.Warn("OpenWorkflow: ensure-origin self-heal failed",
				"project_id", projectID, "remote", remoteURL, "error", eerr)
		}
	}
	return wf, remoteURL, projName, defaultBranch, nil
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

// ResetBotCloneToCleanState wipes the bot clone's worktree
// residue between iterations: drops staged + unstaged changes
// to tracked files (hard reset to HEAD) and removes untracked
// files. After ClaimTask's pre-claim pull, HEAD points at the
// run branch tip the next task should fork from, so this
// effectively syncs the clone to the latest run-branch state
// while clearing the previous task's leftovers.
//
// **Bot-only.** The operator's `enju mcp` working tree is
// the user's actual development directory and may legitimately
// carry uncommitted WIP, scratch notes, or unrelated branches
// — reset would clobber that. The daemon's clone at
// <project>/enju/.clone/ is system-managed: anything not in
// HEAD is residue.
//
// Why between iterations: a previous task's `claude -p` may
// have left scratch files behind, or made unstaged tweaks to
// a tracked file (go.mod, package.json) that didn't end up
// in the commit. The next task's CheckoutBranchFrom can
// produce a "staged-deletion + untracked" desync when the
// new topic branch's tree disagrees with the residue. Clearing
// the worktree first eliminates the desync class entirely.
//
// Idempotent — calling this on an already-clean clone is a
// no-op (HardReset matches HEAD's tree, no untracked files
// to remove).
func (s *FatClient) ResetBotCloneToCleanState(ctx context.Context, projectID int64) error {
	// OpenProject routes through the FatClient's bot-clone stash
	// when present (populated by ResolveBotWorkspace), so this
	// call resolves to the bot's own working tree at
	// <project>/enju/bots/<bot>/clone/ rather than the operator's
	// adopted dir. The contract "this method only touches bot
	// clones" is enforced by the pre-warm requirement: a daemon
	// MUST call ResolveBotWorkspace before ResetBotCloneToCleanState
	// so the stash is populated; the existing daemon flow
	// (runOnce → ResolveBotWorkspace → ClaimTask → reset) honors
	// that ordering.
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return err
	}
	proj.Lock()
	defer proj.Unlock()
	return proj.ResetWorktreeToCleanState()
}

// FetchAllRefsForBot brings every remote branch's refs +
// objects into the bot's clone. Used by the daemon's pre-claim
// path so claude-p sees fresh topic branches pushed by other
// bots since this daemon last fetched. Without this step, per-
// bot clones drift apart: developer-bot pushes its iter-1
// commit, reviewer-bot's clone has no record of it, claude-p
// reads an empty topic branch and emits a bogus
// request_changes.
//
// Best-effort: a fetch failure (network blip, transient
// unreachable) is logged by the caller but doesn't block the
// claim — the lazy-fetch in ReadFileAtCommit picks up the
// commit on first read miss as a fallback.
func (s *FatClient) FetchAllRefsForBot(ctx context.Context, projectID int64) error {
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return err
	}
	return wf.FetchAllRefs()
}

// CheckoutTopicBranchTip switches the bot clone's HEAD to the
// named topic branch. Used by the bot daemon on iter-2+ re-claim
// after a request_changes verdict so the LLM starts on the prior
// iteration's tree (where the reviewer's feedback applies),
// rather than on the run-branch tip the pre-claim pull leaves
// HEAD at.
//
// The branch is expected to already exist locally — iter-1's
// successful submit created it. We don't fetch from origin here
// (the same bot just pushed the topic on iter-1, so the local
// ref is current); a future multi-bot revision flow would need
// fetch + force-update.
//
// Caller-side: call this BEFORE ResetBotCloneToCleanState so the
// reset's HardReset-to-HEAD lands the worktree on topic-branch
// state, not run-branch state.
func (s *FatClient) CheckoutTopicBranchTip(ctx context.Context, projectID int64, branch, baseBranch string) error {
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return err
	}
	proj.Lock()
	defer proj.Unlock()
	// baseBranch matters for the create-new path: when the topic
	// doesn't yet exist locally (iter-N bumped after a terminal
	// reject/invalidate), CheckoutBranchFrom needs a fork base
	// or it falls through to branchBaseHash() = origin/main
	// (seed). That orphans iter-N from any prior run-branch
	// content, the production "iter-2 forked from seed instead
	// of build-1" bug. Callers in the topic-branch flow should
	// pass meta.Branch (run branch) so brand-new topics land on
	// the run-branch tip; existing-topic checkouts ignore
	// baseBranch (the existing-ref short-circuit fires first).
	return proj.CheckoutBranchFrom(branch, baseBranch)
}

// WipeDeclaredWrites removes every file matching the task's
// declared `writes` from the bot clone's worktree. Used by the
// bot daemon on iter-2+ re-claim to give the LLM a clean canvas
// in its declared output paths — without this, iter-2's
// potentially different filenames (LLM non-determinism) end up
// unioned with iter-1's tracked files in the topic-branch tree.
//
// All four declaration shapes are handled by delegating to
// `WriteArtifacts.ExpandAgainstWorkdir`, which the post-handler
// validation already uses for the symmetric "what did the LLM
// actually write" check:
//
//   - Literal path ("src/foo.go"): matches that one file if
//     present.
//   - Glob ("src/*.go"): expands to every matching file.
//   - Directory ("src/foo/"): walks the dir, every file under it.
//   - Templated ("out/{{instance}}.md"): pre-resolved at
//     materialization to a literal — no special handling needed.
//
// A declaration that matches nothing (the prior iteration didn't
// write that path) is silently skipped — there's nothing to
// wipe. Required-vs-optional doesn't matter here; we only care
// about what's actually on disk.
//
// Idempotent: a missing file is fine. A removal failure
// (permission denied on a symlink, etc.) returns an error so the
// daemon fails the iteration loudly rather than handing the LLM
// a half-clean tree.
//
// Caller-side: call AFTER CheckoutTopicBranchTip + the existing
// ResetBotCloneToCleanState (so worktree reflects iter-1's tree
// and ExpandAgainstWorkdir finds the right files), and BEFORE
// the handler runs.
func (s *FatClient) WipeDeclaredWrites(ctx context.Context, projectID int64, writes enjuYaml.WriteArtifacts) error {
	if len(writes) == 0 {
		return nil
	}
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return err
	}
	return wipeDeclaredWritesInDir(proj.WorkDir(), writes)
}

// wipeDeclaredWritesInDir is the pure-function core of
// WipeDeclaredWrites: given a working directory and a set of
// `writes_artifacts` declarations, expand the declarations
// against what's on disk and delete every match. Extracted out
// of the FatClient method so the all-shapes contract (literal +
// glob + directory + pre-resolved template) can be tested with
// just a TempDir, no fat-client / coord / clone scaffolding.
func wipeDeclaredWritesInDir(workDir string, writes enjuYaml.WriteArtifacts) error {
	expanded, _, eerr := writes.ExpandAgainstWorkdir(workDir)
	if eerr != nil {
		return fmt.Errorf("expanding writes_artifacts for wipe: %w", eerr)
	}
	for _, e := range expanded {
		if e.Path == "" {
			continue
		}
		full := filepath.Join(workDir, e.Path)
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing prior iteration's declared write %q: %w", e.Path, err)
		}
	}
	return nil
}
