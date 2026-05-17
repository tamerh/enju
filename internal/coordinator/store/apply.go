package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ApplyPlan is the load-bearing atomicity boundary for row
// mutations on the tasks table. Every feature path that
// changes task state — claim, submit, tally resolution,
// invalidation, park / restore / delete reconciliation,
// run creation, dynamic materialization, review verdict
// cascade — constructs a Plan of typed mutations and applies
// it here, inside a single SQLite transaction. If any
// mutation fails validation, the entire plan rolls back —
// no partial commits, no half-applied cascades.
//
// Invariant: this file is the only place that writes to the
// tasks table. If you're about to add a new `store.UpdateX`
// helper to mutate task rows per-feature, stop — add a new
// mutation type (or extend an existing one, e.g. SetTaskState)
// so it rides the plan's transaction instead. See plan.go for
// the mutation shape.
//
// Scope status (what's covered vs what isn't):
//
//   - tasks, task_claims, task_submissions, artifacts,
//     citizens, projects, project_members — fully governed by
//     ApplyPlan. The corresponding mutation types (CreateTask,
//     SetClaim, RecordSubmission, MoveArtifact, CreateCitizen,
//     CreateProject, AddProjectMember, etc.) are the only
//     supported write path.
//
//   - runs.state — fully governed. PauseRun / ResumeRun /
//     CompleteRun all flow through ApplyPlan (Phase 4c.5);
//     run_paused / run_resumed / run_active / run_waiting /
//     run_completed events ride the EventSink and post-commit
//     drain like every other event.
//
//   - issues, cycle budget, spawned tasks — fully governed via
//     CreateIssue / TriageIssue / MarkIssueInProgress /
//     CloseIssue / SpawnTask / SetCycleBudgetMax mutations
//     (Phase 4c.6 + 4c.7).
//
//   - citizens.role, tokens — fully governed via SetCitizenRole /
//     UpdateCitizenProfile / IssueToken / RevokeToken /
//     RevokeTokenByValue mutations (Phase 4c.8). The auto-issued
//     token's label rides on CreateCitizen.TokenLabel so bot
//     registration is one atomic plan. (citizens.last_seen is
//     written once at registration only; the column is reserved
//     for a future presence indicator and is no longer touched
//     per-request — the heartbeat exception is gone.)
//
//   - runs.auto_triage_template — fully governed via
//     SetAutoTriageTemplate (Phase 4d).
//
// Phase 4d additionally narrowed coordinator-side packages
// (service, api, scheduler, dagcache) to a CoordinatorStore
// interface that exposes ApplyPlan + reads only. Direct
// mutation methods still exist on *Store for in-package use
// by apply handlers, but external callers cannot reach them
// — the chokepoint is now compile-time enforced.
//
// Tx discipline for applyXxx functions: every read and write
// inside an applyXxx body MUST go through the `tx` parameter,
// never `s.db` or a method on `s` that wraps `s.db`. SQLite
// holds the write lock on the tx's connection until commit; a
// sibling call that grabs a different pool connection (a) can't
// see the in-tx writes (it sees pre-commit state) and (b) busy-
// waits or deadlocks on its own write. The applyUpdateReadyTasks
// + s.UpdateReadyTasks pairing was a real instance of this trap:
// the deadline-driven vote/review resolve path silently lost
// downstream readiness propagation until the cascade became
// tx-aware (see updateReadyTasksOn in sqlite.go).
//
// If you need shared logic between an applyXxx and a standalone
// post-commit method, parameterize it over the dbExecQueryer
// interface like updateReadyTasksOn — the apply path passes tx,
// the standalone path passes s.db.
//
// ApplyPlan validates and applies a Plan's mutations inside
// a single transaction. If any mutation fails validation,
// the entire plan is rolled back — no partial commits.
//
// This is the coordinator's ONE write path. Every feature
// (claim, submit, invalidate, tally, run creation, dynamic
// materialization) goes through the same function. The
// engine produces the plan; ApplyPlan is the executor.
//
// Retries on SQLITE_BUSY-class errors with exponential
// backoff. The DSN config (busy_timeout=5000, _txlock=
// immediate) handles the common case — busy_timeout retries
// writer-vs-writer contention and IMMEDIATE prevents
// snapshot conflicts. This wrapper is defense-in-depth: if
// either of those breaks (driver upgrade, DSN regression,
// future code path that opens a separate sql.DB) or if
// busy_timeout overflows under genuinely heavy load, the
// caller sees a successful retry instead of a failed plan.
func (s *Store) ApplyPlan(plan Plan) (ApplyResult, error) {
	return applyWithRetry(applyPlanMaxAttempts, func() (ApplyResult, error) {
		return s.applyPlanOnce(plan)
	})
}

// applyPlanMaxAttempts is the retry budget for ApplyPlan. Exposed as
// a constant so retry tests can reference it without hardcoding.
const applyPlanMaxAttempts = 5

// applyWithRetry invokes fn up to maxAttempts times, retrying only on
// SQLITE_BUSY-class errors with exponential backoff (5ms, 10ms, 20ms,
// 40ms, 80ms — max ~155ms total wait across all retries, with up to
// 50% jitter). Backoff is short because busy_timeout=5s is already
// the long-tail handler — this loop is for the rare case where 5s of
// busy waiting wasn't enough.
//
// On success returns the ApplyResult immediately. On a non-busy error
// returns the result fn produced (which may be a partial; preserving
// it matches pre-refactor semantics for callers that inspect it). On
// retry exhaustion returns a zero ApplyResult and a wrapped error.
//
// Extracted from ApplyPlan as a standalone func so the retry contract
// (count, backoff, busy-vs-non-busy distinction) is unit-testable
// without standing up a real database. See sqlite_concurrency_test.go.
func applyWithRetry(maxAttempts int, fn func() (ApplyResult, error)) (ApplyResult, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		// Only retry SQLITE_BUSY / "database is locked" —
		// every other error is either a validation failure
		// (no point retrying) or a structural problem the
		// caller needs to see.
		if !isSQLiteBusy(err) {
			return result, err
		}
		lastErr = err
		sleepBusyBackoff(attempt)
	}
	return ApplyResult{}, fmt.Errorf("ApplyPlan failed after %d retries on SQLITE_BUSY: %w", maxAttempts, lastErr)
}

// sleepBusyBackoff is the per-attempt sleep between retries. Package
// var (not const) so tests can swap in a no-op to keep the retry tests
// fast without adding fake-clock plumbing.
var sleepBusyBackoff = func(attempt int) {
	backoff := time.Duration(5<<attempt) * time.Millisecond
	jitter := time.Duration(rand.Intn(int(backoff / 2)))
	time.Sleep(backoff + jitter)
}

// isSQLiteBusy reports whether err is a SQLITE_BUSY-class
// error worth retrying. String-match because the modernc
// driver wraps the underlying error and the typed sqlite.Error
// surface isn't part of database/sql. The two patterns it
// produces are "database is locked" (default text) and
// "SQLITE_BUSY" (when the error code is rendered) — covering
// both keeps us robust to driver-version wording shifts.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}

// EventSink is the contract every applyXxx handler honors for
// event emission. A handler must call either Emit (one or more
// times) or SkipEvents (exactly once) before returning. The
// dispatcher enforces this with a runtime check; a handler that
// returns without doing either panics.
//
// Why the contract: the bug class this prevents is "I added a
// new mutation handler and silently forgot to think about
// events." Pre-contract, the events plumbing was a *[]Event
// pointer that handlers could ignore — so they did, leaving
// task_ready / claim_released / claim_timed_out / etc.
// quietly missing for entire production paths. SkipEvents
// forces the author to write down WHY there's nothing to emit,
// which makes the audit obvious to the next reader and to grep.
//
// Use Emit when the mutation is citizen-observable (a state
// transition someone outside the coordinator should learn
// about). Use SkipEvents when the mutation is pure bookkeeping
// (e.g. a derived-column refresh, an artifact-index move whose
// visibility is covered by a downstream task_ready). The reason
// string is documentation, not telemetry — pick something a
// reviewer can match against the handler's behavior.
//
// Scope: the contract covers mutations routed through ApplyPlan.
// A handful of legacy direct-write methods on Store still exist
// outside this path (project create/membership/default-branch,
// citizen role/heartbeat updates, run pause/resume) and emit
// events from their service-layer callers when they emit at all.
// Migrating them to ApplyPlan-based mutations would extend the
// contract to cover them; see the diff-walk invariant test
// (Phase 4d) for the runtime check that catches gaps regardless
// of routing.
type EventSink interface {
	Emit(Event)
	SkipEvents(reason string)
}

// trackingSink is the dispatcher-internal EventSink. One per
// mutation; the dispatcher inspects `handled` after the handler
// returns to enforce the "must declare intent" rule, then drains
// `events` into the plan-wide pending list.
type trackingSink struct {
	events  []Event
	handled bool
}

func newTrackingSink() *trackingSink { return &trackingSink{} }

func (s *trackingSink) Emit(e Event) {
	s.events = append(s.events, e)
	s.handled = true
}

// SkipEvents declares that this mutation has no citizen-
// observable effect. The reason is discarded at runtime but
// reads as documentation at the call site — keep it specific
// so a reviewer can verify it against the handler body.
func (s *trackingSink) SkipEvents(_ string) {
	s.handled = true
}

// applyPlanOnce is the body of ApplyPlan, extracted so the
// retry wrapper can call it multiple times. Same single-
// transaction semantics: any mutation failure rolls back the
// whole plan.
func (s *Store) applyPlanOnce(plan Plan) (ApplyResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	result := ApplyResult{}
	// events are no longer written inside the apply
	// tx — they're collected here and emitted via the EventStore
	// after commit. A roll-back due to a later mutation failure
	// cleanly drops the collected events with the rest of the
	// would-be state.
	var pendingEvents []Event

	for _, mut := range plan.Mutations {
		sink := newTrackingSink()
		switch m := mut.(type) {

		case SetTaskState:
			if err := applySetTaskState(tx, m, &result, sink); err != nil {
				return result, err
			}

		case CreateTask:
			if err := applyCreateTask(tx, m, sink); err != nil {
				return result, err
			}
			result.TasksCreated++

		case DeleteTask:
			if err := applyDeleteTask(tx, m, sink); err != nil {
				return result, err
			}
			result.TasksDeleted++

		case CreateRun:
			id, seq, err := applyCreateRun(tx, m, sink)
			if err != nil {
				return result, err
			}
			result.RunID = id
			result.RunSeq = seq

		case SetClaim:
			if err := applySetClaim(tx, m, sink); err != nil {
				return result, err
			}

		case ReleaseClaim:
			if err := applyReleaseClaim(tx, m, sink); err != nil {
				return result, err
			}

		case ExpireClaim:
			if err := applyExpireClaim(tx, m, sink); err != nil {
				return result, err
			}

		case SetClaimDeadline:
			if err := applySetClaimDeadline(tx, m, sink); err != nil {
				return result, err
			}

		case RecordSubmission:
			if err := applyRecordSubmission(tx, m, sink); err != nil {
				return result, err
			}

		case MoveArtifact:
			if err := applyMoveArtifact(tx, m, sink); err != nil {
				return result, err
			}

		case DeleteArtifact:
			if err := applyDeleteArtifact(tx, m, sink); err != nil {
				return result, err
			}

		case CreateCitizen:
			id, err := applyCreateCitizen(tx, m, sink)
			if err != nil {
				return result, err
			}
			result.CitizenID = id

		case UpdateReadyTasks:
			readied, err := applyUpdateReadyTasks(tx, s, m, sink)
			if err != nil {
				return result, err
			}
			result.TasksReadied += len(readied)
			result.ReadiedTasks = append(result.ReadiedTasks, readied...)

		case CompleteRun:
			completed, err := applyCompleteRun(tx, m, sink)
			if err != nil {
				return result, err
			}
			result.RunCompleted = completed

		case EmitEvent:
			applyEmitEvent(m, sink)

		case CreateProject:
			id, err := applyCreateProject(tx, m, sink)
			if err != nil {
				return result, err
			}
			result.ProjectID = id

		case SetProjectDefaultBranch:
			if err := applySetProjectDefaultBranch(tx, m, sink); err != nil {
				return result, err
			}

		case SetProjectRemoteURL:
			if err := applySetProjectRemoteURL(tx, m, sink); err != nil {
				return result, err
			}

		case AddProjectMember:
			if err := applyAddProjectMember(tx, m, sink); err != nil {
				return result, err
			}

		case RemoveProjectMember:
			if err := applyRemoveProjectMember(tx, m, sink); err != nil {
				return result, err
			}

		case SetProjectMemberRole:
			if err := applySetProjectMemberRole(tx, m, sink); err != nil {
				return result, err
			}

		case MarkOpenClaimsInvalidated:
			if err := applyMarkOpenClaimsInvalidated(tx, m, sink); err != nil {
				return result, err
			}

		case MarkOpenClaimsFailed:
			if err := applyMarkOpenClaimsFailed(tx, m, sink); err != nil {
				return result, err
			}

		case MarkLatestClaimOutcome:
			if err := applyMarkLatestClaimOutcome(tx, m, sink); err != nil {
				return result, err
			}

		case PauseRun:
			if err := applyPauseRun(tx, m, sink); err != nil {
				return result, err
			}

		case ResumeRun:
			if err := applyResumeRun(tx, m, sink); err != nil {
				return result, err
			}

		case TerminateRun:
			if err := applyTerminateRun(tx, m, &result, sink); err != nil {
				return result, err
			}

		case CreateIssue:
			id, seq, err := applyCreateIssue(tx, m, sink)
			if err != nil {
				return result, err
			}
			result.IssueID = id
			result.IssueSeq = seq

		case TriageIssue:
			if err := applyTriageIssue(tx, m, sink); err != nil {
				return result, err
			}

		case MarkIssueInProgress:
			if err := applyMarkIssueInProgress(tx, m, sink); err != nil {
				return result, err
			}

		case CloseIssue:
			if err := applyCloseIssue(tx, m, sink); err != nil {
				return result, err
			}

		case SpawnTask:
			taskID, exhausted, err := applySpawnTask(tx, m, sink)
			if err != nil {
				return result, err
			}
			result.SpawnedTaskID = taskID
			result.BudgetExhausted = exhausted

		case SetCycleBudgetMax:
			if err := applySetCycleBudgetMax(tx, m, sink); err != nil {
				return result, err
			}

		case SetCitizenRole:
			if err := applySetCitizenRole(tx, m, sink); err != nil {
				return result, err
			}

		case UpdateCitizenProfile:
			if err := applyUpdateCitizenProfile(tx, m, sink); err != nil {
				return result, err
			}

		case IssueToken:
			id, err := applyIssueToken(tx, m, sink)
			if err != nil {
				return result, err
			}
			result.TokenID = id

		case RevokeToken:
			if err := applyRevokeToken(tx, m, sink); err != nil {
				return result, err
			}

		case RevokeTokenByValue:
			if err := applyRevokeTokenByValue(tx, m, sink); err != nil {
				return result, err
			}

		case SetAutoTriageTemplate:
			if err := applySetAutoTriageTemplate(tx, m, sink); err != nil {
				return result, err
			}

		default:
			return result, fmt.Errorf("unknown mutation type: %T", mut)
		}
		if !sink.handled {
			panic(fmt.Sprintf("apply handler for %T returned without calling sink.Emit or sink.SkipEvents — every mutation must declare event intent (see EventSink doc)", mut))
		}
		pendingEvents = append(pendingEvents, sink.events...)
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit: %w", err)
	}
	// Drain collected events to the EventStore. Best-effort:
	// individual Record() calls never block or error.
	for _, e := range pendingEvents {
		s.Events().Record(e)
	}
	return result, nil
}

// ApplyResult carries summary data back to the caller
// after a successful plan application.
type ApplyResult struct {
	ProjectID     int64
	RunID         int64
	RunSeq        int
	CitizenID     int64
	TokenID       int64
	IssueID       int64
	IssueSeq      int
	SpawnedTaskID string
	// BudgetExhausted is true when an applySpawnTask handler
	// found the run's cycle budget exhausted. The handler still
	// commits the pause + emits cycle_budget_exhausted (the
	// legacy split between "we paused you" and "we're telling
	// you it failed" stays intact); the service caller checks
	// this flag and converts to a typed user-facing error.
	BudgetExhausted bool
	TasksCreated    int
	TasksDeleted    int
	TasksReadied    int
	// ReadiedTasks is the full per-task detail from the
	// readiness cascade (one entry per task that transitioned
	// PENDING → READY in this Plan). Populated when the Plan
	// includes an UpdateReadyTasks mutation. Callers that only
	// need the count read TasksReadied; callers that need
	// per-task data (assign_to, action, parents) read this.
	ReadiedTasks []ReadiedTask
	RunCompleted bool
	// SkippedTasks and AbandonedClaims are populated by
	// applyTerminateRun. The service layer reads these directly
	// from the result so it can render them in the response
	// without re-querying the event log (which would race with
	// the async event writer and ambiguously report 0,0 if the
	// metadata isn't yet visible).
	SkippedTasks    int
	AbandonedClaims int
	Changed         int // generic "rows affected" counter
}

// --- Per-mutation apply functions ---

func applySetTaskState(tx *sql.Tx, m SetTaskState, result *ApplyResult, sink EventSink) error {
	// Validate: task exists and check current state.
	var currentState string
	if err := tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, m.TaskID).Scan(&currentState); err != nil {
		return fmt.Errorf("set_task_state: task %q not found", m.TaskID)
	}

	if m.ClearClaim {
		// Invalidation-style transition. Validate preconditions:
		// - Target (→READY): must be ACCEPTED, SUBMITTED,
		//   FAILED, or FAILED_RETRYABLE. SUBMITTED added Phase
		//   8.3 for the request_changes path: a single-citizen
		//   reviewed task is in SUBMITTED at the moment its
		//   reviewer submits a request_changes verdict (the
		//   upstream went through SUBMITTED instead of directly
		//   to ACCEPTED), and PerformInvalidate must accept
		//   that row to reset it for revision. Pre-Phase-8.3
		//   ACCEPTED was the only "submission landed" terminal
		//   so the precondition was tighter. FAILED_RETRYABLE
		//   is the enju_retry_task transition: a compute task
		//   that errored on its own merits is sent back to
		//   READY for a fresh attempt (the failed iteration was
		//   already closed by MarkOpenClaimsFailed at fail
		//   time, so this is a clean re-open, not a verdict).
		// - Descendant (→PENDING): skip if already PENDING
		//   (no-op, not an error).
		// - Descendant (→SKIPPED via fail-cascade): same clear
		//   semantics as PENDING, but terminal.
		if m.NewState == TaskReady &&
			TaskState(currentState) != TaskAccepted &&
			TaskState(currentState) != TaskSubmitted &&
			TaskState(currentState) != TaskFailed &&
			TaskState(currentState) != TaskFailedRetryable {
			return fmt.Errorf("task %q cannot be invalidated (state: %s, must be accepted, submitted, failed, or failed_retryable)", m.TaskID, currentState)
		}
		if m.NewState == TaskPending && TaskState(currentState) == TaskPending {
			// Already pending — skip silently, matching the
			// old InvalidateTask behavior.
			sink.SkipEvents("set_task_state clear-claim no-op: task already in pending")
			return nil
		}
		// Fail-cascade skip carries a reason; everything else
		// the ClearClaim path handles wipes all per-claim state.
		//
		// fail_reason is bound to m.FailReason rather than
		// hardcoded ''. Re-ready callers (request_changes,
		// unfail, cascade-pending) leave FailReason zero, so they
		// still clear it — identical to the old behavior. But
		// performComputeFailure parks a task in failed_retryable
		// via this same ClearClaim path (it must drop the claim
		// pointer) WITH a reason; hardcoding '' silently threw
		// that reason away, so every failed_retryable task showed
		// an empty fail_reason and the operator genuinely flew
		// blind. Preserving it here is the load-bearing half of
		// "don't fly blind".
		q := `UPDATE tasks SET state = ?, claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL, commit_sha = '', review_decision = '', vote_choice = '', fail_reason = ?, skip_reason = ?`
		args := []interface{}{m.NewState, m.FailReason, m.SkipReason}
		// depends_on rewrite (singleton-reopen case): the
		// caller supplies a new edge set when a reconciled
		// instance set changes which parents this task should
		// wait on. Must ride the same UPDATE as the state
		// flip — same atomicity argument as the non-clear
		// branch below.
		if m.NewDependsOn != nil {
			q += `, depends_on = ?`
			args = append(args, strings.Join(*m.NewDependsOn, ","))
		}
		q += ` WHERE id = ?`
		args = append(args, m.TaskID)
		if _, err := tx.Exec(q, args...); err != nil {
			return fmt.Errorf("set_task_state (clear): %w", err)
		}
		// Rebound-ready emission: when this clear-claim flips a
		// task ACCEPTED→READY (request_changes cascade) or
		// FAILED→READY (manual unfail), the human assignee needs
		// to know the work is back on their plate. Skipped for
		// non-READY transitions (PENDING / SKIPPED descendants);
		// the cascade emission covers the eventual re-promote.
		if m.NewState == TaskReady {
			var action, assignToJSON, dependsOn string
			var runID int64
			if err := tx.QueryRow(
				`SELECT action, COALESCE(assign_to, ''), COALESCE(depends_on, ''), run_id FROM tasks WHERE id = ?`, m.TaskID,
			).Scan(&action, &assignToJSON, &dependsOn, &runID); err != nil {
				return fmt.Errorf("set_task_state (clear) emit lookup: %w", err)
			}
			if err := emitBirthReadyEvent(tx, m.TaskID, action, assignToJSON, dependsOn, runID, sink); err != nil {
				return err
			}
		} else {
			// Clear-claim transitions to PENDING/SKIPPED/etc. are
			// cascade descendants. The cascade origin (caller of
			// the Plan) emits cascade_fired; per-descendant events
			// would just be noise. Phase 4b may add task_invalidated
			// here if we decide descendants warrant their own signal.
			sink.SkipEvents("set_task_state clear-claim non-ready: cascade descendant, origin emits cascade_fired")
		}
		// Phase 6c — open claims are NOT implicitly marked
		// here. The cascade caller decides:
		//  - Request_changes target: claim stays open
		//   (revision-within-iteration semantics).
		//  - Manual invalidate / reject target: caller
		//   explicitly calls MarkLatestClaimOutcome with
		//   the right terminal outcome.
		//  - Cascade descendants: caller marks their open
		//   claims as 'invalidated' (their work is
		//   collateral — wasn't a verdict against them, but
		//   their iteration is gone).
		// Pre-6c this code unconditionally wrote 'invalidated'
		// here, which broke request_changes by closing the
		// target's claim and forcing iter-N to bump.
	} else {
		q := `UPDATE tasks SET state = ?`
		args := []interface{}{m.NewState}
		if m.VoteChoice != "" {
			q += `, vote_choice = ?`
			args = append(args, m.VoteChoice)
		}
		if m.CommitSHA != "" {
			q += `, commit_sha = ?`
			args = append(args, m.CommitSHA)
		}
		if m.FailReason != "" {
			q += `, fail_reason = ?`
			args = append(args, m.FailReason)
		}
		if m.SkipReason != "" {
			q += `, skip_reason = ?`
			args = append(args, m.SkipReason)
		}
		// Parking/restore semantics on parked_from_state:
		//  - Transition TO parked: caller sets
		//   ParkedFromState to the prior state; we stash
		//   it so restore is a lossless revert.
		//  - Transition FROM parked (restore): caller sets
		//   NewState to the stashed value and leaves
		//   ParkedFromState zero. We must explicitly clear
		//   the column — otherwise the row would look
		//   "previously parked" forever.
		//  - Other transitions: column is left alone. (A row
		//   that was never parked has empty parked_from_state;
		//   nothing to change.)
		if m.NewState == TaskParked {
			q += `, parked_from_state = ?`
			args = append(args, m.ParkedFromState)
		} else if TaskState(currentState) == TaskParked {
			// Restoring from parked — clear the stash so a
			// later park doesn't see stale residue.
			q += `, parked_from_state = ''`
		}
		// depends_on rewrite, when the caller asked for one.
		// Must ride the same UPDATE so the state flip and the
		// edge-set change are atomic — otherwise a crash
		// between them leaves a task in the new state with
		// stale edges, which the scheduler reads as "ready
		// when deps are satisfied" against the wrong deps.
		if m.NewDependsOn != nil {
			q += `, depends_on = ?`
			args = append(args, strings.Join(*m.NewDependsOn, ","))
		}
		q += ` WHERE id = ?`
		args = append(args, m.TaskID)
		if _, err := tx.Exec(q, args...); err != nil {
			return fmt.Errorf("set_task_state: %w", err)
		}
		// task_completed fires on the terminal-good
		// transition. Skip when current already == accepted to
		// avoid double-emitting on re-issued ACCEPTED writes.
		// Single-citizen unreviewed paths emit task_completed
		// from applyRecordSubmission and never come through here
		// (they UPDATE state inline). The paths that DO route
		// through this branch are: vote-tally resolve, review-
		// tally resolve, and any future "operator marks task
		// accepted" admin tool. They each set NewState=accepted
		// here.
		if m.NewState == TaskRunning && TaskState(currentState) == TaskClaimed {
			// task_started fires on CLAIMED → RUNNING. Emitted
			// from the coord side when the fat-client posts
			// /tasks/:id/started right before exec.Run / LLM call,
			// providing a "claimed but stuck" vs "actually running"
			// diagnostic signal. Companion to task_completed:
			// together they bracket the work-execution window.
			var runID, projectID int64
			var taskAction string
			var citizens int
			_ = tx.QueryRow(
				`SELECT t.run_id, r.project_id, t.action, t.citizens
				 FROM tasks t JOIN runs r ON t.run_id = r.id WHERE t.id = ?`,
				m.TaskID,
			).Scan(&runID, &projectID, &taskAction, &citizens)
			sink.Emit(Event{
				EventType:    "task_started",
				EventSubtype: taskAction,
				TaskID:       m.TaskID,
				RunID:        runID,
				ProjectID:    projectID,
				Metadata: MarshalMetadata(map[string]any{
					"citizens":    citizens,
					"prior_state": currentState,
				}),
				CreatedAt: time.Now(),
			})
		} else if m.NewState == TaskAccepted && TaskState(currentState) != TaskAccepted {
			var runID, projectID int64
			var taskAction, taskDefID, instanceKey string
			var citizens int
			var claimedBy sql.NullInt64
			_ = tx.QueryRow(
				`SELECT t.run_id, r.project_id, t.action, t.task_def_id, t.instance_key, t.citizens, t.claimed_by
				 FROM tasks t JOIN runs r ON t.run_id = r.id WHERE t.id = ?`,
				m.TaskID,
			).Scan(&runID, &projectID, &taskAction, &taskDefID, &instanceKey, &citizens, &claimedBy)
			commit := m.CommitSHA
			if commit == "" {
				_ = tx.QueryRow(`SELECT COALESCE(commit_sha, '') FROM tasks WHERE id = ?`, m.TaskID).Scan(&commit)
			}
			// "reviewed" flag: was this task subject to a
			// downstream review? Mirrors the stayOpen check in
			// applyRecordSubmission so the event metadata stays
			// consistent across the SUBMITTED → ACCEPTED moment
			// (single-citizen-unreviewed AND tally / review-
			// approve paths now both fire from this branch).
			reviewed := false
			if taskAction != "review" {
				target := BuildReviewsTargetKey(taskDefID, instanceKey)
				var reviewerID sql.NullString
				_ = tx.QueryRow(
					`SELECT id FROM tasks WHERE run_id = ? AND action = 'review' AND reviews_target = ? LIMIT 1`,
					runID, target,
				).Scan(&reviewerID)
				reviewed = reviewerID.Valid
			}
			// Phase 8.3 — stamp citizen attribution on the
			// terminal-good event. Pre-Phase-8.3 task_completed
			// fired from applyRecordSubmission with the
			// submitter's m.CitizenID; the locus moved to here
			// when the SUBMITTED → ACCEPTED transition was
			// split out from RecordSubmission, but the attribution
			// counter (CountByCitizenAndType in contributions.go)
			// keys on event.citizen_id, not on metadata. Sourcing
			// from tasks.claimed_by is correct for single-citizen
			// (the only claimant submitted) and approximates for
			// multi-citizen (the most-recent claim's owner —
			// matches pre-Phase-8.3 attribution where the row
			// reflected the tally-winning submission).
			ev := Event{
				EventType:    "task_completed",
				EventSubtype: taskAction,
				TaskID:       m.TaskID,
				RunID:        runID,
				ProjectID:    projectID,
				Metadata: MarshalMetadata(map[string]any{
					"commit_sha":  commit,
					"citizens":    citizens,
					"prior_state": currentState,
					"reviewed":    reviewed,
				}),
				CreatedAt: time.Now(),
			}
			if claimedBy.Valid {
				ev.CitizenID = claimedBy.Int64
			}
			sink.Emit(ev)
		} else {
			// Non-clear path covers many transitions: COLLECTING,
			// FAILED tally, parking, restore, depends_on rewrite.
			// Per-state-change events for these are intentionally
			// not emitted today — most are intermediate steps
			// that pair with a downstream cascade. Phase 4b may
			// add task_failed or task_collecting if we find a
			// notification rule that needs them.
			sink.SkipEvents("set_task_state non-clear non-accepted: intermediate transition, no per-step event today")
		}
	}
	result.Changed++
	return nil
}

func applyCreateTask(tx *sql.Tx, m CreateTask, sink EventSink) error {
	t := &m.Task
	citizens := t.Citizens
	if citizens == 0 {
		citizens = 1
	}
	anonymize := 0
	if t.Anonymize {
		anonymize = 1
	}
	_, err := tx.Exec(
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, reviews_target, vote_options, citizens, min_quorum, vote_threshold, vote_deadline, anonymize, visibility, env, mode, run_slug, on_review_reject, on_review_request_changes, remediation_template, closes_issue_seq, container, container_runtime, volumes, executor, resources, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RunID, t.Seq, t.TaskDefID, t.InstanceKey, t.InstanceParams, t.Ref, t.Action,
		t.Prompt, t.UserPrompt, t.Script, t.Outputs, t.Requirements, t.ResultType, t.Timeout,
		t.State, t.DependsOn, t.ReadsArtifacts, t.WritesArtifacts,
		t.AssignTo, t.RequireRole, t.ReviewsTarget,
		t.VoteOptions, citizens, t.MinQuorum, t.VoteThreshold, t.VoteDeadline,
		anonymize, t.Visibility, t.Env, t.Mode, t.RunSlug,
		t.OnReviewReject, t.OnReviewRequestChanges, t.RemediationTemplate,
		t.ClosesIssueSeq, t.Container, t.ContainerRuntime, t.Volumes, t.Executor, t.Resources,
		t.CreatedAt,
	)
	if err != nil {
		return err
	}
	// Birth-ready emission: tasks materialized straight into
	// READY state (root tasks at run-create time, dynamic
	// for_each instances with no upstream deps, spawned tasks
	// with no `depends_on`) need a task_ready event so the
	// assigned_task_ready notification rule fires for the human.
	// Without this, a fresh run lands silently on the assignee's
	// plate. Production was missing ~1/3 of "ready transitions"
	// before this fix.
	if TaskState(t.State) == TaskReady {
		if err := emitBirthReadyEvent(tx, t.ID, t.Action, t.AssignTo, t.DependsOn, t.RunID, sink); err != nil {
			return err
		}
	} else {
		// Non-ready creation (PENDING for tasks awaiting deps,
		// or skipped/parked seed states for materialization).
		// The downstream cascade emits task_ready when this task
		// later promotes; no per-create event is needed.
		sink.SkipEvents("create_task non-ready: cascade emits task_ready when deps satisfied")
	}
	return nil
}

// emitBirthReadyEvent emits task_ready events for one task that
// landed in READY without going through the cascade. Covers two
// paths: born-ready (applyCreateTask) and rebound-ready
// (applySetTaskState clear-claim → READY). The cascade itself
// has its own emit (applyUpdateReadyTasks).
//
// Looks up project_id via the run row so callers don't pre-fetch.
// Reuses buildTaskReadyEvents so the wire shape stays identical
// across all three paths (cascade, birth, rebound).
func emitBirthReadyEvent(tx *sql.Tx, taskID, action, assignToJSON, dependsOn string, runID int64, sink EventSink) error {
	var projectID int64
	if err := tx.QueryRow(`SELECT project_id FROM runs WHERE id = ?`, runID).Scan(&projectID); err != nil {
		return fmt.Errorf("task_ready emit: loading project for run %d: %w", runID, err)
	}
	var assignees []string
	if assignToJSON != "" {
		_ = json.Unmarshal([]byte(assignToJSON), &assignees)
	}
	parents, err := lookupReadiedParents(tx, dependsOn)
	if err != nil {
		return fmt.Errorf("task_ready emit: parent lookup for %s: %w", taskID, err)
	}
	for _, ev := range buildTaskReadyEvents([]ReadiedTask{{
		TaskID:    taskID,
		Action:    action,
		Assignees: assignees,
		RunID:     runID,
		ProjectID: projectID,
		Parents:   parents,
	}}, time.Now()) {
		sink.Emit(ev)
	}
	return nil
}

func applyDeleteTask(tx *sql.Tx, m DeleteTask, sink EventSink) error {
	// Capture run/project + action BEFORE the row is gone, so
	// the event carries the same shape downstream consumers
	// expect (run-scoped routing, action-typed subtype).
	var action string
	var runID, projectID int64
	_ = tx.QueryRow(
		`SELECT t.action, t.run_id, r.project_id
		 FROM tasks t JOIN runs r ON t.run_id = r.id WHERE t.id = ?`,
		m.TaskID,
	).Scan(&action, &runID, &projectID)

	tx.Exec(`DELETE FROM task_claims WHERE task_id = ?`, m.TaskID)
	_, err := tx.Exec(`DELETE FROM tasks WHERE id = ?`, m.TaskID)
	if err != nil {
		return err
	}
	if runID > 0 {
		// task_dematerialized fires when a previously-existing
		// task row is removed by a cascade — partial-remat
		// reconciliation (instance set shrank) or fail-cascade
		// pruning. The cascade origin emits cascade_fired; this
		// per-row event lets diff-walk consumers reconcile the
		// state.db delete with a corresponding event.
		sink.Emit(Event{
			EventType:    "task_dematerialized",
			EventSubtype: action,
			TaskID:       m.TaskID,
			RunID:        runID,
			ProjectID:    projectID,
			Metadata:     MarshalMetadata(map[string]any{}),
			CreatedAt:    time.Now(),
		})
	} else {
		// DELETE against a non-existent row — likely a
		// double-delete in a misordered cascade. UPDATE rows-
		// affected was zero; nothing to announce.
		sink.SkipEvents("delete_task no-op: task row not found at lookup time")
	}
	return nil
}

func applyCreateRun(tx *sql.Tx, m CreateRun, sink EventSink) (int64, int, error) {
	r := &m.Run
	// Compute next seq.
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM runs WHERE project_id = ?`, r.ProjectID).Scan(&maxSeq); err != nil {
		return 0, 0, err
	}
	nextSeq := int(maxSeq.Int64) + 1
	branch := r.Branch
	if branch == "" {
		branch = "main"
	}
	slug := r.Slug
	if slug == "" {
		slug = "run"
	}
	result, err := tx.Exec(
		`INSERT INTO runs (project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, sync_mode_override, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ProjectID, nextSeq, r.Name, r.Ref, r.YAMLData, r.RepoURL, r.State, r.SourcePath, r.SourceCommitSHA, r.Params, branch, slug, r.SyncModeOverride, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return 0, 0, err
	}
	id, _ := result.LastInsertId()
	// run_created is emitted by the service layer
	// (service/create_run.go) once the run + initial task plan
	// have all committed. Emitting from here would fire before
	// the tasks land, which would make the event misleading
	// for any subscriber that reads run state on receipt.
	sink.SkipEvents("run_created emitted by service/create_run.go after the full run+tasks plan commits")
	return id, nextSeq, nil
}

func applySetClaim(tx *sql.Tx, m SetClaim, sink EventSink) error {
	// Read citizens count to decide single vs multi behavior;
	// pull task_def_id + run_slug for the iteration branch
	// name (living-workflow phase 6a).
	var citizens int
	var runSeq int
	var taskDefID, runSlug, taskAction, instanceKey string
	var taskRunID, projectID int64
	if err := tx.QueryRow(
		`SELECT t.citizens, t.task_def_id, COALESCE(t.run_slug, ''), t.action, COALESCE(t.instance_key, ''), r.seq, t.run_id, r.project_id
		 FROM tasks t JOIN runs r ON t.run_id = r.id WHERE t.id = ?`,
		m.TaskID,
	).Scan(&citizens, &taskDefID, &runSlug, &taskAction, &instanceKey, &runSeq, &taskRunID, &projectID); err != nil {
		return fmt.Errorf("set_claim: task %q not found", m.TaskID)
	}
	if citizens <= 0 {
		citizens = 1
	}

	// Enforce "an agent must name a model" at apply time (SQLite
	// CHECK can't conditionally read citizens.kind). Humans and
	// scripts may act with no model.
	if err := requireModelForAgent(tx, m.CitizenID, m.Model, "set_claim"); err != nil {
		return err
	}

	// Phase 6c — iter_seq computation + reuse-on-reopen.
	// Single-citizen reuse: if there's an open claim row for
	// this task by THIS citizen, reuse it (don't INSERT a new
	// one). This is the "request_changes leaves the claim
	// open for revision" semantics — the same iteration row
	// stays around through multiple submission attempts. The
	// task state still flips to 'claimed' below.
	//
	// Different-citizen takeover: if there's an open claim by
	// a DIFFERENT citizen, mark it 'abandoned' (terminal,
	// distinct from 'released' which means timeout/voluntary).
	// iter_seq then bumps for this fresh claim. Only fires
	// for single-citizen tasks; multi-citizen tasks have N
	// open claims by design and citizens don't take each
	// other over.
	now := time.Now()
	if citizens == 1 {
		var openID int64
		var openCitizen int64
		err := tx.QueryRow(
			`SELECT id, citizen_id FROM task_claims WHERE task_id = ? AND outcome IS NULL ORDER BY claimed_at DESC LIMIT 1`,
			m.TaskID,
		).Scan(&openID, &openCitizen)
		if err == nil {
			if openCitizen == m.CitizenID {
				// Same citizen reclaiming after a
				// request_changes round. Reuse — don't
				// INSERT a new claim row, just refresh the
				// deadline and mark the task claimed.
				if _, err := tx.Exec(
					`UPDATE task_claims SET claimed_at = ?, deadline = ? WHERE id = ?`,
					now, m.Deadline, openID,
				); err != nil {
					return fmt.Errorf("set_claim reuse: %w", err)
				}
				if _, err := tx.Exec(
					`UPDATE tasks SET state = 'claimed', claimed_by = ?, claimed_at = ? WHERE id = ?`,
					m.CitizenID, now, m.TaskID,
				); err != nil {
					return err
				}
				// Same-citizen reclaim is the request_changes
				// reopen path. The task_request_changes event
				// fires from the cascade that reopened the
				// claim; this re-claim itself doesn't add new
				// signal beyond that.
				sink.SkipEvents("set_claim same-citizen reuse: request_changes reopen, the request_changes event already fired upstream")
				return nil
			}
			// Different citizen taking over: mark old
			// 'abandoned' so iter_seq counts it as terminal
			// when we INSERT below.
			if _, err := tx.Exec(
				`UPDATE task_claims SET outcome = 'abandoned' WHERE id = ?`,
				openID,
			); err != nil {
				return fmt.Errorf("set_claim abandon: %w", err)
			}
			// Stage iteration_completed for the abandoned
			// claim. Read iter_seq + commit_sha from the row
			// we just closed so the audit captures whatever
			// progress the prior citizen made before takeover.
			var prevIter sql.NullInt64
			var prevCommit sql.NullString
			_ = tx.QueryRow(
				`SELECT iter_seq, COALESCE(commit_sha, '') FROM task_claims WHERE id = ?`,
				openID,
			).Scan(&prevIter, &prevCommit)
			sink.Emit(Event{
				CitizenID:    openCitizen,
				EventType:    "iteration_completed",
				EventSubtype: string(ClaimOutcomeAbandoned),
				TaskID:       m.TaskID,
				RunID:        taskRunID,
				ProjectID:    projectID,
				Metadata: MarshalMetadata(map[string]any{
					"iter_seq":         prevIter.Int64,
					"final_commit_sha": prevCommit.String,
					"taken_over_by":    m.CitizenID,
				}),
				CreatedAt: now,
			})
		}
	}

	// iter_seq for the new claim = (highest iter_seq among
	// terminal-outcome rows) + 1. Open rows are part of the
	// current iteration; we just abandoned the only single-
	// citizen one above (if any), so terminal rows reflect
	// completed prior iterations. Multi-citizen tasks have
	// concurrent open rows that all share the same iter_seq —
	// we read it from any open peer.
	iterSeq := 1
	if citizens > 1 {
		var peerIter sql.NullInt64
		_ = tx.QueryRow(
			`SELECT iter_seq FROM task_claims WHERE task_id = ? AND outcome IS NULL ORDER BY claimed_at DESC LIMIT 1`,
			m.TaskID,
		).Scan(&peerIter)
		if peerIter.Valid && peerIter.Int64 > 0 {
			iterSeq = int(peerIter.Int64)
		} else {
			var maxTerm sql.NullInt64
			_ = tx.QueryRow(
				`SELECT MAX(iter_seq) FROM task_claims WHERE task_id = ? AND outcome IS NOT NULL`,
				m.TaskID,
			).Scan(&maxTerm)
			if maxTerm.Valid {
				iterSeq = int(maxTerm.Int64) + 1
			}
		}
	} else {
		var maxTerm sql.NullInt64
		_ = tx.QueryRow(
			`SELECT MAX(iter_seq) FROM task_claims WHERE task_id = ? AND outcome IS NOT NULL`,
			m.TaskID,
		).Scan(&maxTerm)
		if maxTerm.Valid {
			iterSeq = int(maxTerm.Int64) + 1
		}
	}

	// Living-workflow phase 6b foundational v1: gate the
	// topic-branch flow at the run level. If ANY task in the
	// same run is multi-citizen (vote, multi-reviewer),
	// disable topics for the entire run — the multi-citizen
	// path commits directly to the run branch and would
	// silently advance main between this task's claim and
	// its accept, breaking the FF-merge invariant the topic
	// flow relies on. Conservative for v1; rebase support
	// (v2) lifts this restriction.
	var runHasMulti int
	_ = tx.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE run_id = (SELECT run_id FROM tasks WHERE id = ?) AND citizens > 1`,
		m.TaskID,
	).Scan(&runHasMulti)
	// Topic-branch name encodes iter_seq, so the branch is
	// stable across revisions within an iteration (phase 6c).
	branch := generateIterationBranch(taskAction, taskDefID, instanceKey, runSlug, runSeq, iterSeq-1, citizens, runHasMulti > 0)

	// Insert the claim row with the branch identifier.
	_, err := tx.Exec(
		`INSERT INTO task_claims (task_id, citizen_id, claimed_at, deadline, model, branch, iter_seq) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.TaskID, m.CitizenID, now, m.Deadline, nullableStr(m.Model), branch, iterSeq,
	)
	if err != nil {
		return fmt.Errorf("set_claim: %w", err)
	}
	// Stage iteration_started. Subtype "fresh" for iter_seq=1
	// (very first iteration on this task) vs "reopen" for later
	// iterations created after a prior outcome — both terminal
	// (rejected/invalidated) and same-citizen-after-takeover
	// fall here. The phase 6c reuse path (same citizen reclaim
	// without new INSERT) doesn't emit; that's the
	// task_request_changes signal instead.
	startedSubtype := "fresh"
	if iterSeq > 1 {
		startedSubtype = "reopen"
	}
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "iteration_started",
		EventSubtype: startedSubtype,
		TaskID:       m.TaskID,
		RunID:        taskRunID,
		ProjectID:    projectID,
		Metadata: MarshalMetadata(map[string]any{
			"iter_seq":         iterSeq,
			"iteration_branch": branch,
			"deadline":         m.Deadline.Format(time.RFC3339),
			"action":           taskAction,
		}),
		CreatedAt: now,
	})

	if citizens == 1 {
		// Single-citizen: flip state to CLAIMED.
		_, err = tx.Exec(
			`UPDATE tasks SET state = 'claimed', claimed_by = ?, claimed_at = ? WHERE id = ?`,
			m.CitizenID, now, m.TaskID,
		)
	} else {
		// Multi-citizen: don't change state, just record
		// the most recent claimer for convenience.
		_, err = tx.Exec(
			`UPDATE tasks SET claimed_by = ?, claimed_at = ? WHERE id = ?`,
			m.CitizenID, now, m.TaskID,
		)
	}
	return err
}

func applyReleaseClaim(tx *sql.Tx, m ReleaseClaim, sink EventSink) error {
	// Capture iter_seq + run/project context BEFORE the claim
	// row's outcome flips, so the iteration_completed event
	// carries the same metadata shape applySetClaim uses for
	// abandoned and applyRecordSubmission uses for completed.
	var iterSeq sql.NullInt64
	var commitSHA sql.NullString
	var runID, projectID int64
	_ = tx.QueryRow(
		`SELECT c.iter_seq, COALESCE(c.commit_sha, ''), t.run_id, r.project_id
		 FROM task_claims c JOIN tasks t ON c.task_id = t.id JOIN runs r ON t.run_id = r.id
		 WHERE c.task_id = ? AND c.citizen_id = ? AND c.outcome IS NULL`,
		m.TaskID, m.CitizenID,
	).Scan(&iterSeq, &commitSHA, &runID, &projectID)

	// Reset the task to READY only if this citizen actually
	// holds the claim. Guard prevents one citizen accidentally
	// releasing another citizen's claim if the call sites ever
	// drift.
	if _, err := tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`,
		m.TaskID, m.CitizenID,
	); err != nil {
		return err
	}
	// Mark the open claim row outcome=released. Preserves the
	// row as audit history (who claimed it, when, why it ended)
	// rather than deleting it. Matches the legacy
	// Store.ReleaseTask behavior this mutation now subsumes.
	if _, err := tx.Exec(
		`UPDATE task_claims SET outcome = 'released' WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
		m.TaskID, m.CitizenID,
	); err != nil {
		return err
	}
	// Emit only if we actually closed an open claim. The pre-
	// query returns zero rows when the citizen wasn't holding
	// the claim — UPDATE was a no-op, so no state change to
	// announce. iteration_completed{released} mirrors the
	// {abandoned} and {completed} subtypes for consistent
	// downstream consumption.
	if runID > 0 {
		sink.Emit(Event{
			CitizenID:    m.CitizenID,
			EventType:    "iteration_completed",
			EventSubtype: string(ClaimOutcomeReleased),
			TaskID:       m.TaskID,
			RunID:        runID,
			ProjectID:    projectID,
			Metadata: MarshalMetadata(map[string]any{
				"iter_seq":         iterSeq.Int64,
				"final_commit_sha": commitSHA.String,
			}),
			CreatedAt: time.Now(),
		})
	} else {
		sink.SkipEvents("release_claim no-op: citizen did not hold the claim")
	}
	return nil
}

// applyExpireClaim handles a deadline-driven claim expiration
// (scheduler/reaper). Resets the task to READY without the
// claimant guard (the reaper expires whoever holds it), marks
// the open claim row outcome=timed_out, and applies the
// citizen-stats penalty (timeout counter + score recompute).
//
// Distinct from applyReleaseClaim because (a) the outcome
// string differs and (b) only involuntary timeouts touch the
// citizen score.
//
// Emits iteration_completed{timed_out} for the closed claim
// (parallel to {released}, {abandoned}, {completed}).
func applyExpireClaim(tx *sql.Tx, m ExpireClaim, sink EventSink) error {
	// Capture iter_seq + run/project context BEFORE the claim
	// row's outcome flips, same pattern as applyReleaseClaim.
	var iterSeq sql.NullInt64
	var commitSHA sql.NullString
	var runID, projectID int64
	_ = tx.QueryRow(
		`SELECT c.iter_seq, COALESCE(c.commit_sha, ''), t.run_id, r.project_id
		 FROM task_claims c JOIN tasks t ON c.task_id = t.id JOIN runs r ON t.run_id = r.id
		 WHERE c.task_id = ? AND c.citizen_id = ? AND c.outcome IS NULL`,
		m.TaskID, m.CitizenID,
	).Scan(&iterSeq, &commitSHA, &runID, &projectID)

	if _, err := tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL WHERE id = ?`,
		m.TaskID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE task_claims SET outcome = 'timed_out' WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
		m.TaskID, m.CitizenID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE citizens SET
			tasks_timed_out = tasks_timed_out + 1,
			score = tasks_completed - ((tasks_timed_out + 1) * 0.5) - (tasks_rejected * 1.0)
		WHERE id = ?`,
		m.CitizenID,
	); err != nil {
		return err
	}
	if runID > 0 {
		sink.Emit(Event{
			CitizenID:    m.CitizenID,
			EventType:    "iteration_completed",
			EventSubtype: string(ClaimOutcomeTimedOut),
			TaskID:       m.TaskID,
			RunID:        runID,
			ProjectID:    projectID,
			Metadata: MarshalMetadata(map[string]any{
				"iter_seq":         iterSeq.Int64,
				"final_commit_sha": commitSHA.String,
			}),
			CreatedAt: time.Now(),
		})
	} else {
		// Reaper found an expired-deadline row but the JOIN-
		// driven lookup didn't see it (claim already closed by
		// a concurrent path). UPDATE was a no-op.
		sink.SkipEvents("expire_claim no-op: open claim not found at emit-time lookup")
	}
	return nil
}

// applySetClaimDeadline re-anchors the open claim's deadline. Pure
// lease bookkeeping — no state change, no event, no claim-history
// mutation (the row stays the same open claim, just with a later
// deadline). No-op when the task has no open claim (e.g. a race
// where the claim closed between plan-build and apply).
func applySetClaimDeadline(tx *sql.Tx, m SetClaimDeadline, sink EventSink) error {
	if _, err := tx.Exec(
		`UPDATE task_claims SET deadline = ? WHERE task_id = ? AND outcome IS NULL`,
		m.Deadline, m.TaskID,
	); err != nil {
		return err
	}
	// Pure lease bookkeeping — no state change, no audit signal.
	// (A re-anchor is not a claim/release/expire; nothing
	// observable changed for downstream consumers.)
	sink.SkipEvents("set_claim_deadline: lease re-anchor carries no event")
	return nil
}

func applyRecordSubmission(tx *sql.Tx, m RecordSubmission, sink EventSink) error {
	now := time.Now()

	// Read task to determine single vs multi citizen + action.
	var citizens int
	var taskAction, taskDefID, instanceKey string
	var runID, projectID int64
	var claimedBy sql.NullInt64
	if err := tx.QueryRow(
		`SELECT t.citizens, t.action, t.task_def_id, COALESCE(t.instance_key, ''), t.run_id, t.claimed_by, r.project_id
		 FROM tasks t JOIN runs r ON t.run_id = r.id WHERE t.id = ?`,
		m.TaskID,
	).Scan(&citizens, &taskAction, &taskDefID, &instanceKey, &runID, &claimedBy, &projectID); err != nil {
		return fmt.Errorf("submit: task %q not found", m.TaskID)
	}
	if citizens <= 0 {
		citizens = 1
	}

	// Same constraint as the claim path: an agent can't submit
	// without naming a model; humans and scripts can.
	if err := requireModelForAgent(tx, m.CitizenID, m.Model, "submit"); err != nil {
		return err
	}

	// Phase 6c — find the open claim row for THIS citizen (or
	// the latest one for single-citizen tasks where the
	// claim_by is implicit). The submission attempt is
	// recorded in task_submissions; the claim row's outcome
	// flips to 'completed' ONLY when the submission terminates
	// the iteration. A submitter whose task has a downstream
	// review leaves the claim open (outcome=NULL) — the
	// review's verdict will close it.
	var claimRowID sql.NullInt64
	if citizens == 1 {
		_ = tx.QueryRow(
			`SELECT id FROM task_claims WHERE task_id = ? AND outcome IS NULL ORDER BY id DESC LIMIT 1`,
			m.TaskID,
		).Scan(&claimRowID)
	} else {
		_ = tx.QueryRow(
			`SELECT id FROM task_claims WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
			m.TaskID, m.CitizenID,
		).Scan(&claimRowID)
	}

	choice := m.VoteChoice
	if choice == "" {
		choice = m.Decision
	}

	// Always record the submission attempt. Even if the claim
	// stays open (reviewed task awaiting verdict), the
	// submission row is the audit trail of THIS attempt.
	var attemptSeq int64
	var iterSeq sql.NullInt64
	if claimRowID.Valid {
		// m.Content is intentionally not persisted on the
		// coordinator. Submission prose lives in git as the
		// result.md the fat-client committed at the recorded
		// commit_sha — that's the canonical truth.
		// ARCHITECTURE.md #3 ("coordinator never stores
		// content") rules this out at the principle level;
		// hosted-mode scaling rules it out at the operational
		// level. Readers needing prose use workspace.Project.
		// ReadFileAtCommit against the fat-client's clone.
		if _, err := tx.Exec(
			`INSERT INTO task_submissions (claim_id, submitted_at, commit_sha, decision, option, model) VALUES (?, ?, ?, ?, ?, ?)`,
			claimRowID.Int64, now, m.CommitSHA, m.Decision, choice, nullableStr(m.Model),
		); err != nil {
			return fmt.Errorf("submit: record submission: %w", err)
		}
		// Derive attempt_seq + iter_seq for the event metadata.
		// attempt_seq counts submissions on this claim including
		// the one we just inserted. iter_seq comes from the claim
		// row (set by applySetClaim).
		_ = tx.QueryRow(
			`SELECT COUNT(*) FROM task_submissions WHERE claim_id = ?`,
			claimRowID.Int64,
		).Scan(&attemptSeq)
		_ = tx.QueryRow(
			`SELECT iter_seq FROM task_claims WHERE id = ?`,
			claimRowID.Int64,
		).Scan(&iterSeq)
		// Stage task_submitted: universal "citizen handed in
		// work" event. Fires once per submission attempt
		// regardless of action (answer/compute/review/vote).
		// review_given / vote_cast continue to fire from
		// engine/submit.go as the domain-specific facets — they
		// answer different questions ("what was the verdict?",
		// "which option?") than this universal emit.
		// estimated_tokens flows through here so the profile
		// counter that reads SUM(metadata.estimated_tokens)
		// stays populated after the redefinition (the
		// pre-existing engine/submit.go emission carried the token
		// count; with that gone, this is the universal carrier).
		sink.Emit(Event{
			CitizenID:    m.CitizenID,
			EventType:    "task_submitted",
			EventSubtype: taskAction,
			TaskID:       m.TaskID,
			RunID:        runID,
			ProjectID:    projectID,
			Metadata: MarshalMetadata(map[string]any{
				"iter_seq":         iterSeq.Int64,
				"attempt_seq":      attemptSeq,
				"commit_sha":       m.CommitSHA,
				"decision":         m.Decision,
				"estimated_tokens": m.EstimatedTokens,
			}),
			CreatedAt: now,
		})
	}

	if citizens == 1 {
		// Single-citizen: one submit → SUBMITTED. The actual
		// SUBMITTED → ACCEPTED transition lands later, either
		// inline (no merge needed) or via the /merges handler
		// after the fat-client confirms the topic merged onto
		// the run branch. Phase 8.3: this is the "honest gate"
		// — ACCEPTED now means "merge confirmed, downstream
		// safe to fan out," not just "submission landed
		// locally." See service.acceptTask for the closing
		// sequence both call sites share.
		//
		// The claim's outcome is conditional on whether a
		// downstream review will weigh in (stayOpen below) —
		// orthogonal to the task-level state flip; a claim
		// that closes here does NOT mean the task is accepted.
		_, err := tx.Exec(
			`UPDATE tasks SET state = 'submitted', submitted_at = ?, result_path = ?, commit_sha = ?, review_decision = ?, vote_choice = ? WHERE id = ?`,
			now, m.ResultPath, m.CommitSHA, m.Decision, m.VoteChoice, m.TaskID,
		)
		if err != nil {
			return err
		}

		// Phase 6c — claim outcome is "completed" UNLESS this
		// task has a downstream review still expecting a
		// verdict. A reviewed answer/compute task leaves its
		// claim open through the review round; the review's
		// approve/reject path (in handleSubmitResultReport)
		// closes it. A review task itself, or any task with no
		// downstream reviewer, terminates on submit.
		stayOpen := false
		if taskAction != "review" {
			var reviewerID sql.NullString
			// Use the shared canonical builder so this
			// stayOpen lookup matches the router's merge-
			// gate query (api.taskHasDownstreamReview →
			// store.HasReviewerOfTarget) byte-for-byte.
			// Diverging on the encoding would gate a task
			// here but not there (or vice versa), causing
			// silent merge-on-submit when a review is
			// pending.
			target := BuildReviewsTargetKey(taskDefID, instanceKey)
			_ = tx.QueryRow(
				`SELECT id FROM tasks WHERE run_id = ? AND action = 'review' AND reviews_target = ? LIMIT 1`,
				runID, target,
			).Scan(&reviewerID)
			if reviewerID.Valid {
				stayOpen = true
			}
		}

		if claimRowID.Valid && !stayOpen {
			if _, err := tx.Exec(
				`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, commit_sha = ?, decision = ?, model = COALESCE(?, model) WHERE id = ?`,
				now, m.VoteChoice, m.CommitSHA, m.Decision, nullableStr(m.Model), claimRowID.Int64,
			); err != nil {
				return err
			}
			// Phase 8.3: the citizen's iteration is complete at
			// this moment (claim is closing here; their work is
			// done). task_completed used to ride alongside, but
			// the brief redefined it to fire on the terminal
			// SUBMITTED → ACCEPTED transition — that's now a
			// later moment (acceptTask, called inline or from
			// the /merges handler). applySetTaskState's
			// TaskAccepted branch emits task_completed when the
			// flip lands, so removing the emission here doesn't
			// drop the event; it just shifts WHEN it fires.
			//
			// iteration_completed stays here because it pairs
			// with the claim row's outcome write — closing the
			// claim IS the iteration ending, regardless of
			// whether the task subsequently merges or fails.
			sink.Emit(Event{
				CitizenID:    m.CitizenID,
				EventType:    "iteration_completed",
				EventSubtype: string(ClaimOutcomeCompleted),
				TaskID:       m.TaskID,
				RunID:        runID,
				ProjectID:    projectID,
				Metadata: MarshalMetadata(map[string]any{
					"iter_seq":         iterSeq.Int64,
					"final_commit_sha": m.CommitSHA,
					"action":           taskAction,
				}),
				CreatedAt: now,
			})
		} else if claimRowID.Valid {
			// Stay open — but still update the
			// denormalized fields on the claim row so legacy
			// readers (which haven't been migrated to
			// task_submissions yet) see the latest attempt.
			// outcome stays NULL.
			if _, err := tx.Exec(
				`UPDATE task_claims SET submitted_at = ?, option = ?, commit_sha = ?, decision = ?, model = COALESCE(?, model) WHERE id = ?`,
				now, m.VoteChoice, m.CommitSHA, m.Decision, nullableStr(m.Model), claimRowID.Int64,
			); err != nil {
				return err
			}
		}
		// Score accounting. last_seen is intentionally not
		// touched — see the citizens.last_seen note in models.go.
		if claimedBy.Valid {
			tx.Exec(
				`UPDATE citizens SET tasks_completed = tasks_completed + 1, tokens_contributed = tokens_contributed + ?, score = (tasks_completed + 1) - (tasks_timed_out * 0.5) - (tasks_rejected * 1.0) WHERE id = ?`,
				m.TokensUsed, claimedBy.Int64,
			)
		}
	} else {
		// Multi-citizen: record submission, state → COLLECTING.
		// Each citizen's claim closes on their own submit
		// (their part is done); the task-level tally
		// transition is handled separately.
		_, err := tx.Exec(
			`UPDATE tasks SET state = 'collecting', result_path = ? WHERE id = ?`,
			m.ResultPath, m.TaskID,
		)
		if err != nil {
			return err
		}
		if claimRowID.Valid {
			tx.Exec(
				`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, commit_sha = ?, decision = ?, model = COALESCE(?, model) WHERE id = ?`,
				now, choice, m.CommitSHA, m.Decision, nullableStr(m.Model), claimRowID.Int64,
			)
			// Multi-citizen: this citizen's iteration just
			// closed. The task itself stays in COLLECTING until
			// quorum/threshold; task_completed is NOT emitted
			// here — it fires when the tally resolves the task
			// to ACCEPTED (separate code path).
			sink.Emit(Event{
				CitizenID:    m.CitizenID,
				EventType:    "iteration_completed",
				EventSubtype: string(ClaimOutcomeCompleted),
				TaskID:       m.TaskID,
				RunID:        runID,
				ProjectID:    projectID,
				Metadata: MarshalMetadata(map[string]any{
					"iter_seq":         iterSeq.Int64,
					"final_commit_sha": m.CommitSHA,
					"action":           taskAction,
					"multi_citizen":    true,
				}),
				CreatedAt: now,
			})
		}
		// Token contribution only (score waits for tally).
		// last_seen is intentionally not touched — see the
		// citizens.last_seen note in models.go.
		tx.Exec(
			`UPDATE citizens SET tokens_contributed = tokens_contributed + ? WHERE id = ?`,
			m.TokensUsed, m.CitizenID,
		)
	}
	if !claimRowID.Valid {
		// Degenerate: submit landed without an open claim row.
		// All downstream emits are gated on claimRowID.Valid, so
		// nothing fires. Phase 4b should decide whether to reject
		// this case at the apply layer; today the state UPDATE
		// still ran (submitted_at, etc.) but no signal escapes.
		sink.SkipEvents("submit without open claim row: degenerate path, no events fire (phase 4b: reject at apply?)")
	}
	return nil
}

func applyMoveArtifact(tx *sql.Tx, m MoveArtifact, sink EventSink) error {
	a := &m.Artifact
	branch := a.Branch
	if branch == "" {
		branch = "main"
	}
	// Tracked flips between INSERT and ON-CONFLICT-UPDATE so
	// re-running a compute task that flipped track:true → false
	// (or vice versa) is reflected in-place rather than
	// accumulating two rows. Same story for commit_sha, which
	// is "" whenever tracked is false.
	_, err := tx.Exec(
		`INSERT INTO artifacts (project_id, branch, path, last_writer, last_task_id, last_run_id, commit_sha, tracked, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, branch, path) DO UPDATE SET last_writer=?, last_task_id=?, last_run_id=?, commit_sha=?, tracked=?, updated_at=?`,
		a.ProjectID, branch, a.Path, a.LastWriter, a.LastTaskID, a.LastRunID, a.CommitSHA, boolToInt(a.Tracked), a.CreatedAt, a.UpdatedAt,
		a.LastWriter, a.LastTaskID, a.LastRunID, a.CommitSHA, boolToInt(a.Tracked), a.UpdatedAt,
	)
	// Artifact moves are bookkeeping for the index. Citizen-
	// observable visibility is covered by the downstream
	// task_ready event of the consuming task; an "artifact_moved"
	// event would be redundant noise.
	sink.SkipEvents("artifact index update is bookkeeping; downstream task_ready covers consumer visibility")
	return err
}

// boolToInt maps Go bool -> SQLite INTEGER (0/1). SQLite doesn't
// have a native BOOLEAN type; the application layer normalizes
// both directions so the column stays clean.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func applyDeleteArtifact(tx *sql.Tx, m DeleteArtifact, sink EventSink) error {
	branch := m.Branch
	if branch == "" {
		branch = "main"
	}
	_, err := tx.Exec(`DELETE FROM artifacts WHERE project_id = ? AND branch = ? AND path = ?`, m.ProjectID, branch, m.Path)
	// Same rationale as applyMoveArtifact: index bookkeeping,
	// no citizen-observable signal beyond what downstream task
	// state already conveys.
	sink.SkipEvents("artifact index delete is bookkeeping; cascade emits task_ready/cascade_fired downstream")
	return err
}

func applyCreateCitizen(tx *sql.Tx, m CreateCitizen, sink EventSink) (int64, error) {
	c := &m.Citizen
	if err := ValidateUsername(c.Username); err != nil {
		return 0, err
	}
	// Email + username uniqueness checks. Pre-INSERT so we can
	// surface a friendly error instead of an opaque CONSTRAINT
	// violation. Same logic the legacy Store.CreateCitizen had.
	if c.Email != "" {
		var count int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM citizens WHERE email = ?`, c.Email).Scan(&count)
		if count > 0 {
			return 0, fmt.Errorf("a citizen with this email already exists")
		}
	}
	var uCount int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM citizens WHERE username = ?`, c.Username).Scan(&uCount)
	if uCount > 0 {
		return 0, fmt.Errorf("username %q is already taken", c.Username)
	}

	role := c.Role
	if role == "" {
		role = "citizen"
	}
	// kind + parent_id MUST be in the INSERT or the schema's
	// column defaults silently override the caller's intent
	// (kind=>'human', parent_id=>NULL). Phase 1.1 bug history.
	kind := c.Kind
	if kind == "" {
		kind = CitizenKindHuman
	}
	res, err := tx.Exec(
		`INSERT INTO citizens (username, name, email, role, score, tasks_completed, tasks_rejected, tasks_timed_out, tasks_released, tokens_contributed, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, 0, ?, ?, ?, ?)`,
		c.Username, c.Name, c.Email, role, c.RegisteredAt, c.LastSeen, kind, nullableInt64(c.ParentID),
	)
	if err != nil {
		return 0, err
	}
	newID, _ := res.LastInsertId()
	// Seed the initial bearer token into the tokens table — the
	// only auth authority (citizens has no token column). The
	// token rides on the mutation, not the persisted citizen row.
	// Skipped when no token is supplied (the citizen then can't
	// authenticate until one is issued).
	if m.Token != "" {
		if _, err := tx.Exec(
			`INSERT INTO tokens (citizen_id, token, label, issued_at) VALUES (?, ?, ?, ?)`,
			newID, m.Token, m.TokenLabel, c.RegisteredAt,
		); err != nil {
			return 0, fmt.Errorf("issue initial token: %w", err)
		}
	}
	// Subtype carries the kind discriminator so consumers
	// (audit views, attribution dashboards) can split humans
	// vs agents without re-querying.
	meta := map[string]any{
		"username": c.Username,
		"role":     role,
	}
	if c.ParentID != nil {
		meta["parent_id"] = *c.ParentID
	}
	sink.Emit(Event{
		CitizenID:    newID,
		EventType:    "citizen_registered",
		EventSubtype: string(kind),
		Metadata:     MarshalMetadata(meta),
		CreatedAt:    time.Now(),
	})
	return newID, nil
}

func applySetCitizenRole(tx *sql.Tx, m SetCitizenRole, sink EventSink) error {
	res, err := tx.Exec(`UPDATE citizens SET role = ? WHERE id = ?`, m.Role, m.CitizenID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sink.SkipEvents("set_citizen_role no-op: citizen not found")
		return nil
	}
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "citizen_role_changed",
		EventSubtype: m.Role,
		Metadata:     MarshalMetadata(map[string]any{"role": m.Role}),
		CreatedAt:    time.Now(),
	})
	return nil
}

func applyUpdateCitizenProfile(tx *sql.Tx, m UpdateCitizenProfile, sink EventSink) error {
	if m.Name == nil && m.Email == nil {
		sink.SkipEvents("update_citizen_profile no-op: no fields supplied")
		return nil
	}
	if m.Email != nil && *m.Email != "" {
		var count int
		_ = tx.QueryRow(
			`SELECT COUNT(*) FROM citizens WHERE email = ? AND id != ?`,
			*m.Email, m.CitizenID,
		).Scan(&count)
		if count > 0 {
			return fmt.Errorf("a citizen with this email already exists")
		}
	}
	sets := []string{}
	args := []interface{}{}
	changed := map[string]any{}
	if m.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *m.Name)
		changed["name"] = *m.Name
	}
	if m.Email != nil {
		sets = append(sets, "email = ?")
		args = append(args, *m.Email)
		changed["email"] = *m.Email
	}
	args = append(args, m.CitizenID)
	res, err := tx.Exec(
		"UPDATE citizens SET "+strings.Join(sets, ", ")+" WHERE id = ?",
		args...,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sink.SkipEvents("update_citizen_profile no-op: citizen not found")
		return nil
	}
	sink.Emit(Event{
		CitizenID: m.CitizenID,
		EventType: "citizen_profile_updated",
		Metadata:  MarshalMetadata(changed),
		CreatedAt: time.Now(),
	})
	return nil
}

func applyIssueToken(tx *sql.Tx, m IssueToken, sink EventSink) (int64, error) {
	if m.Token == "" {
		return 0, fmt.Errorf("issue_token: token must be non-empty")
	}
	res, err := tx.Exec(
		`INSERT INTO tokens (citizen_id, token, label, issued_at) VALUES (?, ?, ?, ?)`,
		m.CitizenID, m.Token, m.Label, time.Now(),
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	sink.Emit(Event{
		CitizenID: m.CitizenID,
		EventType: "token_issued",
		Metadata: MarshalMetadata(map[string]any{
			"token_id": id,
			"label":    m.Label,
		}),
		CreatedAt: time.Now(),
	})
	return id, nil
}

func applyRevokeToken(tx *sql.Tx, m RevokeToken, sink EventSink) error {
	// Look up citizen_id before the UPDATE so the event carries
	// it; the WHERE clause filters already-revoked rows so a
	// double-revoke is a no-op (matches the legacy semantics).
	var citizenID int64
	_ = tx.QueryRow(
		`SELECT citizen_id FROM tokens WHERE id = ? AND revoked_at IS NULL`,
		m.TokenID,
	).Scan(&citizenID)
	res, err := tx.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now(), m.TokenID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sink.SkipEvents("revoke_token no-op: token already revoked or not found")
		return nil
	}
	sink.Emit(Event{
		CitizenID: citizenID,
		EventType: "token_revoked",
		Metadata: MarshalMetadata(map[string]any{
			"token_id": m.TokenID,
		}),
		CreatedAt: time.Now(),
	})
	return nil
}

func applyRevokeTokenByValue(tx *sql.Tx, m RevokeTokenByValue, sink EventSink) error {
	var tokenID, citizenID int64
	_ = tx.QueryRow(
		`SELECT id, citizen_id FROM tokens WHERE token = ? AND revoked_at IS NULL`,
		m.Token,
	).Scan(&tokenID, &citizenID)
	res, err := tx.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE token = ? AND revoked_at IS NULL`,
		time.Now(), m.Token,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sink.SkipEvents("revoke_token_by_value no-op: token already revoked or not found")
		return nil
	}
	sink.Emit(Event{
		CitizenID:    citizenID,
		EventType:    "token_revoked",
		EventSubtype: "by_value",
		Metadata: MarshalMetadata(map[string]any{
			"token_id": tokenID,
		}),
		CreatedAt: time.Now(),
	})
	return nil
}

func applyUpdateReadyTasks(tx *sql.Tx, s *Store, m UpdateReadyTasks, sink EventSink) ([]ReadiedTask, error) {
	// Run the cascade against the open tx so it sees in-tx
	// writes (e.g. an upstream task's accept transition earlier
	// in the same Plan) and shares the SQLite write lock for
	// its own UPDATEs. Pre-fix this delegated to s.UpdateReadyTasks
	// (which uses s.db) — that's a separate connection that
	// neither sees uncommitted state nor can write while the
	// tx holds the lock. Symptom was deadline-driven vote/
	// review resolve missing readiness propagation. See
	// dbExecQueryer doc.
	readied, err := updateReadyTasksOn(tx, m.RunID)
	if err != nil {
		return readied, err
	}
	readyEvents := buildTaskReadyEvents(readied, time.Now())
	if len(readyEvents) == 0 {
		// Cascade pass found no PENDING task whose deps just
		// satisfied. Common when the cascade fires defensively
		// after every Plan; not every Plan promotes anything.
		sink.SkipEvents("cascade pass found no newly-ready tasks")
	} else {
		for _, ev := range readyEvents {
			sink.Emit(ev)
		}
	}
	return readied, nil
}

// buildTaskReadyEvents fans out one task_ready event per
// assignee (or one with empty assign_to for unassigned tasks).
// Pure function so the emit shape is unit-testable without
// driving the full ApplyPlan transaction.
//
// Why fan-out: the assigned_task_ready notification rule does
// bare-string equality against assign_to. Stuffing the JSON
// array (`["alice","bob"]`) into metadata would never match
// any user — flattening to per-recipient events keeps the
// predicate simple.
//
// Event-count amplification: a 50-task cascade produces 50+
// events; multi-assignee adds N per task. Display caps at
// limit=20 (notify tool); for_each runs can produce noticeable
// spikes here, but each event is small and the EventStore is
// async.
func buildTaskReadyEvents(readied []ReadiedTask, now time.Time) []Event {
	var out []Event
	for _, rt := range readied {
		// Render parent snapshot once per readied task — same
		// payload regardless of how many assignees fan out below.
		var parentsMeta []map[string]any
		for _, p := range rt.Parents {
			parentsMeta = append(parentsMeta, map[string]any{
				"task_id":    p.TaskID,
				"action":     p.Action,
				"commit_sha": p.CommitSHA,
				"result_dir": p.ResultDir,
			})
		}
		emit := func(assignee string) {
			meta := map[string]any{
				"assign_to": assignee,
			}
			if len(parentsMeta) > 0 {
				meta["parents"] = parentsMeta
			}
			out = append(out, Event{
				EventType:    "task_ready",
				EventSubtype: rt.Action,
				TaskID:       rt.TaskID,
				RunID:        rt.RunID,
				ProjectID:    rt.ProjectID,
				Metadata:     MarshalMetadata(meta),
				CreatedAt:    now,
			})
		}
		if len(rt.Assignees) == 0 {
			emit("")
			continue
		}
		for _, a := range rt.Assignees {
			emit(a)
		}
	}
	return out
}

// applyCompleteRun re-evaluates the run's state from the current
// task counts. The mutation name is historical — phase 1 of the
// living-workflow design expanded it from "flip to completed if
// all terminal" to "compute the right alive-or-completed state."
//
// State rule:
//
//   - Any task in {ready, claimed, running, collecting} → active
//   - Else any task in {pending, parked} → idle
//   - Else (all in {accepted, skipped, failed}) → completed
//
// Paused is preserved — pause is a deliberate operator action and
// must only be left via explicit resume. The function returns
// true when the run transitioned to `completed` so callers that
// surface "run completed" UX (the existing behavior) keep working
// unchanged.
func applyCompleteRun(tx *sql.Tx, m CompleteRun, sink EventSink) (bool, error) {
	var current string
	var projectID int64
	var autoTriage string
	var runSeq int64
	err := tx.QueryRow(
		`SELECT state, project_id, auto_triage_template, seq FROM runs WHERE id = ?`, m.RunID,
	).Scan(&current, &projectID, &autoTriage, &runSeq)
	if err != nil {
		return false, err
	}
	// Paused / failed / terminated runs don't auto-transition.
	// Terminate is irreversible — a stale CompleteRun mutation
	// firing afterward (e.g. from a plan composed before the
	// terminate) must NOT walk task counts and silently flip a
	// terminated run to completed. Terminated belongs in this
	// guard for the same reason failed does.
	if current == string(RunPaused) || current == string(RunFailed) || current == string(RunTerminated) {
		sink.SkipEvents("complete_run no-op: run is paused/failed/terminated, no auto-transition")
		return false, nil
	}

	// 'failed_retryable' counts as holding (→ RunWaiting), not
	// done: a compute task that errored is a live blocker the
	// operator must retry. Without it here, a *leaf* retryable
	// failure (no pending descendants) would fall through to
	// RunCompleted and wrongly settle the run. With pending
	// descendants the result is the same (they're holding too);
	// this also covers the leaf case.
	var active, holding, total int
	err = tx.QueryRow(
		`SELECT
		  COUNT(*),
		  COUNT(CASE WHEN state IN ('ready','claimed','running','collecting') THEN 1 END),
		  COUNT(CASE WHEN state IN ('pending','parked','failed_retryable') THEN 1 END)
		 FROM tasks WHERE run_id = ?`,
		m.RunID,
	).Scan(&total, &active, &holding)
	if err != nil {
		return false, err
	}

	var next RunState
	switch {
	case total == 0:
		// Empty run — keep current. Run is freshly created and
		// tasks haven't been inserted yet; CompleteRun fired
		// from a stale plan should not flip an empty run.
		sink.SkipEvents("complete_run no-op: empty run (tasks not yet inserted)")
		return false, nil
	case active > 0:
		next = RunActive
	case holding > 0:
		next = RunWaiting
	default:
		next = RunCompleted
	}

	// Phase 4c open-ended override: a run with an
	// auto_triage_template + open issues stays alive (idle)
	// instead of completing — the hook may still spawn work.
	// See EvaluateRunState for the matching standalone rule.
	if next == RunCompleted && autoTriage != "" {
		var openCount int
		_ = tx.QueryRow(
			`SELECT COUNT(*) FROM issues WHERE project_id = ? AND status = 'open'`, projectID,
		).Scan(&openCount)
		if openCount > 0 {
			next = RunWaiting
		}
	}

	if string(next) == current {
		sink.SkipEvents("complete_run no-op: state unchanged")
		return next == RunCompleted, nil
	}
	now := time.Now()
	// Phase 8.5 — runs.blocked_by tracks why a WAITING run
	// can't progress. Compute on entry to WAITING; clear on
	// exit. The column rides the same UPDATE as the state
	// flip so a reader who sees state=waiting always sees a
	// matching blocker (or NULL on the rare "stuck but
	// unclassifiable" path), and a reader who sees a non-
	// WAITING state never sees a stale blocker.
	if next == RunWaiting {
		blocker, berr := computeBlockedBy(tx, m.RunID)
		if berr != nil {
			return false, berr
		}
		if _, err := tx.Exec(
			`UPDATE runs SET state = ?, blocked_by = ?, updated_at = ? WHERE id = ?`,
			next, blocker, now, m.RunID,
		); err != nil {
			return false, err
		}
	} else {
		if _, err := tx.Exec(
			`UPDATE runs SET state = ?, blocked_by = NULL, updated_at = ? WHERE id = ?`,
			next, now, m.RunID,
		); err != nil {
			return false, err
		}
	}
	// Stage a lifecycle event for post-commit emission. citizen
	// 0 = system (not initiated by a specific actor — these
	// transitions fall out of task-graph state). The matching
	// post-commit drain in applyPlanOnce flushes to the
	// EventStore only if the whole plan commits.
	// run_seq is included so consumers (notify→live.jsonl→bot
	// supervisor's auto_stop tailer) can identify which run hit
	// a terminal state without joining the runs table. Otherwise
	// the wire-shape event has no run identifier (RunID is the
	// store's internal id, not exposed; seq is what callers know).
	sink.Emit(Event{
		EventType:    "run_" + string(next),
		EventSubtype: current,
		RunID:        m.RunID,
		ProjectID:    projectID,
		Metadata: MarshalMetadata(map[string]any{
			"from":    current,
			"to":      next,
			"run_seq": runSeq,
		}),
		CreatedAt: now,
	})
	return next == RunCompleted, nil
}

// requireModelForAgent enforces the operator/model rule: an agent
// operator must always name the model that produced the words for
// its action; a human (or a script) may act with no model. Used by
// both applySetClaim and applyRecordSubmission so the constraint
// kicks in whether the agent omits its model at claim time or at
// submit time.
//
// This is now a trivial check — the actor's kind plus a string
// presence test, no model-citizen lookup. Implemented in Go (not
// SQL CHECK) because SQLite CHECK can't conditionally read the
// operator's kind. The query is small (one row by primary key) and
// runs inside the apply transaction, so consistency is preserved.
func requireModelForAgent(tx *sql.Tx, operatorID int64, model, op string) error {
	if model != "" {
		return nil // model named — fine for any operator kind
	}
	var rawKind string
	if err := tx.QueryRow(`SELECT kind FROM citizens WHERE id = ?`, operatorID).Scan(&rawKind); err != nil {
		return fmt.Errorf("%s: read operator kind: %w", op, err)
	}
	if CitizenKind(rawKind) == CitizenKindBot {
		return fmt.Errorf("%s: operator citizen %d is an agent — model is required (an agent cannot act without naming a model)", op, operatorID)
	}
	return nil
}

// nullableInt64 converts a *int64 to a value suitable for SQL
// nullable INTEGER columns. Passing a typed nil through database/
// sql works for some drivers but is brittle; sql.NullInt64 with
// Valid=false is the portable form. Callers use this for
// nullable FK columns.
func nullableInt64(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// nullableStr maps an empty string to SQL NULL and any non-empty
// value to itself. Used for task_claims.model / task_submissions.
// model so that "no model" is a real NULL — this keeps the
// multi-attempt COALESCE(?, model) preserve-prior-model semantics
// working (an empty later attempt doesn't blank an earlier model).
func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// applyEmitEvent handles the EmitEvent mutation: pure pass-
// through of one event to the sink. No state change, no
// validation. Exists so metadata-only emits (cascade_fired,
// branch_merged, etc.) can ride the EventSink contract instead
// of an out-of-band Store.Events().Record call.
func applyEmitEvent(m EmitEvent, sink EventSink) {
	sink.Emit(m.Event)
}

func applyCreateProject(tx *sql.Tx, m CreateProject, sink EventSink) (int64, error) {
	p := &m.Project
	var remote sql.NullString
	if p.RemoteURL != "" {
		remote = sql.NullString{String: p.RemoteURL, Valid: true}
	}
	branch := p.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	res, err := tx.Exec(
		`INSERT INTO projects (name, description, created_by, remote_url, default_branch, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Description, p.CreatedBy, remote, branch, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	sink.Emit(Event{
		EventType: "project_created",
		ProjectID: id,
		Metadata: MarshalMetadata(map[string]any{
			"name":           p.Name,
			"created_by":     p.CreatedBy,
			"default_branch": branch,
		}),
		CreatedAt: time.Now(),
	})
	return id, nil
}

func applySetProjectDefaultBranch(tx *sql.Tx, m SetProjectDefaultBranch, sink EventSink) error {
	branch := m.Branch
	if branch == "" {
		branch = "main"
	}
	if _, err := tx.Exec(
		`UPDATE projects SET default_branch = ?, updated_at = ? WHERE id = ?`,
		branch, time.Now(), m.ProjectID,
	); err != nil {
		return err
	}
	sink.Emit(Event{
		EventType:    "project_settings_changed",
		EventSubtype: "default_branch",
		ProjectID:    m.ProjectID,
		Metadata:     MarshalMetadata(map[string]any{"default_branch": branch}),
		CreatedAt:    time.Now(),
	})
	return nil
}

func applySetProjectRemoteURL(tx *sql.Tx, m SetProjectRemoteURL, sink EventSink) error {
	var remote sql.NullString
	if m.RemoteURL != "" {
		remote = sql.NullString{String: m.RemoteURL, Valid: true}
	}
	if _, err := tx.Exec(
		`UPDATE projects SET remote_url = ?, updated_at = ? WHERE id = ?`,
		remote, time.Now(), m.ProjectID,
	); err != nil {
		return err
	}
	// remote_url itself is not in the metadata — could be a
	// secret-bearing URL (token@host syntax). Subscribers that
	// need it can read the projects row; the event signals
	// "the value changed" without leaking what to.
	sink.Emit(Event{
		EventType:    "project_settings_changed",
		EventSubtype: "remote_url",
		ProjectID:    m.ProjectID,
		Metadata:     MarshalMetadata(map[string]any{"remote_set": m.RemoteURL != ""}),
		CreatedAt:    time.Now(),
	})
	return nil
}

func applyAddProjectMember(tx *sql.Tx, m AddProjectMember, sink EventSink) error {
	if m.ProjectID == 0 || m.CitizenID == 0 {
		return fmt.Errorf("project_id and citizen_id are required")
	}
	role := m.Role
	if role == "" {
		role = ProjectRoleMember
	}
	var addedByVal sql.NullInt64
	if m.AddedBy != 0 {
		addedByVal = sql.NullInt64{Int64: m.AddedBy, Valid: true}
	}
	if _, err := tx.Exec(
		`INSERT INTO project_members (project_id, citizen_id, role, added_at, added_by)
		 VALUES (?, ?, ?, ?, ?)`,
		m.ProjectID, m.CitizenID, string(role), time.Now(), addedByVal,
	); err != nil {
		return err
	}
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "project_member_added",
		EventSubtype: string(role),
		ProjectID:    m.ProjectID,
		Metadata: MarshalMetadata(map[string]any{
			"role":     string(role),
			"added_by": m.AddedBy,
		}),
		CreatedAt: time.Now(),
	})
	return nil
}

func applyRemoveProjectMember(tx *sql.Tx, m RemoveProjectMember, sink EventSink) error {
	res, err := tx.Exec(
		`DELETE FROM project_members WHERE project_id = ? AND citizen_id = ?`,
		m.ProjectID, m.CitizenID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Idempotent no-op (citizen wasn't a member). No row
		// change means nothing to announce.
		sink.SkipEvents("remove_project_member no-op: citizen was not a member")
		return nil
	}
	sink.Emit(Event{
		CitizenID: m.CitizenID,
		EventType: "project_member_removed",
		ProjectID: m.ProjectID,
		Metadata:  MarshalMetadata(map[string]any{}),
		CreatedAt: time.Now(),
	})
	return nil
}

func applySpawnTask(tx *sql.Tx, m SpawnTask, sink EventSink) (string, bool, error) {
	spec := m.Spec
	if spec.RunID == 0 {
		return "", false, fmt.Errorf("spawn_task: run_id is required")
	}
	if spec.TaskDefID == "" {
		return "", false, fmt.Errorf("spawn_task: task_def_id is required")
	}
	if spec.Action == "" {
		return "", false, fmt.Errorf("spawn_task: action is required")
	}
	if spec.Trigger == "" {
		spec.Trigger = "human"
	}
	if spec.Citizens <= 0 {
		spec.Citizens = 1
	}
	if spec.ResultType == "" {
		spec.ResultType = "text"
	}

	var (
		runState   string
		projectID  int64
		runSeq     int
		runSlug    string
		budgetUsed int
		budgetMax  int
	)
	if err := tx.QueryRow(
		`SELECT state, project_id, seq, slug, cycle_budget_used, cycle_budget_max
		 FROM runs WHERE id = ?`,
		spec.RunID,
	).Scan(&runState, &projectID, &runSeq, &runSlug, &budgetUsed, &budgetMax); err != nil {
		return "", false, fmt.Errorf("loading run: %w", err)
	}

	switch RunState(runState) {
	case RunCompleted, RunFailed, RunTerminated:
		return "", false, fmt.Errorf("run %d is %s — cannot spawn into a terminal run", spec.RunID, runState)
	case RunPaused:
		return "", false, fmt.Errorf("run %d is paused — resume it first with enju_resume_run", spec.RunID)
	}

	if budgetUsed >= budgetMax {
		now := time.Now()
		// Pause the run + emit cycle_budget_exhausted in this
		// tx. The handler returns (taskID="", exhausted=true,
		// err=nil) so the dispatcher commits — the legacy
		// inline-commit-then-return-error pattern would have
		// undone the pause when the chokepoint rollback ran.
		// Service caller checks ApplyResult.BudgetExhausted
		// and converts to a typed user error.
		if _, err := tx.Exec(
			`UPDATE runs SET state = 'paused', updated_at = ? WHERE id = ? AND state IN ('active', 'waiting')`,
			now, spec.RunID,
		); err != nil {
			return "", false, fmt.Errorf("auto-pausing run on budget exhaustion: %w", err)
		}
		sink.Emit(Event{
			EventType: "cycle_budget_exhausted",
			RunID:     spec.RunID,
			ProjectID: projectID,
			Metadata: MarshalMetadata(map[string]any{
				"used":           budgetUsed,
				"max":            budgetMax,
				"attempted_task": spec.TaskDefID,
				"attempted_by":   spec.SpawnedBy,
			}),
			CreatedAt: now,
		})
		return "", true, nil
	}

	var maxSeq int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM tasks WHERE run_id = ?`, spec.RunID).Scan(&maxSeq); err != nil {
		return "", false, err
	}
	nextSeq := maxSeq + 1
	taskID := fmt.Sprintf("%d:%d:%s", projectID, runSeq, spec.TaskDefID)

	state := TaskReady
	if len(spec.DependsOn) > 0 {
		state = TaskPending
	}
	dependsOn := strings.Join(spec.DependsOn, ",")
	assignTo := ""
	if len(spec.AssignTo) > 0 {
		quoted := make([]string, len(spec.AssignTo))
		for i, u := range spec.AssignTo {
			quoted[i] = fmt.Sprintf("%q", u)
		}
		assignTo = "[" + strings.Join(quoted, ",") + "]"
	}

	now := time.Now()
	if _, err := tx.Exec(
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref,
		                    action, prompt, user_prompt, script, outputs, requirements, result_type,
		                    timeout, state, depends_on, reads_artifacts, writes_artifacts,
		                    assign_to, require_role, citizens, run_slug,
		                    spawned_from, spawn_trigger, closes_issue_seq, created_at)
		 VALUES (?, ?, ?, ?, '', '', '',
		         ?, ?, ?, '', '', '', ?,
		         '', ?, ?, '[]', '[]',
		         ?, ?, ?, ?,
		         ?, ?, ?, ?)`,
		taskID, spec.RunID, nextSeq, spec.TaskDefID,
		spec.Action, spec.Prompt, spec.UserPrompt, spec.ResultType,
		state, dependsOn,
		assignTo, spec.RequireRole, spec.Citizens, runSlug,
		spec.ParentTaskID, spec.Trigger, spec.ClosesIssueSeq, now,
	); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: tasks.id") {
			return "", false, fmt.Errorf("task_def_id %q already exists in run %d — pick a different id (or bump the suffix on a re-spawn)", spec.TaskDefID, spec.RunID)
		}
		return "", false, fmt.Errorf("inserting spawned task: %w", err)
	}

	if _, err := tx.Exec(`UPDATE runs SET cycle_budget_used = cycle_budget_used + 1, updated_at = ? WHERE id = ?`, now, spec.RunID); err != nil {
		return "", false, err
	}

	runReactivated := false
	if state == TaskReady && runState == string(RunWaiting) {
		if _, err := tx.Exec(`UPDATE runs SET state = 'active', updated_at = ? WHERE id = ? AND state = 'waiting'`, now, spec.RunID); err != nil {
			return "", false, err
		}
		runReactivated = true
	}

	if runReactivated {
		sink.Emit(Event{
			EventType:    "run_active",
			EventSubtype: "waiting",
			RunID:        spec.RunID,
			ProjectID:    projectID,
			Metadata: MarshalMetadata(map[string]any{
				"from":    "waiting",
				"to":      "active",
				"trigger": "spawn",
			}),
			CreatedAt: now,
		})
	}
	sink.Emit(Event{
		CitizenID:    spec.SpawnedBy,
		EventType:    "task_spawned",
		EventSubtype: spec.Trigger,
		TaskID:       taskID,
		RunID:        spec.RunID,
		ProjectID:    projectID,
		Metadata: MarshalMetadata(map[string]any{
			"task_def_id":    spec.TaskDefID,
			"action":         spec.Action,
			"parent_task_id": spec.ParentTaskID,
			"trigger":        spec.Trigger,
			"depends_on":     dependsOn,
		}),
		CreatedAt: now,
	})
	return taskID, false, nil
}

func applySetCycleBudgetMax(tx *sql.Tx, m SetCycleBudgetMax, sink EventSink) error {
	var used, oldMax int
	var projectID int64
	if err := tx.QueryRow(
		`SELECT cycle_budget_used, cycle_budget_max, project_id FROM runs WHERE id = ?`, m.RunID,
	).Scan(&used, &oldMax, &projectID); err != nil {
		return err
	}
	if m.NewMax < used {
		return fmt.Errorf("new max %d is below current used %d — would be immediately exhausted", m.NewMax, used)
	}
	if m.NewMax == oldMax {
		// Idempotent no-op; the column doesn't change so nothing
		// to announce.
		sink.SkipEvents("set_cycle_budget_max no-op: new max equals current max")
		return nil
	}
	now := time.Now()
	if _, err := tx.Exec(
		`UPDATE runs SET cycle_budget_max = ?, updated_at = ? WHERE id = ?`,
		m.NewMax, now, m.RunID,
	); err != nil {
		return err
	}
	sink.Emit(Event{
		CitizenID: m.CitizenID,
		EventType: "cycle_budget_changed",
		RunID:     m.RunID,
		ProjectID: projectID,
		Metadata: MarshalMetadata(map[string]any{
			"old_max": oldMax,
			"new_max": m.NewMax,
			"used":    used,
		}),
		CreatedAt: now,
	})
	return nil
}

func applyCreateIssue(tx *sql.Tx, m CreateIssue, sink EventSink) (int64, int, error) {
	rec := m.Issue
	if rec.ProjectID == 0 {
		return 0, 0, fmt.Errorf("create_issue: project_id is required")
	}
	if rec.Title == "" {
		return 0, 0, fmt.Errorf("create_issue: title is required")
	}
	if rec.FiledBy == 0 {
		return 0, 0, fmt.Errorf("create_issue: filed_by is required")
	}
	if rec.Status == "" {
		rec.Status = IssueStatusOpen
	}
	if rec.Severity == "" {
		rec.Severity = IssueSeverityMedium
	}
	now := time.Now()
	if rec.FiledAt.IsZero() {
		rec.FiledAt = now
	}
	rec.UpdatedAt = now

	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM issues WHERE project_id = ?`, rec.ProjectID).Scan(&maxSeq); err != nil {
		return 0, 0, err
	}
	rec.Seq = int(maxSeq.Int64) + 1

	res, err := tx.Exec(
		`INSERT INTO issues (project_id, seq, title, body, status, severity, found_in_run_id, found_in_task_id, filed_by, filed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ProjectID, rec.Seq, rec.Title, rec.Body, rec.Status, rec.Severity,
		rec.FoundInRunID, rec.FoundInTaskID, rec.FiledBy, rec.FiledAt, rec.UpdatedAt,
	)
	if err != nil {
		return 0, 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	sink.Emit(Event{
		CitizenID:    rec.FiledBy,
		EventType:    "issue_filed",
		EventSubtype: string(rec.Severity),
		TaskID:       rec.FoundInTaskID,
		RunID:        rec.FoundInRunID,
		ProjectID:    rec.ProjectID,
		Metadata: MarshalMetadata(map[string]any{
			"issue_seq": rec.Seq,
			"title":     rec.Title,
			"severity":  rec.Severity,
		}),
		CreatedAt: rec.FiledAt,
	})
	return id, rec.Seq, nil
}

func applyTriageIssue(tx *sql.Tx, m TriageIssue, sink EventSink) error {
	now := time.Now()
	q := `UPDATE issues
	     SET status = 'triaged', triaged_by = ?, triaged_at = ?, updated_at = ?`
	args := []interface{}{m.CitizenID, now, now}
	if m.Severity != "" {
		q += `, severity = ?`
		args = append(args, m.Severity)
	}
	q += ` WHERE id = ? AND status = 'open'`
	args = append(args, m.IssueID)

	res, err := tx.Exec(q, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("issue %d cannot be triaged (already triaged/closed/wontfix or not found)", m.IssueID)
	}

	var seq int
	var newSeverity string
	var projectID int64
	if err := tx.QueryRow(
		`SELECT project_id, seq, severity FROM issues WHERE id = ?`, m.IssueID,
	).Scan(&projectID, &seq, &newSeverity); err != nil {
		return err
	}
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "issue_triaged",
		EventSubtype: newSeverity,
		ProjectID:    projectID,
		Metadata: MarshalMetadata(map[string]any{
			"issue_seq": seq,
			"severity":  newSeverity,
		}),
		CreatedAt: now,
	})
	return nil
}

func applyMarkIssueInProgress(tx *sql.Tx, m MarkIssueInProgress, sink EventSink) error {
	now := time.Now()
	res, err := tx.Exec(
		`UPDATE issues
		 SET status = 'in_progress', closed_by_task_id = ?, updated_at = ?
		 WHERE id = ? AND status IN ('open', 'triaged')`,
		m.FixTaskID, now, m.IssueID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("issue %d cannot move to in_progress (already terminal/in_progress or not found)", m.IssueID)
	}

	var seq int
	var projectID int64
	if err := tx.QueryRow(
		`SELECT project_id, seq FROM issues WHERE id = ?`, m.IssueID,
	).Scan(&projectID, &seq); err != nil {
		return err
	}
	sink.Emit(Event{
		CitizenID: m.CitizenID,
		EventType: "issue_in_progress",
		TaskID:    m.FixTaskID,
		ProjectID: projectID,
		Metadata: MarshalMetadata(map[string]any{
			"issue_seq":   seq,
			"fix_task_id": m.FixTaskID,
		}),
		CreatedAt: now,
	})
	return nil
}

func applyCloseIssue(tx *sql.Tx, m CloseIssue, sink EventSink) error {
	if m.Status != IssueStatusClosed && m.Status != IssueStatusWontfix {
		return fmt.Errorf("close_issue: status must be 'closed' or 'wontfix', got %q", m.Status)
	}
	now := time.Now()
	res, err := tx.Exec(
		`UPDATE issues
		 SET status = ?, closed_by_task_id = ?, closed_at = ?, updated_at = ?
		 WHERE id = ? AND status IN ('open', 'triaged', 'in_progress')`,
		m.Status, m.ClosedByTaskID, now, now, m.IssueID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("issue %d cannot be closed (already terminal or not found)", m.IssueID)
	}

	var seq int
	var projectID int64
	if err := tx.QueryRow(
		`SELECT project_id, seq FROM issues WHERE id = ?`, m.IssueID,
	).Scan(&projectID, &seq); err != nil {
		return err
	}
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "issue_closed",
		EventSubtype: string(m.Status),
		TaskID:       m.ClosedByTaskID,
		ProjectID:    projectID,
		Metadata: MarshalMetadata(map[string]any{
			"issue_seq":         seq,
			"status":            m.Status,
			"closed_by_task_id": m.ClosedByTaskID,
		}),
		CreatedAt: now,
	})
	return nil
}

func applyPauseRun(tx *sql.Tx, m PauseRun, sink EventSink) error {
	var current string
	var projectID int64
	if err := tx.QueryRow(
		`SELECT state, project_id FROM runs WHERE id = ?`, m.RunID,
	).Scan(&current, &projectID); err != nil {
		return err
	}
	if RunState(current) == RunPaused {
		// Idempotent: already paused, no state change to announce.
		sink.SkipEvents("pause_run no-op: run already paused")
		return nil
	}
	now := time.Now()
	res, err := tx.Exec(
		`UPDATE runs SET state = 'paused', updated_at = ?
		 WHERE id = ? AND state IN ('active', 'waiting')`,
		now, m.RunID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("run %d cannot be paused (already terminal or not found)", m.RunID)
	}
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "run_paused",
		EventSubtype: current,
		RunID:        m.RunID,
		ProjectID:    projectID,
		Metadata:     MarshalMetadata(map[string]any{"from": current, "to": "paused"}),
		CreatedAt:    now,
	})
	return nil
}

func applyResumeRun(tx *sql.Tx, m ResumeRun, sink EventSink) error {
	var current string
	var projectID int64
	if err := tx.QueryRow(
		`SELECT state, project_id FROM runs WHERE id = ?`, m.RunID,
	).Scan(&current, &projectID); err != nil {
		return err
	}
	switch RunState(current) {
	case RunCompleted, RunFailed, RunTerminated:
		return fmt.Errorf("run %d is %s — cannot resume a terminal run", m.RunID, current)
	case RunActive, RunWaiting:
		// Idempotent: already non-paused, nothing to lift.
		sink.SkipEvents("resume_run no-op: run already in active/idle")
		return nil
	}
	now := time.Now()
	if _, err := tx.Exec(
		`UPDATE runs SET state = 'active', updated_at = ? WHERE id = ? AND state = 'paused'`,
		now, m.RunID,
	); err != nil {
		return err
	}
	// Emit run_resumed; the caller's CompleteRun mutation later
	// in the plan may further transition active → idle, which
	// emits its own run_waiting event with citizen 0 (system).
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "run_resumed",
		EventSubtype: "paused",
		RunID:        m.RunID,
		ProjectID:    projectID,
		Metadata:     MarshalMetadata(map[string]any{"from": "paused", "to": "active"}),
		CreatedAt:    now,
	})
	return nil
}

// applyTerminateRun is the human-pulled-the-plug operation.
// Atomic in one transaction:
//
//  1. Read current run state. Refuse if already terminal
//     (completed/failed/terminated). Pause→terminate IS valid;
//     active|idle|paused all roll forward.
//  2. UPDATE run state → 'terminated'.
//  3. UPDATE every non-terminal task in the run → state='skipped',
//     skip_reason='run_terminated'. Counts the affected rows for
//     the event metadata.
//  4. UPDATE every open claim (outcome IS NULL) on tasks of this
//     run → outcome='abandoned', completed_at=now.
//  5. Emit one run_terminated event with operator + reason +
//     prior state + counts of skipped tasks / abandoned claims.
//
// Topic branches stay (no git work). Late-arriving submits are
// refused at the submit handler — see the run-state guard there.
func applyTerminateRun(tx *sql.Tx, m TerminateRun, result *ApplyResult, sink EventSink) error {
	var current string
	var projectID int64
	var runSeq int64
	if err := tx.QueryRow(
		`SELECT state, project_id, seq FROM runs WHERE id = ?`, m.RunID,
	).Scan(&current, &projectID, &runSeq); err != nil {
		return err
	}
	switch RunState(current) {
	case RunCompleted, RunFailed, RunTerminated:
		return fmt.Errorf("run %d is %s — cannot terminate a terminal run", m.RunID, current)
	}
	now := time.Now()

	// 1. Run state → terminated.
	res, err := tx.Exec(
		`UPDATE runs SET state = 'terminated', updated_at = ?
		 WHERE id = ? AND state IN ('active', 'waiting', 'paused')`,
		now, m.RunID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run %d cannot be terminated (already terminal or not found)", m.RunID)
	}

	// 2. Non-terminal tasks → skipped with run_terminated reason.
	// "Terminal" task states are the ones the cascade leaves alone:
	// already-accepted work stays accepted, already-failed stays
	// failed, etc. Everything else (pending, ready, claimed,
	// running, submitted, collecting, parked) flips to skipped.
	taskRes, err := tx.Exec(
		`UPDATE tasks SET state = 'skipped', skip_reason = 'run_terminated'
		 WHERE run_id = ? AND state NOT IN
		 ('accepted', 'rejected', 'invalid', 'invalidated', 'skipped', 'failed')`,
		m.RunID,
	)
	if err != nil {
		return fmt.Errorf("skipping non-terminal tasks: %w", err)
	}
	skippedTasks, _ := taskRes.RowsAffected()

	// 3. Open claims → outcome='abandoned'.
	// JOIN through tasks to scope by run_id. The schema has no
	// per-claim completion timestamp; outcome alone marks the
	// row closed (abandoned matches what cascade-skip already
	// uses elsewhere — see applyMarkOpenClaimsInvalidated).
	claimRes, err := tx.Exec(
		`UPDATE task_claims SET outcome = 'abandoned'
		 WHERE outcome IS NULL
		 AND task_id IN (SELECT id FROM tasks WHERE run_id = ?)`,
		m.RunID,
	)
	if err != nil {
		return fmt.Errorf("abandoning open claims: %w", err)
	}
	abandonedClaims, _ := claimRes.RowsAffected()
	result.SkippedTasks = int(skippedTasks)
	result.AbandonedClaims = int(abandonedClaims)

	// 4. Emit ONE coarse-grained run_terminated event. We
	// deliberately do NOT emit per-task task_skipped events for
	// the cascade. Terminate is the human-pulled-the-plug
	// operation; flooding the event log with N task_skipped
	// rows would conflate "operator aborted everything in one
	// stroke" with "this specific task was skipped because its
	// upstream rejected" — they're different signals to the
	// inbox/audit consumers. Counts of skipped tasks and
	// abandoned claims ride in this event's metadata so
	// downstream still has the totals.
	//
	// Reason capping is the service layer's job; if a too-long
	// reason slips through it'll be persisted as-is.
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "run_terminated",
		EventSubtype: current,
		RunID:        m.RunID,
		ProjectID:    projectID,
		Metadata: MarshalMetadata(map[string]any{
			"from":             current,
			"to":               "terminated",
			"reason":           m.Reason,
			"skipped_tasks":    skippedTasks,
			"abandoned_claims": abandonedClaims,
			"run_seq":          runSeq,
		}),
		CreatedAt: now,
	})
	return nil
}

func applyMarkOpenClaimsInvalidated(tx *sql.Tx, m MarkOpenClaimsInvalidated, sink EventSink) error {
	type closedClaim struct {
		claimID, citizenID, runID, projectID int64
		iterSeq                              sql.NullInt64
		commit                               sql.NullString
		taskAction                           string
	}
	var affected []closedClaim
	rows, err := tx.Query(
		`SELECT tc.id, tc.citizen_id, tc.iter_seq, COALESCE(tc.commit_sha, ''),
		    t.run_id, r.project_id, t.action
		 FROM task_claims tc
		 JOIN tasks t ON tc.task_id = t.id
		 JOIN runs r ON t.run_id = r.id
		 WHERE tc.task_id = ? AND tc.outcome IS NULL`,
		m.TaskID,
	)
	if err == nil {
		for rows.Next() {
			var c closedClaim
			if scanErr := rows.Scan(&c.claimID, &c.citizenID, &c.iterSeq, &c.commit,
				&c.runID, &c.projectID, &c.taskAction); scanErr == nil {
				affected = append(affected, c)
			}
		}
		rows.Close()
	}
	res, err := tx.Exec(
		`UPDATE task_claims SET outcome = 'invalidated' WHERE task_id = ? AND outcome IS NULL`,
		m.TaskID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sink.SkipEvents("mark_open_claims_invalidated no-op: no open claims to close")
		return nil
	}
	now := time.Now()
	for _, c := range affected {
		sink.Emit(Event{
			CitizenID:    c.citizenID,
			EventType:    "iteration_completed",
			EventSubtype: string(ClaimOutcomeInvalidated),
			TaskID:       m.TaskID,
			RunID:        c.runID,
			ProjectID:    c.projectID,
			Metadata: MarshalMetadata(map[string]any{
				"iter_seq":         c.iterSeq.Int64,
				"final_commit_sha": c.commit.String,
				"action":           c.taskAction,
				"reason":           "cascade_invalidate",
			}),
			CreatedAt: now,
		})
	}
	return nil
}

// applyMarkOpenClaimsFailed mirrors applyMarkOpenClaimsInvalidated
// but stamps outcome='failed': the attempt's compute script
// errored on its own merits. The closed row keeps its iter_seq /
// commit so the failed attempt stays an auditable iteration, and
// — because the open row is now closed — a later enju_retry_task
// re-claim computes MAX(iter_seq WHERE outcome IS NOT NULL)+1
// instead of reusing the dead attempt's seq. Idempotent.
func applyMarkOpenClaimsFailed(tx *sql.Tx, m MarkOpenClaimsFailed, sink EventSink) error {
	type closedClaim struct {
		claimID, citizenID, runID, projectID int64
		iterSeq                              sql.NullInt64
		commit                               sql.NullString
		taskAction                           string
	}
	var affected []closedClaim
	rows, err := tx.Query(
		`SELECT tc.id, tc.citizen_id, tc.iter_seq, COALESCE(tc.commit_sha, ''),
		    t.run_id, r.project_id, t.action
		 FROM task_claims tc
		 JOIN tasks t ON tc.task_id = t.id
		 JOIN runs r ON t.run_id = r.id
		 WHERE tc.task_id = ? AND tc.outcome IS NULL`,
		m.TaskID,
	)
	if err == nil {
		for rows.Next() {
			var c closedClaim
			if scanErr := rows.Scan(&c.claimID, &c.citizenID, &c.iterSeq, &c.commit,
				&c.runID, &c.projectID, &c.taskAction); scanErr == nil {
				affected = append(affected, c)
			}
		}
		rows.Close()
	}
	res, err := tx.Exec(
		`UPDATE task_claims SET outcome = 'failed' WHERE task_id = ? AND outcome IS NULL`,
		m.TaskID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sink.SkipEvents("mark_open_claims_failed no-op: no open claims to close")
		return nil
	}
	now := time.Now()
	for _, c := range affected {
		sink.Emit(Event{
			CitizenID:    c.citizenID,
			EventType:    "iteration_completed",
			EventSubtype: string(ClaimOutcomeFailed),
			TaskID:       m.TaskID,
			RunID:        c.runID,
			ProjectID:    c.projectID,
			Metadata: MarshalMetadata(map[string]any{
				"iter_seq":         c.iterSeq.Int64,
				"final_commit_sha": c.commit.String,
				"action":           c.taskAction,
				"reason":           "compute_error",
			}),
			CreatedAt: now,
		})
	}
	return nil
}

func applyMarkLatestClaimOutcome(tx *sql.Tx, m MarkLatestClaimOutcome, sink EventSink) error {
	if m.Outcome == "" {
		return fmt.Errorf("mark_latest_claim_outcome: outcome is required")
	}
	if !validRelabelOutcomes[m.Outcome] {
		return fmt.Errorf("mark_latest_claim_outcome: invalid outcome %q", m.Outcome)
	}
	var claimID, citizenID, runID, projectID int64
	var iterSeq sql.NullInt64
	var commit sql.NullString
	var taskAction string
	var citizens int
	_ = tx.QueryRow(
		`SELECT tc.id, tc.citizen_id, tc.iter_seq, COALESCE(tc.commit_sha, ''),
		    t.run_id, r.project_id, t.action, t.citizens
		 FROM task_claims tc
		 JOIN tasks t ON tc.task_id = t.id
		 JOIN runs r ON t.run_id = r.id
		 WHERE tc.task_id = ?
		 ORDER BY tc.id DESC LIMIT 1`,
		m.TaskID,
	).Scan(&claimID, &citizenID, &iterSeq, &commit, &runID, &projectID, &taskAction, &citizens)
	res, err := tx.Exec(
		`UPDATE task_claims SET outcome = ?
		 WHERE id = (
		  SELECT id FROM task_claims
		  WHERE task_id = ?
		  ORDER BY id DESC LIMIT 1
		 )`,
		m.Outcome, m.TaskID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 || claimID == 0 {
		sink.SkipEvents("mark_latest_claim_outcome no-op: no claim row found")
		return nil
	}
	now := time.Now()
	sink.Emit(Event{
		CitizenID:    citizenID,
		EventType:    "iteration_completed",
		EventSubtype: string(m.Outcome),
		TaskID:       m.TaskID,
		RunID:        runID,
		ProjectID:    projectID,
		Metadata: MarshalMetadata(map[string]any{
			"iter_seq":         iterSeq.Int64,
			"final_commit_sha": commit.String,
			"action":           taskAction,
		}),
		CreatedAt: now,
	})
	// outcome="completed" via this path is review-approve closing
	// the upstream's claim (Phase 6c). The task itself was already
	// in ACCEPTED state from the earlier submit, so applySetTaskState's
	// task_completed emission won't fire — emit it here instead.
	if m.Outcome == ClaimOutcomeCompleted {
		sink.Emit(Event{
			CitizenID:    citizenID,
			EventType:    "task_completed",
			EventSubtype: taskAction,
			TaskID:       m.TaskID,
			RunID:        runID,
			ProjectID:    projectID,
			Metadata: MarshalMetadata(map[string]any{
				"iter_seq":   iterSeq.Int64,
				"commit_sha": commit.String,
				"reviewed":   true,
			}),
			CreatedAt: now,
		})
	}
	return nil
}

func applySetProjectMemberRole(tx *sql.Tx, m SetProjectMemberRole, sink EventSink) error {
	res, err := tx.Exec(
		`UPDATE project_members SET role = ? WHERE project_id = ? AND citizen_id = ?`,
		string(m.Role), m.ProjectID, m.CitizenID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sink.SkipEvents("set_project_member_role no-op: citizen is not a member")
		return nil
	}
	sink.Emit(Event{
		CitizenID:    m.CitizenID,
		EventType:    "project_member_role_changed",
		EventSubtype: string(m.Role),
		ProjectID:    m.ProjectID,
		Metadata:     MarshalMetadata(map[string]any{"role": string(m.Role)}),
		CreatedAt:    time.Now(),
	})
	return nil
}

// applySetAutoTriageTemplate persists the run-level auto-triage
// rule. Pure state write — no event emission (the rule is run
// metadata, not a citizen-facing contribution).
func applySetAutoTriageTemplate(tx *sql.Tx, m SetAutoTriageTemplate, sink EventSink) error {
	if _, err := tx.Exec(
		`UPDATE runs SET auto_triage_template = ?, updated_at = ? WHERE id = ?`,
		m.TemplateJSON, time.Now(), m.RunID,
	); err != nil {
		return err
	}
	sink.SkipEvents("auto_triage_template is run metadata, not a contribution event")
	return nil
}
