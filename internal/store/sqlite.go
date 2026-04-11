package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed state store.
type Store struct {
	db *sql.DB
}

// New creates a new Store and initializes the schema.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Enable WAL mode for better concurrent read performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migration: %w", err)
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS problems (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		yaml_data TEXT,
		repo_url TEXT,
		state TEXT NOT NULL DEFAULT 'active',
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		problem_id TEXT NOT NULL REFERENCES problems(id),
		task_def_id TEXT NOT NULL,
		instance_key TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT 'autonomous',
		prompt TEXT,
		user_prompt TEXT,
		script TEXT,
		result_type TEXT NOT NULL DEFAULT 'text',
		timeout TEXT,
		state TEXT NOT NULL DEFAULT 'pending',
		claimed_by TEXT,
		claimed_at TIMESTAMP,
		submitted_at TIMESTAMP,
		result_path TEXT,
		depends_on TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS participants (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		token TEXT UNIQUE NOT NULL,
		score REAL NOT NULL DEFAULT 0,
		tasks_completed INTEGER NOT NULL DEFAULT 0,
		tasks_rejected INTEGER NOT NULL DEFAULT 0,
		tasks_timed_out INTEGER NOT NULL DEFAULT 0,
		tasks_released INTEGER NOT NULL DEFAULT 0,
		tokens_contributed INTEGER NOT NULL DEFAULT 0,
		registered_at TIMESTAMP NOT NULL,
		last_seen TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS task_claims (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL REFERENCES tasks(id),
		participant_id TEXT NOT NULL REFERENCES participants(id),
		claimed_at TIMESTAMP NOT NULL,
		deadline TIMESTAMP NOT NULL,
		outcome TEXT,
		submitted_at TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_problem ON tasks(problem_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);
	CREATE INDEX IF NOT EXISTS idx_tasks_claimed_by ON tasks(claimed_by);
	CREATE INDEX IF NOT EXISTS idx_task_claims_task ON task_claims(task_id);
	CREATE INDEX IF NOT EXISTS idx_participants_token ON participants(token);
	`
	_, err := s.db.Exec(schema)
	return err
}

// --- Problems ---

// CreateProblem inserts a new problem.
func (s *Store) CreateProblem(p *ProblemRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO problems (id, name, yaml_data, repo_url, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.YAMLData, p.RepoURL, p.State, p.CreatedAt, p.UpdatedAt,
	)
	return err
}

// GetProblem retrieves a problem by ID.
func (s *Store) GetProblem(id string) (*ProblemRecord, error) {
	var p ProblemRecord
	err := s.db.QueryRow(
		`SELECT id, name, yaml_data, repo_url, state, created_at, updated_at FROM problems WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &p.YAMLData, &p.RepoURL, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// ListProblems returns all problems.
func (s *Store) ListProblems() ([]ProblemRecord, error) {
	rows, err := s.db.Query(`SELECT id, name, yaml_data, repo_url, state, created_at, updated_at FROM problems ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var problems []ProblemRecord
	for rows.Next() {
		var p ProblemRecord
		if err := rows.Scan(&p.ID, &p.Name, &p.YAMLData, &p.RepoURL, &p.State, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		problems = append(problems, p)
	}
	return problems, rows.Err()
}

// UpdateProblemState updates the state of a problem.
func (s *Store) UpdateProblemState(id string, state ProblemState) error {
	_, err := s.db.Exec(
		`UPDATE problems SET state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now(), id,
	)
	return err
}

// --- Tasks ---

// CreateTask inserts a new task.
func (s *Store) CreateTask(t *TaskRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO tasks (id, problem_id, task_def_id, instance_key, type, mode, prompt, user_prompt, script, result_type, timeout, state, depends_on, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProblemID, t.TaskDefID, t.InstanceKey, t.Type, t.Mode,
		t.Prompt, t.UserPrompt, t.Script, t.ResultType, t.Timeout,
		t.State, t.DependsOn, t.CreatedAt,
	)
	return err
}

// GetTask retrieves a task by ID.
func (s *Store) GetTask(id string) (*TaskRecord, error) {
	var t TaskRecord
	var claimedAt, submittedAt sql.NullTime
	var claimedBy, resultPath, prompt, userPrompt, script, timeout sql.NullString
	err := s.db.QueryRow(
		`SELECT id, problem_id, task_def_id, instance_key, type, mode, prompt, user_prompt, script, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, depends_on, created_at
		 FROM tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.ProblemID, &t.TaskDefID, &t.InstanceKey, &t.Type, &t.Mode,
		&prompt, &userPrompt, &script, &t.ResultType, &timeout,
		&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.DependsOn, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Prompt = prompt.String
	t.UserPrompt = userPrompt.String
	t.Script = script.String
	t.Timeout = timeout.String
	t.ClaimedBy = claimedBy.String
	t.ResultPath = resultPath.String
	if claimedAt.Valid {
		t.ClaimedAt = &claimedAt.Time
	}
	if submittedAt.Valid {
		t.SubmittedAt = &submittedAt.Time
	}
	return &t, nil
}

// ListTasksByProblem returns all tasks for a problem.
func (s *Store) ListTasksByProblem(problemID string) ([]TaskRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, problem_id, task_def_id, instance_key, type, mode, prompt, user_prompt, script, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, depends_on, created_at
		 FROM tasks WHERE problem_id = ? ORDER BY created_at`, problemID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ListReadyTasks returns all tasks in READY state, optionally filtered by problem.
func (s *Store) ListReadyTasks(problemID string) ([]TaskRecord, error) {
	query := `SELECT id, problem_id, task_def_id, instance_key, type, mode, prompt, user_prompt, script, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, depends_on, created_at
		 FROM tasks WHERE state = 'ready'`
	args := []interface{}{}
	if problemID != "" {
		query += " AND problem_id = ?"
		args = append(args, problemID)
	}
	query += " ORDER BY created_at"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ClaimTask atomically claims a task for a participant.
func (s *Store) ClaimTask(taskID, participantID string, deadline time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Check state is READY
	var state string
	err = tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&state)
	if err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if TaskState(state) != TaskReady {
		return fmt.Errorf("task %q is not ready (state: %s)", taskID, state)
	}

	now := time.Now()

	// Update task state
	_, err = tx.Exec(
		`UPDATE tasks SET state = 'claimed', claimed_by = ?, claimed_at = ? WHERE id = ?`,
		participantID, now, taskID,
	)
	if err != nil {
		return err
	}

	// Record claim
	_, err = tx.Exec(
		`INSERT INTO task_claims (task_id, participant_id, claimed_at, deadline) VALUES (?, ?, ?, ?)`,
		taskID, participantID, now, deadline,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// SubmitTaskResult records a task result and marks it as accepted.
func (s *Store) SubmitTaskResult(taskID, resultPath string, tokensUsed int64) error {
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Verify task is claimed or running
	var state, claimedBy string
	err = tx.QueryRow(`SELECT state, COALESCE(claimed_by, '') FROM tasks WHERE id = ?`, taskID).Scan(&state, &claimedBy)
	if err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if TaskState(state) != TaskClaimed && TaskState(state) != TaskRunning {
		return fmt.Errorf("task %q cannot accept result (state: %s)", taskID, state)
	}

	// Update task
	_, err = tx.Exec(
		`UPDATE tasks SET state = 'accepted', submitted_at = ?, result_path = ? WHERE id = ?`,
		now, resultPath, taskID,
	)
	if err != nil {
		return err
	}

	// Update claim record
	_, err = tx.Exec(
		`UPDATE task_claims SET outcome = 'completed', submitted_at = ? WHERE task_id = ? AND outcome IS NULL`,
		now, taskID,
	)
	if err != nil {
		return err
	}

	// Update participant stats
	if claimedBy != "" {
		_, err = tx.Exec(
			`UPDATE participants SET tasks_completed = tasks_completed + 1, tokens_contributed = tokens_contributed + ?, last_seen = ? WHERE id = ?`,
			tokensUsed, now, claimedBy,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ReleaseTask releases a claimed task back to READY.
func (s *Store) ReleaseTask(taskID, participantID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`,
		taskID, participantID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`UPDATE task_claims SET outcome = 'released' WHERE task_id = ? AND participant_id = ? AND outcome IS NULL`,
		taskID, participantID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// InvalidateTask marks a task and all its descendants as invalidated.
func (s *Store) InvalidateTask(taskID string, descendantIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Mark the source task as invalid
	_, err = tx.Exec(`UPDATE tasks SET state = 'invalid' WHERE id = ?`, taskID)
	if err != nil {
		return err
	}

	// Mark descendants as invalidated
	for _, descID := range descendantIDs {
		_, err = tx.Exec(`UPDATE tasks SET state = 'invalidated' WHERE id = ?`, descID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// UpdateReadyTasks checks all pending tasks and marks them READY if all dependencies are accepted.
func (s *Store) UpdateReadyTasks(problemID string) (int, error) {
	// Get all pending tasks for this problem
	rows, err := s.db.Query(
		`SELECT id, depends_on FROM tasks WHERE problem_id = ? AND state = 'pending'`, problemID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type pendingTask struct {
		id        string
		dependsOn string
	}
	var pending []pendingTask
	for rows.Next() {
		var pt pendingTask
		if err := rows.Scan(&pt.id, &pt.dependsOn); err != nil {
			return 0, err
		}
		pending = append(pending, pt)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Get all accepted task IDs for this problem
	acceptedRows, err := s.db.Query(
		`SELECT id FROM tasks WHERE problem_id = ? AND state = 'accepted'`, problemID,
	)
	if err != nil {
		return 0, err
	}
	defer acceptedRows.Close()

	accepted := make(map[string]bool)
	for acceptedRows.Next() {
		var id string
		if err := acceptedRows.Scan(&id); err != nil {
			return 0, err
		}
		accepted[id] = true
	}

	// Check which pending tasks have all dependencies satisfied
	count := 0
	for _, pt := range pending {
		if pt.dependsOn == "" {
			// No dependencies — should be ready
			_, err := s.db.Exec(`UPDATE tasks SET state = 'ready' WHERE id = ?`, pt.id)
			if err != nil {
				return count, err
			}
			count++
			continue
		}

		deps := strings.Split(pt.dependsOn, ",")
		allDone := true
		for _, dep := range deps {
			if !accepted[strings.TrimSpace(dep)] {
				allDone = false
				break
			}
		}
		if allDone {
			_, err := s.db.Exec(`UPDATE tasks SET state = 'ready' WHERE id = ?`, pt.id)
			if err != nil {
				return count, err
			}
			count++
		}
	}

	return count, nil
}

// GetExpiredClaims finds tasks that are claimed but past their deadline.
func (s *Store) GetExpiredClaims() ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT tc.id, tc.task_id, tc.participant_id, tc.claimed_at, tc.deadline
		 FROM task_claims tc
		 JOIN tasks t ON t.id = tc.task_id
		 WHERE tc.outcome IS NULL AND tc.deadline < ? AND t.state IN ('claimed', 'running')`,
		time.Now(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claims []TaskClaimRecord
	for rows.Next() {
		var c TaskClaimRecord
		if err := rows.Scan(&c.ID, &c.TaskID, &c.ParticipantID, &c.ClaimedAt, &c.Deadline); err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// ExpireClaimedTask resets a timed-out task back to READY.
func (s *Store) ExpireClaimedTask(taskID, participantID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL WHERE id = ?`,
		taskID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`UPDATE task_claims SET outcome = 'timed_out' WHERE task_id = ? AND participant_id = ? AND outcome IS NULL`,
		taskID, participantID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`UPDATE participants SET tasks_timed_out = tasks_timed_out + 1 WHERE id = ?`,
		participantID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// --- Participants ---

// CreateParticipant registers a new participant.
func (s *Store) CreateParticipant(p *ParticipantRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO participants (id, name, token, score, registered_at, last_seen) VALUES (?, ?, ?, 0, ?, ?)`,
		p.ID, p.Name, p.Token, p.RegisteredAt, p.LastSeen,
	)
	return err
}

// GetParticipantByToken retrieves a participant by their auth token.
func (s *Store) GetParticipantByToken(token string) (*ParticipantRecord, error) {
	var p ParticipantRecord
	err := s.db.QueryRow(
		`SELECT id, name, token, score, tasks_completed, tasks_rejected, tasks_timed_out, tasks_released, tokens_contributed, registered_at, last_seen
		 FROM participants WHERE token = ?`, token,
	).Scan(&p.ID, &p.Name, &p.Token, &p.Score, &p.TasksCompleted, &p.TasksRejected,
		&p.TasksTimedOut, &p.TasksReleased, &p.TokensContrib, &p.RegisteredAt, &p.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// TouchParticipant updates the last_seen timestamp.
func (s *Store) TouchParticipant(id string) error {
	_, err := s.db.Exec(`UPDATE participants SET last_seen = ? WHERE id = ?`, time.Now(), id)
	return err
}

// --- Helpers ---

func scanTasks(rows *sql.Rows) ([]TaskRecord, error) {
	var tasks []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var claimedAt, submittedAt sql.NullTime
		var claimedBy, resultPath, prompt, userPrompt, script, timeout sql.NullString
		if err := rows.Scan(&t.ID, &t.ProblemID, &t.TaskDefID, &t.InstanceKey, &t.Type, &t.Mode,
			&prompt, &userPrompt, &script, &t.ResultType, &timeout,
			&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.DependsOn, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Prompt = prompt.String
		t.UserPrompt = userPrompt.String
		t.Script = script.String
		t.Timeout = timeout.String
		t.ClaimedBy = claimedBy.String
		t.ResultPath = resultPath.String
		if claimedAt.Valid {
			t.ClaimedAt = &claimedAt.Time
		}
		if submittedAt.Valid {
			t.SubmittedAt = &submittedAt.Time
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
