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
	// Two DSN parameters carry the concurrent-write story:
	//
	//   _pragma=busy_timeout(5000)
	//     Per-connection wait-for-lock budget. Pool-wide
	//     because modernc applies _pragma DSN params on every
	//     new connection it opens. Without this, a writer
	//     contending with another writer fails fast with
	//     SQLITE_BUSY instead of waiting.
	//
	//   _txlock=immediate
	//     Makes db.Begin() issue `BEGIN IMMEDIATE` instead of
	//     the default `BEGIN DEFERRED`. This is the LOAD-
	//     BEARING bit for parallel execute_run.
	//
	//     Why: ApplyPlan runs mutations like applySetClaim
	//     that SELECT then INSERT in the same transaction.
	//     Under DEFERRED, the SELECT acquires a read snapshot
	//     and the INSERT later upgrades to a write — but if
	//     another transaction committed between the SELECT
	//     and the INSERT, SQLite returns SQLITE_BUSY_SNAPSHOT
	//     and busy_timeout does NOT retry it. Application
	//     would have to roll back and retry the whole
	//     transaction. IMMEDIATE acquires the writer lock
	//     upfront, before any reads, so snapshot drift can't
	//     happen — busy_timeout fully covers the remaining
	//     writer-vs-writer contention. See
	//     TestReadThenWriteInDeferredTxHitsSnapshotBusy for
	//     the failing repro that motivated this.
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}
	// Belt-and-suspenders busy_timeout PRAGMA. The DSN form
	// above is what makes the timeout pool-safe; this
	// startup exec ensures the FIRST connection (the one
	// the migration runs on) also has the timeout, in case
	// a future driver swap doesn't honor the DSN form.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("setting busy_timeout: %w", err)
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
		params TEXT NOT NULL DEFAULT '',
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
		branch TEXT NOT NULL DEFAULT 'main',
		path TEXT NOT NULL,
		last_writer INTEGER REFERENCES citizens(id),
		last_task_id TEXT,
		last_run_id INTEGER,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		-- tracked=1: content lives in git at commit_sha.
		-- tracked=0: artifact is produced but not committed (the
		-- task declared track:false); commit_sha stays empty.
		-- The index still records provenance so downstream
		-- readiness + history tools work uniformly.
		tracked INTEGER NOT NULL DEFAULT 1,
		PRIMARY KEY (project_id, branch, path)
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

	-- Project membership (Phase J — project_members). Flat two-tier
	-- role model: 'owner' (can remove members, promote/demote,
	-- transfer) and 'member' (can add members, read everything,
	-- claim/submit tasks, leave). Creator is auto-added as owner
	-- on CreateProject. Invariant: every project has ≥1 owner —
	-- last-owner leave/demote/remove is refused.
	--
	-- Role is stored as TEXT so future tiers (viewer, maintainer,
	-- etc.) can slot in without a schema migration. Permission
	-- checks live in one helper per action, not scattered.
	CREATE TABLE IF NOT EXISTS project_members (
		project_id INTEGER NOT NULL REFERENCES projects(id),
		citizen_id INTEGER NOT NULL REFERENCES citizens(id),
		role TEXT NOT NULL DEFAULT 'member',
		added_at TIMESTAMP NOT NULL,
		added_by INTEGER REFERENCES citizens(id),
		PRIMARY KEY (project_id, citizen_id)
	);
	CREATE INDEX IF NOT EXISTS idx_project_members_citizen ON project_members(citizen_id);

	-- Phase G: contribution events log. Append-only — events
	-- are never deleted, even when the underlying task is
	-- invalidated (the invalidation is recorded as a separate
	-- event). This mirrors the append-only git philosophy and
	-- gives future scoring functions a complete audit trail.
	CREATE TABLE IF NOT EXISTS contribution_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		citizen_id INTEGER NOT NULL,
		event_type TEXT NOT NULL,
		event_subtype TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '',
		run_id INTEGER NOT NULL DEFAULT 0,
		project_id INTEGER NOT NULL DEFAULT 0,
		metadata TEXT NOT NULL DEFAULT '{}',
		created_at TIMESTAMP NOT NULL
	);

	-- operator/model design — tokens move out of the
	-- citizens row into their own table. Multiple tokens per
	-- citizen (rotation, per-deployment labels), revocable
	-- (revoked_at IS NULL = active), audit-preserved (revoke =
	-- mark, never delete). The citizens.token column is left
	-- intact as a legacy mirror until a future cleanup phase can
	-- safely drop it. See docs/operator-model-design.md.
	CREATE TABLE IF NOT EXISTS tokens (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		citizen_id      INTEGER NOT NULL REFERENCES citizens(id),
		token           TEXT NOT NULL UNIQUE,
		label           TEXT NOT NULL DEFAULT '',
		issued_at       TIMESTAMP NOT NULL,
		revoked_at      TIMESTAMP,
		last_used_at    TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_run ON tasks(run_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);
	CREATE INDEX IF NOT EXISTS idx_tasks_claimed_by ON tasks(claimed_by);
	CREATE INDEX IF NOT EXISTS idx_task_claims_task ON task_claims(task_id);
	CREATE INDEX IF NOT EXISTS idx_citizens_token ON citizens(token);
	CREATE INDEX IF NOT EXISTS idx_tokens_citizen ON tokens(citizen_id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_citizens_email ON citizens(email) WHERE email IS NOT NULL AND email != '';
	CREATE UNIQUE INDEX IF NOT EXISTS idx_citizens_username ON citizens(username);
	CREATE INDEX IF NOT EXISTS idx_runs_project ON runs(project_id);
	CREATE INDEX IF NOT EXISTS idx_artifacts_project ON artifacts(project_id);
	CREATE INDEX IF NOT EXISTS idx_contribution_events_citizen ON contribution_events(citizen_id);
	CREATE INDEX IF NOT EXISTS idx_contribution_events_type ON contribution_events(event_type);
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
		`ALTER TABLE tasks ADD COLUMN fail_reason TEXT NOT NULL DEFAULT ''`,
		// Populated when a task is skipped because an upstream
		// went FAILED (review reject or enju_fail_task), so the
		// run_status formatter can render ⊘ "skipped (upstream
		// failed: X)" distinctly from vote-cascade skips (⚫).
		// Empty for vote-cascade skips.
		`ALTER TABLE tasks ADD COLUMN skip_reason TEXT NOT NULL DEFAULT ''`,
		// J.2 partial re-materialization — stashes the state a
		// parked task came from so a matched-key reconciliation
		// can losslessly restore. Empty for non-parked rows.
		`ALTER TABLE tasks ADD COLUMN parked_from_state TEXT NOT NULL DEFAULT ''`,
		// Phase H.1 — template provenance. Records the
		// repo-relative templates/*.yaml path this run was
		// instantiated from. Empty string for inline-YAML
		// submissions.
		`ALTER TABLE runs ADD COLUMN source_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN source_commit_sha TEXT NOT NULL DEFAULT ''`,
		// Compute-env-vars feature — persist the submitted
		// params map (JSON-encoded) so the executor can
		// rehydrate it into ENJU_PARAM_<name> env vars for
		// scripts that need to reach run-level context.
		`ALTER TABLE runs ADD COLUMN params TEXT NOT NULL DEFAULT ''`,
		// Task-level env: block (Phase J+). JSON-encoded
		// map[string]string. Populated on compute tasks that
		// declare env:; empty string for every other task.
		`ALTER TABLE tasks ADD COLUMN env TEXT NOT NULL DEFAULT ''`,
		// Branch-per-run model. Every project has a default
		// branch (new runs land there when the caller doesn't
		// specify otherwise); every run carries the branch it
		// commits to. Legacy rows default to 'main' so pre-
		// migration projects and runs keep working unchanged.
		// See docs/runs-and-branches.md.
		`ALTER TABLE projects ADD COLUMN default_branch TEXT NOT NULL DEFAULT 'main'`,
		`ALTER TABLE runs ADD COLUMN branch TEXT NOT NULL DEFAULT 'main'`,
		// Compute-task execution mode (Phase 4 of async compute).
		// Values: '' (non-compute or default-sync), 'sync', 'async'.
		// The yaml parser validates shape; this column stores it
		// raw so the execute handler can read yaml.ResolvedMode
		// off the task record without re-parsing the YAML.
		`ALTER TABLE tasks ADD COLUMN mode TEXT NOT NULL DEFAULT ''`,
		// Untracked artifacts (Phase B). Existing rows predate
		// the feature and are, by definition, committed to git —
		// default them to tracked=1 so the column's semantics
		// stay uniform across old and new data.
		`ALTER TABLE artifacts ADD COLUMN tracked INTEGER NOT NULL DEFAULT 1`,
		// Docker containerization (Phase A of containers).
		// Empty string = no container (run script on host).
		// Otherwise carries an image reference (biocontainers/samtools:1.18,
		// ghcr.io/org/tool@sha256:..., etc.) the wrapper hands to
		// `docker run` at execute time.
		`ALTER TABLE tasks ADD COLUMN container TEXT NOT NULL DEFAULT ''`,
		// Per-run slug for the self-documenting
		// enju/runs/{seq}-{slug}/ directory layout. Stored on
		// the run as the source of truth; denormalized onto
		// each task so engine.ComputeResultDir stays a pure
		// function of a single TaskRecord (no JOIN per
		// serialization). Empty slug falls back to "run" in
		// the layout helper, so old rows render as
		// enju/runs/{seq}-run/ — still parseable, just
		// doesn't advertise the template origin.
		`ALTER TABLE runs ADD COLUMN slug TEXT NOT NULL DEFAULT 'run'`,
		`ALTER TABLE tasks ADD COLUMN run_slug TEXT NOT NULL DEFAULT 'run'`,
		// operator/model design — citizen kind discriminator
		// + bot-ownership chain. Existing rows default to kind='human'
		// (the only kind that existed pre-migration) and parent_id=NULL.
		// Bots set parent_id to the owner citizen's id; models stay NULL
		// (they belong to the catalog, not to any owner). See
		// docs/operator-model-design.md.
		`ALTER TABLE citizens ADD COLUMN kind TEXT NOT NULL DEFAULT 'human'`,
		`ALTER TABLE citizens ADD COLUMN parent_id INTEGER REFERENCES citizens(id)`,
		// operator/model design — submission attribution.
		// task_claims.citizen_id is the operator (existing); model_id
		// is the LLM that produced the words for this submit (new).
		// Nullable: humans may submit without an LLM (hand-review);
		// bots must always have a model — that constraint is enforced
		// in applySetClaim / applyRecordSubmission since SQLite CHECK
		// constraints can't reference another table's columns.
		`ALTER TABLE task_claims ADD COLUMN model_id INTEGER REFERENCES citizens(id)`,
	}
	for _, q := range altered {
		if _, err := s.db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("schema alter %q: %w", q, err)
		}
	}

	// operator/model design — backfill the tokens table
	// from existing citizens.token values. Idempotent: only inserts
	// rows that don't already exist (keyed on citizen_id, since each
	// pre-migration citizen had exactly one token). After this
	// runs, existing humans authenticate via the tokens table the
	// same way they authenticate today via citizens.token.
	if _, err := s.db.Exec(`
		INSERT INTO tokens (citizen_id, token, label, issued_at)
		SELECT id, token, 'legacy', registered_at
		FROM citizens
		WHERE token != ''
		  AND NOT EXISTS (SELECT 1 FROM tokens WHERE tokens.citizen_id = citizens.id)
	`); err != nil {
		return fmt.Errorf("schema: backfill tokens: %w", err)
	}

	// operator/model design — seed a hand-curated model
	// catalog so submit-attribution has real model
	// citizens to reference. OpenRouter auto-fetch is deferred —
	// see docs/operator-model-design.md "OpenRouter catalog fetch"
	// in the deferred section. Local-mode users add their own
	// (Ollama, internal finetunes) via enju_register_model.
	if err := s.seedModelCitizens(); err != nil {
		return fmt.Errorf("schema: seed model citizens: %w", err)
	}

	// Serial-runs-per-branch invariant enforced at the DB
	// level. Partial unique index so only ACTIVE runs collide —
	// completed / failed runs on the same branch are fine.
	// Belt-and-suspenders alongside the application-level
	// ActiveRunOnBranch check in handleCreateRun, which races
	// under concurrent requests without this guard. Lives here
	// (not in the schema block above) because it references the
	// `branch` column which comes in via ALTER TABLE.
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_active_branch ON runs(project_id, branch) WHERE state = 'active'`); err != nil {
		return fmt.Errorf("schema: create idx_runs_active_branch: %w", err)
	}

	// Branch-per-run artifact index migration. SQLite can't
	// alter a PRIMARY KEY in place, so: check if the artifacts
	// table is missing the `branch` column (old schema keyed
	// by (project_id, path)), and if so rebuild it keyed by
	// (project_id, branch, path). All existing rows default
	// to branch="main" since that's the only branch pre-
	// migration runs targeted.
	hasBranch, err := columnExists(s.db, "artifacts", "branch")
	if err != nil {
		return fmt.Errorf("checking artifacts.branch column: %w", err)
	}
	if !hasBranch {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin artifacts migration: %w", err)
		}
		stmts := []string{
			`CREATE TABLE artifacts_v2 (
				project_id INTEGER NOT NULL REFERENCES projects(id),
				branch TEXT NOT NULL DEFAULT 'main',
				path TEXT NOT NULL,
				last_writer INTEGER REFERENCES citizens(id),
				last_task_id TEXT,
				last_run_id INTEGER,
				commit_sha TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMP NOT NULL,
				updated_at TIMESTAMP NOT NULL,
				PRIMARY KEY (project_id, branch, path)
			)`,
			`INSERT INTO artifacts_v2 (project_id, branch, path, last_writer, last_task_id, last_run_id, commit_sha, created_at, updated_at)
			 SELECT project_id, 'main', path, last_writer, last_task_id, last_run_id, commit_sha, created_at, updated_at FROM artifacts`,
			`DROP TABLE artifacts`,
			`ALTER TABLE artifacts_v2 RENAME TO artifacts`,
			`CREATE INDEX IF NOT EXISTS idx_artifacts_project ON artifacts(project_id)`,
		}
		for _, q := range stmts {
			if _, err := tx.Exec(q); err != nil {
				tx.Rollback()
				return fmt.Errorf("artifacts migration %q: %w", q, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit artifacts migration: %w", err)
		}
	}
	return nil
}

// columnExists reports whether a column is present in a SQLite
// table. Used by one-off schema migrations that need to detect
// "old shape vs new shape" because SQLite doesn't let you ALTER
// a PRIMARY KEY in place. A missing table is treated as
// "column absent" so fresh databases (which create the table
// with the new shape) skip the migration entirely.
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// --- Projects (long-lived containers) ---

// CreateProject creates a new long-lived project.
func (s *Store) CreateProject(p *ProjectRecord) (int64, error) {
	var remote sql.NullString
	if p.RemoteURL != "" {
		remote = sql.NullString{String: p.RemoteURL, Valid: true}
	}
	// Default branch fallback: empty in → "main" on disk. The
	// column default on the schema is also "main" but we set
	// it explicitly here so the canonical INSERT shape always
	// carries the value (simpler to reason about downstream).
	branch := p.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	result, err := s.db.Exec(
		`INSERT INTO projects (name, description, created_by, remote_url, default_branch, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Description, p.CreatedBy, remote, branch, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SetProjectDefaultBranch updates a project's default branch.
// Empty input defaults to "main" so the column is never left
// blank. Validation (shape, length) is the caller's job — the
// API handler calls validateBranchName before this point.
func (s *Store) SetProjectDefaultBranch(projectID int64, branch string) error {
	if branch == "" {
		branch = "main"
	}
	_, err := s.db.Exec(
		`UPDATE projects SET default_branch = ?, updated_at = ? WHERE id = ?`,
		branch, time.Now(), projectID,
	)
	return err
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
const projectSelectColumns = `id, name, description, created_by, remote_url, default_branch, created_at, updated_at`

// scanProject reads one project row from a scanner into p.
func scanProject(row interface {
	Scan(dest ...interface{}) error
}, p *ProjectRecord) error {
	var desc, createdBy, remote sql.NullString
	if err := row.Scan(&p.ID, &p.Name, &desc, &createdBy, &remote, &p.DefaultBranch, &p.CreatedAt, &p.UpdatedAt); err != nil {
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
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, created_at, updated_at
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
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Seq, &r.Name, &ref, &r.YAMLData, &r.RepoURL, &r.State, &r.SourcePath, &r.SourceCommitSHA, &r.Params, &r.Branch, &r.Slug, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Ref = ref.String
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// --- Project members ---

// AddProjectMember inserts a (project, citizen, role) row. addedBy is
// the citizens.id of the adder, or 0 for the creator self-add row.
// Returns an error if the citizen is already a member (the caller
// should check GetProjectMember first for a friendly path).
func (s *Store) AddProjectMember(projectID, citizenID int64, role ProjectRole, addedBy int64) error {
	if projectID == 0 || citizenID == 0 {
		return fmt.Errorf("project_id and citizen_id are required")
	}
	if role == "" {
		role = ProjectRoleMember
	}
	var addedByVal sql.NullInt64
	if addedBy != 0 {
		addedByVal = sql.NullInt64{Int64: addedBy, Valid: true}
	}
	_, err := s.db.Exec(
		`INSERT INTO project_members (project_id, citizen_id, role, added_at, added_by)
		 VALUES (?, ?, ?, ?, ?)`,
		projectID, citizenID, string(role), time.Now(), addedByVal,
	)
	return err
}

// RemoveProjectMember deletes the membership row. No-op if the
// citizen is not a member. Caller is responsible for invariant
// checks (last-owner refusal, active-claim release) — the store
// layer will happily drop the row as asked.
func (s *Store) RemoveProjectMember(projectID, citizenID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM project_members WHERE project_id = ? AND citizen_id = ?`,
		projectID, citizenID,
	)
	return err
}

// SetProjectMemberRole updates a citizen's role within a project.
// No-op if the citizen is not a member (zero rows affected; no
// error). Invariant enforcement (last-owner refusal) is the
// caller's responsibility.
func (s *Store) SetProjectMemberRole(projectID, citizenID int64, role ProjectRole) error {
	_, err := s.db.Exec(
		`UPDATE project_members SET role = ? WHERE project_id = ? AND citizen_id = ?`,
		string(role), projectID, citizenID,
	)
	return err
}

// GetProjectMember returns the membership record for (project, citizen)
// or nil if the citizen is not a member.
func (s *Store) GetProjectMember(projectID, citizenID int64) (*ProjectMemberRecord, error) {
	var m ProjectMemberRecord
	var addedBy sql.NullInt64
	err := s.db.QueryRow(
		`SELECT project_id, citizen_id, role, added_at, added_by
		 FROM project_members WHERE project_id = ? AND citizen_id = ?`,
		projectID, citizenID,
	).Scan(&m.ProjectID, &m.CitizenID, &m.Role, &m.AddedAt, &addedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.AddedBy = addedBy.Int64
	return &m, nil
}

// ListProjectMembers returns every membership row for a project,
// ordered by (role DESC, added_at ASC) so owners surface first.
func (s *Store) ListProjectMembers(projectID int64) ([]ProjectMemberRecord, error) {
	rows, err := s.db.Query(
		`SELECT project_id, citizen_id, role, added_at, added_by
		 FROM project_members WHERE project_id = ?
		 ORDER BY role DESC, added_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []ProjectMemberRecord
	for rows.Next() {
		var m ProjectMemberRecord
		var addedBy sql.NullInt64
		if err := rows.Scan(&m.ProjectID, &m.CitizenID, &m.Role, &m.AddedAt, &addedBy); err != nil {
			return nil, err
		}
		m.AddedBy = addedBy.Int64
		members = append(members, m)
	}
	return members, rows.Err()
}

// CountProjectOwners returns the number of owners on a project.
// Used for the >=1 owner invariant check on leave/demote/remove.
func (s *Store) CountProjectOwners(projectID int64) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM project_members WHERE project_id = ? AND role = ?`,
		projectID, string(ProjectRoleOwner),
	).Scan(&n)
	return n, err
}

// CountProjectMembers returns the total number of members on a
// project regardless of role. A project with zero members is a
// legacy project (pre-membership migration) — read/write gating
// treats it as open for backwards compatibility.
func (s *Store) CountProjectMembers(projectID int64) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM project_members WHERE project_id = ?`,
		projectID,
	).Scan(&n)
	return n, err
}

// ListProjectsForCitizen returns every project this citizen is a
// member of, ordered by project id. Used by enju_list_projects to
// scope the listing to projects the caller can see.
func (s *Store) ListProjectsForCitizen(citizenID int64) ([]ProjectRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+projectSelectColumns+` FROM projects
		 WHERE id IN (SELECT project_id FROM project_members WHERE citizen_id = ?)
		 ORDER BY id ASC`,
		citizenID,
	)
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

	// Branch fallback: empty in → "main". Mirrors the schema
	// default so the column is always populated with something
	// meaningful (not the empty string).
	branch := p.Branch
	if branch == "" {
		branch = "main"
	}
	slug := p.Slug
	if slug == "" {
		slug = "run"
	}
	result, err := tx.Exec(
		`INSERT INTO runs (project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProjectID, nextSeq, p.Name, p.Ref, p.YAMLData, p.RepoURL, p.State, p.SourcePath, p.SourceCommitSHA, p.Params, branch, slug, p.CreatedAt, p.UpdatedAt,
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
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, created_at, updated_at FROM runs WHERE id = ?`, id,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.Params, &p.Branch, &p.Slug, &p.CreatedAt, &p.UpdatedAt)
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
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, created_at, updated_at
		 FROM runs WHERE project_id = ? AND seq = ?`, projectID, seq,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.Params, &p.Branch, &p.Slug, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Ref = ref.String
	return &p, err
}

func (s *Store) ListRuns() ([]RunRecord, error) {
	rows, err := s.db.Query(`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, created_at, updated_at FROM runs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var p RunRecord
		var ref sql.NullString
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.Params, &p.Branch, &p.Slug, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Ref = ref.String
		runs = append(runs, p)
	}
	return runs, rows.Err()
}

// ActiveRunOnBranch returns the first ACTIVE run on the given
// project+branch pair, or nil if none exists. Used by
// handleCreateRun to enforce the serial-runs-per-branch
// invariant: a second run on the same branch would step on the
// first one's artifact writes, so we refuse it with a clear
// error pointing at the existing run.
func (s *Store) ActiveRunOnBranch(projectID int64, branch string) (*RunRecord, error) {
	var r RunRecord
	var ref sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, created_at, updated_at
		 FROM runs WHERE project_id = ? AND branch = ? AND state = 'active'
		 ORDER BY seq ASC LIMIT 1`,
		projectID, branch,
	).Scan(&r.ID, &r.ProjectID, &r.Seq, &r.Name, &ref, &r.YAMLData, &r.RepoURL, &r.State, &r.SourcePath, &r.SourceCommitSHA, &r.Params, &r.Branch, &r.Slug, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Ref = ref.String
	return &r, nil
}

// ListRunBranches returns every distinct branch used by runs in
// the given project. Used by the "auto" branch-name allocator
// to pick an unused run-N.
func (s *Store) ListRunBranches(projectID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT branch FROM runs WHERE project_id = ? ORDER BY branch ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
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
		`SELECT COUNT(*), COUNT(CASE WHEN state IN ('accepted', 'skipped', 'failed') THEN 1 END) FROM tasks WHERE run_id = ?`,
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

const taskColumns = `id, run_id, seq, task_def_id, instance_key, instance_params, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, commit_sha, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, reviews_target, review_decision, vote_options, vote_choice, citizens, min_quorum, vote_threshold, vote_deadline, anonymize, visibility, fail_reason, skip_reason, parked_from_state, env, mode, container, run_slug, created_at`

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
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, reviews_target, vote_options, citizens, min_quorum, vote_threshold, vote_deadline, anonymize, visibility, env, mode, container, run_slug, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RunID, t.Seq, t.TaskDefID, t.InstanceKey, t.InstanceParams, t.Ref, t.Action,
		t.Prompt, t.UserPrompt, t.Script, t.Outputs, t.Requirements, t.ResultType, t.Timeout,
		t.State, t.DependsOn, t.ReadsArtifacts, t.WritesArtifacts,
		t.AssignTo, t.RequireRole, t.ReviewsTarget,
		t.VoteOptions, citizens, t.MinQuorum, t.VoteThreshold, t.VoteDeadline,
		anonymize, t.Visibility, t.Env, t.Mode, t.Container, t.RunSlug,
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
		&anonymizeInt, &t.Visibility, &t.FailReason, &t.SkipReason, &t.ParkedFromState, &t.Env, &t.Mode, &t.Container, &t.RunSlug,
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
		`SELECT `+taskClaimColumns+`
		 FROM task_claims
		 WHERE task_id = ? AND outcome = 'completed'
		 ORDER BY submitted_at`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskClaims(rows)
}

// ListActiveClaims returns every open (outcome = NULL) claim row
// for a task. Used by the task response marshaller so MCP
// formatters can show "active claimants" on multi-citizen tasks
// that haven't resolved yet.
func (s *Store) ListActiveClaims(taskID string) ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+taskClaimColumns+`
		 FROM task_claims
		 WHERE task_id = ? AND outcome IS NULL
		 ORDER BY claimed_at`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskClaims(rows)
}

// taskClaimColumns is the canonical column list for task_claims
// reads. model_id is the current attribution column; future
// operator/model-design columns (route, model_resolved, paid_by)
// extend this list when they ship.
const taskClaimColumns = `id, task_id, citizen_id, claimed_at, deadline, outcome, submitted_at, option, content, model_id`

// scanTaskClaims is the shared scanner used by ListVoteSubmissions
// and ListActiveClaims. Centralizing the scan keeps the two paths
// in sync — adding a column means one edit, not two.
func scanTaskClaims(rows *sql.Rows) ([]TaskClaimRecord, error) {
	var out []TaskClaimRecord
	for rows.Next() {
		var r TaskClaimRecord
		var outcome sql.NullString
		var submittedAt sql.NullTime
		var modelID sql.NullInt64
		if err := rows.Scan(
			&r.ID, &r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline,
			&outcome, &submittedAt, &r.Option, &r.Content, &modelID,
		); err != nil {
			return nil, err
		}
		if outcome.Valid {
			r.Outcome = outcome.String
		}
		if submittedAt.Valid {
			r.SubmittedAt = &submittedAt.Time
		}
		if modelID.Valid {
			v := modelID.Int64
			r.ModelID = &v
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
	// Pull project_id + branch once for this run — every
	// pending task shares both, and the artifact index lookups
	// below need (project, branch) to find the right row.
	var projectID int64
	var runBranch string
	if err := s.db.QueryRow(`SELECT project_id, branch FROM runs WHERE id = ?`, runID).Scan(&projectID, &runBranch); err != nil {
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
	// branch is done, not pending. FAILED is NOT satisfied — a
	// failed upstream should block its downstream, not unblock it.
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
					a, err := s.GetArtifact(projectID, runBranch, p)
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
// scan functions expect. Keep this in sync with CitizenRecord. Each
// column is qualified with `citizens.` so the same list works both
// for plain `FROM citizens` queries and for joins where another
// table has overlapping column names (notably tokens.id and
// tokens.token, which would collide otherwise).
const citizenColumns = `citizens.id, citizens.username, citizens.name, citizens.email, citizens.role, citizens.token, citizens.score, citizens.tasks_completed, citizens.tasks_rejected, citizens.tasks_timed_out, citizens.tasks_released, citizens.tokens_contributed, citizens.registered_at, citizens.last_seen, citizens.kind, citizens.parent_id`

// scanCitizen reads one citizen row into a CitizenRecord. Used by
// GetCitizen, GetCitizenByUsername, GetCitizenByToken.
func scanCitizen(row *sql.Row) (*CitizenRecord, error) {
	var p CitizenRecord
	var email, role sql.NullString
	var parentID sql.NullInt64
	err := row.Scan(
		&p.ID, &p.Username, &p.Name, &email, &role, &p.Token, &p.Score,
		&p.TasksCompleted, &p.TasksRejected, &p.TasksTimedOut, &p.TasksReleased,
		&p.TokensContrib, &p.RegisteredAt, &p.LastSeen,
		&p.Kind, &parentID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Email = email.String
	p.Role = role.String
	if parentID.Valid {
		v := parentID.Int64
		p.ParentID = &v
	}
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

	// Insert citizen + matching tokens row in one transaction so we
	// can't leave a citizen authenticatable through citizens.token
	// (legacy mirror) but invisible to the tokens-table auth path.
	// Model citizens won't have a token; the conditional insert
	// below skips the tokens row when p.Token is empty.
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// operator/model design — kind and parent_id MUST be in the INSERT
	// or the schema's column defaults silently override the
	// caller's intent (kind=>'human', parent_id=>NULL). That's
	// the exact bug a previous iteration shipped: enju_register_bot
	// produced kind='human' rows that ListBotsByParent never
	// returned, and requireModelForBot became dead code. The Phase
	// 1.1 test already pinned that scanCitizen reads these
	// columns; this insert path is the matching write side.
	kind := p.Kind
	if kind == "" {
		kind = "human"
	}
	res, err := tx.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		p.Username, p.Name, p.Email, role, p.Token, p.RegisteredAt, p.LastSeen, kind, nullableInt64(p.ParentID),
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if p.Token != "" {
		if _, err := tx.Exec(
			`INSERT INTO tokens (citizen_id, token, label, issued_at) VALUES (?, ?, '', ?)`,
			id, p.Token, p.RegisteredAt,
		); err != nil {
			return 0, fmt.Errorf("issue initial token: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit citizen+token: %w", err)
	}
	return id, nil
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

// GetCitizenByToken resolves a token to its owning citizen for
// authentication. Reads from the tokens table and
// rejects revoked tokens — `revoked_at IS NULL` is the active
// filter. Returns (nil, nil) when the token doesn't exist OR has
// been revoked; the auth middleware treats both the same way.
//
// The citizens.token column is no longer consulted on this path.
// It remains populated as a legacy mirror until a future cleanup
// phase drops it; new revocations / rotations only show up on the
// tokens table.
func (s *Store) GetCitizenByToken(token string) (*CitizenRecord, error) {
	return scanCitizen(s.db.QueryRow(
		`SELECT `+citizenColumns+`
		   FROM citizens
		   JOIN tokens ON tokens.citizen_id = citizens.id
		  WHERE tokens.token = ?
		    AND tokens.revoked_at IS NULL`,
		token,
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

// --- Tokens ---
//
// operator/model design — tokens table CRUD. The auth
// path (GetCitizenByToken) joins through this table; the helpers
// below are the management surface (issue, revoke, list) that
// the bot-registration tools lean on.

// IssueToken creates a new token row for the given citizen. Returns
// the new token's primary key. Multiple active tokens per citizen
// are allowed — used for rotation (issue new, distribute, revoke
// old) and per-deployment labels (e.g. "ci-server", "laptop").
// Token uniqueness is enforced by the schema; callers must supply
// an unguessable random value.
func (s *Store) IssueToken(citizenID int64, token, label string) (int64, error) {
	if token == "" {
		return 0, fmt.Errorf("token must be non-empty")
	}
	res, err := s.db.Exec(
		`INSERT INTO tokens (citizen_id, token, label, issued_at) VALUES (?, ?, ?, ?)`,
		citizenID, token, label, time.Now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RevokeToken marks a token as revoked. Subsequent
// GetCitizenByToken calls return nil for the same token string.
// The row is preserved for audit (never deleted). Idempotent: a
// double-revoke is a no-op (the WHERE clause filters already-
// revoked rows so revoked_at doesn't get overwritten with a later
// timestamp).
func (s *Store) RevokeToken(tokenID int64) error {
	_, err := s.db.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now(), tokenID,
	)
	return err
}

// RevokeTokenByValue is the same as RevokeToken but keyed by the
// token string instead of the row id. Convenience for callers (CLI,
// API) that hold the token but not its row id.
func (s *Store) RevokeTokenByValue(token string) error {
	_, err := s.db.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE token = ? AND revoked_at IS NULL`,
		time.Now(), token,
	)
	return err
}

// ListTokensByCitizen returns all tokens for the given citizen,
// active and revoked, most recently issued first. Callers that
// only want active tokens filter on RevokedAt == nil.
func (s *Store) ListTokensByCitizen(citizenID int64) ([]TokenRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, citizen_id, token, label, issued_at, revoked_at, last_used_at
		   FROM tokens WHERE citizen_id = ? ORDER BY issued_at DESC`,
		citizenID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRecord
	for rows.Next() {
		var t TokenRecord
		var label sql.NullString
		var revokedAt, lastUsedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.CitizenID, &t.Token, &label, &t.IssuedAt, &revokedAt, &lastUsedAt); err != nil {
			return nil, err
		}
		t.Label = label.String
		if revokedAt.Valid {
			v := revokedAt.Time
			t.RevokedAt = &v
		}
		if lastUsedAt.Valid {
			v := lastUsedAt.Time
			t.LastUsedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- Model catalog ---
//
// operator/model design — model citizens are LLM catalog
// entries (kind='model'), passive: no token, can't claim, can't
// submit. They exist to be REFERENCED on submission rows for
// per-submit attribution. Seeded with a hand-curated list at
// first migration; users extend the catalog via the
// enju_register_model MCP tool for local models the seed
// doesn't cover (Ollama, lab finetunes, etc.).

// modelCatalogSeed is the hand-curated set of popular models inserted
// at first migration. Versions match the model identifiers users
// would type at the -model flag today; "Display Name" is the
// human-readable form for run_status / dashboards. Adding to this
// list is non-breaking — seedModelCitizens is idempotent (skips
// usernames already in the table), so a coordinator restart picks
// up new entries without touching existing ones.
//
// Models with provider-prefixed identifiers (e.g. OpenRouter's
// "anthropic/claude-opus-4-7") are stored without the prefix here.
// Provider routing is in the deferred section of the design doc;
// when it ships, the prefix becomes a separate column on the
// submission row, not part of the model's identity.
var modelCatalogSeed = []struct {
	Username string
	Name     string
}{
	{"claude-opus-4-7", "Claude Opus 4.7"},
	{"claude-sonnet-4-6", "Claude Sonnet 4.6"},
	{"claude-haiku-4-5", "Claude Haiku 4.5"},
	{"gpt-4o", "GPT-4o"},
	{"gpt-4-turbo", "GPT-4 Turbo"},
	{"gemini-2-5-pro", "Gemini 2.5 Pro"},
	{"llama-3-1-70b", "Llama 3.1 70B"},
	{"deepseek-v3", "DeepSeek V3"},
	{"mistral-large", "Mistral Large"},
	{"qwen-2-5-72b", "Qwen 2.5 72B"},
}

// seedModelCitizens inserts the hand-curated catalog entries that
// don't already exist. Idempotent on username collision. Called from
// migrate() after schema is in place.
func (s *Store) seedModelCitizens() error {
	for _, m := range modelCatalogSeed {
		if err := s.upsertModelCitizen(m.Username, m.Name); err != nil {
			return fmt.Errorf("seed %s: %w", m.Username, err)
		}
	}
	return nil
}

// upsertModelCitizen inserts a model-kind citizen if no row with the
// given username exists. The token column gets a non-functional
// placeholder ("model:<username>") to satisfy the legacy NOT NULL
// UNIQUE constraint on citizens.token; the placeholder NEVER
// authenticates because the auth path queries the tokens table
// and we deliberately don't insert a tokens row for
// model citizens.
//
// SECURITY GOTCHA-OF-FUTURE: the placeholder string is PREDICTABLE
// ("model:gpt-4o", "model:claude-opus-4-7", etc. — derivable from
// the public catalog). If a future maintainer "fixes" the auth path
// to fall back to citizens.token (e.g., for backward compatibility
// during a migration), every model citizen's token becomes
// guessable instantly. Anyone who could enumerate model usernames
// could authenticate as them and submit on their behalf.
//
// Mitigation: never restore citizens.token as an auth source. The
// tokens table is the only authority. The legacy column is on a
// path to be dropped entirely in a future cleanup phase. If you're
// touching auth and considering "fall back to citizens.token" — DO
// NOT. Read this comment first; the placeholder rule depends on it.
func (s *Store) upsertModelCitizen(username, displayName string) error {
	if err := ValidateUsername(username); err != nil {
		return err
	}
	var existingID int64
	err := s.db.QueryRow(`SELECT id FROM citizens WHERE username = ?`, username).Scan(&existingID)
	if err == nil {
		return nil // already in the catalog
	}
	if err != sql.ErrNoRows {
		return err
	}
	now := time.Now()
	_, err = s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, 'model')`,
		username, displayName, "model:"+username, now, now,
	)
	return err
}

// CreateModelCitizen registers a new model in the catalog. Called by
// the API handler behind enju_register_model (any authenticated
// citizen in local mode; hosted-mode policy gating deferred — see
// docs/operator-model-design.md). Returns the new citizen ID and
// the standard "username taken" error on conflict.
func (s *Store) CreateModelCitizen(username, displayName string) (int64, error) {
	if err := ValidateUsername(username); err != nil {
		return 0, err
	}
	var existingID int64
	err := s.db.QueryRow(`SELECT id FROM citizens WHERE username = ?`, username).Scan(&existingID)
	if err == nil {
		return 0, fmt.Errorf("username %q is already taken", username)
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, 'model')`,
		username, displayName, "model:"+username, now, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListBotsByParent returns every kind='bot' citizen whose parent_id
// matches the given citizen, ordered by registration time (newest
// first). Used by enju_my_bots and revocation cascades. Empty slice
// if the parent has no bots — not an error.
func (s *Store) ListBotsByParent(parentID int64) ([]CitizenRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+citizenColumns+` FROM citizens WHERE kind = 'bot' AND parent_id = ? ORDER BY registered_at DESC`,
		parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CitizenRecord
	for rows.Next() {
		var p CitizenRecord
		var email, role sql.NullString
		var pid sql.NullInt64
		if err := rows.Scan(
			&p.ID, &p.Username, &p.Name, &email, &role, &p.Token, &p.Score,
			&p.TasksCompleted, &p.TasksRejected, &p.TasksTimedOut, &p.TasksReleased,
			&p.TokensContrib, &p.RegisteredAt, &p.LastSeen,
			&p.Kind, &pid,
		); err != nil {
			return nil, err
		}
		p.Email = email.String
		p.Role = role.String
		if pid.Valid {
			v := pid.Int64
			p.ParentID = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LookupTokenOwner resolves a token to its owning citizen ID,
// regardless of revoked state. Used by revoke endpoint authorization
// (caller must own the token they're revoking). Returns 0 (not 0,
// nil — distinct from a real error) when the token doesn't exist.
//
// Either token (string value) or tokenID (row id) must be non-zero;
// the other is ignored. The endpoint accepts both forms because
// callers may have either: list_tokens returns rows with ids; the
// CLI/agent that holds the token at compromise time has only the
// string.
func (s *Store) LookupTokenOwner(token string, tokenID int64) (int64, error) {
	var ownerID int64
	var err error
	if tokenID != 0 {
		err = s.db.QueryRow(`SELECT citizen_id FROM tokens WHERE id = ?`, tokenID).Scan(&ownerID)
	} else if token != "" {
		err = s.db.QueryRow(`SELECT citizen_id FROM tokens WHERE token = ?`, token).Scan(&ownerID)
	} else {
		return 0, fmt.Errorf("either token or tokenID required")
	}
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return ownerID, nil
}

// DB exposes the underlying *sql.DB for ad-hoc queries. Reserved
// for narrow cases (router needs to update a tokens row's label
// in place during bot registration). New callers should prefer
// adding a typed helper.
func (s *Store) DB() *sql.DB { return s.db }

// ListModelCitizens returns all kind='model' citizens in
// alphabetical-by-username order. Used by run_status / dashboard /
// `enju_list_models` to render the catalog. Filters out
// soft-deleted models when that's a concept (not yet — flagged for
// future).
func (s *Store) ListModelCitizens() ([]CitizenRecord, error) {
	rows, err := s.db.Query(
		`SELECT ` + citizenColumns + ` FROM citizens WHERE kind = 'model' ORDER BY username`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CitizenRecord
	for rows.Next() {
		var p CitizenRecord
		var email, role sql.NullString
		var parentID sql.NullInt64
		if err := rows.Scan(
			&p.ID, &p.Username, &p.Name, &email, &role, &p.Token, &p.Score,
			&p.TasksCompleted, &p.TasksRejected, &p.TasksTimedOut, &p.TasksReleased,
			&p.TokensContrib, &p.RegisteredAt, &p.LastSeen,
			&p.Kind, &parentID,
		); err != nil {
			return nil, err
		}
		p.Email = email.String
		p.Role = role.String
		if parentID.Valid {
			v := parentID.Int64
			p.ParentID = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
			&anonymizeInt, &t.Visibility, &t.FailReason, &t.SkipReason, &t.ParkedFromState, &t.Env, &t.Mode, &t.Container, &t.RunSlug,
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

// The canonical write path for artifact-index rows is
// applyMoveArtifact in apply.go (driven by MoveArtifact plan
// mutations). Direct-write helpers lived here historically but
// went unused once the plan-apply machinery took over, so
// they've been removed to keep writes funneled through one
// idempotent code path.

// GetArtifact looks up one artifact's index row by
// (project_id, branch, path). Empty branch resolves to "main"
// for back-compat with callers that don't know the branch yet.
// Returns nil if no matching row exists.
func (s *Store) GetArtifact(projectID int64, branch, path string) (*ArtifactRecord, error) {
	if branch == "" {
		branch = "main"
	}
	var a ArtifactRecord
	var lastTaskID sql.NullString
	var lastWriter, lastRunID sql.NullInt64
	var trackedInt int
	err := s.db.QueryRow(
		`SELECT project_id, branch, path, last_writer, last_task_id, last_run_id, commit_sha, tracked, created_at, updated_at
		 FROM artifacts WHERE project_id = ? AND branch = ? AND path = ?`,
		projectID, branch, path,
	).Scan(&a.ProjectID, &a.Branch, &a.Path, &lastWriter, &lastTaskID, &lastRunID, &a.CommitSHA, &trackedInt, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.LastWriter = lastWriter.Int64
	a.LastTaskID = lastTaskID.String
	a.LastRunID = lastRunID.Int64
	a.Tracked = trackedInt != 0
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

// ListTasksReadingArtifact was removed with the branch-per-run
// model: cross-run reader cascade no longer exists, so no
// caller needs to enumerate readers across runs. Within a
// single run the reads_artifacts declaration plus UpdateReadyTasks
// gating is enough to sequence things correctly.

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
// DeleteArtifact removes an artifact index row keyed by
// (project_id, branch, path). Empty branch resolves to "main".
func (s *Store) DeleteArtifact(projectID int64, branch, path string) error {
	if branch == "" {
		branch = "main"
	}
	_, err := s.db.Exec(
		`DELETE FROM artifacts WHERE project_id = ? AND branch = ? AND path = ?`,
		projectID, branch, path,
	)
	return err
}

// ListArtifactsByProject returns all artifacts for a project on
// a specific branch, ordered by path. Empty branch resolves to
// "main". If pathPrefix is non-empty, only artifacts whose path
// starts with it are returned (useful for listing a directory
// subtree).
func (s *Store) ListArtifactsByProject(projectID int64, branch, pathPrefix string) ([]ArtifactRecord, error) {
	if branch == "" {
		branch = "main"
	}
	query := `SELECT project_id, branch, path, last_writer, last_task_id, last_run_id, commit_sha, tracked, created_at, updated_at
	          FROM artifacts WHERE project_id = ? AND branch = ?`
	args := []interface{}{projectID, branch}
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
		var trackedInt int
		if err := rows.Scan(&a.ProjectID, &a.Branch, &a.Path, &lastWriter, &lastTaskID, &lastRunID, &a.CommitSHA, &trackedInt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.LastWriter = lastWriter.Int64
		a.LastTaskID = lastTaskID.String
		a.LastRunID = lastRunID.Int64
		a.Tracked = trackedInt != 0
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}
