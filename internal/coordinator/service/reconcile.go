package service

import (
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReconcileEntry is one item in a reconcile batch. Same
// semantics as the submit fields but flattened for batch
// transport. All fields are optional on the wire except
// TaskID + CommitSHA + ExitCode — the fetch-path scanner
// extracts them from commit trailers and forwards whatever it
// parsed.
type ReconcileEntry struct {
	TaskID      string
	CommitSHA    string
	ExitCode     int
	ResultPath    string
	ArtifactsWritten []string
	Content     string
	FailReason    string // optional override when ExitCode != 0
	Username     string
	Model      string
}

// ReconcileResult is the per-entry outcome. Status is one of
// "accepted", "failed", "noop" (already terminal at this
// commit), or "error" — the scanner advances its cursor past
// entries regardless of status so a persistent error doesn't
// wedge the queue, but surfaces the error text so humans can
// diagnose.
type ReconcileResult struct {
	TaskID  string `json:"task_id"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// ReconcileTask handles a single reconcile entry. Returns the
// per-entry result; never errors at the Go level — every
// failure path surfaces via res.Error so a batch caller can
// aggregate without aborting the loop.
//
// Trust model: matches today's submit — the coordinator takes
// the client's word for the commit contents. Shape-check on
// commit_sha still fires; project membership stays gated so a
// client can only reconcile tasks it's allowed to see.
func (c *Coordinator) ReconcileTask(caller *store.CitizenRecord, entry ReconcileEntry) ReconcileResult {
	res := ReconcileResult{TaskID: entry.TaskID, CommitSHA: entry.CommitSHA}

	if entry.TaskID == "" {
		res.Status = "error"
		res.Error = "task_id is required"
		return res
	}
	if entry.CommitSHA == "" {
		res.Status = "error"
		res.Error = "commit_sha is required"
		return res
	}
	if !IsValidCommitSHAShape(entry.CommitSHA) {
		res.Status = "error"
		res.Error = fmt.Sprintf("commit_sha %q is not a valid git SHA (expected 40 or 64 hex characters)", entry.CommitSHA)
		return res
	}

	task, err := c.Store.GetTask(entry.TaskID)
	if err != nil || task == nil {
		res.Status = "error"
		res.Error = fmt.Sprintf("task %q not found", entry.TaskID)
		return res
	}

	// Idempotency: a task that already reached a terminal
	// state at THIS commit is a no-op success. Different
	// commit at terminal state means the caller is trying to
	// rewrite history — error, not a silent overwrite.
	if task.State == store.TaskAccepted || task.State == store.TaskFailed {
		if task.CommitSHA == entry.CommitSHA {
			res.Status = "noop"
			return res
		}
		res.Status = "error"
		res.Error = fmt.Sprintf("task already %s at commit %s — cannot reconcile a different commit %s",
			task.State, shortCommit(task.CommitSHA), shortCommit(entry.CommitSHA))
		return res
	}
	// Reconcile only advances tasks from in-flight states
	// (claimed / running). Anything else is a stale trailer
	// (e.g. scanner re-reads an old completion commit on a
	// task that has since been invalidated). Advancing would
	// silently resurrect the old completion and clobber any
	// in-progress re-run, so treat as no-op.
	if task.State != store.TaskClaimed && task.State != store.TaskRunning {
		res.Status = "noop"
		return res
	}

	// Membership gate. Done after the cheap shape/state checks
	// so a stale-trailer no-op for a project the caller can't
	// see still no-ops cleanly without leaking authorization-
	// signal back through Status="error".
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		res.Status = "error"
		res.Error = fmt.Sprintf("run not found for task %q", entry.TaskID)
		return res
	}
	if !CanReadProject(c.Store, run.ProjectID, callerID(caller)) {
		res.Status = "error"
		res.Error = "not a member of this project"
		return res
	}

	// Route exit != 0 to the fail cascade — same path the
	// sync submit handler uses for compute-script failures.
	if entry.ExitCode != 0 {
		reason := entry.FailReason
		if reason == "" {
			reason = fmt.Sprintf("script exited with code %d", entry.ExitCode)
		}
		if _, ferr := engine.New(c.Store, c.Logger).ComputeFailTask(entry.TaskID, reason); ferr != nil {
			res.Status = "error"
			res.Error = "fail precondition: " + ferr.Error()
			return res
		}
		if _, ferr := c.PerformFailCascade(entry.TaskID, reason); ferr != nil {
			res.Status = "error"
			res.Error = "fail cascade: " + ferr.Error()
			return res
		}
		res.Status = "failed"
		return res
	}

	// Exit 0 — delegate to the shared submit core (same path
	// the sync submit uses).
	engineReq := &engine.SubmitRequest{
		TaskID:      task.ID,
		ResultPath:    entry.ResultPath,
		CommitSHA:    entry.CommitSHA,
		Username:     entry.Username,
		Content:     entry.Content,
		ArtifactsWritten: entry.ArtifactsWritten,
	}
	if _, aerr := c.AcceptComputeTaskCore(task, run, engineReq, entry.Model); aerr != nil {
		res.Status = "error"
		res.Error = aerr.Error()
		return res
	}
	// Ready-task sweep. Without this, any downstream whose only
	// remaining blocker was this task stays in PENDING. Errors
	// logged-and-swallowed (the same pattern the sync path uses)
	// since a sweep failure mid-flight still leaves the
	// submission applied correctly. Fired through ApplyPlan so
	// the single emit site (applyUpdateReadyTasks) handles it —
	// AcceptComputeTaskCore's submit plan doesn't itself include
	// the cascade.
	// Cascade + run-state evaluation in one plan: same shape
	// as submit.go's step 7 — one tx, one drain.
	if _, uerr := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CompleteRun{RunID: task.RunID},
		},
	}.AppendCascade(task.RunID)); uerr != nil {
		c.Logger.Warn("reconcile ready-sweep", "task_id", task.ID, "run_id", task.RunID, "error", uerr)
	}
	res.Status = "accepted"
	return res
}

// IsValidCommitSHAShape reports whether s looks like a git
// commit SHA — 40 hex (SHA-1) or 64 hex (SHA-256) lower-case
// — AND isn't one of the obviously-phantom patterns (all
// zeros, all same digit) that a buggy or test-harness client
// would submit by accident.
//
// Shape sanity check only — under the trust-the-client
// architecture, existence verification by fetching the commit
// is a client responsibility. Server-side verify-by-fetching
// is tracked as pre-launch work for hosted mode where the
// trust-the-client assumption no longer holds.
//
// Rejecting well-known phantoms (all-zero especially) closes
// the "I sent '0000...000' manually and it was accepted" class
// of reports without requiring the coordinator to clone and
// verify remotes.
func IsValidCommitSHAShape(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	// All-same-digit phantoms: "0000..." is the empty-SHA
	// sentinel go-git uses as a nil-ref marker; "ffff..." and
	// other repeats are common test-garbage values. A real
	// commit SHA has entropy — accidental all-same-char is
	// cryptographically impossible.
	first := s[0]
	for i := 1; i < len(s); i++ {
		if s[i] != first {
			return true
		}
	}
	return false
}

// shortCommit formats a commit SHA for display in error
// messages (7 chars, git's standard abbreviation).
func shortCommit(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
