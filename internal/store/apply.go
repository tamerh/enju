package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ApplyPlan validates and applies a Plan's mutations inside
// a single transaction. If any mutation fails validation,
// the entire plan is rolled back — no partial commits.
//
// This is the coordinator's ONE write path. Every feature
// (claim, submit, invalidate, tally, run creation, dynamic
// materialization) goes through the same function. The
// engine produces the plan; ApplyPlan is the executor.
func (s *Store) ApplyPlan(plan Plan) (ApplyResult, error) {
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
		if m.NewState == TaskReady && TaskState(currentState) != TaskAccepted && TaskState(currentState) != TaskFailed {
			return fmt.Errorf("task %q cannot be invalidated (state: %s, must be accepted or failed)", m.TaskID, currentState)
		}
		if m.NewState == TaskPending && TaskState(currentState) == TaskPending {
			// Already pending — skip silently, matching the
			// old InvalidateTask behavior.
			return nil
		}
		_, err := tx.Exec(
			`UPDATE tasks SET state = ?, claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL, commit_sha = '', review_decision = '', vote_choice = '', fail_reason = '' WHERE id = ?`,
			m.NewState, m.TaskID,
		)
		if err != nil {
			return fmt.Errorf("set_task_state (clear): %w", err)
		}
		// Mark open claims as invalidated.
		tx.Exec(`UPDATE task_claims SET outcome = 'invalidated' WHERE task_id = ? AND outcome IS NULL`, m.TaskID)
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
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, reviews_target, vote_options, citizens, min_quorum, vote_threshold, vote_deadline, anonymize, visibility, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RunID, t.Seq, t.TaskDefID, t.InstanceKey, t.InstanceParams, t.Ref, t.Action,
		t.Prompt, t.UserPrompt, t.Script, t.Outputs, t.Requirements, t.ResultType, t.Timeout,
		t.State, t.DependsOn, t.ReadsArtifacts, t.WritesArtifacts,
		t.AssignTo, t.RequireRole, t.ReviewsTarget,
		t.VoteOptions, citizens, t.MinQuorum, t.VoteThreshold, t.VoteDeadline,
		anonymize, t.Visibility,
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
	result, err := tx.Exec(
		`INSERT INTO runs (project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ProjectID, nextSeq, r.Name, r.Ref, r.YAMLData, r.RepoURL, r.State, r.SourcePath, r.SourceCommitSHA, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return 0, 0, err
	}
	id, _ := result.LastInsertId()
	return id, nextSeq, nil
}

func applySetClaim(tx *sql.Tx, m SetClaim) error {
	// Read citizens count to decide single vs multi behavior.
	var citizens int
	if err := tx.QueryRow(`SELECT citizens FROM tasks WHERE id = ?`, m.TaskID).Scan(&citizens); err != nil {
		return fmt.Errorf("set_claim: task %q not found", m.TaskID)
	}
	if citizens <= 0 {
		citizens = 1
	}

	now := time.Now()
	// Insert the claim row.
	_, err := tx.Exec(
		`INSERT INTO task_claims (task_id, citizen_id, claimed_at, deadline) VALUES (?, ?, ?, ?)`,
		m.TaskID, m.CitizenID, now, m.Deadline,
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

	// Read task to determine single vs multi citizen.
	var citizens int
	var claimedBy sql.NullInt64
	if err := tx.QueryRow(`SELECT citizens, claimed_by FROM tasks WHERE id = ?`, m.TaskID).Scan(&citizens, &claimedBy); err != nil {
		return fmt.Errorf("submit: task %q not found", m.TaskID)
	}
	if citizens <= 0 {
		citizens = 1
	}

	if citizens == 1 {
		// Single-citizen: one submit → ACCEPTED.
		_, err := tx.Exec(
			`UPDATE tasks SET state = 'accepted', submitted_at = ?, result_path = ?, commit_sha = ?, review_decision = ?, vote_choice = ? WHERE id = ?`,
			now, m.ResultPath, m.CommitSHA, m.Decision, m.VoteChoice, m.TaskID,
		)
		if err != nil {
			return err
		}
		_, err = tx.Exec(
			`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ? WHERE task_id = ? AND outcome IS NULL`,
			now, m.VoteChoice, m.TaskID,
		)
		if err != nil {
			return err
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
		_, err := tx.Exec(
			`UPDATE tasks SET state = 'collecting', result_path = ? WHERE id = ?`,
			m.ResultPath, m.TaskID,
		)
		if err != nil {
			return err
		}
		// Find the citizen's open claim row.
		var claimRowID sql.NullInt64
		tx.QueryRow(
			`SELECT id FROM task_claims WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
			m.TaskID, m.CitizenID,
		).Scan(&claimRowID)

		choice := m.VoteChoice
		if choice == "" {
			choice = m.Decision
		}
		if claimRowID.Valid {
			tx.Exec(
				`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, content = ? WHERE id = ?`,
				now, choice, m.Content, claimRowID.Int64,
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
	_, err := tx.Exec(
		`INSERT INTO artifacts (project_id, path, last_writer, last_task_id, last_run_id, commit_sha, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, path) DO UPDATE SET last_writer=?, last_task_id=?, last_run_id=?, commit_sha=?, updated_at=?`,
		a.ProjectID, a.Path, a.LastWriter, a.LastTaskID, a.LastRunID, a.CommitSHA, a.CreatedAt, a.UpdatedAt,
		a.LastWriter, a.LastTaskID, a.LastRunID, a.CommitSHA, a.UpdatedAt,
	)
	return err
}

func applyDeleteArtifact(tx *sql.Tx, m DeleteArtifact) error {
	_, err := tx.Exec(`DELETE FROM artifacts WHERE project_id = ? AND path = ?`, m.ProjectID, m.Path)
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

func applyCompleteRun(tx *sql.Tx, m CompleteRun) (bool, error) {
	// Check if all tasks in the run are terminal.
	var pending int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE run_id = ? AND state NOT IN ('accepted', 'skipped', 'failed')`,
		m.RunID,
	).Scan(&pending)
	if err != nil {
		return false, err
	}
	if pending > 0 {
		return false, nil
	}
	_, err = tx.Exec(`UPDATE runs SET state = 'completed', updated_at = ? WHERE id = ? AND state = 'active'`, time.Now(), m.RunID)
	return err == nil, err
}
