package store

import (
	"database/sql"
	"encoding/json"
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
		remote_url TEXT,
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
		source_path TEXT NOT NULL DEFAULT '',
		source_commit_sha TEXT NOT NULL DEFAULT '',
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
		reviews_target TEXT NOT NULL DEFAULT '',
		review_decision TEXT NOT NULL DEFAULT '',
		vote_options TEXT NOT NULL DEFAULT '',
		vote_choice TEXT NOT NULL DEFAULT '',
		citizens INTEGER NOT NULL DEFAULT 1,
		min_quorum INTEGER NOT NULL DEFAULT 0,
		vote_threshold TEXT NOT NULL DEFAULT '',
		vote_deadline TEXT NOT NULL DEFAULT '',
		anonymize INTEGER NOT NULL DEFAULT 0,
		visibility TEXT NOT NULL DEFAULT '',
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
		submitted_at TIMESTAMP,
		option TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT ''
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
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Idempotent column additions for pre-existing databases. SQLite's
	// CREATE TABLE IF NOT EXISTS leaves existing tables untouched, so
	// new columns go in via ALTER TABLE. A duplicate-column error is
	// expected on every run after the first and is swallowed here.
	altered := []string{
		`ALTER TABLE projects ADD COLUMN remote_url TEXT`,
		// Note: projects.last_push_at / last_push_error were added
		// by iteration 4 for coordinator-side push status tracking.
		// Iteration A moved pushes to the client, iteration A.8
		// dropped the Go-side references. The columns still exist
		// on databases that were migrated through iteration 4 but
		// the coordinator no longer reads or writes them; they're
		// harmless dead columns pending a future schema migration.
		`ALTER TABLE tasks ADD COLUMN instance_params TEXT NOT NULL DEFAULT ''`,
		// Iteration A.2 — client-side writes. Populated by the new
		// report submit path; empty for tasks submitted via the
		// legacy coordinator-writes path.
		`ALTER TABLE tasks ADD COLUMN commit_sha TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE artifacts ADD COLUMN commit_sha TEXT NOT NULL DEFAULT ''`,
		// Phase E — review action. Target is the task def id this
		// review evaluates; decision is approve/reject, populated
		// on submit and cleared on invalidation.
		`ALTER TABLE tasks ADD COLUMN reviews_target TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN review_decision TEXT NOT NULL DEFAULT ''`,
		// Phase E.2 — vote action. VoteOptions is the declared
		// options list (JSON), VoteChoice is the submitted option
		// id. Citizens / MinQuorum / VoteThreshold / VoteDeadline
		// carry the tally-rule fields from YAML. Session 1 ships
		// single-voter only; citizens defaults to 1.
		`ALTER TABLE tasks ADD COLUMN vote_options TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN vote_choice TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN citizens INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE tasks ADD COLUMN min_quorum INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tasks ADD COLUMN vote_threshold TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN vote_deadline TEXT NOT NULL DEFAULT ''`,
		// Phase E.2 session 2a — multi-citizen tasks carry
		// per-claim vote choices and short commentary on the
		// task_claims rows so the tally and dashboard renders
		// don't need to fetch git content on every call.
		`ALTER TABLE task_claims ADD COLUMN option TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE task_claims ADD COLUMN content TEXT NOT NULL DEFAULT ''`,
		// Phase E.2 session 2c — anonymize + visibility fields
		// for vote/review tasks (blind voting, hidden voter
		// usernames).
		`ALTER TABLE tasks ADD COLUMN anonymize INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tasks ADD COLUMN visibility TEXT NOT NULL DEFAULT ''`,
		// Phase H.1 — template provenance. Records the
		// repo-relative templates/*.yaml path this run was
		// instantiated from. Empty string for inline-YAML
		// submissions.
		`ALTER TABLE runs ADD COLUMN source_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN source_commit_sha TEXT NOT NULL DEFAULT ''`,
	}
	for _, q := range altered {
		if _, err := s.db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("schema alter %q: %w", q, err)
		}
	}
	return nil
}

// --- Projects (long-lived containers) ---

// CreateProject creates a new long-lived project.
func (s *Store) CreateProject(p *ProjectRecord) (int64, error) {
	var remote sql.NullString
	if p.RemoteURL != "" {
		remote = sql.NullString{String: p.RemoteURL, Valid: true}
	}
	result, err := s.db.Exec(
		`INSERT INTO projects (name, description, created_by, remote_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.Name, p.Description, p.CreatedBy, remote, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SetProjectRemoteURL updates a project's external git remote URL.
// Passing an empty string clears the remote.
func (s *Store) SetProjectRemoteURL(projectID int64, remoteURL string) error {
	var remote sql.NullString
	if remoteURL != "" {
		remote = sql.NullString{String: remoteURL, Valid: true}
	}
	_, err := s.db.Exec(
		`UPDATE projects SET remote_url = ?, updated_at = ? WHERE id = ?`,
		remote, time.Now(), projectID,
	)
	return err
}

// projectSelectColumns is the canonical SELECT list for project rows,
// kept in one place so every scanner pulls the same set in the same
// order.
const projectSelectColumns = `id, name, description, created_by, remote_url, created_at, updated_at`

// scanProject reads one project row from a scanner into p.
func scanProject(row interface {
	Scan(dest ...interface{}) error
}, p *ProjectRecord) error {
	var desc, createdBy, remote sql.NullString
	if err := row.Scan(&p.ID, &p.Name, &desc, &createdBy, &remote, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return err
	}
	p.Description = desc.String
	p.CreatedBy = createdBy.String
	p.RemoteURL = remote.String
	return nil
}

// GetProject retrieves a project by ID.
func (s *Store) GetProject(id int64) (*ProjectRecord, error) {
	var p ProjectRecord
	err := scanProject(s.db.QueryRow(
		`SELECT `+projectSelectColumns+` FROM projects WHERE id = ?`, id,
	), &p)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProjectByName retrieves a project by its unique name.
func (s *Store) GetProjectByName(name string) (*ProjectRecord, error) {
	var p ProjectRecord
	err := scanProject(s.db.QueryRow(
		`SELECT `+projectSelectColumns+` FROM projects WHERE name = ?`, name,
	), &p)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListProjects returns all projects.
func (s *Store) ListProjects() ([]ProjectRecord, error) {
	rows, err := s.db.Query(`SELECT ` + projectSelectColumns + ` FROM projects ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []ProjectRecord
	for rows.Next() {
		var p ProjectRecord
		if err := scanProject(rows, &p); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// ListRunsByProject returns all runs in a project, ordered by seq.
func (s *Store) ListRunsByProject(projectID int64) ([]RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, created_at, updated_at
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
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Seq, &r.Name, &ref, &r.YAMLData, &r.RepoURL, &r.State, &r.SourcePath, &r.SourceCommitSHA, &r.CreatedAt, &r.UpdatedAt); err != nil {
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
		`INSERT INTO runs (project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProjectID, nextSeq, p.Name, p.Ref, p.YAMLData, p.RepoURL, p.State, p.SourcePath, p.SourceCommitSHA, p.CreatedAt, p.UpdatedAt,
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
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, created_at, updated_at FROM runs WHERE id = ?`, id,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.CreatedAt, &p.UpdatedAt)
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
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, created_at, updated_at
		 FROM runs WHERE project_id = ? AND seq = ?`, projectID, seq,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Ref = ref.String
	return &p, err
}

func (s *Store) ListRuns() ([]RunRecord, error) {
	rows, err := s.db.Query(`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, created_at, updated_at FROM runs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var p RunRecord
		var ref sql.NullString
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.CreatedAt, &p.UpdatedAt); err != nil {
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
	var total, terminal int
	// SKIPPED tasks count as terminal alongside ACCEPTED — a run
	// completes when every task has reached a "done" state, and
	// "skipped by a gate vote" is one of those. This mirrors the
	// invariant UpdateReadyTasks relies on for dep satisfaction.
	err := s.db.QueryRow(
		`SELECT COUNT(*), COUNT(CASE WHEN state IN ('accepted', 'skipped') THEN 1 END) FROM tasks WHERE run_id = ?`,
		runID,
	).Scan(&total, &terminal)
	if err != nil {
		return false, err
	}
	if total > 0 && total == terminal {
		err = s.UpdateRunState(runID, RunCompleted)
		return err == nil, err
	}
	return false, nil
}

// --- Tasks ---

const taskColumns = `id, run_id, seq, task_def_id, instance_key, instance_params, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, commit_sha, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, reviews_target, review_decision, vote_options, vote_choice, citizens, min_quorum, vote_threshold, vote_deadline, anonymize, visibility, created_at`

func (s *Store) CreateTask(t *TaskRecord) error {
	// commit_sha / review_decision / vote_choice are never set at
	// create time; they're populated by the submit path. Omitting
	// them here lets the column defaults fire. reviews_target and
	// vote_options (and the rest of the vote config) ARE set at
	// create time from YAML.
	citizens := t.Citizens
	if citizens == 0 {
		citizens = 1
	}
	anonymize := 0
	if t.Anonymize {
		anonymize = 1
	}
	_, err := s.db.Exec(
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

func (s *Store) GetTask(id string) (*TaskRecord, error) {
	var t TaskRecord
	var claimedAt, submittedAt sql.NullTime
	var claimedBy sql.NullInt64
	var resultPath, prompt, userPrompt, script, outputs, requirements, timeout, ref sql.NullString
	var anonymizeInt int
	err := s.db.QueryRow(
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.RunID, &t.Seq, &t.TaskDefID, &t.InstanceKey, &t.InstanceParams, &ref, &t.Action,
		&prompt, &userPrompt, &script, &outputs, &requirements, &t.ResultType, &timeout,
		&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.CommitSHA, &t.DependsOn,
		&t.ReadsArtifacts, &t.WritesArtifacts,
		&t.AssignTo, &t.RequireRole, &t.ReviewsTarget, &t.ReviewDecision,
		&t.VoteOptions, &t.VoteChoice, &t.Citizens, &t.MinQuorum, &t.VoteThreshold, &t.VoteDeadline,
		&anonymizeInt, &t.Visibility,
		&t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Anonymize = anonymizeInt != 0
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
	// Sort by seq (assigned in a deterministic creation order) with id
	// as a tiebreaker. Using created_at alone leaks Go's map iteration
	// order into the output because all tasks created in a single
	// run-creation burst share a timestamp.
	rows, err := s.db.Query(
		`SELECT `+taskColumns+` FROM tasks WHERE run_id = ? ORDER BY seq, id`, runID,
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
	var citizens int
	err = tx.QueryRow(`SELECT state, citizens FROM tasks WHERE id = ?`, taskID).Scan(&state, &citizens)
	if err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if citizens <= 0 {
		citizens = 1
	}

	// Single-citizen tasks: must be READY, exclusive claim via
	// tasks.claimed_by. Same as before.
	if citizens == 1 {
		if TaskState(state) != TaskReady {
			return fmt.Errorf("task %q is not ready (state: %s)", taskID, state)
		}
	} else {
		// Multi-citizen tasks: READY or COLLECTING is fine —
		// additional citizens can join while the task is still
		// collecting submissions. Check own-slot existence
		// BEFORE the cap count so a citizen who's already
		// claimed gets a specific "you already hold a slot"
		// error rather than a misleading "cap reached" one.
		if TaskState(state) != TaskReady && TaskState(state) != TaskCollecting {
			return fmt.Errorf("task %q is not accepting claims (state: %s)", taskID, state)
		}
		var mine int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM task_claims WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
			taskID, citizenID,
		).Scan(&mine); err != nil {
			return err
		}
		if mine > 0 {
			return fmt.Errorf("you already have an active claim on task %q (one slot per citizen)", taskID)
		}
		var active int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM task_claims WHERE task_id = ? AND outcome IS NULL`,
			taskID,
		).Scan(&active); err != nil {
			return err
		}
		if active >= citizens {
			return fmt.Errorf("task %q has reached its citizens cap (%d active claim(s))", taskID, active)
		}
	}

	now := time.Now()

	// tasks.claimed_by is only meaningful for citizens=1 tasks.
	// For multi-citizen tasks we leave it as whatever the most
	// recent claimer is — the task_claims table is the source of
	// truth for who's actually working on it.
	if citizens == 1 {
		_, err = tx.Exec(
			`UPDATE tasks SET state = 'claimed', claimed_by = ?, claimed_at = ? WHERE id = ?`,
			citizenID, now, taskID,
		)
	} else {
		// For multi-citizen tasks, don't flip the state on
		// claim — stay in READY/COLLECTING until a submission
		// arrives. Track the most recent claimer for
		// convenience but don't transition.
		_, err = tx.Exec(
			`UPDATE tasks SET claimed_by = ?, claimed_at = ? WHERE id = ?`,
			citizenID, now, taskID,
		)
	}
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

// SubmitResult is the outcome of a single submission on a task.
// Callers read Resolved to know whether the task transitioned to
// a terminal state this submission, or whether it's still
// collecting more submissions from other citizens.
type SubmitResult struct {
	// Resolved is true when this submission transitioned the task
	// to ACCEPTED. False for intermediate submissions on a
	// multi-citizen task that hasn't yet met quorum+threshold.
	Resolved bool
	// Collecting is true when the task is in COLLECTING state
	// after this submission — a multi-citizen task that has
	// received at least one submission but is still waiting for
	// more.
	Collecting bool
}

// SubmitTaskResult records a task's submission. For single-citizen
// tasks (citizens = 1) this is the classic "claim → submit →
// accepted" path and Resolved is always true on success. For
// multi-citizen tasks (citizens > 1) this records the caller's
// individual submission on their task_claims row, transitions the
// task to COLLECTING if it was still READY, and leaves the final
// ACCEPTED transition to a separate tally step driven by the
// caller (router) that runs the threshold rule against the
// collected votes.
//
// citizenID identifies which claim slot this submission fills for
// multi-citizen tasks. Ignored for single-citizen tasks (they use
// tasks.claimed_by as the implicit claimer).
//
// decision is the optional review verdict ("approve" or "reject")
// for review-action tasks.
//
// voteChoice is the selected option id for vote-action tasks.
// Recorded on the citizen's task_claims row for multi-voter
// tasks; also copied to tasks.vote_choice for single-voter tasks
// so the session-1 single-voter path keeps working unchanged.
//
// content is the citizen's prose commentary (for multi-citizen
// vote/review tasks). Stored on the task_claims row so the
// coordinator can surface it via {{task.responses}} without
// reading per-citizen result.md from git — this is the
// authoritative location for multi-citizen submissions (git may
// or may not have a commit, commit_sha is optional for
// vote/review actions).
func (s *Store) SubmitTaskResult(taskID string, citizenID int64, resultPath, commitSHA, decision, voteChoice, content string, tokensUsed int64) (*SubmitResult, error) {
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var state string
	var citizens int
	var claimedBy sql.NullInt64
	err = tx.QueryRow(`SELECT state, citizens, claimed_by FROM tasks WHERE id = ?`, taskID).Scan(&state, &citizens, &claimedBy)
	if err != nil {
		return nil, fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if citizens <= 0 {
		citizens = 1
	}

	// Single-citizen path: exactly the pre-session-2a behavior.
	// One submit flips the task to ACCEPTED; tally runs trivially
	// with a single vote.
	if citizens == 1 {
		if TaskState(state) != TaskClaimed && TaskState(state) != TaskRunning {
			return nil, fmt.Errorf("task %q cannot accept result (state: %s)", taskID, state)
		}
		_, err = tx.Exec(
			`UPDATE tasks SET state = 'accepted', submitted_at = ?, result_path = ?, commit_sha = ?, review_decision = ?, vote_choice = ? WHERE id = ?`,
			now, resultPath, commitSHA, decision, voteChoice, taskID,
		)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(
			`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ? WHERE task_id = ? AND outcome IS NULL`,
			now, voteChoice, taskID,
		)
		if err != nil {
			return nil, err
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
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &SubmitResult{Resolved: true}, nil
	}

	// Multi-citizen path. The task must be READY (first submit)
	// or COLLECTING (subsequent submits). If the task has
	// already resolved (ACCEPTED / SKIPPED / terminal), a late
	// submit from a citizen who had an open claim gets a
	// clean state error rather than a 500 from downstream
	// code that assumes the state is still mutable. This races
	// with the Nth-submit resolution — two citizens hitting
	// the tally concurrently can both see "collecting" at the
	// start of their transactions, one wins and transitions,
	// the other's transition is caught here on its second
	// check.
	switch TaskState(state) {
	case TaskReady, TaskCollecting:
		// OK, proceed.
	case TaskAccepted, TaskSkipped:
		return nil, fmt.Errorf("task %q already resolved (state: %s) — your submission arrived after the tally closed", taskID, state)
	default:
		return nil, fmt.Errorf("task %q cannot accept result (state: %s)", taskID, state)
	}
	var claimRow sql.NullInt64
	if err := tx.QueryRow(
		`SELECT id FROM task_claims WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
		taskID, citizenID,
	).Scan(&claimRow); err != nil || !claimRow.Valid {
		return nil, fmt.Errorf("task %q has no open claim for this citizen — claim it first", taskID)
	}

	// Transition to COLLECTING on first submission; stay there on
	// subsequent ones. Result path stays at the task's root for
	// now (the actual per-citizen subdirs live under result_path
	// on disk; the task's result_path is the common parent).
	_, err = tx.Exec(
		`UPDATE tasks SET state = 'collecting', result_path = ? WHERE id = ?`,
		resultPath, taskID,
	)
	if err != nil {
		return nil, err
	}
	// Mark this citizen's claim as completed. The `option`
	// column carries whatever "choice" this citizen made:
	//   - vote tasks: the selected option id
	//   - review tasks: "approve" or "reject"
	// The router's tally function interprets the column
	// depending on task.Action. `content` stores the prose
	// commentary so {{task.responses}} can render it without
	// reading git — for multi-citizen submits with no
	// commit_sha (votes/reviews don't strictly need one),
	// this row is the only place the prose lives.
	choice := voteChoice
	if choice == "" {
		choice = decision
	}
	_, err = tx.Exec(
		`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, content = ? WHERE id = ?`,
		now, choice, content, claimRow.Int64,
	)
	if err != nil {
		return nil, err
	}
	// Score accounting on the submitting citizen alone — don't
	// increment tasks_completed until the tally actually resolves
	// (score should reflect "I completed a task," not "I
	// submitted a vote that's still being tallied"). Token
	// contribution can still be credited per submit.
	_, err = tx.Exec(
		`UPDATE citizens SET tokens_contributed = tokens_contributed + ?, last_seen = ? WHERE id = ?`,
		tokensUsed, now, citizenID,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &SubmitResult{Collecting: true}, nil
}

// ResolveMultiCitizenVote transitions a task from COLLECTING to
// ACCEPTED with the given winning option. Called by the router
// after the tally function says "winner found." Also credits the
// completed task to every citizen who submitted (score rolls up
// once the group decision lands, not per-vote).
func (s *Store) ResolveMultiCitizenVote(taskID, winningOption, commitSHA string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()

	var state string
	if err := tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&state); err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if TaskState(state) != TaskCollecting {
		return fmt.Errorf("task %q is not in collecting state (state: %s)", taskID, state)
	}
	_, err = tx.Exec(
		`UPDATE tasks SET state = 'accepted', submitted_at = ?, vote_choice = ?, commit_sha = ? WHERE id = ?`,
		now, winningOption, commitSHA, taskID,
	)
	if err != nil {
		return err
	}
	// Credit every submitting citizen.
	if _, err := tx.Exec(
		`UPDATE citizens SET
			tasks_completed = tasks_completed + 1,
			score = (tasks_completed + 1) - (tasks_timed_out * 0.5) - (tasks_rejected * 1.0),
			last_seen = ?
		WHERE id IN (SELECT citizen_id FROM task_claims WHERE task_id = ? AND outcome = 'completed')`,
		now, taskID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveMultiCitizenReview transitions a multi-reviewer review
// task from COLLECTING to ACCEPTED, recording the tally's final
// verdict ("approve" or "reject") on the task's review_decision
// column. Credits the completed task to every submitting reviewer
// (score rolls up once the group decision lands, same as the
// vote path). Called by the router after the review tally
// resolves. The caller then fires the existing reject cascade if
// the verdict was "reject".
func (s *Store) ResolveMultiCitizenReview(taskID, verdict, commitSHA string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()

	var state string
	if err := tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, taskID).Scan(&state); err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}
	if TaskState(state) != TaskCollecting {
		return fmt.Errorf("task %q is not in collecting state (state: %s)", taskID, state)
	}
	_, err = tx.Exec(
		`UPDATE tasks SET state = 'accepted', submitted_at = ?, review_decision = ?, commit_sha = ? WHERE id = ?`,
		now, verdict, commitSHA, taskID,
	)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE citizens SET
			tasks_completed = tasks_completed + 1,
			score = (tasks_completed + 1) - (tasks_timed_out * 0.5) - (tasks_rejected * 1.0),
			last_seen = ?
		WHERE id IN (SELECT citizen_id FROM task_claims WHERE task_id = ? AND outcome = 'completed')`,
		now, taskID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ListVoteSubmissions returns the per-citizen votes cast so far on
// a multi-citizen vote task. One row per submitted claim, in
// submission order. Used by the router's tally function and by
// formatters that render collection progress.
func (s *Store) ListVoteSubmissions(taskID string) ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, task_id, citizen_id, claimed_at, deadline, outcome, submitted_at, option, content
		 FROM task_claims
		 WHERE task_id = ? AND outcome = 'completed'
		 ORDER BY submitted_at`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskClaimRecord
	for rows.Next() {
		var r TaskClaimRecord
		var outcome sql.NullString
		var submittedAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline, &outcome, &submittedAt, &r.Option, &r.Content); err != nil {
			return nil, err
		}
		if outcome.Valid {
			r.Outcome = outcome.String
		}
		if submittedAt.Valid {
			r.SubmittedAt = &submittedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListActiveClaims returns every open (outcome = NULL) claim row
// for a task. Used by the task response marshaller so MCP
// formatters can show "active claimants" on multi-citizen tasks
// that haven't resolved yet.
func (s *Store) ListActiveClaims(taskID string) ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, task_id, citizen_id, claimed_at, deadline, outcome, submitted_at, option, content
		 FROM task_claims
		 WHERE task_id = ? AND outcome IS NULL
		 ORDER BY claimed_at`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskClaimRecord
	for rows.Next() {
		var r TaskClaimRecord
		var outcome sql.NullString
		var submittedAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline, &outcome, &submittedAt, &r.Option, &r.Content); err != nil {
			return nil, err
		}
		if outcome.Valid {
			r.Outcome = outcome.String
		}
		if submittedAt.Valid {
			r.SubmittedAt = &submittedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EarliestClaimTime returns the timestamp of the first claim
// on a task — used by the vote deadline-enforcement path as
// the "voting opened" anchor. Returns the zero time if no
// citizen has ever claimed.
//
// NOTE: the SQLite driver doesn't automatically parse MIN()
// aggregate results back into time.Time (MIN returns the raw
// stored string representation, not a TIMESTAMP column value).
// So we ORDER BY + LIMIT 1 instead of MIN() — scanning a
// regular column goes through the driver's time.Time unmarshal
// path correctly.
func (s *Store) EarliestClaimTime(taskID string) (time.Time, error) {
	var t sql.NullTime
	err := s.db.QueryRow(
		`SELECT claimed_at FROM task_claims WHERE task_id = ? ORDER BY claimed_at ASC LIMIT 1`,
		taskID,
	).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if !t.Valid {
		return time.Time{}, nil
	}
	return t.Time, nil
}

// HasActiveClaim returns true when the given citizen holds an
// open (outcome = NULL) claim row on the task. Used by the
// submit handler to reject submissions from citizens who never
// claimed the task — the check runs before the commit_sha
// validator so a drive-by submit gets a claim-specific error
// rather than a misleading contract error.
func (s *Store) HasActiveClaim(taskID string, citizenID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM task_claims WHERE task_id = ? AND citizen_id = ? AND outcome IS NULL`,
		taskID, citizenID,
	).Scan(&n)
	return n > 0, err
}

// CountActiveClaims returns the number of open (outcome = NULL)
// claim slots on a task. Used by the router to cap multi-citizen
// claims at the declared citizens count.
func (s *Store) CountActiveClaims(taskID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM task_claims WHERE task_id = ? AND outcome IS NULL`,
		taskID,
	).Scan(&n)
	return n, err
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

// MarkTasksSkipped is the Phase E.2 skip-cascade's state flip.
// Called by performSkipCascade after a vote resolves with an
// `activates:` option that leaves some branch tasks stranded.
// Each task in skipIDs transitions to SKIPPED (terminal) and has
// its claim/result fields cleared so stale provenance doesn't
// leak into subsequent re-runs if the vote is later invalidated.
// Tasks already in a terminal state (accepted / skipped / invalid)
// are left alone — only non-terminal rows flip.
func (s *Store) MarkTasksSkipped(skipIDs []string) (int, error) {
	if len(skipIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	for _, id := range skipIDs {
		var state string
		err := tx.QueryRow(`SELECT state FROM tasks WHERE id = ?`, id).Scan(&state)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, err
		}
		switch TaskState(state) {
		case TaskAccepted, TaskSkipped, TaskInvalid, TaskInvalidated:
			// Already terminal — leave alone.
			continue
		}
		if _, err := tx.Exec(
			`UPDATE tasks SET state = 'skipped', claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL WHERE id = ?`,
			id,
		); err != nil {
			return 0, err
		}
		count++
	}
	return count, tx.Commit()
}

// ResetSkippedTasksToPending flips every SKIPPED task in the given
// set back to PENDING. Used during invalidation cascade when a
// previously-resolved vote gets invalidated — the branches that
// were dead because of the vote need to reconsider themselves.
// The scheduler's UpdateReadyTasks will then re-evaluate them
// from PENDING and promote to READY once their deps line up.
func (s *Store) ResetSkippedTasksToPending(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(
			`UPDATE tasks SET state = 'pending' WHERE id = ? AND state = 'skipped'`,
			id,
		); err != nil {
			return err
		}
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
	// fresh citizen sees no stale provenance when they claim. Also
	// clears review_decision and vote_choice so a re-run of a
	// review/vote task lands a fresh verdict instead of re-using
	// the previous one.
	if _, err = tx.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL, review_decision = '', vote_choice = '' WHERE id = ?`,
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
			`UPDATE tasks SET state = 'pending', claimed_by = NULL, claimed_at = NULL, submitted_at = NULL, result_path = NULL, review_decision = '', vote_choice = '' WHERE id = ?`,
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
	// Pull project_id once for this run — every pending task in the run
	// belongs to the same project, and we need it to check whether any
	// reads_artifacts have been produced yet.
	var projectID int64
	if err := s.db.QueryRow(`SELECT project_id FROM runs WHERE id = ?`, runID).Scan(&projectID); err != nil {
		return 0, fmt.Errorf("loading run project: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT id, depends_on, reads_artifacts FROM tasks WHERE run_id = ? AND state = 'pending'`, runID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type pendingTask struct {
		id             string
		dependsOn      string
		readsArtifacts string
	}
	var pending []pendingTask
	for rows.Next() {
		var pt pendingTask
		if err := rows.Scan(&pt.id, &pt.dependsOn, &pt.readsArtifacts); err != nil {
			return 0, err
		}
		pending = append(pending, pt)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// "Satisfied" parents for dependency readiness include both
	// ACCEPTED (the normal terminal) AND SKIPPED (the Phase E.2
	// vote skip-cascade terminal). A task whose parent was
	// skipped-by-gate is still unblocked — the gate decided that
	// branch is done, not pending. If the child references a
	// skipped parent's content via a template, resolution will
	// fail loudly at claim time; that's an author-error, not a
	// scheduler concern.
	acceptedRows, err := s.db.Query(
		`SELECT id FROM tasks WHERE run_id = ? AND state IN ('accepted', 'skipped')`, runID,
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
		allDone := true

		if pt.dependsOn != "" {
			for _, dep := range strings.Split(pt.dependsOn, ",") {
				if !accepted[strings.TrimSpace(dep)] {
					allDone = false
					break
				}
			}
		}

		// Artifact-aware gating: a task that declares reads_artifacts
		// stays PENDING until every declared path exists in the
		// project's artifacts index. This covers (a) fresh tasks in
		// the run that read not-yet-written artifacts and (b) tasks
		// in other runs that read artifacts from this one. Without
		// this, cross-run readers land in READY immediately (the
		// known limitation from iteration 3.2).
		if allDone && pt.readsArtifacts != "" {
			var paths []string
			if err := json.Unmarshal([]byte(pt.readsArtifacts), &paths); err == nil {
				for _, p := range paths {
					if p == "" {
						continue
					}
					a, err := s.GetArtifact(projectID, p)
					if err != nil {
						return count, fmt.Errorf("checking artifact %s: %w", p, err)
					}
					if a == nil {
						allDone = false
						break
					}
				}
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

// UpdateCitizenProfile updates a citizen's name and/or email with
// merge semantics: nil pointers mean "leave this column alone",
// non-nil pointers mean "set to the pointed-at value (which may
// be the empty string if the caller explicitly wants to clear it)".
// Username is intentionally immutable — never touched.
func (s *Store) UpdateCitizenProfile(id int64, name, email *string) error {
	if name == nil && email == nil {
		return nil
	}
	if email != nil && *email != "" {
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM citizens WHERE email = ? AND id != ?`, *email, id).Scan(&count)
		if count > 0 {
			return fmt.Errorf("a citizen with this email already exists")
		}
	}
	sets := []string{}
	args := []interface{}{}
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *name)
	}
	if email != nil {
		sets = append(sets, "email = ?")
		args = append(args, *email)
	}
	args = append(args, id)
	_, err := s.db.Exec(
		"UPDATE citizens SET "+strings.Join(sets, ", ")+" WHERE id = ?",
		args...,
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
		var anonymizeInt int
		if err := rows.Scan(&t.ID, &t.RunID, &t.Seq, &t.TaskDefID, &t.InstanceKey, &t.InstanceParams, &ref, &t.Action,
			&prompt, &userPrompt, &script, &outputs, &requirements, &t.ResultType, &timeout,
			&t.State, &claimedBy, &claimedAt, &submittedAt, &resultPath, &t.CommitSHA, &t.DependsOn,
			&t.ReadsArtifacts, &t.WritesArtifacts,
			&t.AssignTo, &t.RequireRole, &t.ReviewsTarget, &t.ReviewDecision,
			&t.VoteOptions, &t.VoteChoice, &t.Citizens, &t.MinQuorum, &t.VoteThreshold, &t.VoteDeadline,
			&anonymizeInt, &t.Visibility,
			&t.CreatedAt); err != nil {
			return nil, err
		}
		t.Anonymize = anonymizeInt != 0
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
// fields (last_writer, last_task_id, last_run_id, commit_sha,
// updated_at) are refreshed and created_at is preserved.
func (s *Store) UpsertArtifact(a *ArtifactRecord) error {
	if a.ProjectID == 0 || a.Path == "" {
		return fmt.Errorf("UpsertArtifact: project_id and path are required")
	}
	_, err := s.db.Exec(
		`INSERT INTO artifacts (project_id, path, last_writer, last_task_id, last_run_id, commit_sha, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, path) DO UPDATE SET
		   last_writer  = excluded.last_writer,
		   last_task_id = excluded.last_task_id,
		   last_run_id  = excluded.last_run_id,
		   commit_sha   = excluded.commit_sha,
		   updated_at   = excluded.updated_at`,
		a.ProjectID, a.Path, a.LastWriter, a.LastTaskID, a.LastRunID, a.CommitSHA,
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
		`SELECT project_id, path, last_writer, last_task_id, last_run_id, commit_sha, created_at, updated_at
		 FROM artifacts WHERE project_id = ? AND path = ?`,
		projectID, path,
	).Scan(&a.ProjectID, &a.Path, &lastWriter, &lastTaskID, &lastRunID, &a.CommitSHA, &a.CreatedAt, &a.UpdatedAt)
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

// ListTasksWritingArtifact returns tasks in a project whose
// writes_artifacts declaration contains the given path. Mirror of
// ListTasksReadingArtifact but targeting the writer side — used by
// the iteration A DB-only invalidation to find candidate prior
// writers when the current artifact index pointer is being
// invalidated.
func (s *Store) ListTasksWritingArtifact(projectID int64, path string, acceptedOnly bool) ([]TaskRecord, error) {
	pattern := `%"` + path + `"%`
	query := `SELECT ` + taskColumns + ` FROM tasks
	          WHERE writes_artifacts LIKE ?
	            AND run_id IN (SELECT id FROM runs WHERE project_id = ?)`
	args := []interface{}{pattern, projectID}
	if acceptedOnly {
		query += ` AND state = 'accepted'`
	}
	query += ` ORDER BY submitted_at DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
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

// DeleteTasksByDefInRun removes every task row in the given
// run whose task_def_id matches. Used by the Phase J.1
// dynamic for_each materializer to clean up stale instances
// before re-materializing after an upstream invalidation +
// re-accept with a different output list. Also removes any
// associated task_claims rows so the re-materialization
// starts from a clean slate.
func (s *Store) DeleteTasksByDefInRun(runID int64, defID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Find the affected task IDs first so we can also delete
	// their task_claims rows (foreign-key-style cleanup even
	// though SQLite isn't enforcing it here).
	rows, err := tx.Query(
		`SELECT id FROM tasks WHERE run_id = ? AND task_def_id = ?`,
		runID, defID,
	)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM task_claims WHERE task_id = ?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM tasks WHERE run_id = ? AND task_def_id = ?`,
		runID, defID,
	); err != nil {
		return err
	}
	return tx.Commit()
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
	query := `SELECT project_id, path, last_writer, last_task_id, last_run_id, commit_sha, created_at, updated_at
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
		if err := rows.Scan(&a.ProjectID, &a.Path, &lastWriter, &lastTaskID, &lastRunID, &a.CommitSHA, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.LastWriter = lastWriter.Int64
		a.LastTaskID = lastTaskID.String
		a.LastRunID = lastRunID.Int64
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}
