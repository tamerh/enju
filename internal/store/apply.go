package store

import (
	"database/sql"
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
// tasks table (aside from a small set of legitimate
// escape-hatch helpers for scheduler sweeps in sqlite.go —
// UpdateReadyTasks, ExpireClaimedTask, ReleaseTask — which
// don't participate in per-feature atomicity because they
// run as separate follow-up passes). If you're about to add
// a new `store.UpdateX` helper to mutate task rows
// per-feature, stop — add a new mutation type (or extend an
// existing one, e.g. SetTaskState) so it rides the plan's
// transaction instead. See plan.go for the mutation shape.
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

	for _, mut := range plan.Mutations {
		switch m := mut.(type) {

		case SetTaskState:
			if err := applySetTaskState(tx, m, &result); err != nil {
				return result, err
			}

		case CreateTask:
			if err := applyCreateTask(tx, m); err != nil {
				return result, err
			}
			result.TasksCreated++

		case DeleteTask:
			if err := applyDeleteTask(tx, m); err != nil {
				return result, err
			}
			result.TasksDeleted++

		case CreateRun:
			id, seq, err := applyCreateRun(tx, m)
			if err != nil {
				return result, err
			}
			result.RunID = id
			result.RunSeq = seq

		case SetClaim:
			if err := applySetClaim(tx, m); err != nil {
				return result, err
			}

		case ReleaseClaim:
			if err := applyReleaseClaim(tx, m); err != nil {
				return result, err
			}

		case RecordSubmission:
			if err := applyRecordSubmission(tx, m); err != nil {
				return result, err
			}

		case MoveArtifact:
			if err := applyMoveArtifact(tx, m); err != nil {
				return result, err
			}

		case DeleteArtifact:
			if err := applyDeleteArtifact(tx, m); err != nil {
				return result, err
			}

		case CreateCitizen:
			id, err := applyCreateCitizen(tx, m)
			if err != nil {
				return result, err
			}
			result.CitizenID = id

		case UpdateReadyTasks:
			n, err := applyUpdateReadyTasks(tx, s, m)
			if err != nil {
				return result, err
			}
			result.TasksReadied += n

		case CompleteRun:
			completed, err := applyCompleteRun(tx, m)
			if err != nil {
				return result, err
			}
			result.RunCompleted = completed

		default:
			return result, fmt.Errorf("unknown mutation type: %T", mut)
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// ApplyResult carries summary data back to the caller
// after a successful plan application.
type ApplyResult struct {
	RunID        int64
	RunSeq       int
	CitizenID    int64
	TasksCreated int
	TasksDeleted int
	TasksReadied int
	RunCompleted bool
	Changed      int // generic "rows affected" counter
}

// --- Per-mutation apply functions ---

func applySetTaskState(tx *sql.Tx, m SetTaskState, result *ApplyResult) error {
	// Validate: task exists and check current state.
	var currentState string
	if err := tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, m.TaskID).Scan(&currentState); err != nil {
		return fmt.Errorf("set_task_state: task %q not found", m.TaskID)
	}

	if m.ClearClaim {
		// Invalidation-style transition. Validate preconditions:
		// - Target (→READY): must be in ACCEPTED.
		// - Descendant (→PENDING): skip if already PENDING
		//   (no-op, not an error).
		// - Descendant (→SKIPPED via fail-cascade): same clear
		//   semantics as PENDING, but terminal.
		if m.NewState == TaskReady && TaskState(currentState) != TaskAccepted && TaskState(currentState) != TaskFailed {
			return fmt.Errorf("task %q cannot be invalidated (state: %s, must be accepted or failed)", m.TaskID, currentState)
		}
		if m.NewState == TaskPending && TaskState(currentState) == TaskPending {
			// Already pending — skip silently, matching the
			// old InvalidateTask behavior.
			return nil
		}
		// Fail-cascade skip carries a reason; everything else
		// the ClearClaim path handles wipes all per-claim state.
		q := `UPDATE tasks SET state = ?, claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL, commit_sha = '', review_decision = '', vote_choice = '', fail_reason = '', skip_reason = ?`
		args := []interface{}{m.NewState, m.SkipReason}
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
		// Phase 6c — open claims are NOT implicitly marked
		// here. The cascade caller decides:
		//   - Request_changes target: claim stays open
		//     (revision-within-iteration semantics).
		//   - Manual invalidate / reject target: caller
		//     explicitly calls MarkLatestClaimOutcome with
		//     the right terminal outcome.
		//   - Cascade descendants: caller marks their open
		//     claims as 'invalidated' (their work is
		//     collateral — wasn't a verdict against them, but
		//     their iteration is gone).
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
		//   - Transition TO parked: caller sets
		//     ParkedFromState to the prior state; we stash
		//     it so restore is a lossless revert.
		//   - Transition FROM parked (restore): caller sets
		//     NewState to the stashed value and leaves
		//     ParkedFromState zero. We must explicitly clear
		//     the column — otherwise the row would look
		//     "previously parked" forever.
		//   - Other transitions: column is left alone. (A row
		//     that was never parked has empty parked_from_state;
		//     nothing to change.)
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
	}
	result.Changed++
	return nil
}

func applyCreateTask(tx *sql.Tx, m CreateTask) error {
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
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, reviews_target, vote_options, citizens, min_quorum, vote_threshold, vote_deadline, anonymize, visibility, env, mode, container, run_slug, on_review_reject, on_review_request_changes, remediation_template, closes_issue_seq, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RunID, t.Seq, t.TaskDefID, t.InstanceKey, t.InstanceParams, t.Ref, t.Action,
		t.Prompt, t.UserPrompt, t.Script, t.Outputs, t.Requirements, t.ResultType, t.Timeout,
		t.State, t.DependsOn, t.ReadsArtifacts, t.WritesArtifacts,
		t.AssignTo, t.RequireRole, t.ReviewsTarget,
		t.VoteOptions, citizens, t.MinQuorum, t.VoteThreshold, t.VoteDeadline,
		anonymize, t.Visibility, t.Env, t.Mode, t.Container, t.RunSlug,
		t.OnReviewReject, t.OnReviewRequestChanges, t.RemediationTemplate,
		t.ClosesIssueSeq,
		t.CreatedAt,
	)
	return err
}

func applyDeleteTask(tx *sql.Tx, m DeleteTask) error {
	tx.Exec(`DELETE FROM task_claims WHERE task_id = ?`, m.TaskID)
	_, err := tx.Exec(`DELETE FROM tasks WHERE id = ?`, m.TaskID)
	return err
}

func applyCreateRun(tx *sql.Tx, m CreateRun) (int64, int, error) {
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
		`INSERT INTO runs (project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, branch, slug, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ProjectID, nextSeq, r.Name, r.Ref, r.YAMLData, r.RepoURL, r.State, r.SourcePath, r.SourceCommitSHA, branch, slug, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return 0, 0, err
	}
	id, _ := result.LastInsertId()
	return id, nextSeq, nil
}

func applySetClaim(tx *sql.Tx, m SetClaim) error {
	// Read citizens count to decide single vs multi behavior;
	// pull task_def_id + run_slug for the iteration branch
	// name (living-workflow phase 6a).
	var citizens int
	var runSeq int
	var taskDefID, runSlug, taskAction, instanceKey string
	if err := tx.QueryRow(
		`SELECT t.citizens, t.task_def_id, COALESCE(t.run_slug, ''), t.action, COALESCE(t.instance_key, ''), r.seq
		 FROM tasks t JOIN runs r ON t.run_id = r.id WHERE t.id = ?`,
		m.TaskID,
	).Scan(&citizens, &taskDefID, &runSlug, &taskAction, &instanceKey, &runSeq); err != nil {
		return fmt.Errorf("set_claim: task %q not found", m.TaskID)
	}
	if citizens <= 0 {
		citizens = 1
	}

	// operator/model design — enforce "bots must have a
	// model" at apply time (SQLite CHECK can't cross-table-reference
	// to look up citizens.kind). Human operators may submit unaided.
	if err := requireModelForBot(tx, m.CitizenID, m.ModelID, "set_claim"); err != nil {
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
		`INSERT INTO task_claims (task_id, citizen_id, claimed_at, deadline, model_id, branch, iter_seq) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.TaskID, m.CitizenID, now, m.Deadline, nullableInt64(m.ModelID), branch, iterSeq,
	)
	if err != nil {
		return fmt.Errorf("set_claim: %w", err)
	}

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

func applyReleaseClaim(tx *sql.Tx, m ReleaseClaim) error {
	_, err := tx.Exec(
		`DELETE FROM task_claims WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
		m.TaskID, m.CitizenID,
	)
	if err != nil {
		return err
	}
	// Reset task state if it was claimed by this citizen.
	_, err = tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`,
		m.TaskID, m.CitizenID,
	)
	return err
}

func applyRecordSubmission(tx *sql.Tx, m RecordSubmission) error {
	now := time.Now()

	// Read task to determine single vs multi citizen + action.
	var citizens int
	var taskAction, taskDefID, instanceKey string
	var runID int64
	var claimedBy sql.NullInt64
	if err := tx.QueryRow(
		`SELECT citizens, action, task_def_id, COALESCE(instance_key, ''), run_id, claimed_by FROM tasks WHERE id = ?`,
		m.TaskID,
	).Scan(&citizens, &taskAction, &taskDefID, &instanceKey, &runID, &claimedBy); err != nil {
		return fmt.Errorf("submit: task %q not found", m.TaskID)
	}
	if citizens <= 0 {
		citizens = 1
	}

	// operator/model design — same constraint as the
	// claim path. Bots can't submit without naming a model; humans
	// can.
	if err := requireModelForBot(tx, m.CitizenID, m.ModelID, "submit"); err != nil {
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
	if claimRowID.Valid {
		if _, err := tx.Exec(
			`INSERT INTO task_submissions (claim_id, submitted_at, commit_sha, decision, option, content, model_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			claimRowID.Int64, now, m.CommitSHA, m.Decision, choice, m.Content, nullableInt64(m.ModelID),
		); err != nil {
			return fmt.Errorf("submit: record submission: %w", err)
		}
	}

	if citizens == 1 {
		// Single-citizen: one submit → ACCEPTED at the task
		// level (engine state machine, unchanged). The claim's
		// outcome is conditional on whether a downstream
		// review will weigh in.
		_, err := tx.Exec(
			`UPDATE tasks SET state = 'accepted', submitted_at = ?, result_path = ?, commit_sha = ?, review_decision = ?, vote_choice = ? WHERE id = ?`,
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
				`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, commit_sha = ?, decision = ?, model_id = COALESCE(?, model_id) WHERE id = ?`,
				now, m.VoteChoice, m.CommitSHA, m.Decision, nullableInt64(m.ModelID), claimRowID.Int64,
			); err != nil {
				return err
			}
		} else if claimRowID.Valid {
			// Stay open — but still update the
			// denormalized fields on the claim row so legacy
			// readers (which haven't been migrated to
			// task_submissions yet) see the latest attempt.
			// outcome stays NULL.
			if _, err := tx.Exec(
				`UPDATE task_claims SET submitted_at = ?, option = ?, commit_sha = ?, decision = ?, model_id = COALESCE(?, model_id) WHERE id = ?`,
				now, m.VoteChoice, m.CommitSHA, m.Decision, nullableInt64(m.ModelID), claimRowID.Int64,
			); err != nil {
				return err
			}
		}
		// Score accounting.
		if claimedBy.Valid {
			tx.Exec(
				`UPDATE citizens SET tasks_completed = tasks_completed + 1, tokens_contributed = tokens_contributed + ?, score = (tasks_completed + 1) - (tasks_timed_out * 0.5) - (tasks_rejected * 1.0), last_seen = ? WHERE id = ?`,
				m.TokensUsed, now, claimedBy.Int64,
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
				`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, content = ?, commit_sha = ?, decision = ?, model_id = COALESCE(?, model_id) WHERE id = ?`,
				now, choice, m.Content, m.CommitSHA, m.Decision, nullableInt64(m.ModelID), claimRowID.Int64,
			)
		}
		// Token contribution only (score waits for tally).
		tx.Exec(
			`UPDATE citizens SET tokens_contributed = tokens_contributed + ?, last_seen = ? WHERE id = ?`,
			m.TokensUsed, now, m.CitizenID,
		)
	}
	return nil
}

func applyMoveArtifact(tx *sql.Tx, m MoveArtifact) error {
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

func applyDeleteArtifact(tx *sql.Tx, m DeleteArtifact) error {
	branch := m.Branch
	if branch == "" {
		branch = "main"
	}
	_, err := tx.Exec(`DELETE FROM artifacts WHERE project_id = ? AND branch = ? AND path = ?`, m.ProjectID, branch, m.Path)
	return err
}

func applyCreateCitizen(tx *sql.Tx, m CreateCitizen) (int64, error) {
	c := &m.Citizen
	result, err := tx.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, tasks_completed, tasks_rejected, tasks_timed_out, tasks_released, tokens_contributed, registered_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, 0, 0, 0, 0, 0, 0, ?, ?)`,
		c.Username, c.Name, c.Email, c.Role, c.Token, c.RegisteredAt, c.LastSeen,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func applyUpdateReadyTasks(tx *sql.Tx, s *Store, m UpdateReadyTasks) (int, error) {
	// Delegate to the existing UpdateReadyTasks logic but
	// it runs outside the tx for now. TODO: inline the
	// ready-task sweep into the transaction.
	return s.UpdateReadyTasks(m.RunID)
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
func applyCompleteRun(tx *sql.Tx, m CompleteRun) (bool, error) {
	var current string
	var projectID int64
	var autoTriage string
	err := tx.QueryRow(
		`SELECT state, project_id, auto_triage_template FROM runs WHERE id = ?`, m.RunID,
	).Scan(&current, &projectID, &autoTriage)
	if err != nil {
		return false, err
	}
	// Paused / failed runs don't auto-transition.
	if current == string(RunPaused) || current == string(RunFailed) {
		return false, nil
	}

	var active, holding, total int
	err = tx.QueryRow(
		`SELECT
		   COUNT(*),
		   COUNT(CASE WHEN state IN ('ready','claimed','running','collecting') THEN 1 END),
		   COUNT(CASE WHEN state IN ('pending','parked') THEN 1 END)
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
		return false, nil
	case active > 0:
		next = RunActive
	case holding > 0:
		next = RunIdle
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
			next = RunIdle
		}
	}

	if string(next) == current {
		return next == RunCompleted, nil
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE runs SET state = ?, updated_at = ? WHERE id = ?`, next, now, m.RunID); err != nil {
		return false, err
	}
	// Emit a lifecycle event in the same transaction so the
	// event log is consistent with the run-state UPDATE.
	// citizen 0 = system (not initiated by a specific actor —
	// these transitions fall out of task-graph state).
	// projectID was already loaded above for the auto-triage
	// override; reuse it.
	if _, err := tx.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (0, ?, ?, '', ?, ?, ?, ?)`,
		"run_"+string(next), current, m.RunID, projectID,
		fmt.Sprintf(`{"from":%q,"to":%q}`, current, next), now,
	); err != nil {
		return false, err
	}
	return next == RunCompleted, nil
}

// requireModelForBot enforces the operator/model rule from
// docs/operator-model-design.md: a bot operator must always name
// the model that produced the words for its action; a human
// operator may submit unaided. Used by both applySetClaim and
// applyRecordSubmission so the constraint kicks in whether the
// bot lies about its model at claim time or at submit time.
//
// Implemented in Go (not SQL CHECK) because SQLite CHECK can't
// reference another table, and we need to read citizens.kind to
// decide. The query is small (one row by primary key) and runs
// inside the apply transaction, so consistency is preserved.
func requireModelForBot(tx *sql.Tx, operatorID int64, modelID *int64, op string) error {
	if modelID != nil {
		return nil // model named — fine for any operator kind
	}
	var kind string
	if err := tx.QueryRow(`SELECT kind FROM citizens WHERE id = ?`, operatorID).Scan(&kind); err != nil {
		return fmt.Errorf("%s: read operator kind: %w", op, err)
	}
	if kind == "bot" {
		return fmt.Errorf("%s: operator citizen %d is a bot — model_id is required (bots cannot act without naming a model)", op, operatorID)
	}
	return nil
}

// nullableInt64 converts a *int64 to a value suitable for SQL
// nullable INTEGER columns. Passing a typed nil through database/
// sql works for some drivers but is brittle; sql.NullInt64 with
// Valid=false is the portable form. Callers use this for
// task_claims.model_id and other nullable FKs.
func nullableInt64(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
