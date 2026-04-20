package mcpserver

// Fetch-path reconciliation: the mechanism that turns a scanner-
// spotted commit into a coordinator state transition. Phase 4c
// wires the three primitives from internal/mcpgit
// (FetchBranch + ScanBranchSince + Cursors) together with the
// coordinator's /tasks/reconcile endpoint.
//
// Design: a single helper `reconcileBranch(projectID, branch)`
// that a handler can call opportunistically. It fetches the
// branch, scans new commits since the persisted cursor, POSTs
// any Enju-Task-Complete trailers to the coordinator, and
// advances the cursor on success. Best-effort — if the fetch
// fails (no network) or reconcile fails (server down), the
// cursor stays put and the next call retries.
//
// Hook points: places the fat client naturally touches git +
// talks to the coordinator already, so reconciliation adds no
// new round trips on the fast path. Phase 4c starts with
// run_status; phase 4d integration test will drive this via
// a completed async compute.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enju-ai/enju/internal/mcpgit"
)

// buildReconcileBody turns scanner results into the batch shape
// the /tasks/reconcile endpoint consumes. Matches api.reconcileEntry
// field names — keep in sync if one side changes.
func buildReconcileBody(trailers []mcpgit.CommitTrailer) map[string]interface{} {
	entries := make([]map[string]interface{}, 0, len(trailers))
	for _, t := range trailers {
		entry := map[string]interface{}{
			"task_id":    t.Trailers.TaskID,
			"commit_sha": t.CommitSHA,
		}
		if t.Trailers.ExitSet {
			entry["exit_code"] = t.Trailers.ExitCode
		}
		if len(t.Trailers.Artifacts) > 0 {
			entry["artifacts_written"] = t.Trailers.Artifacts
		}
		entries = append(entries, entry)
	}
	return map[string]interface{}{"tasks": entries}
}

// stateDir returns the directory used for per-project cursor
// files. Derived from the workspace root so:
//
//   - Production (workspace root = ~/.enju/workspaces) puts
//     state at ~/.enju/workspaces/.state/, keeping everything
//     the fat client persists under one directory tree.
//   - Tests (workspace root = t.TempDir()/.../workspaces) get
//     isolated state per test run — no writes to the real
//     user home, no cursor contention across unrelated
//     tests, cleanup is automatic when the test temp dir is
//     removed.
//
// Falls back to ~/.enju/state/ only when no workspace is
// configured (local-only / legacy callers).
func (c *apiClient) stateDir() string {
	if c.workspace != nil {
		return filepath.Join(c.workspace.RootDir(), ".state")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".enju", "state")
}

// reconcileRunBranch is the sugar handlers (run_status) use when
// they have a run payload in hand but don't want the pull —
// run_status is read-only, the worktree needn't be updated, but
// the async-completion sweep is still worthwhile. Piggybacks on
// reconcileBranch's own FetchBranch call so the scanner sees
// the latest tips.
func (c *apiClient) reconcileRunBranch(ctx context.Context, projectID int64, runData []byte) {
	if c.workspace == nil {
		return
	}
	branch := runBranchFromData(runData)
	if branch == "" {
		return
	}
	proj, _, _, _, err := c.openProject(ctx, projectID)
	if err != nil || proj == nil {
		return
	}
	// Git phase: fetch + scan under proj.Lock. Release BEFORE
	// posting to the coordinator so concurrent ops don't
	// block on the HTTP round-trip.
	proj.Lock()
	ferr := proj.FetchBranch(branch)
	var trailers []mcpgit.CommitTrailer
	var newTip, preCursor string
	if ferr == nil {
		cursorMu := mcpgit.CursorMutexFor(c.stateDir(), projectID)
		cursorMu.Lock()
		cursors, _ := mcpgit.LoadCursors(c.stateDir(), projectID)
		preCursor = cursors.Get(branch)
		// See pullBranchWithReconcile's identical block for
		// the full rationale: persist-on-first-touch so a
		// brand-new run branch doesn't fall back to the
		// first-scan baseline (or race with the wrapper's
		// own local-ref advance) and orphan a task.
		if preCursor == "" {
			if h, herr := proj.LocalBranchHash(branch); herr == nil && h != "" {
				preCursor = h
				cursors.Set(branch, h)
				_ = cursors.Save()
			}
		}
		cursorMu.Unlock()
		if tip, found, serr := proj.ScanBranchSince(branch, preCursor); serr == nil {
			newTip = tip
			trailers = found
		} else {
			c.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
		}
	}
	proj.Unlock()

	if len(trailers) > 0 {
		if _, perr := c.post(ctx, "/api/v1/tasks/reconcile", buildReconcileBody(trailers)); perr != nil {
			c.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			c.reapWrapperFailures(ctx, proj, projectID)
			return
		}
	}
	if newTip != "" && newTip != preCursor {
		cursorMu := mcpgit.CursorMutexFor(c.stateDir(), projectID)
		cursorMu.Lock()
		latest, _ := mcpgit.LoadCursors(c.stateDir(), projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}
	c.reapWrapperFailures(ctx, proj, projectID)
}

// pullBranchWithReconcile is the shared "freshen local clone +
// sweep for async completions" helper that handlers call in
// place of a bare proj.PullBranch(branch). One call does
// everything the fat-client wants to do before operating on a
// task's branch:
//
//   1. Pull the branch so the worktree reflects the latest
//      remote state (scripts, templates, upstream results).
//   2. Scan new commits on origin/<branch> for Enju-Task-Complete
//      trailers and POST them to /tasks/reconcile.
//   3. Reap any local .wrap-result.json files that show a
//      non-zero exit so failed async compute tasks get marked
//      failed instead of hanging in claimed.
//
// All three cost roughly one git network round-trip + a walk of
// unread commits. Running them together on every touch of a
// branch keeps the "implicit reconcile" invariant: a user who
// runs any branch-touching tool automatically surfaces whatever
// async work finished since the last check. Errors from the
// scanner + reaper are logged but never propagated — the
// handler's own pull error is the only thing a caller sees.
//
// Locks are handled internally. Caller MUST NOT hold proj.Lock
// across this call (it would deadlock); subsequent operations
// that need the lock (resolver reads, commit writes) re-acquire
// after this returns.
func (c *apiClient) pullBranchWithReconcile(ctx context.Context, proj *mcpgit.Project, projectID int64, branch string) error {
	if proj == nil {
		return nil
	}
	// Git phase: checkout + pull + fetch-for-scan + scan,
	// all under proj.Lock.
	//
	// Checkout FIRST so the worktree reflects the target run's
	// branch. Without this, running-multiple-runs-per-session
	// breaks: the fat client stays on whatever branch the last
	// tool call switched to, and template snapshots / script
	// paths for other runs aren't on disk.
	//
	// PullBranch then fetches + merges into HEAD, advancing
	// the LOCAL branch ref. Important detail: it does NOT
	// reliably update `refs/remotes/origin/<branch>` (the
	// remote-tracking ref the scanner reads for `tip`). We
	// observed this directly — after a prior sync submit
	// pushed a commit on this branch, the local origin-
	// tracking ref stayed at the pre-push commit, and the
	// scanner then walked from an up-to-date cursor BACK
	// through history (because the tracking ref pointed at an
	// older ancestor), silently re-posting already-reconciled
	// trailers. That cascaded into "task already accepted at
	// commit X — cannot reconcile Y" errors that rejected the
	// real new commit.
	//
	// Fix: explicit FetchBranch after PullBranch so origin/
	// <branch> refs are refreshed before the scan reads them.
	// Small cost (one ls-remote + short fetch) in exchange
	// for correct scan semantics.
	//
	// Dirty-worktree case: go-git's non-Force Checkout refuses
	// to switch when there are uncommitted changes — the error
	// propagates to the user with a clear "you have local
	// changes" message, which is the safest default.
	proj.Lock()
	if branch != "" {
		if err := proj.CheckoutBranch(branch); err != nil {
			proj.Unlock()
			return fmt.Errorf("switching workspace to branch %q: %w", branch, err)
		}
	}
	pullErr := proj.PullBranch(branch)
	if branch != "" {
		// Best-effort — if fetch fails (network, missing
		// remote ref) we still proceed with whatever the
		// scanner can see. ScanBranchSince already tolerates
		// a stale / missing origin ref.
		_ = proj.FetchBranch(branch)
	}
	var trailers []mcpgit.CommitTrailer
	var newTip string
	var preCursor string
	if branch != "" {
		cursorMu := mcpgit.CursorMutexFor(c.stateDir(), projectID)
		cursorMu.Lock()
		cursors, _ := mcpgit.LoadCursors(c.stateDir(), projectID)
		preCursor = cursors.Get(branch)
		// Cursor baseline seeding: without this, a freshly-
		// forked run branch (no origin/<branch> yet when the
		// handler first touches it) gets an empty cursor;
		// after the wrapper pushes, a subsequent scan finds
		// preCursor="" and either hits ScanBranchSince's
		// "first-time baseline" branch (tip+nil, silently
		// skipping the wrapper's trailer) OR reads a local
		// branch ref that the wrapper itself has already
		// advanced to the completion commit (preCursor==tip,
		// walks nothing). Either way the task orphans.
		//
		// Fix: on first touch, capture the current LOCAL
		// branch ref hash AND persist it to the cursor file
		// BEFORE anything else runs. That pins the scanner's
		// starting point to the pre-wrapper state (baseHash
		// for a fresh branch; current tip for an existing
		// one). The cursor is now a real commit anchor, not
		// a sentinel "first scan" value, so the next scan
		// walks commits beyond it correctly.
		if preCursor == "" {
			if h, herr := proj.LocalBranchHash(branch); herr == nil && h != "" {
				preCursor = h
				cursors.Set(branch, h)
				_ = cursors.Save()
			}
		}
		cursorMu.Unlock()
		tip, found, serr := proj.ScanBranchSince(branch, preCursor)
		if serr != nil {
			c.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
		} else {
			newTip = tip
			trailers = found
		}
	}
	proj.Unlock()

	// Network phase: POST /tasks/reconcile without the git
	// lock so concurrent goroutines touching the same project
	// don't block on our round-trip to the coordinator.
	if len(trailers) > 0 {
		body := buildReconcileBody(trailers)
		if _, perr := c.post(ctx, "/api/v1/tasks/reconcile", body); perr != nil {
			// Leave cursor unchanged on POST failure so a
			// retry replays the batch. Reconcile is
			// idempotent at the coordinator.
			c.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			// Reap still useful regardless.
			c.reapWrapperFailures(ctx, proj, projectID)
			return pullErr
		}
	}

	// Cursor-save phase: advance past the scanned tip.
	// Fresh load to absorb any parallel advanceCursorPastCommit
	// that might have run while we were posting.
	if newTip != "" && newTip != preCursor {
		cursorMu := mcpgit.CursorMutexFor(c.stateDir(), projectID)
		cursorMu.Lock()
		latest, _ := mcpgit.LoadCursors(c.stateDir(), projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}

	c.reapWrapperFailures(ctx, proj, projectID)
	return pullErr
}

// Compile-time guard: the reconcile body's shape is JSON-marshalable.
var _ = json.Marshal
