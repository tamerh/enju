package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// usernameRe matches the GitHub username rules:
//   - 1 to 39 characters
//   - Starts and ends with alphanumeric
//   - Middle can contain alphanumerics and hyphens
//
// Picking the same shape as GitHub means that when we add GitHub OAuth
// later, existing usernames don't need translation.
var usernameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?$`)

// ValidateUsername returns nil if the string is a valid username, or a
// descriptive error otherwise. Public so the API layer can reject bad
// input before hitting the DB.
func ValidateUsername(u string) error {
	if u == "" {
		return fmt.Errorf("username is required")
	}
	if len(u) > 39 {
		return fmt.Errorf("username must be at most 39 characters")
	}
	if !usernameRe.MatchString(u) {
		return fmt.Errorf("username must be lowercase alphanumerics and hyphens, not starting or ending with a hyphen")
	}
	return nil
}

// SlugifyName generates a username candidate from a display name. It
// lowercases, replaces whitespace with hyphens, strips everything else,
// collapses consecutive hyphens, and trims leading/trailing hyphens.
// Returns an empty string if nothing usable remains (caller should fall
// back to a generic default like "user").
func SlugifyName(name string) string {
	var b strings.Builder
	lastWasHyphen := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastWasHyphen = false
		case r == ' ' || r == '_' || r == '-' || r == '.' || r == '\t':
			if b.Len() > 0 && !lastWasHyphen {
				b.WriteByte('-')
				lastWasHyphen = true
			}
		}
	}
	s := b.String()
	s = strings.TrimSuffix(s, "-")
	if len(s) > 39 {
		s = strings.TrimSuffix(s[:39], "-")
	}
	return s
}

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
		name TEXT NOT NULL UNIQUE,
		description TEXT,
		created_by TEXT,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		seq INTEGER NOT NULL,
		name TEXT NOT NULL,
		ref TEXT,
		yaml_data TEXT,
		repo_url TEXT,
		state TEXT NOT NULL DEFAULT 'active',
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		UNIQUE(project_id, seq)
	);

	CREATE TABLE IF NOT EXISTS citizens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
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

	CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		run_id INTEGER NOT NULL REFERENCES runs(id),
		seq INTEGER NOT NULL DEFAULT 0,
		task_def_id TEXT NOT NULL,
		instance_key TEXT NOT NULL DEFAULT '',
		ref TEXT,
		action TEXT NOT NULL DEFAULT 'answer',
		prompt TEXT,
		user_prompt TEXT,
		script TEXT,
		outputs TEXT,
		requirements TEXT,
		result_type TEXT NOT NULL DEFAULT 'text',
		timeout TEXT,
		state TEXT NOT NULL DEFAULT 'pending',
		claimed_by INTEGER REFERENCES citizens(id),
		claimed_at TIMESTAMP,
		submitted_at TIMESTAMP,
		result_path TEXT,
		depends_on TEXT NOT NULL DEFAULT '',
		reads_artifacts TEXT NOT NULL DEFAULT '',
		writes_artifacts TEXT NOT NULL DEFAULT '',
		assign_to TEXT NOT NULL DEFAULT '',
		require_role TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS artifacts (
		project_id INTEGER NOT NULL REFERENCES projects(id),
		path TEXT NOT NULL,
		last_writer INTEGER REFERENCES citizens(id),
		last_task_id TEXT,
		last_run_id INTEGER,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		PRIMARY KEY (project_id, path)
	);

	CREATE TABLE IF NOT EXISTS task_claims (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL REFERENCES tasks(id),
		citizen_id INTEGER NOT NULL REFERENCES citizens(id),
		claimed_at TIMESTAMP NOT NULL,
		deadline TIMESTAMP NOT NULL,
		outcome TEXT,
		submitted_at TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_run ON tasks(run_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);
	CREATE INDEX IF NOT EXISTS idx_tasks_claimed_by ON tasks(claimed_by);
	CREATE INDEX IF NOT EXISTS idx_task_claims_task ON task_claims(task_id);
	CREATE INDEX IF NOT EXISTS idx_citizens_token ON citizens(token);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_citizens_email ON citizens(email) WHERE email IS NOT NULL AND email != '';
	CREATE UNIQUE INDEX IF NOT EXISTS idx_citizens_username ON citizens(username);
	CREATE INDEX IF NOT EXISTS idx_runs_project ON runs(project_id);
	CREATE INDEX IF NOT EXISTS idx_artifacts_project ON artifacts(project_id);
	`
	_, err := s.db.Exec(schema)
	return err
}

// --- Projects (long-lived containers) ---

// CreateProject creates a new long-lived project.
func (s *Store) CreateProject(p *ProjectRecord) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO projects (name, description, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		p.Name, p.Description, p.CreatedBy, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetProject retrieves a project by ID.
func (s *Store) GetProject(id int64) (*ProjectRecord, error) {
	var p ProjectRecord
	var desc, createdBy sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, description, created_by, created_at, updated_at FROM projects WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &desc, &createdBy, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Description = desc.String
	p.CreatedBy = createdBy.String
	return &p, nil
}

// GetProjectByName retrieves a project by its unique name.
func (s *Store) GetProjectByName(name string) (*ProjectRecord, error) {
	var p ProjectRecord
	var desc, createdBy sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, description, created_by, created_at, updated_at FROM projects WHERE name = ?`, name,
	).Scan(&p.ID, &p.Name, &desc, &createdBy, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Description = desc.String
	p.CreatedBy = createdBy.String
	return &p, nil
}

// ListProjects returns all projects.
// DeleteProject removes a project row by ID. Used for rolling back a
// project creation when the per-project repo init fails. This is a hard
// delete — only safe when the project has no runs yet.
func (s *Store) DeleteProject(id int64) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}

func (s *Store) ListProjects() ([]ProjectRecord, error) {
	rows, err := s.db.Query(`SELECT id, name, description, created_by, created_at, updated_at FROM projects ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []ProjectRecord
	for rows.Next() {
		var p ProjectRecord
		var desc, createdBy sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &desc, &createdBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Description = desc.String
		p.CreatedBy = createdBy.String
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// ListRunsByProject returns all runs in a project, ordered by seq.
func (s *Store) ListRunsByProject(projectID int64) ([]RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, created_at, updated_at
		 FROM runs WHERE project_id = ? ORDER BY seq ASC`, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var r RunRecord
		var ref sql.NullString
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Seq, &r.Name, &ref, &r.YAMLData, &r.RepoURL, &r.State, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Ref = ref.String
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// --- Runs ---

// CreateRun inserts a new run. The run's sequence number within its project
// is computed automatically. Returns (global_id, project_seq).
func (s *Store) CreateRun(p *RunRecord) (int64, int, error) {
	if p.ProjectID == 0 {
		return 0, 0, fmt.Errorf("project_id is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// Compute next seq within this project
	var maxSeq sql.NullInt64
	err = tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM runs WHERE project_id = ?`, p.ProjectID).Scan(&maxSeq)
	if err != nil {
		return 0, 0, err
	}
	nextSeq := int(maxSeq.Int64) + 1

	result, err := tx.Exec(
		`INSERT INTO runs (project_id, seq, name, ref, yaml_data, repo_url, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProjectID, nextSeq, p.Name, p.Ref, p.YAMLData, p.RepoURL, p.State, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return id, nextSeq, nil
}

// GetRun retrieves a run by its global ID.
func (s *Store) GetRun(id int64) (*RunRecord, error) {
	var p RunRecord
	var ref sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, created_at, updated_at FROM runs WHERE id = ?`, id,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Ref = ref.String
	return &p, err
}

// GetRunByProjectSeq retrieves a run by (project_id, seq).
func (s *Store) GetRunByProjectSeq(projectID int64, seq int) (*RunRecord, error) {
	var p RunRecord
	var ref sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, created_at, updated_at
		 FROM runs WHERE project_id = ? AND seq = ?`, projectID, seq,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Ref = ref.String
	return &p, err
}

func (s *Store) ListRuns() ([]RunRecord, error) {
	rows, err := s.db.Query(`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, created_at, updated_at FROM runs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var p RunRecord
		var ref sql.NullString
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Ref = ref.String
		runs = append(runs, p)
	}
	return runs, rows.Err()
}

func (s *Store) UpdateRunState(id int64, state RunState) error {
	_, err := s.db.Exec(
		`UPDATE runs SET state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now(), id,
	)
	return err
}

func (s *Store) CheckAndCompleteRun(runID int64) (bool, error) {
	var total, accepted int
	err := s.db.QueryRow(
		`SELECT COUNT(*), COUNT(CASE WHEN state = 'accepted' THEN 1 END) FROM tasks WHERE run_id = ?`,
		runID,
	).Scan(&total, &accepted)
	if err != nil {
		return false, err
	}
	if total > 0 && total == accepted {
		err = s.UpdateRunState(runID, RunCompleted)
		return err == nil, err
	}
	return false, nil
}

// --- Tasks ---

const taskColumns = `id, run_id, seq, task_def_id, instance_key, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, created_at`

func (s *Store) CreateTask(t *TaskRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RunID, t.Seq, t.TaskDefID, t.InstanceKey, t.Ref, t.Action,
		t.Prompt, t.UserPrompt, t.Script, t.Outputs, t.Requirements, t.ResultType, t.Timeout,
		t.State, t.DependsOn, t.ReadsArtifacts, t.WritesArtifacts,
		t.AssignTo, t.RequireRole, t.CreatedAt,
	)
	return err
}

func (s *Store) GetTask(id string) (*TaskRecord, error) {
	var t TaskRecord
	var claimedAt, submittedAt sql.NullTime
	var claimedBy sql.NullInt64
	var resultPath, prompt, userPrompt, script, outputs, requirements, timeout, ref sql.NullString
	err := s.db.QueryRow(
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.RunID, &t.Seq, &t.TaskDefID, &t.InstanceKey, &ref, &t.Action,
		&prompt, &userPrompt, &script, &outputs, &requirements, &t.ResultType, &timeout,
		&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.DependsOn,
		&t.ReadsArtifacts, &t.WritesArtifacts,
		&t.AssignTo, &t.RequireRole, &t.CreatedAt)
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
	t.Requirements = requirements.String
	t.Timeout = timeout.String
	t.ClaimedBy = claimedBy.Int64
	t.ResultPath = resultPath.String
	if claimedAt.Valid {
		t.ClaimedAt = &claimedAt.Time
	}
	if submittedAt.Valid {
		t.SubmittedAt = &submittedAt.Time
	}
	return &t, nil
}

// GetTaskBySeq finds a task by run ID and sequence number.
func (s *Store) GetTaskBySeq(runID string, seq int) (*TaskRecord, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM tasks WHERE run_id = ? AND seq = ?`, runID, seq).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetTask(id)
}

func (s *Store) ListTasksByRun(runID int64) ([]TaskRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+taskColumns+` FROM tasks WHERE run_id = ? ORDER BY created_at`, runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) ListReadyTasks(runID int64) ([]TaskRecord, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE state = 'ready'`
	args := []interface{}{}
	if runID > 0 {
		query += " AND run_id = ?"
		args = append(args, runID)
	}
	query += " ORDER BY created_at"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *Store) ClaimTask(taskID string, citizenID int64, deadline time.Time) error {
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

	var state string
	var claimedBy sql.NullInt64
	err = tx.QueryRow(`SELECT state, claimed_by FROM tasks WHERE id = ?`, taskID).Scan(&state, &claimedBy)
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

	if claimedBy.Valid {
		_, err = tx.Exec(
			`UPDATE citizens SET
				tasks_completed = tasks_completed + 1,
				tokens_contributed = tokens_contributed + ?,
				score = (tasks_completed + 1) - (tasks_timed_out * 0.5) - (tasks_rejected * 1.0),
				last_seen = ?
			WHERE id = ?`,
			tokensUsed, now, claimedBy.Int64,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) ReleaseTask(taskID string, citizenID int64) error {
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

// InvalidateTask cascades an invalidation starting from taskID. The
// target transitions to READY (ready to re-claim) and each descendant
// transitions to PENDING (waiting for the target to re-complete).
// claimed_by, claimed_at, and result_path are cleared on every touched
// task, so old provenance doesn't leak into the next run.
//
// Git history preserves the previous results — the new result written
// on re-claim overwrites the same files, but git keeps both versions.
//
// Returns the number of tasks that actually changed state (useful for
// logging and for returning in the API response).
func (s *Store) InvalidateTask(taskID string, descendantIDs []string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Verify the target exists and is in a state we can invalidate.
	// Only ACCEPTED tasks make sense to invalidate — they've produced
	// a result that we now believe is wrong.
	var state string
	if err := tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&state); err != nil {
		return 0, fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if TaskState(state) != TaskAccepted {
		return 0, fmt.Errorf("task %q cannot be invalidated (state: %s, must be accepted)", taskID, state)
	}

	changed := 0

	// Target: ACCEPTED → READY. Clear all claim/result fields so a
	// fresh citizen sees no stale provenance when they claim.
	if _, err = tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL WHERE id = ?`,
		taskID,
	); err != nil {
		return 0, err
	}
	changed++

	// Record the invalidation in the task_claims history so the audit
	// trail captures what happened. Any open claim row for this task
	// that doesn't already have an outcome gets marked 'invalidated'.
	if _, err = tx.Exec(
		`UPDATE task_claims SET outcome = 'invalidated' WHERE task_id = ? AND outcome IS NULL`,
		taskID,
	); err != nil {
		return 0, err
	}

	// Descendants: any state that implies they're running, ran, or
	// about to run against now-stale upstream data → PENDING. Already
	// PENDING descendants are left alone.
	//
	// TaskPending is explicitly the no-op case; everything else
	// (READY, CLAIMED, RUNNING, SUBMITTED, ACCEPTED, INVALID,
	// INVALIDATED, REJECTED) transitions to PENDING so the scheduler
	// re-evaluates it when the target re-completes.
	for _, descID := range descendantIDs {
		var descState string
		err := tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, descID).Scan(&descState)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, err
		}
		if TaskState(descState) == TaskPending {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE tasks SET state = 'pending', claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL WHERE id = ?`,
			descID,
		); err != nil {
			return 0, err
		}
		// Also clear any open claim row so re-claims get a fresh
		// history entry instead of overwriting the original.
		if _, err := tx.Exec(
			`UPDATE task_claims SET outcome = 'invalidated' WHERE task_id = ? AND outcome IS NULL`,
			descID,
		); err != nil {
			return 0, err
		}
		changed++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

func (s *Store) UpdateReadyTasks(runID int64) (int, error) {
	rows, err := s.db.Query(
		`SELECT id, depends_on FROM tasks WHERE run_id = ? AND state = 'pending'`, runID,
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
		`SELECT id FROM tasks WHERE run_id = ? AND state = 'accepted'`, runID,
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

func (s *Store) ExpireClaimedTask(taskID string, citizenID int64) error {
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

// citizenColumns lists every citizen column we read in the same order
// scan functions expect. Keep this in sync with CitizenRecord.
const citizenColumns = `id, username, name, email, role, token, score, tasks_completed, tasks_rejected, tasks_timed_out, tasks_released, tokens_contributed, registered_at, last_seen`

// scanCitizen reads one citizen row into a CitizenRecord. Used by
// GetCitizen, GetCitizenByUsername, GetCitizenByToken.
func scanCitizen(row *sql.Row) (*CitizenRecord, error) {
	var p CitizenRecord
	var email, role sql.NullString
	err := row.Scan(
		&p.ID, &p.Username, &p.Name, &email, &role, &p.Token, &p.Score,
		&p.TasksCompleted, &p.TasksRejected, &p.TasksTimedOut, &p.TasksReleased,
		&p.TokensContrib, &p.RegisteredAt, &p.LastSeen,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Email = email.String
	p.Role = role.String
	return &p, nil
}

// CreateCitizen inserts a new citizen and returns the generated int64
// primary key. The caller must provide a validated, unique username.
// Uniqueness of email (if provided) and username is checked here and
// enforced again by the DB.
func (s *Store) CreateCitizen(p *CitizenRecord) (int64, error) {
	if err := ValidateUsername(p.Username); err != nil {
		return 0, err
	}
	// Check email uniqueness if provided
	if p.Email != "" {
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM citizens WHERE email = ?`, p.Email).Scan(&count)
		if count > 0 {
			return 0, fmt.Errorf("a citizen with this email already exists")
		}
	}
	// Check username uniqueness
	var uCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM citizens WHERE username = ?`, p.Username).Scan(&uCount)
	if uCount > 0 {
		return 0, fmt.Errorf("username %q is already taken", p.Username)
	}

	role := p.Role
	if role == "" {
		role = "citizen"
	}

	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen) VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		p.Username, p.Name, p.Email, role, p.Token, p.RegisteredAt, p.LastSeen,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetCitizenRole updates a citizen's global role (citizen / author /
// reviewer / etc). This is a privileged operation — no admin UI yet, so
// it's set via the store directly by tests and (eventually) an admin
// CLI. Per-project roles are a Phase 2 feature.
func (s *Store) SetCitizenRole(id int64, role string) error {
	_, err := s.db.Exec(`UPDATE citizens SET role = ? WHERE id = ?`, role, id)
	return err
}

// UpdateCitizenProfile updates a citizen's name and email. Username is
// intentionally immutable once set — that's enforced here by not
// touching the column.
func (s *Store) UpdateCitizenProfile(id int64, name, email string) error {
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
	return scanCitizen(s.db.QueryRow(
		`SELECT `+citizenColumns+` FROM citizens WHERE token = ?`, token,
	))
}

func (s *Store) TouchCitizen(id int64) error {
	_, err := s.db.Exec(`UPDATE citizens SET last_seen = ? WHERE id = ?`, time.Now(), id)
	return err
}

// GetCitizen retrieves a citizen by internal int64 ID. For user-facing
// lookups prefer GetCitizenByUsername.
func (s *Store) GetCitizen(id int64) (*CitizenRecord, error) {
	return scanCitizen(s.db.QueryRow(
		`SELECT `+citizenColumns+` FROM citizens WHERE id = ?`, id,
	))
}

// GetCitizenByUsername is the primary user-facing lookup. Returns
// (nil, nil) when the username doesn't exist.
func (s *Store) GetCitizenByUsername(username string) (*CitizenRecord, error) {
	return scanCitizen(s.db.QueryRow(
		`SELECT `+citizenColumns+` FROM citizens WHERE username = ?`, username,
	))
}

// ListCitizenActiveTasks returns tasks currently claimed by a citizen.
func (s *Store) ListCitizenActiveTasks(citizenID int64) ([]TaskRecord, error) {
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
func (s *Store) ListCitizenCompletedTasks(citizenID int64, limit int) ([]TaskRecord, error) {
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
		var claimedBy sql.NullInt64
		var resultPath, prompt, userPrompt, script, outputs, requirements, timeout, ref sql.NullString
		if err := rows.Scan(&t.ID, &t.RunID, &t.Seq, &t.TaskDefID, &t.InstanceKey, &ref, &t.Action,
			&prompt, &userPrompt, &script, &outputs, &requirements, &t.ResultType, &timeout,
			&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.DependsOn,
			&t.ReadsArtifacts, &t.WritesArtifacts,
			&t.AssignTo, &t.RequireRole, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Ref = ref.String
		t.Prompt = prompt.String
		t.UserPrompt = userPrompt.String
		t.Script = script.String
		t.Outputs = outputs.String
		t.Requirements = requirements.String
		t.Timeout = timeout.String
		t.ClaimedBy = claimedBy.Int64
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

// --- Artifacts ---

// UpsertArtifact records that a task wrote an artifact at the given path.
// On first write the row is created; on subsequent writes the provenance
// fields (last_writer, last_task_id, last_run_id, updated_at) are
// refreshed and created_at is preserved.
func (s *Store) UpsertArtifact(a *ArtifactRecord) error {
	if a.ProjectID == 0 || a.Path == "" {
		return fmt.Errorf("UpsertArtifact: project_id and path are required")
	}
	_, err := s.db.Exec(
		`INSERT INTO artifacts (project_id, path, last_writer, last_task_id, last_run_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, path) DO UPDATE SET
		   last_writer  = excluded.last_writer,
		   last_task_id = excluded.last_task_id,
		   last_run_id  = excluded.last_run_id,
		   updated_at   = excluded.updated_at`,
		a.ProjectID, a.Path, a.LastWriter, a.LastTaskID, a.LastRunID,
		a.CreatedAt, a.UpdatedAt,
	)
	return err
}

// GetArtifact looks up one artifact's index row by (project_id, path).
// Returns nil if the artifact doesn't exist.
func (s *Store) GetArtifact(projectID int64, path string) (*ArtifactRecord, error) {
	var a ArtifactRecord
	var lastTaskID sql.NullString
	var lastWriter, lastRunID sql.NullInt64
	err := s.db.QueryRow(
		`SELECT project_id, path, last_writer, last_task_id, last_run_id, created_at, updated_at
		 FROM artifacts WHERE project_id = ? AND path = ?`,
		projectID, path,
	).Scan(&a.ProjectID, &a.Path, &lastWriter, &lastTaskID, &lastRunID, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.LastWriter = lastWriter.Int64
	a.LastTaskID = lastTaskID.String
	a.LastRunID = lastRunID.Int64
	return &a, nil
}

// ListTasksReadingArtifact returns tasks in a project whose
// reads_artifacts declaration contains the given path. If acceptedOnly
// is true, only currently-accepted tasks are returned.
//
// Used by the invalidation cascade to find cross-run readers — tasks
// that consumed an artifact version that's now being rolled back.
//
// Implementation: reads_artifacts is stored as a JSON array string
// (e.g. `["notes/intro.md","src/main.py"]`). A LIKE pattern with the
// quoted path anchors matches so `notes/intro` doesn't false-match on
// `notes/intro.md`. Paths aren't allowed to contain `"` so this is
// safe against embedded-quote false positives.
func (s *Store) ListTasksReadingArtifact(projectID int64, path string, acceptedOnly bool) ([]TaskRecord, error) {
	pattern := `%"` + path + `"%`
	query := `SELECT ` + taskColumns + ` FROM tasks
	          WHERE reads_artifacts LIKE ?
	            AND run_id IN (SELECT id FROM runs WHERE project_id = ?)`
	args := []interface{}{pattern, projectID}
	if acceptedOnly {
		query += ` AND state = 'accepted'`
	}
	query += ` ORDER BY id`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// DeleteArtifact removes an artifact's index row by (project_id, path).
// Used by the rollback path when an invalidated task was the file's
// first writer — the artifact stops existing entirely. Git history
// still has the previous state, so this is not destructive.
func (s *Store) DeleteArtifact(projectID int64, path string) error {
	_, err := s.db.Exec(
		`DELETE FROM artifacts WHERE project_id = ? AND path = ?`,
		projectID, path,
	)
	return err
}

// ListArtifactsByProject returns all artifacts for a project, ordered by
// path. If pathPrefix is non-empty, only artifacts whose path starts with
// it are returned (useful for listing a directory subtree).
func (s *Store) ListArtifactsByProject(projectID int64, pathPrefix string) ([]ArtifactRecord, error) {
	query := `SELECT project_id, path, last_writer, last_task_id, last_run_id, created_at, updated_at
	          FROM artifacts WHERE project_id = ?`
	args := []interface{}{projectID}
	if pathPrefix != "" {
		query += ` AND path LIKE ?`
		args = append(args, pathPrefix+"%")
	}
	query += ` ORDER BY path ASC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifacts []ArtifactRecord
	for rows.Next() {
		var a ArtifactRecord
		var lastTaskID sql.NullString
		var lastWriter, lastRunID sql.NullInt64
		if err := rows.Scan(&a.ProjectID, &a.Path, &lastWriter, &lastTaskID, &lastRunID, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.LastWriter = lastWriter.Int64
		a.LastTaskID = lastTaskID.String
		a.LastRunID = lastRunID.Int64
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}
