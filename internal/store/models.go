// Package store provides the SQLite database layer for Cedar state management.
package store

import "time"

// TaskState represents the lifecycle state of a task.
type TaskState string

const (
	TaskPending     TaskState = "pending"
	TaskReady       TaskState = "ready"
	TaskClaimed     TaskState = "claimed"
	TaskRunning     TaskState = "running"
	TaskSubmitted   TaskState = "submitted"
	TaskAccepted    TaskState = "accepted"
	TaskRejected    TaskState = "rejected"
	TaskInvalid     TaskState = "invalid"
	TaskInvalidated TaskState = "invalidated"
)

// RunState represents the state of a run.
type RunState string

const (
	RunActive    RunState = "active"
	RunCompleted RunState = "completed"
	RunFailed    RunState = "failed"
)

// ProjectRecord is a long-lived project container stored in the database.
// A project holds many runs over time, plus shared artifacts.
type ProjectRecord struct {
	ID          int64
	Name        string
	Description string
	CreatedBy   string // citizen ID
	RemoteURL   string // optional external git remote (push target after each commit)
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RunRecord is a run stored in the database.
type RunRecord struct {
	ID        int64  // global primary key (auto-increment)
	ProjectID int64  // the long-lived project this run belongs to
	Seq       int    // sequential run number within the project (#1, #2, #3)
	Name      string
	Ref       string // external reference (GitHub issue URL, etc.)
	YAMLData  string // raw YAML content
	RepoURL   string
	State     RunState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskRecord is a task instance stored in the database.
type TaskRecord struct {
	ID          string // full ID (e.g., "endometriosis:foundation")
	RunID   int64
	Seq         int    // sequential number within run (1-based, for quick reference)
	TaskDefID   string // original task ID from YAML
	InstanceKey string // for_each key (e.g., "endometriosis"), empty if no for_each
	// InstanceParams is a JSON-encoded map of for_each variable name ->
	// value for this instance, e.g. `{"gene":"BRCA1","tissue":"breast"}`.
	// Empty string for non-expanded tasks. Stored separately from
	// InstanceKey so iteration metadata can be rendered with real
	// variable names (gene=BRCA1) instead of reverse-parsing the slug,
	// and so underscore-in-value collisions are harmless.
	InstanceParams string
	Ref         string // external reference (URL)
	Action      string // "answer", "contribute", "compute", "review", "vote"
	Prompt      string
	UserPrompt  string
	Script      string
	Outputs      string // JSON: map of output name -> description/file/format
	Requirements string // JSON: categorized environment requirements
	ResultType   string
	Timeout     string
	State       TaskState
	ClaimedBy   int64 // citizens.id, 0 if unclaimed
	ClaimedAt   *time.Time
	SubmittedAt *time.Time
	ResultPath  string // path in git repo
	// CommitSHA is the git commit that landed this task's result.
	// Populated by the iteration A.2 client-side-writes path;
	// empty for tasks submitted via the legacy coordinator-writes
	// path (which uses the working tree's current state as the
	// implicit version).
	CommitSHA   string
	DependsOn   string // comma-separated list of dependency full IDs
	// ReadsArtifacts and WritesArtifacts are JSON arrays of repo-relative
	// artifact paths (e.g., ["src/analyze.py", "data/genes.csv"]). Reads
	// can be inferred from {{artifact:path}} prompt references; writes
	// must be declared explicitly.
	ReadsArtifacts  string
	WritesArtifacts string

	// Assignment and access control. Both optional — the default is
	// open: any registered citizen can claim. When set they narrow who
	// can claim. AssignTo is a JSON array of citizen IDs; RequireRole
	// is a role name checked against citizens.role. See
	// docs/task-assignment.md.
	AssignTo    string
	RequireRole string

	// Review action fields (Phase E). ReviewsTarget is the task def
	// id this review task evaluates, copied from YAML `reviews:`.
	// ReviewDecision is populated on submit for review-action tasks:
	// "approve" or "reject". Both are empty for non-review tasks.
	// On invalidation, ReviewDecision is cleared so a re-run of the
	// review can land a fresh verdict without the old one leaking
	// through.
	ReviewsTarget  string
	ReviewDecision string

	CreatedAt time.Time
}

// ArtifactRecord is the index row for one mutable file inside a project's
// repository. The file content itself lives only in git — this record
// just tracks who wrote it last and when, for provenance and listings.
type ArtifactRecord struct {
	ProjectID    int64
	Path         string // repo-relative path under artifacts/
	LastWriter   int64  // citizens.id of the last writer, 0 if never written
	LastTaskID   string // fully-qualified task ID that last wrote it
	LastRunID    int64  // run that did the last write
	// CommitSHA is the git commit that currently holds this artifact's
	// content. Used by the client-side template resolver (iteration
	// A.2) to read the exact version the index points at rather than
	// whatever happens to be in the working tree right now.
	CommitSHA string
	UpdatedAt    time.Time
	CreatedAt    time.Time
}

// CitizenRecord is a citizen stored in the database.
//
// Identity is a three-layer model:
//   - ID: internal integer primary key, never surfaced in user-facing output
//   - Username: immutable handle shown everywhere (assign_to, errors,
//     provenance). GitHub-compatible regex, unique.
//   - Name: freely mutable display name ("Tamer Gur"), used in greetings.
type CitizenRecord struct {
	ID              int64
	Username        string
	Name            string
	Email           string
	Role            string // "citizen", "author", "reviewer"
	Token           string
	Score           float64
	TasksCompleted  int
	TasksRejected   int
	TasksTimedOut   int
	TasksReleased   int
	TokensContrib   int64
	RegisteredAt    time.Time
	LastSeen        time.Time
}

// TaskClaimRecord tracks the history of task claims.
type TaskClaimRecord struct {
	ID            int64
	TaskID        string
	CitizenID     int64
	ClaimedAt     time.Time
	Deadline      time.Time
	Outcome       string // "completed", "timed_out", "released", "rejected"
	SubmittedAt   *time.Time
}

// ArtifactRecord is the index row for one mutable file inside a project's
// repository. The file content itself lives only in git — this record
// just tracks who wrote it last and when, for provenance and listings.
