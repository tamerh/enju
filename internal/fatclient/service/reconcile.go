package service

// Fetch-path reconciliation: turns scanner-spotted commits into
// coordinator state transitions. Wires the three primitives from
// internal/fatclient/project (FetchBranch + ScanBranchSince +
// Cursors) together with the coordinator's /tasks/reconcile
// endpoint, plus an async-wrapper failure reaper that catches
// non-zero exits the trailer scanner can't see.
//
// Lives on FatClient because every per-tool service call that
// touches a project's branch wants the same "freshen + sweep"
// semantics. Hook points: places the fat client naturally touches
// git + talks to the coordinator already, so reconciliation adds
// no new round trips on the fast path.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/project"
)

// BuildReconcileBody turns scanner results into the batch shape
// the /tasks/reconcile endpoint consumes. Matches api.reconcileEntry
// field names — keep in sync if one side changes.
//
// `artifacts_written` carries both tracked (from the commit's
// Enju-Artifacts trailer) and untracked (from
// Enju-Untracked-Artifacts) paths as a single union — the
// coordinator's engine looks up each path's Track flag from
// the task's writes_artifacts declaration and routes the
// index mutation accordingly. Missing the untracked set was
// the bug that blocked async downstream tasks reading
// track:false outputs: sync path POSTs via /tasks/:id/result
// with the full union; async path had to rely on the trailer.
func BuildReconcileBody(trailers []project.CommitTrailer) map[string]interface{} {
	entries := make([]map[string]interface{}, 0, len(trailers))
	for _, t := range trailers {
		entry := map[string]interface{}{
			"task_id":    t.Trailers.TaskID,
			"commit_sha": t.CommitSHA,
		}
		if t.Trailers.ExitSet {
			entry["exit_code"] = t.Trailers.ExitCode
		}
		combined := append([]string(nil), t.Trailers.Artifacts...)
		combined = append(combined, t.Trailers.UntrackedArtifacts...)
		if len(combined) > 0 {
			entry["artifacts_written"] = combined
		}
		entries = append(entries, entry)
	}
	return map[string]interface{}{"tasks": entries}
}

// StateDir returns the directory used for per-project cursor
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
func (s *FatClient) StateDir() string {
	if s.project != nil {
		return filepath.Join(s.project.RootDir(), ".state")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".enju", "state")
}

// PullBranchWithReconcile is the shared "freshen local clone +
// sweep for async completions" helper that handlers call in
// place of a bare proj.PullBranch(branch). One call does
// everything the fat-client wants to do before operating on a
// task's branch:
//
//  1. Pull the branch so the worktree reflects the latest
//     remote state (scripts, templates, upstream results).
//  2. Scan new commits on origin/<branch> for Enju-Task-Complete
//     trailers and POST them to /tasks/reconcile.
//  3. Reap any local .wrap-result.json files that show a
//     non-zero exit so failed async compute tasks get marked
//     failed instead of hanging in claimed.
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
func (s *FatClient) PullBranchWithReconcile(ctx context.Context, proj *project.Clone, projectID int64, branch string) error {
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
	var trailers []project.CommitTrailer
	var newTip string
	var preCursor string
	if branch != "" {
		cursorMu := project.CursorMutexFor(s.StateDir(), projectID)
		cursorMu.Lock()
		cursors, _ := project.LoadCursors(s.StateDir(), projectID)
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
			s.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
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
		body := BuildReconcileBody(trailers)
		if _, perr := s.coord.Post(ctx, "/api/v1/tasks/reconcile", body); perr != nil {
			// Leave cursor unchanged on POST failure so a
			// retry replays the batch. Reconcile is
			// idempotent at the coordinator.
			s.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			// Reap still useful regardless.
			s.ReapWrapperFailures(ctx, proj, projectID)
			return pullErr
		}
	}

	// Cursor-save phase: advance past the scanned tip.
	// Fresh load to absorb any parallel advanceCursorPastCommit
	// that might have run while we were posting.
	if newTip != "" && newTip != preCursor {
		cursorMu := project.CursorMutexFor(s.StateDir(), projectID)
		cursorMu.Lock()
		latest, _ := project.LoadCursors(s.StateDir(), projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}

	s.ReapWrapperFailures(ctx, proj, projectID)
	return pullErr
}

// ReconcileRunBranch is the read-only reconcile path used by
// handlers (run_status) that have a run payload in hand and
// don't want a full pull — fetch + scan only, then post any
// new trailers. Cheaper than PullBranchWithReconcile and safe
// to call on every render of run_status.
//
// Pulls the branch from runData (a coordinator run-detail
// payload), opens the project workspace, fetches + scans for
// trailers, posts to /tasks/reconcile, advances the cursor,
// reaps wrapper failures. Best-effort throughout — fetch /
// scan / post errors are logged at Debug and the call returns
// without surfacing them.
func (s *FatClient) ReconcileRunBranch(ctx context.Context, projectID int64, runData []byte) {
	if s.project == nil {
		return
	}
	branch := RunBranchFromData(runData)
	if branch == "" {
		return
	}
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil || proj == nil {
		return
	}
	// Git phase: fetch + scan under proj.Lock. Release BEFORE
	// posting to the coordinator so concurrent ops don't block
	// on the HTTP round-trip.
	proj.Lock()
	ferr := proj.FetchBranch(branch)
	var trailers []project.CommitTrailer
	var newTip, preCursor string
	if ferr == nil {
		stateDir := s.StateDir()
		cursorMu := project.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := project.LoadCursors(stateDir, projectID)
		preCursor = cursors.Get(branch)
		// Persist-on-first-touch so a brand-new run branch
		// doesn't fall back to the first-scan baseline (or
		// race with the wrapper's own local-ref advance) and
		// orphan a task. See PullBranchWithReconcile's
		// identical block for the full rationale.
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
			s.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
		}
	}
	proj.Unlock()

	if len(trailers) > 0 {
		if _, perr := s.coord.Post(ctx, "/api/v1/tasks/reconcile", BuildReconcileBody(trailers)); perr != nil {
			s.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			s.ReapWrapperFailures(ctx, proj, projectID)
			return
		}
	}
	if newTip != "" && newTip != preCursor {
		stateDir := s.StateDir()
		cursorMu := project.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		latest, _ := project.LoadCursors(stateDir, projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}
	s.ReapWrapperFailures(ctx, proj, projectID)
}

// ReapWrapperFailures walks the project's enju/runs tree
// looking for detached-wrapper result files whose recorded exit
// is non-zero. For each, posts /tasks/:id/fail and moves the
// result file aside so we don't re-notify. Silent on failures —
// a post that errors leaves the file in place so a retry on
// next call sweeps it up.
//
// Called from reconcile hook points; cost is one directory
// walk bounded by the number of runs × instances in the
// project's enju/runs tree. Empty or sync-only projects
// terminate quickly because most directories hold no
// .wrap-result.json file.
//
// Companion to the fetch-path scanner: the scanner catches
// SUCCESSFUL async completions via Enju-Task-Complete commit
// trailers; this reaper catches FAILURES by walking the
// `.wrap-result.json` files the detached wrapper writes
// alongside each task's result directory. Today's wrapper
// (matching the sync path) doesn't commit on exit != 0 so a
// failed async task has no scanner-visible signal — the reaper
// fills that gap when the submitter comes back online.
func (s *FatClient) ReapWrapperFailures(ctx context.Context, proj *project.Clone, projectID int64) {
	if proj == nil {
		return
	}
	root := filepath.Join(proj.WorkDir(), "enju", "runs")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) != ".wrap-result.json" {
			return nil
		}
		s.handleOneWrapperResult(ctx, path)
		return nil
	})
}

// handleOneWrapperResult processes one wrapper result file.
// Reads result + corresponding spec, and when the result
// indicates a script failure posts /tasks/:id/fail to the
// coordinator. On success the result file is renamed to
// .wrap-result.done.json so a later reap doesn't revisit it.
// Idempotent — the coordinator rejects /fail on tasks already
// in terminal state, which we treat as "already handled, move
// on."
func (s *FatClient) handleOneWrapperResult(ctx context.Context, resultPath string) {
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return
	}
	var res compute.Result
	if err := json.Unmarshal(data, &res); err != nil {
		// Malformed file — don't keep retrying forever; rename
		// so the reaper skips it. A human can inspect after.
		_ = os.Rename(resultPath, resultPath+".malformed")
		return
	}
	if res.ExitCode == 0 && res.Error == "" {
		// Success case — the trailer scanner handles this via
		// /tasks/reconcile. We just need to mark the file
		// processed so the next reap walk doesn't slow down
		// re-reading it.
		_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
		return
	}

	// Failure path: read the companion spec file for the task
	// id. Spec lives next to result in the wrapper's kickoff
	// payload (see kickoffAsyncWrapTask).
	specPath := filepath.Join(filepath.Dir(resultPath), ".wrap-spec.json")
	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		// No spec → can't name the task. Skip; a future
		// wrapper version might emit task_id in the result
		// directly to avoid this dependency.
		return
	}
	var spec compute.Spec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return
	}
	if spec.TaskID == "" {
		return
	}

	reason := buildFailReason(spec, res)
	_, postErr := s.coord.Post(ctx, fmt.Sprintf("/api/v1/tasks/%s/fail", spec.TaskID), map[string]string{
		"reason": reason,
	})
	if postErr != nil {
		// Coordinator-side refusal (already terminal,
		// membership, etc) is fine — treat as "handled,
		// move on." Network errors also leave the file
		// alone; next reap will retry.
		if !strings.Contains(postErr.Error(), "terminal") {
			s.logger.Debug("reap post fail", "task_id", spec.TaskID, "error", postErr)
			return
		}
	}
	_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
}

// buildFailReason renders a short human-readable failure
// message from the wrapper's result. Prefers a wrapper-level
// Error (spec parse / fork failures) over script exit code +
// stderr tail, since the former is the truer root cause. Caps
// the stderr tail to keep the coordinator payload reasonable.
func buildFailReason(spec compute.Spec, res compute.Result) string {
	if res.Error != "" {
		return fmt.Sprintf("async wrap-task failed: %s", res.Error)
	}
	label := spec.ScriptLabel
	if label == "" {
		label = "script"
	}
	msg := fmt.Sprintf("async %s exited with code %d", label, res.ExitCode)
	if res.Stderr != "" {
		tail := res.Stderr
		if len(tail) > 800 {
			tail = tail[:800] + "...(truncated)"
		}
		msg += ": " + tail
	}
	return msg
}
