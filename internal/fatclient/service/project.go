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
// not yet present. Honors externalDirs (a folder adopted via
// `enju_create_project path=`) over the managed `~/.enju/
// workspaces/` path so the operator's edits in their adopted
// tree are visible to MCP / webui directly.
func (s *FatClient) ResolveProjectWorkspace(ctx context.Context, projectID int64) (string, error) {
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return "", err
	}
	if wf == nil {
		return "", fmt.Errorf("no workspace project for project_id=%d", projectID)
	}
	return wf.WorkDir(), nil
}

// ProjectGitDir returns the absolute path to the project clone's
// .git directory. Used by the bot daemon to populate
// $ENJU_GIT_DIR for handlers that read git history via
// `git --git-dir=$ENJU_GIT_DIR log ...`. Returns "" when no
// workspace is configured (legacy callers without an enjugit
// workspace), in which case the env var is simply not exported.
func (s *FatClient) ProjectGitDir(ctx context.Context, projectID int64) (string, error) {
	if s.enjugit == nil {
		return "", nil
	}
	workDir, err := s.ResolveProjectWorkspace(ctx, projectID)
	if err != nil {
		return "", err
	}
	if workDir == "" {
		return "", nil
	}
	return filepath.Join(workDir, ".git"), nil
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
//   - **Empty (path-mode):** fall back to the projectRegistry's
//     home path. Call PromoteWorkingTreeToBare to bare-clone
//     the home into `<home>/enju/.bare.git/`, rewire the home's
//     `origin` to that bare. Returns the bare path.
//   - **No registry entry:** error pointing at enju_create_project.
//
// No coord PUT: the bare path is local-per-machine and
// derivable from the project home anyway. Other fatclient
// processes on the same machine compute the same path; the
// coord doesn't need to know.
//
// Historical note: Option B (commit d8e97b6) removed auto-bare
// from project creation because once the scanner gained a
// `refs/heads/<branch>` fallback (when no
// `refs/remotes/origin/<branch>` exists), the bare became
// redundant for single-citizen flows. Phase 1 of the no-remote
// collapse re-introduced ensureManagedBare at create time
// because every project needs an origin push target. In solo
// mode nobody pushes — submit wrappers commit straight to the
// working
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
	if s.enjugit == nil {
		return "", false, fmt.Errorf("no workspace configured")
	}
	remoteURL, _, _, ferr := s.FetchProjectMetaExpanded(ctx, projectID)
	if ferr != nil {
		return "", false, ferr
	}

	// Real remote (https/git/ssh) — github plays the bare role.
	// coord-side remote_url is now always either "" (path-mode)
	// or a real network URL, so non-empty == real remote, no-op.
	if remoteURL != "" {
		return remoteURL, false, nil
	}

	// Path-mode: the project's home in the registry is the source
	// to bare-clone from. Validate it's actually a working tree
	// before promoting (defensive — a corrupted registry entry
	// shouldn't silently produce an empty bare).
	var source string
	if s.projectRegistry != nil {
		entry, gerr := s.projectRegistry.Get(projectID)
		if gerr == nil && entry != nil {
			source = entry.LocalPath
		}
	}
	if source == "" {
		return "", false, fmt.Errorf(
			"%w: project %d — set a real remote with "+
				"`enju_set_project_remote`, or register a project home "+
				"with `enju_create_project path=`",
			ErrNoCloneSource, projectID)
	}
	if !enjugit.IsLocalWorkingTree(source) {
		return "", false, fmt.Errorf(
			"registered project home %q for project %d is not a git working tree; "+
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

	if err := enjugit.PromoteWorkingTreeToBare(source, barePath); err != nil {
		return "", false, fmt.Errorf("promoting %q to bare at %q: %w", source, barePath, err)
	}
	return barePath, wasFresh, nil
}

// OpenWorkflow fetches project metadata, opens (or reuses the bot
// stash for) the workspace clone, and pre-configures the Workflow
// with the project's default branch so subsequent template / submit
// verbs target the right ref. Every call site that pairs
// FetchProjectMetaExpanded + enjugit.ForProject should use this
// helper instead.
//
// Returns an error when no enjugit workspace is configured (test
// fixtures without a local fs). Errors from the underlying open
// propagate as-is.
func (s *FatClient) OpenWorkflow(ctx context.Context, projectID int64) (wf *enjugit.Workflow, remoteURL, projName, defaultBranch string, err error) {
	if s.enjugit == nil {
		return nil, "", "", "", fmt.Errorf("no workspace configured")
	}
	remoteURL, projName, defaultBranch, err = s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return nil, "", "", "", err
	}

	wf, err = s.enjugit.ForProject(projectID, remoteURL, projName)
	if err != nil {
		return nil, remoteURL, projName, defaultBranch, err
	}
	wf.SetDefaultBranch(defaultBranch)
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

// FetchAllRefsForBot brings every remote branch's refs +
// objects into the project clone. Used by the daemon's pre-
// claim path so claude-p sees fresh topic branches pushed by
// other citizens since this daemon last fetched. Without this
// step, a reading citizen may see an empty topic branch and
// emit a bogus request_changes — the developer's commit is on
// the remote, just not yet in the local object store.
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

// MarkTaskStarted posts /api/v1/tasks/:id/started to flip the
// task CLAIMED → RUNNING. Phase 8.2 observability hook: tells
// the coord (and any operator watching enju_run_status) that
// the citizen has actually kicked off execution rather than
// just claimed and stalled. Used by the bot daemon's
// processAndSubmit right before the LLM call. The compute path
// posts inline from execute.go since it has direct s.coord
// access. Surfacing this on FatClient lets the daemon stay
// behind its narrow `fatClient` interface without leaking the
// raw coord client.
//
// Best-effort callers: a duplicate POST on a retry resume hits
// the coord-side state==CLAIMED guard and returns an error
// the caller should log + ignore.
func (s *FatClient) MarkTaskStarted(ctx context.Context, taskID string) error {
	_, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/started", nil)
	return err
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
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return err
	}
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
	return wf.CheckoutBranchFrom(branch, baseBranch)
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
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return err
	}
	return wipeDeclaredWritesInDir(wf.WorkDir(), writes)
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
