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
	CREATE TABLE IF NOT EXISTS projects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		ref TEXT,
		yaml_data TEXT,
		repo_url TEXT,
		state TEXT NOT NULL DEFAULT 'active',
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		seq INTEGER NOT NULL DEFAULT 0,
		task_def_id TEXT NOT NULL,
		instance_key TEXT NOT NULL DEFAULT '',
		ref TEXT,
		action TEXT NOT NULL DEFAULT 'answer',
		prompt TEXT,
		user_prompt TEXT,
		script TEXT,
		outputs TEXT,
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

	CREATE TABLE IF NOT EXISTS citizens (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		email TEXT,
		role TEXT NOT NULL DEFAULT 'citizen',
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
		citizen_id TEXT NOT NULL REFERENCES citizens(id),
		claimed_at TIMESTAMP NOT NULL,
		deadline TIMESTAMP NOT NULL,
		outcome TEXT,
		submitted_at TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);
	CREATE INDEX IF NOT EXISTS idx_tasks_claimed_by ON tasks(claimed_by);
	CREATE INDEX IF NOT EXISTS idx_task_claims_task ON task_claims(task_id);
	CREATE INDEX IF NOT EXISTS idx_citizens_token ON citizens(token);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_citizens_email ON citizens(email) WHERE email IS NOT NULL AND email != '';
	`
	_, err := s.db.Exec(schema)
	return err
}

// --- Projects ---

func (s *Store) CreateProject(p *ProjectRecord) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO projects (name, ref, yaml_data, repo_url, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Ref, p.YAMLData, p.RepoURL, p.State, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) GetProject(id int64) (*ProjectRecord, error) {
	var p ProjectRecord
	var ref sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, ref, yaml_data, repo_url, state, created_at, updated_at FROM projects WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Ref = ref.String
	return &p, err
}

func (s *Store) ListProjects() ([]ProjectRecord, error) {
	rows, err := s.db.Query(`SELECT id, name, ref, yaml_data, repo_url, state, created_at, updated_at FROM projects ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []ProjectRecord
	for rows.Next() {
		var p ProjectRecord
		var ref sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Ref = ref.String
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) UpdateProjectState(id int64, state ProjectState) error {
	_, err := s.db.Exec(
		`UPDATE projects SET state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now(), id,
	)
	return err
}

func (s *Store) CheckAndCompleteProject(projectID int64) (bool, error) {
	var total, accepted int
	err := s.db.QueryRow(
		`SELECT COUNT(*), COUNT(CASE WHEN state = 'accepted' THEN 1 END) FROM tasks WHERE project_id = ?`,
		projectID,
	).Scan(&total, &accepted)
	if err != nil {
		return false, err
	}
	if total > 0 && total == accepted {
		err = s.UpdateProjectState(projectID, ProjectCompleted)
		return err == nil, err
	}
	return false, nil
}

// --- Tasks ---

const taskColumns = `id, project_id, seq, task_def_id, instance_key, ref, action, prompt, user_prompt, script, outputs, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, depends_on, created_at`

func (s *Store) CreateTask(t *TaskRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO tasks (id, project_id, seq, task_def_id, instance_key, ref, action, prompt, user_prompt, script, outputs, result_type, timeout, state, depends_on, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Seq, t.TaskDefID, t.InstanceKey, t.Ref, t.Action,
		t.Prompt, t.UserPrompt, t.Script, t.Outputs, t.ResultType, t.Timeout,
		t.State, t.DependsOn, t.CreatedAt,
	)
	return err
}

func (s *Store) GetTask(id string) (*TaskRecord, error) {
	var t TaskRecord
	var claimedAt, submittedAt sql.NullTime
	var claimedBy, resultPath, prompt, userPrompt, script, outputs, timeout, ref sql.NullString
	err := s.db.QueryRow(
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.ProjectID, &t.Seq, &t.TaskDefID, &t.InstanceKey, &ref, &t.Action,
		&prompt, &userPrompt, &script, &outputs, &t.ResultType, &timeout,
		&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.DependsOn, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Ref = ref.String
	t.Prompt = prompt.String
	t.UserPrompt = userPrompt.String
	t.Script = script.String
	t.Outputs = outputs.String
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

// GetTaskBySeq finds a task by project ID and sequence number.
func (s *Store) GetTaskBySeq(projectID string, seq int) (*TaskRecord, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM tasks WHERE project_id = ? AND seq = ?`, projectID, seq).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetTask(id)
}

func (s *Store) ListTasksByProject(projectID int64) ([]TaskRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+taskColumns+` FROM tasks WHERE project_id = ? ORDER BY created_at`, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) ListReadyTasks(projectID int64) ([]TaskRecord, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE state = 'ready'`
	args := []interface{}{}
	if projectID > 0 {
		query += " AND project_id = ?"
		args = append(args, projectID)
	}
	query += " ORDER BY created_at"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) ClaimTask(taskID, citizenID string, deadline time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&state)
	if err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if TaskState(state) != TaskReady {
		return fmt.Errorf("task %q is not ready (state: %s)", taskID, state)
	}

	now := time.Now()

	_, err = tx.Exec(
		`UPDATE tasks SET state = 'claimed', claimed_by = ?, claimed_at = ? WHERE id = ?`,
		citizenID, now, taskID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`INSERT INTO task_claims (task_id, citizen_id, claimed_at, deadline) VALUES (?, ?, ?, ?)`,
		taskID, citizenID, now, deadline,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) SubmitTaskResult(taskID, resultPath string, tokensUsed int64) error {
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var state, claimedBy string
	err = tx.QueryRow(`SELECT state, COALESCE(claimed_by, '') FROM tasks WHERE id = ?`, taskID).Scan(&state, &claimedBy)
	if err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if TaskState(state) != TaskClaimed && TaskState(state) != TaskRunning {
		return fmt.Errorf("task %q cannot accept result (state: %s)", taskID, state)
	}

	_, err = tx.Exec(
		`UPDATE tasks SET state = 'accepted', submitted_at = ?, result_path = ? WHERE id = ?`,
		now, resultPath, taskID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`UPDATE task_claims SET outcome = 'completed', submitted_at = ? WHERE task_id = ? AND outcome IS NULL`,
		now, taskID,
	)
	if err != nil {
		return err
	}

	if claimedBy != "" {
		_, err = tx.Exec(
			`UPDATE citizens SET
				tasks_completed = tasks_completed + 1,
				tokens_contributed = tokens_contributed + ?,
				score = (tasks_completed + 1) - (tasks_timed_out * 0.5) - (tasks_rejected * 1.0),
				last_seen = ?
			WHERE id = ?`,
			tokensUsed, now, claimedBy,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) ReleaseTask(taskID, citizenID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`,
		taskID, citizenID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`UPDATE task_claims SET outcome = 'released' WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
		taskID, citizenID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) InvalidateTask(taskID string, descendantIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`UPDATE tasks SET state = 'invalid' WHERE id = ?`, taskID)
	if err != nil {
		return err
	}

	for _, descID := range descendantIDs {
		_, err = tx.Exec(`UPDATE tasks SET state = 'invalidated' WHERE id = ?`, descID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) UpdateReadyTasks(projectID int64) (int, error) {
	rows, err := s.db.Query(
		`SELECT id, depends_on FROM tasks WHERE project_id = ? AND state = 'pending'`, projectID,
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

	acceptedRows, err := s.db.Query(
		`SELECT id FROM tasks WHERE project_id = ? AND state = 'accepted'`, projectID,
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

	count := 0
	for _, pt := range pending {
		if pt.dependsOn == "" {
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

func (s *Store) GetExpiredClaims() ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT tc.id, tc.task_id, tc.citizen_id, tc.claimed_at, tc.deadline
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
		if err := rows.Scan(&c.ID, &c.TaskID, &c.CitizenID, &c.ClaimedAt, &c.Deadline); err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

func (s *Store) ExpireClaimedTask(taskID, citizenID string) error {
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
		`UPDATE task_claims SET outcome = 'timed_out' WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
		taskID, citizenID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`UPDATE citizens SET
			tasks_timed_out = tasks_timed_out + 1,
			score = tasks_completed - ((tasks_timed_out + 1) * 0.5) - (tasks_rejected * 1.0)
		WHERE id = ?`,
		citizenID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// --- Citizens ---

func (s *Store) CreateCitizen(p *CitizenRecord) error {
	// Check email uniqueness if provided
	if p.Email != "" {
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM citizens WHERE email = ?`, p.Email).Scan(&count)
		if count > 0 {
			return fmt.Errorf("a citizen with this email already exists")
		}
	}

	role := p.Role
	if role == "" {
		role = "citizen"
	}

	_, err := s.db.Exec(
		`INSERT INTO citizens (id, name, email, role, token, score, registered_at, last_seen) VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		p.ID, p.Name, p.Email, role, p.Token, p.RegisteredAt, p.LastSeen,
	)
	return err
}

// UpdateCitizenProfile updates a citizen's name and email.
func (s *Store) UpdateCitizenProfile(id, name, email string) error {
	if email != "" {
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM citizens WHERE email = ? AND id != ?`, email, id).Scan(&count)
		if count > 0 {
			return fmt.Errorf("a citizen with this email already exists")
		}
	}

	_, err := s.db.Exec(
		`UPDATE citizens SET name = ?, email = ? WHERE id = ?`,
		name, email, id,
	)
	return err
}

func (s *Store) GetCitizenByToken(token string) (*CitizenRecord, error) {
	var p CitizenRecord
	var email, role sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, email, role, token, score, tasks_completed, tasks_rejected, tasks_timed_out, tasks_released, tokens_contributed, registered_at, last_seen
		 FROM citizens WHERE token = ?`, token,
	).Scan(&p.ID, &p.Name, &email, &role, &p.Token, &p.Score, &p.TasksCompleted, &p.TasksRejected,
		&p.TasksTimedOut, &p.TasksReleased, &p.TokensContrib, &p.RegisteredAt, &p.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Email = email.String
	p.Role = role.String
	return &p, err
}

func (s *Store) TouchCitizen(id string) error {
	_, err := s.db.Exec(`UPDATE citizens SET last_seen = ? WHERE id = ?`, time.Now(), id)
	return err
}

// GetCitizen retrieves a citizen by ID.
func (s *Store) GetCitizen(id string) (*CitizenRecord, error) {
	var p CitizenRecord
	var email, role sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, email, role, token, score, tasks_completed, tasks_rejected, tasks_timed_out, tasks_released, tokens_contributed, registered_at, last_seen
		 FROM citizens WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &email, &role, &p.Token, &p.Score, &p.TasksCompleted, &p.TasksRejected,
		&p.TasksTimedOut, &p.TasksReleased, &p.TokensContrib, &p.RegisteredAt, &p.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Email = email.String
	p.Role = role.String
	return &p, err
}

// ListCitizenActiveTasks returns tasks currently claimed by a citizen.
func (s *Store) ListCitizenActiveTasks(citizenID string) ([]TaskRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+taskColumns+` FROM tasks WHERE claimed_by = ? AND state IN ('claimed', 'running') ORDER BY claimed_at`, citizenID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ListCitizenCompletedTasks returns tasks completed by a citizen (most recent first).
func (s *Store) ListCitizenCompletedTasks(citizenID string, limit int) ([]TaskRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+taskColumns+` FROM tasks WHERE claimed_by = ? AND state = 'accepted' ORDER BY submitted_at DESC LIMIT ?`, citizenID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// --- Helpers ---

func scanTasks(rows *sql.Rows) ([]TaskRecord, error) {
	var tasks []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var claimedAt, submittedAt sql.NullTime
		var claimedBy, resultPath, prompt, userPrompt, script, outputs, timeout, ref sql.NullString
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Seq, &t.TaskDefID, &t.InstanceKey, &ref, &t.Action,
			&prompt, &userPrompt, &script, &outputs, &t.ResultType, &timeout,
			&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.DependsOn, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Ref = ref.String
		t.Prompt = prompt.String
		t.UserPrompt = userPrompt.String
		t.Script = script.String
		t.Outputs = outputs.String
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
