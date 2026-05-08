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
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

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
	if s.enjugit != nil {
		return filepath.Join(s.enjugit.RootDir(), ".state")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".enju", "state")
}

// PullBranchWithReconcileWF is the enjugit-Workflow flavored
// counterpart to PullBranchWithReconcile. Same semantics:
// checkout + pull + fetch + scan + reconcile-post + cursor
// advance, plus wrapper-failure reap. Used by claim.go's
// fat-client paths after the port to enjugit.
//
// Mirrors PullBranchWithReconcile step-for-step but on a
// *enjugit.Workflow handle, using enjugit's CursorMutexFor /
// LoadCursors and the Workflow's PullBranch / FetchBranch /
// ScanBranchSince / LocalBranchHash.
//
// Errors: same contract as the project version. Pull error is
// the only thing surfaced; scanner / reconcile-post / reaper
// failures are logged at Debug.
func (s *FatClient) PullBranchWithReconcileWF(ctx context.Context, wf *enjugit.Workflow, projectID int64, branch string) error {
	if wf == nil {
		return nil
	}
	if branch != "" {
		if err := wf.CheckoutBranch(branch); err != nil {
			return fmt.Errorf("switching workspace to branch %q: %w", branch, err)
		}
	}
	pullErr := wf.PullBranch(branch)
	if branch != "" {
		_ = wf.FetchBranch(branch)
	}
	var trailers []enjugit.CommitTrailer
	var newTip string
	var preCursor string
	if branch != "" {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := enjugit.LoadCursors(stateDir, projectID)
		preCursor = cursors.Get(branch)
		// Persist-on-first-touch — same baseline-seeding rationale
		// as the project-flavored version. See its comment block.
		if preCursor == "" {
			if h, herr := wf.LocalBranchHash(branch); herr == nil && h != "" {
				preCursor = h
				cursors.Set(branch, h)
				_ = cursors.Save()
			}
		}
		cursorMu.Unlock()
		res, serr := wf.ScanBranchSince(branch, preCursor)
		if serr != nil {
			s.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
		} else {
			newTip = res.NewTip
			trailers = res.Trailers
		}
	}

	// Network phase: POST /tasks/reconcile.
	if len(trailers) > 0 {
		body := buildReconcileBodyWF(trailers)
		if _, perr := s.coord.Post(ctx, "/api/v1/tasks/reconcile", body); perr != nil {
			s.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			s.ReapWrapperFailuresWF(ctx, wf)
			return pullErr
		}
	}

	// Cursor-save phase.
	if newTip != "" && newTip != preCursor {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		latest, _ := enjugit.LoadCursors(stateDir, projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}

	s.ReapWrapperFailuresWF(ctx, wf)
	return pullErr
}

// buildReconcileBodyWF mirrors BuildReconcileBody but on
// enjugit.CommitTrailer (vs project.CommitTrailer). Same
// payload shape — keep in sync if either side changes.
func buildReconcileBodyWF(trailers []enjugit.CommitTrailer) map[string]interface{} {
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

// ReapWrapperFailuresWF is the enjugit-Workflow flavored
// counterpart to ReapWrapperFailures — walks the workflow's
// worktree for .wrap-result.json files and posts /tasks/:id/fail
// for non-zero exits.
func (s *FatClient) ReapWrapperFailuresWF(ctx context.Context, wf *enjugit.Workflow) {
	if wf == nil {
		return
	}
	root := filepath.Join(wf.WorkDir(), "enju", "runs")
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
	if s.enjugit == nil {
		return
	}
	branch := RunBranchFromData(runData)
	if branch == "" {
		return
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return
	}
	ferr := wf.FetchBranch(branch)
	var trailers []enjugit.CommitTrailer
	var newTip, preCursor string
	if ferr == nil {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		cursors, _ := enjugit.LoadCursors(stateDir, projectID)
		preCursor = cursors.Get(branch)
		// Persist-on-first-touch — same rationale as
		// PullBranchWithReconcileWF's identical block.
		if preCursor == "" {
			if h, herr := wf.LocalBranchHash(branch); herr == nil && h != "" {
				preCursor = h
				cursors.Set(branch, h)
				_ = cursors.Save()
			}
		}
		cursorMu.Unlock()
		if res, serr := wf.ScanBranchSince(branch, preCursor); serr == nil {
			newTip = res.NewTip
			trailers = res.Trailers
		} else {
			s.logger.Debug("reconcile scan", "project", projectID, "branch", branch, "error", serr)
		}
	}

	if len(trailers) > 0 {
		if _, perr := s.coord.Post(ctx, "/api/v1/tasks/reconcile", buildReconcileBodyWF(trailers)); perr != nil {
			s.logger.Debug("reconcile post", "project", projectID, "branch", branch, "error", perr)
			s.ReapWrapperFailuresWF(ctx, wf)
			return
		}
	}
	if newTip != "" && newTip != preCursor {
		stateDir := s.StateDir()
		cursorMu := enjugit.CursorMutexFor(stateDir, projectID)
		cursorMu.Lock()
		latest, _ := enjugit.LoadCursors(stateDir, projectID)
		latest.Set(branch, newTip)
		_ = latest.Save()
		cursorMu.Unlock()
	}
	s.ReapWrapperFailuresWF(ctx, wf)
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
