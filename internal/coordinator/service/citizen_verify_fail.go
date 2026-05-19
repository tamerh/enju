package service

// Layer-① contract-gate failure handling (citizen-task-retryable).
//
// A citizen task passes through two independent gates, in order:
//
//	① CONTRACT gate (writes-verify): did the agent produce the
//	   declared writes_artifacts? If not, the submission is REFUSED
//	   before any reviewer sees it — no content exists.
//	② REVIEW gate (quality): is the content good enough? Reviewer
//	   request_changes → iterate (bounded by the cycle budget).
//
// Layer ② already had a recovery path. THIS file is layer ①: a
// citizen task that repeatedly fails the contract gate is bounded
// by a durable, per-task, COORDINATOR-side counter and then parked
// failed_retryable — recoverable via enju_retry_task, descendants
// left PENDING. The two layers must never be conflated: a verify
// failure must not consume cycle budget, and request_changes must
// not bump the verify-fail counter (it is structurally impossible
// here — request_changes is submitted→accepted→review, and the
// counter is reset at accept).
//
// Why coordinator-side enforcement (not just client-reported):
// only the client sees the claim CWD, so it DETECTS the miss — but
// the count must survive daemon restart / lease-reclaim / a
// different claimant, and a crashed client must still escalate.
// The coordinator is therefore the enforcement boundary: it counts
// on the client report AND, independently, on the reaper observing
// the lease expire with no submission for the iteration (see
// reaper backstop). The (task_id, iter_seq) idempotency key makes
// those two producers count an iteration exactly once.
//
// Configuration hierarchy (mirrors taskClaimTimeout):
//
//	defaultVerifyFailCap const (3)
//	  ← overridden by  defaults: verify_retry_cap   (workflow)
//	  ← overridden by  verify_retry_cap              (per-task)

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// defaultVerifyFailCap is the coordinator's built-in cap on
// CONSECUTIVE layer-① non-delivery iterations. Mirrors the
// defaultClaimTimeout (30m) decision: a const an operator can
// override per-task via YAML without a server restart. 3 is enough
// to tell a non-deterministic one-off whiff from a deterministic
// miss without burning many wasted LLM iterations.
const defaultVerifyFailCap = 3

// taskVerifyFailCap resolves the effective cap. Per-task
// verify_retry_cap wins; 0 falls back to the coordinator's
// built-in const. Exact mirror of taskClaimTimeout.
func taskVerifyFailCap(task *store.TaskRecord) int {
	if task != nil && task.VerifyRetryCap > 0 {
		return task.VerifyRetryCap
	}
	return defaultVerifyFailCap
}

// CitizenVerifyFailResponse is the wire shape returned to the
// fat-client from POST /tasks/:id/citizen-verify-failed.
type CitizenVerifyFailResponse struct {
	Status    string `json:"status"` // "counted" | "escalated"
	TaskID    string `json:"task_id"`
	FailCount int    `json:"fail_count"` // count AFTER this report
	Cap       int    `json:"cap"`
	Reason    string `json:"reason,omitempty"` // set when escalated
}

// ReportCitizenVerifyFail is the coordinator-side handler for the
// fat-client's layer-① verify-fail report (and the operator escape
// hatch when force is set). It:
//
//  1. Authenticates and gates on project membership.
//  2. Enforces the claimant invariant (failTaskOwnershipOK): a bot
//     may only report a task it currently holds — a bot that merely
//     couldn't claim must not drive that task's state. Humans keep
//     operator-override.
//  3. State-gates to CLAIMED/RUNNING: a report on an
//     already-parked / accepted / terminal task is late or
//     duplicate (e.g. the reaper backstop already escalated) and is
//     refused with a clear error rather than counted.
//  4. Multi-claimant guard (v1): rejects citizens>1 — N-claimant
//     verify-fail semantics are deferred.
//  5. force=true (operator escape hatch, D5): park failed_retryable
//     NOW, bypassing the counter — but the SAME recoverable state,
//     never terminal failed.
//  6. Otherwise: idempotently increments the durable per-task
//     counter keyed on the open claim's iter_seq, then re-reads the
//     authoritative count and, at the effective cap, parks the task
//     failed_retryable in the same logical step.
func (c *Coordinator) ReportCitizenVerifyFail(caller *store.CitizenRecord, taskID, reason string, force bool) (*CitizenVerifyFailResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalidArgument)
	}

	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("%w: task %q not found", ErrNotFound, taskID)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("%w: run for task %q not found", ErrNotFound, taskID)
	}
	if !CanReadProject(c.Store, run.ProjectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrForbidden)
	}
	// Same ownership invariant FailTask enforces: a bot may only
	// report a task it holds the claim on; a human operator may
	// report any task in a project they can read (escape hatch).
	if err := failTaskOwnershipOK(caller, task); err != nil {
		return nil, err
	}

	// State gate. CLAIMED or RUNNING only. Any other state means a
	// late / duplicate report — most importantly, if the reaper
	// backstop already parked the task failed_retryable, this
	// refusal is what stops a slow client report from being
	// re-counted on top of the already-final escalation.
	st := store.TaskState(task.State)
	if st != store.TaskClaimed && st != store.TaskRunning {
		return nil, fmt.Errorf(
			"%w: task %q is %s — citizen-verify-fail requires CLAIMED or RUNNING (late/duplicate report)",
			ErrInvalidArgument, taskID, st)
	}

	// Multi-claimant tasks (vote/review, citizens>1): which
	// claimant's miss counts? does one claimant's verify-fail park
	// every peer? Deferred in v1 — reject clearly.
	if task.Citizens > 1 {
		return nil, fmt.Errorf(
			"%w: citizen-verify-fail is not supported for multi-claimant tasks "+
				"(task %q has citizens=%d); N-claimant semantics are deferred",
			ErrInvalidArgument, taskID, task.Citizens)
	}

	cap := taskVerifyFailCap(task)
	claimant := task.ClaimedBy // captured BEFORE the park clears it

	// Operator escape hatch (D5): escalate now, same recoverable
	// state. Bypasses the counter but NOT the recovery semantics —
	// failed_retryable, descendants PENDING, never terminal failed.
	if force {
		failReason := fmt.Sprintf(
			"citizen-verify-fail: operator-forced escalation of task %q (cap=%d); reason: %s",
			taskID, cap, reason)
		if err := c.performCitizenVerifyFailure(taskID, failReason); err != nil {
			return nil, err
		}
		c.recordVerifyFailContribution(claimant, taskID, task.RunID, run.ProjectID, failReason, task.VerifyFailCount, cap)
		c.Logger.Info("citizen verify-fail force-escalated to failed_retryable",
			"task_id", taskID, "cap", cap, "by", caller.Username)
		return &CitizenVerifyFailResponse{
			Status: "escalated", TaskID: taskID,
			FailCount: task.VerifyFailCount, Cap: cap, Reason: failReason,
		}, nil
	}

	// The iteration being charged is the open claim's iter_seq.
	// The state gate guarantees an open claim exists; a 0 here
	// means the claim was concurrently closed (e.g. the reaper
	// just expired it) — that path will count via the reaper
	// backstop, so refuse this racing report rather than apply a
	// no-op increment and mislead the daemon with a "counted".
	iter, err := c.Store.GetOpenClaimIterSeq(taskID)
	if err != nil {
		return nil, fmt.Errorf("resolving open claim iter_seq for %q: %w", taskID, err)
	}
	if iter <= 0 {
		return nil, fmt.Errorf(
			"%w: task %q has no open claim to charge (claim closed concurrently); the reaper backstop will count this iteration",
			ErrInvalidArgument, taskID)
	}

	// Idempotent on (task_id, iter_seq): a duplicate report for the
	// same iteration, or an iteration the reaper backstop already
	// charged, is a no-op increment — the re-read below then yields
	// the unchanged authoritative count.
	if _, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.IncrementVerifyFailCount{TaskID: taskID, IterSeq: int(iter)},
		},
	}); err != nil {
		return nil, fmt.Errorf("incrementing verify-fail count: %w", err)
	}

	updated, err := c.Store.GetTask(taskID)
	if err != nil || updated == nil {
		return nil, fmt.Errorf("%w: task %q vanished after increment", ErrNotFound, taskID)
	}
	newCount := updated.VerifyFailCount

	if newCount < cap {
		c.Logger.Info("citizen verify-fail counted",
			"task_id", taskID, "count", newCount, "cap", cap, "iter_seq", iter)
		return &CitizenVerifyFailResponse{
			Status: "counted", TaskID: taskID,
			FailCount: newCount, Cap: cap,
		}, nil
	}

	// At/over the cap — park failed_retryable. performCitizenVerifyFailure
	// is idempotent, so a concurrent reaper escalation racing this
	// one is safe (the second is a no-op).
	failReason := fmt.Sprintf(
		"citizen-verify-fail: task %q failed the writes-verify gate %d consecutive iteration(s) (cap=%d); last reason: %s",
		taskID, newCount, cap, reason)
	if err := c.performCitizenVerifyFailure(taskID, failReason); err != nil {
		return nil, err
	}
	c.recordVerifyFailContribution(claimant, taskID, updated.RunID, run.ProjectID, failReason, newCount, cap)
	c.Logger.Info("citizen verify-fail escalated to failed_retryable",
		"task_id", taskID, "count", newCount, "cap", cap, "iter_seq", iter)
	return &CitizenVerifyFailResponse{
		Status: "escalated", TaskID: taskID,
		FailCount: newCount, Cap: cap, Reason: failReason,
	}, nil
}

// recordVerifyFailContribution attributes the parked attempt to the
// claimant. The claimant id MUST be captured before the park (the
// ClearClaim in the park composition NULLs claimed_by) — the prior
// attempt read it after and so never recorded the event.
func (c *Coordinator) recordVerifyFailContribution(claimant int64, taskID string, runID, projectID int64, reason string, count, cap int) {
	if claimant <= 0 {
		return
	}
	c.Store.RecordContributionEvent(&store.ContributionEvent{
		CitizenID: claimant,
		EventType: "task_failed",
		TaskID:    taskID,
		RunID:     runID,
		ProjectID: projectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"reason":    reason,
			"kind":      "citizen_verify_fail",
			"retryable": true,
			"count":     count,
			"cap":       cap,
		}),
		CreatedAt: time.Now(),
	})
}

// performCitizenVerifyFailure parks a citizen task that exhausted
// its verify-fail cap as failed_retryable. The citizen sibling of
// performComputeFailure (fail.go) — same parking state, descendants
// left PENDING (NOT cascade-SKIPPED), so enju_retry_task recovers
// the run without a fresh run — but deliberately lighter:
//
//   - NO artifact rollback: verify fails BEFORE commit/accept, so
//     there is no partial artifact/commit in the index to undo.
//   - NO counter increment: the increment already happened (the
//     caller's idempotent IncrementVerifyFailCount, or the reaper's).
//
// IDEMPOTENT: this is reachable from two independent producers (the
// client report and the reaper backstop), possibly racing. If the
// task is no longer CLAIMED/RUNNING it is already parked (or moved
// on) — return success without re-applying. MarkOpenClaimsFailed is
// itself a no-op when the looping claim was already closed by the
// reaper, so iter_seq still advances correctly on the later retry.
func (c *Coordinator) performCitizenVerifyFailure(taskID, reason string) error {
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return fmt.Errorf("run not found for task %q", taskID)
	}

	// Idempotency gate: only an in-flight attempt can be parked.
	// A task already failed_retryable (this path ran) or in any
	// other non-active state is a no-op — the racing producer won.
	st := store.TaskState(task.State)
	if st != store.TaskClaimed && st != store.TaskRunning {
		c.Logger.Info("citizen verify-fail park skipped — task no longer in-flight (already escalated or moved on)",
			"task_id", taskID, "state", st)
		return nil
	}

	plan := store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			// Close the looping iteration's claim row so a later
			// enju_retry_task re-claim advances iter_seq instead of
			// reusing the dead row (collapsing the retry into the
			// failed attempt). No-op if the reaper already closed it.
			store.MarkOpenClaimsFailed{TaskID: taskID},

			// Park failed_retryable WITH the reason. ClearClaim
			// drops the task-level claim pointer and (per the
			// applySetTaskState ClearClaim path) resets
			// verify_fail_count / verify_fail_counted_iter to 0 so
			// the eventual retry starts a fresh consecutive count.
			// Descendants are NOT touched — left PENDING, so the
			// run lands WAITING (retryable), not terminal.
			store.SetTaskState{
				TaskID:     taskID,
				NewState:   store.TaskFailedRetryable,
				FailReason: reason,
				ClearClaim: true,
			},
		},
	}.AppendCascade(task.RunID)
	plan.Mutations = append(plan.Mutations, store.EmitEvent{Event: store.Event{
		EventType:    "cascade_fired",
		EventSubtype: "citizen_verify_fail",
		TaskID:       taskID,
		RunID:        task.RunID,
		ProjectID:    run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"reason":    reason,
			"retryable": true,
		}),
		CreatedAt: time.Now(),
	}})
	if _, err := c.Store.ApplyPlan(plan); err != nil {
		return err
	}
	// failed_retryable target + descendants PENDING → the run lands
	// WAITING, not terminal; triage may pick it up.
	c.EvaluateRunStateAndMaybeTriage(task.RunID)
	return nil
}

// runTerminal reports whether a run state is terminal. Mirrors the
// store-internal runStateTerminal gate (unexported there); a
// terminal run's tasks must never be re-driven, so the reaper path
// must defer to the plain expire (whose own runStateTerminal gate
// then leaves the task as-is).
func runTerminal(s store.RunState) bool {
	switch s {
	case store.RunCompleted, store.RunFailed, store.RunTerminated:
		return true
	}
	return false
}

// plainExpireClaim issues the reaper's original CLAIMED→READY
// expiry plan, unchanged. Used for every claim the verify-fail
// gate does not own (compute task, multi-claimant, run terminal,
// already-delivered, or under-cap citizen — the daemon re-claims
// and gets another bounded attempt).
func (c *Coordinator) plainExpireClaim(taskID string, citizenID int64) error {
	_, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.ExpireClaim{TaskID: taskID, CitizenID: citizenID},
		},
	})
	return err
}

// ReapExpiredClaim is the coordinator-owned entry point the reaper
// calls for EVERY expired claim. It is the spec's D3 backstop and
// the reason the run-#3 livelock is impossible: the coordinator —
// not the client — decides, and it fires on the reaper's lease
// cadence whether or not the fat-client ever POSTs a report.
//
// The deterministic signal (no heuristic): the reaper only ever
// hands us claims that are STILL OPEN past their deadline. For a
// single-claimant citizen task still in CLAIMED/RUNNING, that means
// the agent held the task for a full lease and never delivered the
// declared writes_artifacts — a layer-① non-delivery for exactly
// this iter_seq, by construction. A delivered iteration would have
// flipped the task to SUBMITTED (and reset-on-submitted would have
// zeroed the counter); a request_changes round is
// submitted→accepted→review and never reaches this branch (D4
// separation, structurally enforced).
//
// Everything else (compute task, multi-claimant, terminal run,
// already-delivered) takes the unchanged plain CLAIMED→READY
// expiry — so this is a drop-in replacement for the reaper's old
// single mutation across all cases.
func (c *Coordinator) ReapExpiredClaim(taskID string, citizenID int64) error {
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return c.plainExpireClaim(taskID, citizenID)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return c.plainExpireClaim(taskID, citizenID)
	}

	// Gate to single-claimant citizen tasks on a non-terminal run.
	// Compute tasks keep their own fail/retry path; citizens>1
	// N-semantics are deferred (v1); a terminal run must defer to
	// the plain expire so the store's runStateTerminal gate runs.
	if task.Script != "" || task.Citizens > 1 || runTerminal(run.State) {
		return c.plainExpireClaim(taskID, citizenID)
	}
	// Defensive: the reaper only expires still-open claims, so the
	// task should be CLAIMED/RUNNING here. If it already moved on
	// (delivered, or a concurrent path resolved it) this lease
	// expiry is not a layer-① non-delivery — plain expire.
	st := store.TaskState(task.State)
	if st != store.TaskClaimed && st != store.TaskRunning {
		return c.plainExpireClaim(taskID, citizenID)
	}
	// The iteration to charge = the open claim's iter_seq, read
	// BEFORE the expiry closes the claim row.
	iter, err := c.Store.GetOpenClaimIterSeq(taskID)
	if err != nil || iter <= 0 {
		return c.plainExpireClaim(taskID, citizenID)
	}

	// Charge the non-delivery, idempotent on (task_id, iter_seq):
	// if the client already reported THIS iteration the increment
	// is a no-op and the re-read count is unchanged — the client
	// and the reaper can never double-count one iteration.
	if _, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.IncrementVerifyFailCount{TaskID: taskID, IterSeq: int(iter)},
		},
	}); err != nil {
		return err
	}
	updated, err := c.Store.GetTask(taskID)
	if err != nil || updated == nil {
		return c.plainExpireClaim(taskID, citizenID)
	}
	cap := taskVerifyFailCap(updated)

	if updated.VerifyFailCount < cap {
		// Under cap — expire so the daemon re-claims and the agent
		// gets another bounded attempt (LLM work is stochastic).
		c.Logger.Info("reaper: citizen layer-① non-delivery counted via lease expiry",
			"task_id", taskID, "count", updated.VerifyFailCount, "cap", cap, "iter_seq", iter)
		return c.plainExpireClaim(taskID, citizenID)
	}

	// At/over cap — park failed_retryable. performCitizenVerifyFailure
	// is idempotent and closes the still-open looping claim itself
	// (MarkOpenClaimsFailed), so we deliberately do NOT also
	// ExpireClaim — that would race the state flip. The full
	// composition (events + run-state eval + triage) runs here, so
	// a reaper-driven escalation is downstream-indistinguishable
	// from a client-reported one. claimant captured before the
	// park clears it.
	claimant := updated.ClaimedBy
	reason := fmt.Sprintf(
		"citizen-verify-fail: task %q lease expired with no delivery for %d consecutive iteration(s) (cap=%d); coordinator-reaper escalation — no client report required",
		taskID, updated.VerifyFailCount, cap)
	if err := c.performCitizenVerifyFailure(taskID, reason); err != nil {
		return err
	}
	c.recordVerifyFailContribution(claimant, taskID, updated.RunID, run.ProjectID, reason, updated.VerifyFailCount, cap)
	c.Logger.Info("reaper: citizen task parked failed_retryable (coordinator-independent gate)",
		"task_id", taskID, "count", updated.VerifyFailCount, "cap", cap, "iter_seq", iter)
	return nil
}
