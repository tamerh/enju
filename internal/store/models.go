// Package store provides the SQLite database layer for Cedar state management.
package store

import "time"

// TaskState represents the lifecycle state of a task.
type TaskState string

const (
	TaskPending    TaskState = "pending"
	TaskReady      TaskState = "ready"
	TaskClaimed    TaskState = "claimed"
	TaskRunning    TaskState = "running"
	TaskSubmitted  TaskState = "submitted"
	TaskAccepted   TaskState = "accepted"
	TaskRejected   TaskState = "rejected"
	TaskInvalid    TaskState = "invalid"
	TaskInvalidated TaskState = "invalidated"
	// TaskCollecting is Phase E.2 session 2a's intermediate state
	// for multi-citizen tasks. A task with `citizens: N > 1`
	// enters COLLECTING on first submission and stays there until
	// the tally resolves (quorum + threshold met, or deadline
	// forces resolution). During COLLECTING: additional citizens
	// can still claim slots, additional submissions are accepted,
	// and the task is NOT terminal (run completion waits on it,
	// dependent tasks stay blocked).
	TaskCollecting TaskState = "collecting"
	// TaskSkipped is a terminal state introduced in Phase E.2
	// for vote skip cascades. When an `action: vote` task resolves
	// with an option that has `activates:`, tasks reachable only
	// through *losing* options flip to SKIPPED. The scheduler
	// treats SKIPPED identically to ACCEPTED for run-completion
	// counting and dependency satisfaction — a skipped task is
	// "done, not taken," not "failed" or "pending."
	TaskSkipped TaskState = "skipped"
	// TaskFailed is a terminal state for tasks that a citizen
	// explicitly failed (via enju_fail_task) or that a compute
	// script exited non-zero on. Downstream descendants
	// cascade to FAILED. Recovery: enju_invalidate_task
	// bounces the task back to READY.
	TaskFailed TaskState = "failed"
	// TaskParked is introduced by the J.2 "partial
	// re-materialization" pass. When a dynamic for_each source
	// is invalidated, its materialized descendants used to be
	// deleted on the spot — destroying any in-flight reviews /
	// ballots / accepted work. They now transition to PARKED
	// instead: the row stays, claimed_by / commit_sha /
	// task_claims are untouched, and the prior state is
	// stashed in `parked_from_state` so a matched-key
	// reconciliation on re-accept can losslessly restore
	// (state = parked_from_state, parked_from_state = '').
	//
	// Semantics vs. other states:
	//   - NOT terminal for run-completion purposes. A run with
	//     parked tasks stays active — they're awaiting
	//     reconciliation, not done.
	//   - NOT in any scheduler state set (ready / claimed /
	//     collecting). Parked tasks are invisible to
	//     enju_list_ready_tasks, the Your Queue view, and the
	//     UpdateReadyTasks sweep.
	//   - NOT in terminal sets (accepted / skipped / failed).
	//     Run completion checks naturally don't count parked
	//     as done.
	//
	// Stale (non-matching-key) parked rows are removed by the
	// reconciliation pass (Phase 2) via the regular
	// subtree-delete machinery. Parked is always a
	// "provisionally preserved" state, never long-lived in a
	// completed run.
	TaskParked TaskState = "parked"
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
	// SourcePath is the repo-relative template path this run was
	// instantiated from, if any. Populated when the run was
	// created via enju_create_run with a `path:` pointing at a
	// templates/*.yaml file. Empty for inline-YAML submissions.
	// Used for provenance display ("this run came from
	// templates/gwas.yaml") and by future tooling that wants to
	// surface "all runs from template X."
	SourcePath string
	// SourceCommitSHA is the project HEAD at template
	// instantiation time. Captured so future reproducibility
	// tooling can answer "which commit of templates/gwas.yaml
	// produced this run?" — the file on disk may have changed
	// between runs, but given this SHA the original version is
	// always reachable via `git show SHA:templates/gwas.yaml`.
	// Empty for inline-YAML submissions.
	SourceCommitSHA string
	// Params is the JSON-encoded map of run-level params the
	// caller supplied at create_run time, AFTER defaults were
	// filled in for any omitted optional params. Persisted so
	// the compute executor can expose them to scripts as
	// ENJU_PARAM_<name> env vars — without this, submitted
	// params were substituted into prompts at parse time and
	// thrown away, unreachable from compute-task scripts.
	//
	// Empty for runs with no params: block.
	Params    string
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

	// Vote action fields (Phase E.2). VoteOptions is the JSON-
	// encoded list of declared options with their id/label/activates
	// lists, copied from YAML at run-creation time. VoteChoice is
	// the submitted option id, populated on submit and cleared on
	// invalidation so a re-run lands a fresh choice. Citizens,
	// MinQuorum, VoteThreshold, and VoteDeadline are the tally-
	// rule fields parsed from YAML; VoteDeadline is stored as a
	// Go duration string ("2h", "24h"). All zero-value for
	// non-vote tasks.
	VoteOptions   string
	VoteChoice    string
	Citizens      int
	MinQuorum     int
	VoteThreshold string
	VoteDeadline  string
	// Anonymize hides citizen usernames in {{task.responses}}
	// and in the task-detail voting/review block. Valid on
	// action:vote and action:review. Copied from the YAML
	// `anonymize:` field at run creation.
	Anonymize bool
	// Visibility is "open" (default) or "blind". "blind"
	// hides sibling ballots from a claimer while the task is
	// still COLLECTING; once ACCEPTED everyone sees the full
	// tally. Copied from the YAML `visibility:` field.
	Visibility string
	// FailReason records why a task was explicitly failed
	// (via enju_fail_task or compute exit non-zero). Empty
	// for non-failed tasks.
	FailReason string
	// SkipReason records why a task was skipped. Empty for
	// vote-cascade skips (losing branch); populated with
	// "upstream failed: <taskID>" when a descendant is
	// skipped because its parent went FAILED via review
	// reject or enju_fail_task. The run_status formatter
	// keys the ⊘-vs-⚫ glyph off this field.
	SkipReason string
	// ParkedFromState stashes the prior state when a task
	// transitions to TaskParked during J.2 partial
	// re-materialization. Restored lossless on reconciliation:
	// `state = parked_from_state, parked_from_state = ''`.
	// Empty string when state is not (and was never) parked.
	//
	// The stash is NOT the same as a transition log — we only
	// carry the immediately-prior state, not a full history.
	// Two consecutive park-then-reconcile cycles work by
	// induction: the second park stashes whatever state the
	// first restore produced.
	ParkedFromState string

	CreatedAt time.Time
}

// ProjectRole is a citizen's role within a specific project. Stored
// as TEXT in project_members.role so new tiers (viewer, maintainer,
// etc.) can slot in without a schema migration. The GitHub-style
// starting model ships with two values: owner (admin) and member
// (contributor). Orthogonal to CitizenRecord.Role, which is a
// cross-project role slot reserved for future use.
type ProjectRole string

const (
	ProjectRoleOwner  ProjectRole = "owner"
	ProjectRoleMember ProjectRole = "member"
)

// ProjectMemberRecord is one row in the project_members join table:
// a citizen's membership of a single project, with their role and
// who added them. AddedBy is zero for rows created by the
// creator-auto-add path (no one added the creator; they created
// the project).
type ProjectMemberRecord struct {
	ProjectID int64
	CitizenID int64
	Role      ProjectRole
	AddedAt   time.Time
	AddedBy   int64 // citizens.id of the adder; 0 for the creator row
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

// TaskClaimRecord tracks the history of task claims. In multi-
// citizen tasks (citizens > 1), one row per (task, citizen) pair —
// the table functions as the per-citizen submission log. The Option
// column carries the citizen's vote choice for vote-action tasks,
// populated on submit alongside SubmittedAt and outcome.
type TaskClaimRecord struct {
	ID          int64
	TaskID      string
	CitizenID   int64
	ClaimedAt   time.Time
	Deadline    time.Time
	Outcome     string // "completed", "timed_out", "released", "rejected"
	SubmittedAt *time.Time
	// Option is the citizen's submitted choice on a vote-action
	// task. Empty for non-vote submissions and for claims that
	// haven't submitted yet.
	Option string
	// Content is the citizen's prose commentary (best-effort,
	// stored denormalized from git for quick tally rendering).
	// Kept short — formatters that need the full prose still read
	// from the per-citizen result.md in the project's git repo.
	Content string
}


// ContributionEvent records a single scoreable action by a
// citizen. The events log is append-only — events are never
// deleted, even when the underlying task is invalidated
// (recorded as a separate event). This gives future scoring
// functions a complete audit trail and mirrors the append-
// only git philosophy.
//
// Event types:
//   - task_completed (subtype: action, e.g. "answer")
//   - review_given (subtype: "approve" or "reject")
//   - vote_cast (subtype: the chosen option id)
//   - run_created (subtype: empty)
//   - task_rejected (subtype: empty — task was invalidated)
//   - task_timed_out (subtype: empty)
//   - task_released (subtype: empty)
type ContributionEvent struct {
	ID           int64
	CitizenID    int64
	EventType    string
	EventSubtype string
	TaskID       string
	RunID        int64
	ProjectID    int64
	Metadata     string // JSON blob (tokens, compute time, etc.)
	CreatedAt    time.Time
}
