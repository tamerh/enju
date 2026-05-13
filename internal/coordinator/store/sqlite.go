package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/layout"
	_ "modernc.org/sqlite"
)

// usernameRe matches the GitHub username rules:
//  - 1 to 39 characters
//  - Starts and ends with alphanumeric
//  - Middle can contain alphanumerics and hyphens
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

// Store is the SQLite-backed state store. A sibling
// EventStore (typically SQLiteEventStore) lives in its own
// database file with its own connection pool; callers that
// need events go through Store.Events(). The two are
// completely independent — events failures cannot affect
// state operations and vice versa. See eventstore.go for
// the architectural contract.
type Store struct {
	db   *sql.DB
	events EventStore
}

// New creates a new Store and initializes the schema.
//
// The events DB is NOT opened here — call AttachEventStore
// after construction to wire it up. Callers that don't need
// events (unit tests, local-only fixtures) can leave it
// unset; Store.Events() returns a no-op store in that case.
func New(dbPath string) (*Store, error) {
	// Two DSN parameters carry the concurrent-write story:
	//
	//  _pragma=busy_timeout(5000)
	//   Per-connection wait-for-lock budget. Pool-wide
	//   because modernc applies _pragma DSN params on every
	//   new connection it opens. Without this, a writer
	//   contending with another writer fails fast with
	//   SQLITE_BUSY instead of waiting.
	//
	//  _txlock=immediate
	//   Makes db.Begin() issue `BEGIN IMMEDIATE` instead of
	//   the default `BEGIN DEFERRED`. This is the LOAD-
	//   BEARING bit for parallel execute_run.
	//
	//   Why: ApplyPlan runs mutations like applySetClaim
	//   that SELECT then INSERT in the same transaction.
	//   Under DEFERRED, the SELECT acquires a read snapshot
	//   and the INSERT later upgrades to a write — but if
	//   another transaction committed between the SELECT
	//   and the INSERT, SQLite returns SQLITE_BUSY_SNAPSHOT
	//   and busy_timeout does NOT retry it. Application
	//   would have to roll back and retry the whole
	//   transaction. IMMEDIATE acquires the writer lock
	//   upfront, before any reads, so snapshot drift can't
	//   happen — busy_timeout fully covers the remaining
	//   writer-vs-writer contention. See
	//   TestReadThenWriteInDeferredTxHitsSnapshotBusy for
	//   the failing repro that motivated this.
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
	if err := s.initSchema(); err != nil {
		return nil, fmt.Errorf("schema init: %w", err)
	}
	return s, nil
}

// AttachEventStore wires an EventStore (typically opened via
// NewSQLiteEventStore against ~/.enju/events.db) to this state
// store. Called by the coordinator's startup path AFTER New
// returns. Optional — if no event store is attached, Events()
// returns a no-op store that silently drops all writes (used
// by unit tests and offline fixtures that don't care about
// the audit ledger).
func (s *Store) AttachEventStore(events EventStore) {
	s.events = events
}

// Events returns the attached EventStore, or a no-op store if
// none has been attached. Callers can always call Record on
// the returned EventStore — it never panics, never errors.
func (s *Store) Events() EventStore {
	if s.events == nil {
		return noopEventStore{}
	}
	return s.events
}

// Close closes the database connection. The attached
// EventStore (if any) is NOT closed here — the coordinator's
// shutdown path closes both stores in order: events first
// (so any final emissions drain), then state. Closing them
// in the wrong order on a busy system would lose late
// events.
func (s *Store) Close() error {
	return s.db.Close()
}

// initSchema is the schema-bootstrap function (formerly named
// migrate). Despite its old name it does no versioned data
// migration — every statement is CREATE TABLE IF NOT EXISTS,
// CREATE INDEX IF NOT EXISTS, or ALTER TABLE … ADD COLUMN
// guarded by "duplicate column" error tolerance. Plus the
// model-citizen catalog seed.
//
// Lifecycle position: runs once inside New() at startup,
// before the EventStore is attached and before any service
// caller exists. That's why the in-function s.db.Exec calls
// are NOT chokepoint violations — the chokepoint contract
// ("every state mutation flows through ApplyPlan and emits
// an audit event") applies to runtime state mutations, not
// to schema DDL or pre-EventStore seed inserts.
func (s *Store) initSchema() error {
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
		-- last_seen is reserved for a future presence indicator.
		-- Populated on registration only; not updated per-request
		-- (the per-API UPDATE was costly with no readers). See
		-- CitizenRecord.LastSeen in models.go.
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
		option TEXT NOT NULL DEFAULT ''
	);

	-- Phase 6c — task_submissions captures per-attempt
	-- submission state. Living-workflow design intent: an
	-- iteration is one accept-cycle (claim → maybe-revise →
	-- terminal); within an iteration there can be multiple
	-- submission attempts (e.g. submit → request_changes →
	-- revise → re-submit → approve). Each attempt is a row
	-- here, hung off the claim row by claim_id.
	--
	-- Coexistence with the denormalized fields on task_claims
	-- (submitted_at, commit_sha, option, decision, model_id):
	-- applyRecordSubmission writes both. The task_submissions
	-- row is the audit-of-record (each attempt is preserved);
	-- the task_claims fields hold the "latest attempt"
	-- denormalization for legacy readers that haven't been
	-- migrated to JOIN with task_submissions yet (notably
	-- ListVoteSubmissions and ListActiveClaims via
	-- taskClaimColumns). Future cleanup pass can drop the
	-- duplicated columns once every reader pulls from
	-- task_submissions.
	--
	-- Schema note: there is no content column on either table.
	-- Submission prose lives in git as the result.md committed
	-- by the fat-client at the recorded commit_sha — that is
	-- the canonical truth (ARCHITECTURE.md #3, "coordinator
	-- never stores content"). Multi-citizen fan-in
	-- (workspace/resolve.go) reads each citizen's result.md from
	-- their commit_sha. See ARCHITECTURE.md #25 (audit log +
	-- state DB + git, not event sourcing) for the broader
	-- architecture.
	CREATE TABLE IF NOT EXISTS task_submissions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		claim_id INTEGER NOT NULL REFERENCES task_claims(id) ON DELETE CASCADE,
		submitted_at TIMESTAMP NOT NULL,
		commit_sha TEXT NOT NULL DEFAULT '',
		decision TEXT NOT NULL DEFAULT '',
		option TEXT NOT NULL DEFAULT '',
		model_id INTEGER REFERENCES citizens(id)
	);
	CREATE INDEX IF NOT EXISTS idx_task_submissions_claim ON task_submissions(claim_id);

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
	-- (, 2026-04-30) Audit events moved to a separate
	-- subsystem. State.db no longer carries the
	-- events table; emissions and reads route
	-- through Store.Events() (the EventStore interface backed
	-- by SQLiteEventStore in events_sqlite.go), which lives in
	-- its own events.db with its own connection pool and an
	-- async writer goroutine. Architectural rationale: events
	-- are a strict consumer of the system, never on the
	-- critical path. See docs/event-log.md.

	-- operator/model design — tokens move out of the
	-- citizens row into their own table. Multiple tokens per
	-- citizen (rotation, per-deployment labels), revocable
	-- (revoked_at IS NULL = active), audit-preserved (revoke =
	-- mark, never delete). The citizens.token column is left
	-- intact as a legacy mirror until a future cleanup phase can
	-- safely drop it. See docs/operator-model-design.md.
	CREATE TABLE IF NOT EXISTS tokens (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		citizen_id   INTEGER NOT NULL REFERENCES citizens(id),
		token      TEXT NOT NULL UNIQUE,
		label      TEXT NOT NULL DEFAULT '',
		issued_at    TIMESTAMP NOT NULL,
		revoked_at   TIMESTAMP,
		last_used_at  TIMESTAMP
	);

	-- Issues are project-level structured artifacts (living-
	-- workflow phase 3). Outlive individual runs — filed in run
	-- #2, triaged in run #4, fixed in run #7 is normal. Status
	-- transitions: open → triaged → closed (or wontfix). Atomic
	-- per-project counter via MAX(seq)+1 inside CreateIssue's
	-- transaction; the UNIQUE (project_id, seq) constraint
	-- doubles as the racing-create guard. See
	-- docs/living-workflow-design-notes.md § 6.
	CREATE TABLE IF NOT EXISTS issues (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id     INTEGER NOT NULL,
		seq         INTEGER NOT NULL,
		title        TEXT NOT NULL,
		body        TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'open',
		severity      TEXT NOT NULL DEFAULT 'medium',
		found_in_run_id   INTEGER NOT NULL DEFAULT 0,
		found_in_task_id  TEXT NOT NULL DEFAULT '',
		filed_by      INTEGER NOT NULL,
		filed_at      TIMESTAMP NOT NULL,
		triaged_by     INTEGER NOT NULL DEFAULT 0,
		triaged_at     TIMESTAMP,
		closed_by_task_id  TEXT NOT NULL DEFAULT '',
		closed_at      TIMESTAMP,
		updated_at     TIMESTAMP NOT NULL,
		UNIQUE (project_id, seq)
	);

	CREATE INDEX IF NOT EXISTS idx_issues_project_status ON issues(project_id, status);

	CREATE INDEX IF NOT EXISTS idx_tasks_run ON tasks(run_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);
	CREATE INDEX IF NOT EXISTS idx_tasks_claimed_by ON tasks(claimed_by);
	CREATE INDEX IF NOT EXISTS idx_tasks_reviews_target
	 ON tasks(run_id, reviews_target)
	 WHERE reviews_target != '' AND action = 'review';
	CREATE INDEX IF NOT EXISTS idx_task_claims_task ON task_claims(task_id);
	CREATE INDEX IF NOT EXISTS idx_citizens_token ON citizens(token);
	CREATE INDEX IF NOT EXISTS idx_tokens_citizen ON tokens(citizen_id);
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
		// Living-workflow phase 4 — per-run cycle budget. Caps how
		// many tasks can be spawned into a run at runtime to prevent
		// runaway loops where bot A spawns bot B spawns bot A.
		// Counter increments on every successful spawn; when used
		// reaches max, further spawns are refused and the run is
		// auto-paused until an operator extends the budget. Default
		// 200 per the design notes — generous enough that legitimate
		// remediation chains don't trip it, tight enough that a
		// runaway is caught within a few minutes. Existing rows
		// default to 0/200; pre-spawn runs are unaffected.
		`ALTER TABLE runs ADD COLUMN cycle_budget_used INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN cycle_budget_max INTEGER NOT NULL DEFAULT 200`,
		// Living-workflow phase 4 — task spawn provenance. Records
		// the parent task that triggered a spawn (or 0 for tasks
		// authored at run-create time) and the spawn trigger source
		// (human / bot / template_rule / auto_triage). The audit
		// log lives in events as task_spawned; these
		// columns make the lineage queryable without a join.
		`ALTER TABLE tasks ADD COLUMN spawned_from TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN spawn_trigger TEXT NOT NULL DEFAULT ''`,
		// Living-workflow phase 4b — declarative review-failure
		// spawn rules. Declared on the task that gets reviewed
		// (the dev task), not the review task itself. Empty
		// string preserves the historical cascade-invalidate
		// behavior so existing templates parse and run
		// unchanged. RemediationTemplate is JSON-encoded;
		// empty when the rule is "continue_iteration" or the
		// default invalidate path.
		`ALTER TABLE tasks ADD COLUMN on_review_reject TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN on_review_request_changes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN remediation_template TEXT NOT NULL DEFAULT ''`,
		// Living-workflow phase 4c — run-level auto-triage rule
		// + per-task issue linkage. auto_triage_template is the
		// JSON-encoded RemediationTemplate the engine uses to
		// spawn a fix task when a run lands on idle and has open
		// issues. closes_issue_seq on the spawned task records
		// which issue (per-project seq) the task is fixing — when
		// it accepts, the auto-close hook transitions the issue
		// to "closed". 0 on every other task. See
		// docs/living-workflow-design-notes.md § 7.
		`ALTER TABLE runs ADD COLUMN auto_triage_template TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN closes_issue_seq INTEGER NOT NULL DEFAULT 0`,
		// Living-workflow phase 6a — iteration branch as a
		// first-class identifier on task_claims. Generated at
		// claim time as "<run-slug>/<task_def_id>/iter-<N>".
		// Phase 6a stores the value and surfaces it in the
		// audit projection; the actual fat-client git workflow
		// (checkout/commit on topic branch, automerge on
		// approve, cleanup on reject) is phase 6b. Empty for
		// rows that predate the column or for vote/review
		// tasks where branching is meaningless (no git
		// artifact). See docs/living-workflow-design-notes.md § 4.
		`ALTER TABLE task_claims ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
		// Phase 5 fidelity columns — capture commit_sha and
		// review decision per-claim at submit time, so the
		// iteration projection returns the historical value
		// even after the task-level fields are cleared by
		// invalidation. Without these, ListTaskIterations
		// joined to t.commit_sha / t.review_decision and
		// iter-1's value vanished the moment iter-2 started
		// (because invalidate clears the task-level fields).
		// Captured at the four submit-path UPDATE sites.
		`ALTER TABLE task_claims ADD COLUMN commit_sha TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE task_claims ADD COLUMN decision TEXT NOT NULL DEFAULT ''`,
		// Phase 6c — iter_seq turns task_claims into the
		// iteration envelope. iter_seq counts accept-cycles,
		// not raw claim rows: bumps only when the prior claim
		// reaches a terminal outcome (completed reviewed-and-
		// reject, invalidated, abandoned, released). A
		// request_changes round leaves the claim open and
		// re-submission lands as another row in
		// task_submissions, sharing the same claim_id and
		// therefore the same iter_seq.
		//
		// Default 0 is a sentinel for any pre-6c row that
		// somehow survives a pre-launch DB reset. The expected
		// state is that no such rows exist on production
		// stores: rollouts wipe the DB, and ClaimTask /
		// applySetClaim stamp a real iter_seq (≥1) on every
		// new claim. The MAX(iter_seq) WHERE outcome IS NOT
		// NULL lookup correctly ignores 0-stamped legacy rows
		// because they have NULL outcome (they were never
		// closed); a partial-migration scenario where a
		// terminal-outcome legacy row sneaks through with
		// iter_seq=0 would silently produce iter_seq=1 on
		// the next claim — acceptable under the "no upgrade
		// path for existing data" pre-launch contract.
		`ALTER TABLE task_claims ADD COLUMN iter_seq INTEGER NOT NULL DEFAULT 0`,
		// Phase 8.5 — JSON blob describing why a WAITING run
		// can't make progress. Populated by applyCompleteRun
		// at the moment of the active|whatever → waiting
		// transition; cleared when the run leaves waiting.
		// One of:
		//   {kind:"review",       task, assignee, since}
		//   {kind:"human_claim",  task, assignee}
		//   {kind:"artifact",     task, awaiting_path}
		//   {kind:"stuck",        detail}
		// Nullable on purpose — non-WAITING runs have no
		// blocker. Surface readers (enju_run_status) check
		// state==waiting before reading the column.
		`ALTER TABLE runs ADD COLUMN blocked_by TEXT`,
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
	// level. Partial unique index so only ALIVE runs collide —
	// completed / failed runs on the same branch are fine.
	// "Alive" expanded in living-workflow phase 1 to include
	// idle and paused: an idle run is still the run for its
	// branch and should block a new run from being created on
	// the same branch. Belt-and-suspenders alongside the
	// application-level ActiveRunOnBranch check in
	// handleCreateRun, which races under concurrent requests
	// without this guard. Lives here (not in the schema block
	// above) because it references the `branch` column which
	// comes in via ALTER TABLE.
	//
	// The phase-1 migration drops the old active-only index and
	// recreates it covering active|idle|paused. The DROP is a
	// no-op for fresh DBs (CREATE IF NOT EXISTS earlier was
	// skipped) and silently rebuilds the index for upgrades.
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_runs_active_branch`); err != nil {
		return fmt.Errorf("schema: drop idx_runs_active_branch: %w", err)
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_active_branch ON runs(project_id, branch) WHERE state IN ('active', 'waiting', 'paused')`); err != nil {
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
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, blocked_by, created_at, updated_at
		 FROM runs WHERE project_id = ? ORDER BY seq ASC`, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var r RunRecord
		var ref, blocked sql.NullString
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Seq, &r.Name, &ref, &r.YAMLData, &r.RepoURL, &r.State, &r.SourcePath, &r.SourceCommitSHA, &r.Params, &r.Branch, &r.Slug, &blocked, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Ref = ref.String
		r.BlockedBy = blocked.String
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// --- Project members ---

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

// CreateRun (direct method) was removed in the chokepoint
// migration — callers route through ApplyPlan with a
// store.CreateRun mutation. The handler in apply.go preserves
// the per-project seq computation; tests use helperCreateRun.

// GetRun retrieves a run by its global ID.
func (s *Store) GetRun(id int64) (*RunRecord, error) {
	var p RunRecord
	var ref, blocked sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, blocked_by, created_at, updated_at FROM runs WHERE id = ?`, id,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.Params, &p.Branch, &p.Slug, &blocked, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Ref = ref.String
	p.BlockedBy = blocked.String
	return &p, err
}

// GetRunByProjectSeq retrieves a run by (project_id, seq).
func (s *Store) GetRunByProjectSeq(projectID int64, seq int) (*RunRecord, error) {
	var p RunRecord
	var ref, blocked sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, blocked_by, created_at, updated_at
		 FROM runs WHERE project_id = ? AND seq = ?`, projectID, seq,
	).Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.Params, &p.Branch, &p.Slug, &blocked, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.Ref = ref.String
	p.BlockedBy = blocked.String
	return &p, err
}

func (s *Store) ListRuns() ([]RunRecord, error) {
	rows, err := s.db.Query(`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, blocked_by, created_at, updated_at FROM runs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var p RunRecord
		var ref, blocked sql.NullString
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Seq, &p.Name, &ref, &p.YAMLData, &p.RepoURL, &p.State, &p.SourcePath, &p.SourceCommitSHA, &p.Params, &p.Branch, &p.Slug, &blocked, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Ref = ref.String
		p.BlockedBy = blocked.String
		runs = append(runs, p)
	}
	return runs, rows.Err()
}

// ActiveRunOnBranch returns the first ALIVE run on the given
// project+branch pair, or nil if none exists. Used by
// handleCreateRun to enforce the serial-runs-per-branch
// invariant: a second run on the same branch would step on the
// first one's artifact writes, so we refuse it with a clear
// error pointing at the existing run.
//
// Living-workflow phase 1 widened the alive predicate from
// active-only to active|idle|paused — an idle run is still
// alive and still owns its branch slot. The predicate must
// match idx_runs_active_branch's WHERE clause; if it drifts,
// app-level pre-flight returns nil and the user sees a raw SQL
// constraint error from the DB instead of the friendly "run X
// already alive on this branch" message. Name kept as
// ActiveRunOnBranch for callsite stability.
func (s *Store) ActiveRunOnBranch(projectID int64, branch string) (*RunRecord, error) {
	var r RunRecord
	var ref, blocked sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, seq, name, ref, yaml_data, repo_url, state, source_path, source_commit_sha, params, branch, slug, blocked_by, created_at, updated_at
		 FROM runs WHERE project_id = ? AND branch = ? AND state IN ('active', 'waiting', 'paused')
		 ORDER BY seq ASC LIMIT 1`,
		projectID, branch,
	).Scan(&r.ID, &r.ProjectID, &r.Seq, &r.Name, &ref, &r.YAMLData, &r.RepoURL, &r.State, &r.SourcePath, &r.SourceCommitSHA, &r.Params, &r.Branch, &r.Slug, &blocked, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Ref = ref.String
	r.BlockedBy = blocked.String
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

// --- Tasks ---

const taskColumns = `id, run_id, seq, task_def_id, instance_key, instance_params, ref, action, prompt, user_prompt, script, outputs, requirements, result_type, timeout, state, claimed_by, claimed_at, submitted_at, result_path, commit_sha, depends_on, reads_artifacts, writes_artifacts, assign_to, require_role, reviews_target, review_decision, vote_options, vote_choice, citizens, min_quorum, vote_threshold, vote_deadline, anonymize, visibility, fail_reason, skip_reason, parked_from_state, env, mode, run_slug, on_review_reject, on_review_request_changes, remediation_template, closes_issue_seq, created_at`

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
		&anonymizeInt, &t.Visibility, &t.FailReason, &t.SkipReason, &t.ParkedFromState, &t.Env, &t.Mode, &t.RunSlug,
		&t.OnReviewReject, &t.OnReviewRequestChanges, &t.RemediationTemplate,
		&t.ClosesIssueSeq,
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

// submitTaskResultForTest is a test-only helper preserved from
// the pre-engine submit path. Production code goes through
// engine.ComputeSubmission → ApplyPlan(RecordSubmission) — see
// engine/submit.go. This function stays as a one-shot used by
// store-package unit tests that exercise the SubmitResult flow
// without booting the engine. Behavior matches the production
// path's effect on task_claims + tasks; it does NOT emit events
// or invoke the cascade. New tests should prefer ApplyPlan.
//
// citizenID identifies which claim slot this submission fills
// for multi-citizen tasks; ignored for single-citizen tasks.
// decision / voteChoice mirror the review-verdict / vote-option
// fields on the wire.
func (s *Store) submitTaskResultForTest(taskID string, citizenID int64, resultPath, commitSHA, decision, voteChoice string, tokensUsed int64) (*SubmitResult, error) {
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
	// Phase 8.3 — one submit flips the task to SUBMITTED (was
	// ACCEPTED); the test helper does NOT drive the subsequent
	// /merges-equivalent work, so callers asserting end state
	// see SUBMITTED here. Production code goes through
	// engine.ComputeSubmission → applyRecordSubmission → and
	// then service.acceptTask (inline or via the /merges
	// handler) for the SUBMITTED → ACCEPTED transition.
	if citizens == 1 {
		if TaskState(state) != TaskClaimed && TaskState(state) != TaskRunning {
			return nil, fmt.Errorf("task %q cannot accept result (state: %s)", taskID, state)
		}
		// Phase 8.3 — the submitTaskResultForTest helper writes
		// state='accepted' directly (skipping the SUBMITTED gate)
		// because store-package callers using this helper are
		// asserting the end-state of a single-attempt submit
		// pipeline, not the multi-step submit/merge protocol that
		// production routes through engine.ComputeSubmission +
		// service.acceptTask. Going through SUBMITTED would force
		// every store test to chain a follow-up SetTaskState plan
		// just to reach the assertion baseline. The state here is
		// the post-acceptTask state.
		_, err = tx.Exec(
			`UPDATE tasks SET state = 'accepted', submitted_at = ?, result_path = ?, commit_sha = ?, review_decision = ?, vote_choice = ? WHERE id = ?`,
			now, resultPath, commitSHA, decision, voteChoice, taskID,
		)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(
			`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, commit_sha = ?, decision = ? WHERE task_id = ? AND outcome IS NULL`,
			now, voteChoice, commitSHA, decision, taskID,
		)
		if err != nil {
			return nil, err
		}
		if claimedBy.Valid {
			// last_seen is intentionally not touched — see the
			// citizens.last_seen note in models.go.
			_, err = tx.Exec(
				`UPDATE citizens SET
					tasks_completed = tasks_completed + 1,
					tokens_contributed = tokens_contributed + ?,
					score = (tasks_completed + 1) - (tasks_timed_out * 0.5) - (tasks_rejected * 1.0)
				WHERE id = ?`,
				tokensUsed, claimedBy.Int64,
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
	//  - vote tasks: the selected option id
	//  - review tasks: "approve" or "reject"
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
		`UPDATE task_claims SET outcome = 'completed', submitted_at = ?, option = ?, commit_sha = ?, decision = ? WHERE id = ?`,
		now, choice, commitSHA, decision, claimRow.Int64,
	)
	if err != nil {
		return nil, err
	}
	// Score accounting on the submitting citizen alone — don't
	// increment tasks_completed until the tally actually resolves
	// (score should reflect "I completed a task," not "I
	// submitted a vote that's still being tallied"). Token
	// contribution can still be credited per submit. last_seen is
	// intentionally not touched — see citizens.last_seen note in
	// models.go.
	_, err = tx.Exec(
		`UPDATE citizens SET tokens_contributed = tokens_contributed + ? WHERE id = ?`,
		tokensUsed, citizenID,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &SubmitResult{Collecting: true}, nil
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

// validRelabelOutcomes is the closed set of outcome values
// MarkLatestClaimOutcome will write. Defends against caller
// typos that would otherwise silently land a bogus outcome the
// projection layer doesn't know how to render (or worse, would
// case-fall-through and look "completed" to a string-equality
// check).
var validRelabelOutcomes = map[ClaimOutcome]bool{
	ClaimOutcomeCompleted:   true, // review approve transitions an open reviewed-task claim to terminal-success
	ClaimOutcomeRejected:    true, // request_changes / reject cascade (review verdict)
	ClaimOutcomeInvalidated: true, // manual enju_invalidate_task — operator wiped the task without a verdict
	ClaimOutcomeAbandoned:   true, // citizen-takeover: a different citizen claimed an open row, prior is closed without verdict
	ClaimOutcomeReleased:    true, // timeout / voluntary release
}

// HasReviewerOfTarget reports whether any task in `runID` is
// an action:review whose `reviews_target` matches the given
// (taskDefID, instanceKey) pair. Backed by the partial index
// idx_tasks_reviews_target so the lookup is O(log N) regardless
// of run size.
//
// Used by the merge gate (router.taskHasDownstreamReview) to
// decide whether a just-submitted task's auto-accept should
// also trigger a merge to main, or whether it's waiting for a
// downstream reviewer's verdict.
//
// Caller passes (defID, instanceKey) and we reconstruct the
// canonical reviews_target shape — bare def id for singleton
// reviews, "instanceKey:defID" for per-instance ones (the
// shape build.go's MakeFullID writes). Keeping the shape
// reconstruction here, not at the call site, means the merge
// gate doesn't need to know whether the run is for_each-
// expanded or not.
func (s *Store) HasReviewerOfTarget(runID int64, taskDefID, instanceKey string) (bool, error) {
	target := BuildReviewsTargetKey(taskDefID, instanceKey)
	var present int
	err := s.db.QueryRow(
		`SELECT 1 FROM tasks
		 WHERE run_id = ? AND action = 'review' AND reviews_target = ?
		 LIMIT 1`,
		runID, target,
	).Scan(&present)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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

// ListOpenClaimsForCitizen returns every open (outcome = NULL)
// claim row currently held by a given citizen across all tasks.
// Used by the bot daemon's startup recovery path: when a daemon
// restarts (operator-initiated stop+start, fatclient crash, coord
// crash), the new process has no in-memory record of claims it
// "had" before. Without this list, those claims sit until natural
// reaper expiry (~30min), wasting an iteration cycle each.
//
// The daemon iterates the result and applies ReleaseClaim per
// row, freeing the tasks back to READY immediately.
func (s *Store) ListOpenClaimsForCitizen(citizenID int64) ([]TaskClaimRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+taskClaimColumns+`
		 FROM task_claims
		 WHERE citizen_id = ? AND outcome IS NULL
		 ORDER BY claimed_at`,
		citizenID,
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
const taskClaimColumns = `id, task_id, citizen_id, claimed_at, deadline, outcome, submitted_at, option, model_id, branch, commit_sha, decision, iter_seq`

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
		var iterSeq sql.NullInt64
		if err := rows.Scan(
			&r.ID, &r.TaskID, &r.CitizenID, &r.ClaimedAt, &r.Deadline,
			&outcome, &submittedAt, &r.Option, &modelID,
			&r.Branch, &r.CommitSHA, &r.Decision, &iterSeq,
		); err != nil {
			return nil, err
		}
		if iterSeq.Valid {
			r.IterSeq = int(iterSeq.Int64)
		}
		if outcome.Valid {
			r.Outcome = ClaimOutcome(outcome.String)
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

// ReadiedTask describes one task that flipped pending→ready in
// a single UpdateReadyTasks pass. Returned so the caller can emit
// task_ready events carrying the assignee(s) — the field the
// assigned_task_ready notification rule filters on.
//
// Assignees is the parsed list from the tasks.assign_to column
// (JSON-encoded array on disk). Empty/nil means unassigned.
// applyUpdateReadyTasks fans out one task_ready event per
// assignee so the predicate matcher can do bare-string equality
// against the user's username; unassigned tasks still fire one
// event with an empty assignee so the audit timeline records
// every transition.
//
// Parents is the snapshot of every dependency at the moment this
// task became ready, with each parent's commit_sha + result_dir
// + action embedded. Parents are guaranteed terminal (accepted
// or skipped) at this moment by the cascade's gating, so the
// snapshot is well-defined. Embedding parent info here makes the
// task_ready event self-contained for the fat-client inbox view —
// no event-correlation, no coordinator round-trips.
type ReadiedTask struct {
	TaskID    string
	Action    string
	Assignees []string
	ProjectID int64
	RunID     int64
	Parents   []ReadiedParent
}

// ReadiedParent is one upstream task captured at the moment its
// downstream became ready. CommitSHA + ResultDir together let the
// fat-client read the parent's submitted result.md from git
// (`git show {commit_sha}:{result_dir}/result.md`) without any
// coordinator call. CommitSHA may be empty for skipped parents
// (no submission); inbox renders those with an explanatory note.
type ReadiedParent struct {
	TaskID    string
	Action    string
	CommitSHA string
	ResultDir string
}

// dbExecQueryer is the read+write surface shared by *sql.DB and
// *sql.Tx. The cascade (updateReadyTasksOn) is parameterized over
// this so it can run inside the ApplyPlan transaction (taking
// `tx`) or as a standalone post-commit call (taking `s.db`).
//
// Why this matters: a tx holds the SQLite write lock until commit.
// If the cascade runs through `s.db`, it grabs a different pool
// connection that (a) sees pre-commit state — readiness updates
// from the same plan are invisible — and (b) busy-waits on the
// write lock when it tries to UPDATE. Pre-fix this manifested as
// the deadline-driven vote/review resolve path silently missing
// downstream readiness propagation; post-fix it's a single tx.
type dbExecQueryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// lookupReadiedParents resolves a comma-separated depends_on list
// into the snapshot the inbox needs at task_ready emit time:
// each parent's action, commit_sha, and result_dir. Result_dir is
// computed inline using the same convention as
// engine.ComputeResultDir; we duplicate the few lines of layout
// logic here rather than circular-import engine. SlugInstanceKey
// is shared with the parser via internal/yaml.
//
// Empty input returns nil. A parent that doesn't exist (deleted
// task) is skipped. CommitSHA is empty for skipped parents — they
// have no submission.
func lookupReadiedParents(q dbExecQueryer, dependsOn string) ([]ReadiedParent, error) {
	if dependsOn == "" {
		return nil, nil
	}
	var out []ReadiedParent
	for _, raw := range strings.Split(dependsOn, ",") {
		parentID := strings.TrimSpace(raw)
		if parentID == "" {
			continue
		}
		var (
			action         string
			commitSHA      sql.NullString
			runSlug        sql.NullString
			taskDefID      string
			instanceParams sql.NullString
		)
		err := q.QueryRow(
			`SELECT action, commit_sha, run_slug, task_def_id, instance_params
			 FROM tasks WHERE id = ?`,
			parentID,
		).Scan(&action, &commitSHA, &runSlug, &taskDefID, &instanceParams)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lookup parent %s: %w", parentID, err)
		}
		out = append(out, ReadiedParent{
			TaskID:    parentID,
			Action:    action,
			CommitSHA: commitSHA.String,
			ResultDir: renderResultDir(parentID, runSlug.String, taskDefID, instanceParams.String),
		})
	}
	return out, nil
}

// renderResultDir delegates to internal/common/layout. Lives in
// the cascade emit path so task_ready events can carry the
// parent's result_dir; the layout convention is shared with
// engine via the core package, no duplication.
func renderResultDir(taskID, runSlug, taskDefID, instanceParamsJSON string) string {
	var params map[string]string
	if instanceParamsJSON != "" {
		_ = json.Unmarshal([]byte(instanceParamsJSON), &params)
	}
	return layout.ComputeResultDirForInstance(layout.RunSeqFromTaskID(taskID), runSlug, taskDefID, params)
}

// updateReadyTasksOn is the tx-aware cascade body, run inside an
// ApplyPlan transaction by applyUpdateReadyTasks. Pre-Phase-3 it
// also had a direct-call entry point (Store.UpdateReadyTasks)
// that read pre-commit state through a separate connection; that
// path is gone — every cascade now flows through ApplyPlan.
func updateReadyTasksOn(q dbExecQueryer, runID int64) ([]ReadiedTask, error) {
	// Pull project_id + branch once for this run — every
	// pending task shares both, and the artifact index lookups
	// below need (project, branch) to find the right row.
	var projectID int64
	var runBranch string
	if err := q.QueryRow(`SELECT project_id, branch FROM runs WHERE id = ?`, runID).Scan(&projectID, &runBranch); err != nil {
		return nil, fmt.Errorf("loading run project: %w", err)
	}

	type pendingTask struct {
		id             string
		dependsOn      string
		readsArtifacts string
		action         string
		assignTo       string
		// reviewsTarget is the canonical reviews_target key from
		// BuildReviewsTargetKey (bare def id for singletons,
		// "instanceKey:defID" for for_each instances). Empty for
		// non-review tasks. Used by the artifact-visibility gate's
		// reviewer-exception below.
		reviewsTarget string
	}
	var pending []pendingTask
	{
		rows, err := q.Query(
			`SELECT id, depends_on, reads_artifacts, action, COALESCE(assign_to, ''), COALESCE(reviews_target, '') FROM tasks WHERE run_id = ? AND state = 'pending'`, runID,
		)
		if err != nil {
			return nil, err
		}
		// Drain + close eagerly — when q is a *sql.Tx the same
		// connection is needed for the next Query, and an open
		// Rows on that connection would error. With *sql.DB
		// closing here is a no-op cost. Same pattern below.
		for rows.Next() {
			var pt pendingTask
			if err := rows.Scan(&pt.id, &pt.dependsOn, &pt.readsArtifacts, &pt.action, &pt.assignTo, &pt.reviewsTarget); err != nil {
				rows.Close()
				return nil, err
			}
			pending = append(pending, pt)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	// "Satisfied" parents for dependency readiness:
	//   - ACCEPTED: normal terminal, content on run-branch.
	//   - SKIPPED: vote-skip cascade or upstream-failed cascade
	//     decided this branch is done — downstream unblocks.
	//   - SUBMITTED (Phase 8.3): reviewer tasks need their
	//     target to be merge-pending, not merge-confirmed, so
	//     they can read submitted content from the topic
	//     branch and produce a verdict. Pure-depends_on
	//     downstream (no reads_artifacts) also unblocks here.
	//     Downstreams that DO read artifacts stay gated on the
	//     stricter writer-state {accepted, skipped} via the
	//     reads_artifacts loop below — SUBMITTED-writer rows
	//     are invisible there until merge confirms.
	// FAILED is NOT satisfied — a failed upstream blocks
	// downstream, not unblocks it.
	accepted := make(map[string]bool)
	skippedSet := make(map[string]bool)
	{
		acceptedRows, err := q.Query(
			`SELECT id, state FROM tasks WHERE run_id = ? AND state IN ('accepted', 'submitted', 'skipped')`, runID,
		)
		if err != nil {
			return nil, err
		}
		for acceptedRows.Next() {
			var id, state string
			if err := acceptedRows.Scan(&id, &state); err != nil {
				acceptedRows.Close()
				return nil, err
			}
			accepted[id] = true
			if state == "skipped" {
				skippedSet[id] = true
			}
		}
		if err := acceptedRows.Err(); err != nil {
			acceptedRows.Close()
			return nil, err
		}
		acceptedRows.Close()
	}

	var readied []ReadiedTask
	for _, pt := range pending {
		allDone := true
		// Track whether every dependency is in the SKIPPED
		// subset of the satisfied set (vs. having at least one
		// ACCEPTED / SUBMITTED dep with real content). A task
		// whose every dep was skipped has nothing to consume
		// and should propagate the skip rather than run on
		// empty inputs. Load-bearing for the fan-in aggregator
		// case: when all source instances cascade-skip via
		// upstream-failed, the singleton aggregator's deps are
		// all SKIPPED — without this check it would silently
		// promote to READY and "succeed" with an empty fan-in
		// block.
		allDepsSkipped := pt.dependsOn != ""

		if pt.dependsOn != "" {
			for _, dep := range strings.Split(pt.dependsOn, ",") {
				d := strings.TrimSpace(dep)
				if !accepted[d] {
					allDone = false
					break
				}
				if !skippedSet[d] {
					allDepsSkipped = false
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
		//
		// Inlined existence check (not a call out to GetArtifact)
		// so the read shares the cascade's connection — see the
		// dbExecQueryer doc.
		if allDone && pt.readsArtifacts != "" {
			branch := runBranch
			if branch == "" {
				branch = "main"
			}
			var paths []string
			if err := json.Unmarshal([]byte(pt.readsArtifacts), &paths); err == nil {
				for _, p := range paths {
					if p == "" {
						continue
					}
					var one int
					// Phase 8.3 — gate artifact visibility on the
					// writer task's state. An artifact-index row is
					// inserted at submit time (so the index has the
					// metadata for downstream lookups), but
					// downstream readiness must NOT see it as
					// satisfied until the writer task hits ACCEPTED
					// (merge confirmed) or SKIPPED (cascade decided
					// the dep is moot). Without this gate, a
					// SUBMITTED-but-not-yet-merged upstream would
					// fan its downstreams out against an artifact
					// whose underlying commit hasn't landed on the
					// run branch — the silent-cascade-stall bug
					// Phase 8 closes.
					//
					// Reviewer exception: when the pending task is
					// the REVIEW of the writer (pt.reviewsTarget
					// matches the writer's BuildReviewsTargetKey),
					// SUBMITTED counts as visible too. A reviewer's
					// whole job is to read SUBMITTED content and
					// decide approve/reject — making it wait for
					// ACCEPTED deadlocks against the merge gate that
					// holds the writer at SUBMITTED until review
					// approves (see collectAcceptedMerges'
					// skipMergeOfSelf logic). The reader resolves
					// the actual file bytes via ReadFileAtCommit
					// against the artifact-index's last_commit_sha
					// (the writer's iter-branch tip), not via main —
					// so reading SUBMITTED-but-not-merged content
					// works.
					//
					// Empty/NULL last_task_id passes through
					// (orphan rows from legacy paths or test
					// fixtures) so the gate doesn't accidentally
					// hide pre-Phase-8 data.
					err := q.QueryRow(
						`SELECT 1 FROM artifacts a
						 WHERE a.project_id = ? AND a.branch = ? AND a.path = ?
						   AND (
						     a.last_task_id IS NULL OR a.last_task_id = '' OR
						     EXISTS (
						       SELECT 1 FROM tasks writer
						       WHERE writer.id = a.last_task_id
						         AND (
						           writer.state IN ('accepted', 'skipped')
						           OR (
						             writer.state = 'submitted'
						             AND ? != ''
						             AND ? = CASE
						               WHEN writer.instance_key = '' THEN writer.task_def_id
						               ELSE writer.instance_key || ':' || writer.task_def_id
						             END
						           )
						         )
						     )
						   )
						 LIMIT 1`,
						projectID, branch, p,
						pt.reviewsTarget, pt.reviewsTarget,
					).Scan(&one)
					if err == sql.ErrNoRows {
						allDone = false
						break
					}
					if err != nil {
						return readied, fmt.Errorf("checking artifact %s: %w", p, err)
					}
				}
			}
		}

		if allDone {
			// All-deps-skipped → propagate the skip instead of
			// promoting. The task has nothing to consume.
			// Naturally fixes the fan-in aggregator case where
			// cross-iteration sibling sources all cascade-skip:
			// the singleton aggregator's dep list ends up
			// entirely SKIPPED, which under the old rule
			// promoted it to READY with empty `{{source.content}}`.
			// The skip reason is generic — applies to
			// aggregators and any other task pattern that
			// reaches this state.
			if allDepsSkipped {
				_, err := q.Exec(
					`UPDATE tasks SET state = 'skipped', skip_reason = ? WHERE id = ?`,
					"all dependencies skipped — no content to consume",
					pt.id,
				)
				if err != nil {
					return readied, err
				}
				// Add to skippedSet so any downstream task
				// considered later in THIS pass sees the
				// updated state and cascades correctly. Without
				// this, a multi-level all-skipped chain would
				// need multiple cascade passes to converge.
				skippedSet[pt.id] = true
				accepted[pt.id] = true
				continue
			}
			_, err := q.Exec(`UPDATE tasks SET state = 'ready' WHERE id = ?`, pt.id)
			if err != nil {
				return readied, err
			}
			// assign_to on disk is a JSON-encoded array (engine
			// materialize + spawn both write `["alice"]` /
			// `["alice","bob"]`). Parse it; empty string and
			// malformed both surface as zero-len assignees so
			// the apply emit-site can treat them uniformly as
			// "unassigned, emit one event with empty assignee."
			var assignees []string
			if pt.assignTo != "" {
				_ = json.Unmarshal([]byte(pt.assignTo), &assignees)
			}
			// Snapshot parents at the moment of readiness so the
			// task_ready event is self-contained for the fat-
			// client inbox view (no event-correlation, no
			// coordinator round-trips).
			parents, perr := lookupReadiedParents(q, pt.dependsOn)
			if perr != nil {
				return readied, perr
			}
			readied = append(readied, ReadiedTask{
				TaskID:    pt.id,
				Action:    pt.action,
				Assignees: assignees,
				ProjectID: projectID,
				RunID:     runID,
				Parents:   parents,
			})
		}
	}

	return readied, nil
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

// CreateCitizen / SetCitizenRole logic lives in apply.go
// (applyCreateCitizen, applySetCitizenRole). Callers build
// store.Plan with the matching mutation and call ApplyPlan.

// UpdateCitizenProfile updates a citizen's name and/or email with
// UpdateCitizenProfile logic lives in apply.go
// (applyUpdateCitizenProfile). Caller builds a store.Plan with
// the UpdateCitizenProfile mutation and calls ApplyPlan.

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

// IssueToken / RevokeToken / RevokeTokenByValue logic lives in
// apply.go (applyIssueToken, applyRevokeToken,
// applyRevokeTokenByValue). Callers build a store.Plan with
// the matching mutation and call ApplyPlan.

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
//
// Same principle for snapshot/version drift: providers silently
// update weights under the same nominal name (today's
// claude-opus-4-7 ≠ next year's claude-opus-4-7). The catalog
// stays one bucket per logical model; when the model_resolved
// column ships (also deferred), each submit records the literal
// identifier the API echoed back ("anthropic/claude-opus-4-7-
// 20260415"). Both deferred items in docs/operator-model-design.md
// — "Provider routing" and "Model drift / snapshot pinning".
var modelCatalogSeed = []struct {
	Username string
	Name   string
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
// initSchema() after schema is in place.
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
//
// CHOKEPOINT EXEMPTION: ONLY called from initSchema() at startup,
// before the EventStore is attached and before any service
// caller can issue an ApplyPlan. This direct write deliberately
// bypasses the chokepoint because the schema-seeding phase
// runs serially with no contention and predates the EventSink
// contract (no mutation event would have anywhere to land).
// DO NOT call this at runtime; route runtime model registration
// through service.RegisterModel → CreateCitizen{Kind:"model"}.
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

// CreateModelCitizen logic absorbed into applyCreateCitizen with
// kind="model" — callers build a CreateCitizen mutation with that
// kind and the synthetic "model:<username>" token convention.
// See service/citizens_write.go RegisterModel for the canonical
// caller shape.

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

// GetOpenClaimIterSeq returns the iter_seq of the most recent
// open (outcome IS NULL) claim row for taskID, or 0 if none
// exists. 's task_request_changes emission needs this
// to stamp iter_seq into the event metadata; future iter_seq /
// branch lookups should grow as siblings here rather than
// reaching for Store.DB(). Returns nil error on no-row (zero
// is the well-defined "no open claim" answer).
func (s *Store) GetOpenClaimIterSeq(taskID string) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(
		`SELECT iter_seq FROM task_claims
		 WHERE task_id = ? AND outcome IS NULL
		 ORDER BY id DESC LIMIT 1`,
		taskID,
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return n.Int64, nil
}

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
			&anonymizeInt, &t.Visibility, &t.FailReason, &t.SkipReason, &t.ParkedFromState, &t.Env, &t.Mode, &t.RunSlug,
			&t.OnReviewReject, &t.OnReviewRequestChanges, &t.RemediationTemplate,
			&t.ClosesIssueSeq,
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
//
// Phase 8.3 note: this read is INTENTIONALLY ungated. The
// cascade's readiness-check (applyUpdateReadyTasks) gates
// artifact visibility on the writer's state because fanning
// downstreams out against a SUBMITTED-not-yet-merged writer
// would create the silent-cascade-stall bug. Surface readers
// (enju_get_task's provenance, enju_get_artifact, the
// invalidation rollback logic) want full visibility — they
// render lineage that's true regardless of merge state, and
// gating them would erase pending-but-real provenance from
// the operator's view. The asymmetry is deliberate: ONE place
// (the cascade) cares about merge confirmation; everywhere
// else, the index row IS the truth.
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

// DeleteArtifact removes an artifact's index row by (project_id, path).
// ListArtifactsByProject returns all artifacts for a project on
// a specific branch, ordered by path. Empty branch resolves to
// "main". If pathPrefix is non-empty, only artifacts whose path
// starts with it are returned (useful for listing a directory
// subtree).
func (s *Store) ListArtifactsByProject(projectID int64, branch, pathPrefix string) ([]ArtifactRecord, error) {
	if branch == "" {
		branch = "main"
	}
	// Phase 8.3 — intentionally UNGATED, same rationale as
	// GetArtifact. Surface readers want every artifact-index
	// row including pending-merge writers; only the cascade's
	// readiness check (applyUpdateReadyTasks) gates on writer
	// state to avoid premature fan-out.
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
