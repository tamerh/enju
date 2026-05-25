// Package store provides the SQLite database layer for Cedar state management.
package store

import (
	"time"

	"github.com/enju-ai/enju/internal/common/types"
)

// Re-exports of shared domain types from internal/common/types.
// The canonical definitions live there so the fat-client can
// import them without violating the coord↔fatclient boundary;
// the store package re-exports them so coordinator-side code
// keeps writing `store.ReviewDecisionApprove` without having
// to add a second import.
type ReviewDecision = types.ReviewDecision

const (
	ReviewDecisionApprove        = types.ReviewDecisionApprove
	ReviewDecisionReject         = types.ReviewDecisionReject
	ReviewDecisionRequestChanges = types.ReviewDecisionRequestChanges
	ReviewDecisionComment        = types.ReviewDecisionComment
)

// IsValidReviewDecision is the package-local re-export of
// types.IsValidReviewDecision. Same body, single source of
// truth in common.
func IsValidReviewDecision(s string) bool { return types.IsValidReviewDecision(s) }

type ClaimOutcome = types.ClaimOutcome

const (
	ClaimOutcomeCompleted   = types.ClaimOutcomeCompleted
	ClaimOutcomeRejected    = types.ClaimOutcomeRejected
	ClaimOutcomeReleased    = types.ClaimOutcomeReleased
	ClaimOutcomeTimedOut    = types.ClaimOutcomeTimedOut
	ClaimOutcomeInvalidated = types.ClaimOutcomeInvalidated
	ClaimOutcomeAbandoned   = types.ClaimOutcomeAbandoned
	ClaimOutcomeFailed      = types.ClaimOutcomeFailed
)

// IsValidClaimOutcome re-exports types.IsValidClaimOutcome.
func IsValidClaimOutcome(s string) bool { return types.IsValidClaimOutcome(s) }

type CitizenKind = types.CitizenKind

const (
	CitizenKindHuman = types.CitizenKindHuman
	CitizenKindAgent   = types.CitizenKindAgent
)

// IsValidCitizenKind re-exports types.IsValidCitizenKind.
func IsValidCitizenKind(s string) bool { return types.IsValidCitizenKind(s) }

// TaskState represents the lifecycle state of a task.
type TaskState string

const (
	TaskPending  TaskState = "pending"
	TaskReady   TaskState = "ready"
	TaskClaimed  TaskState = "claimed"
	TaskRunning  TaskState = "running"
	TaskSubmitted TaskState = "submitted"
	TaskAccepted  TaskState = "accepted"
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
	// TaskFailed is the TERMINAL "stop, this is dead" state: a
	// citizen explicitly failed it (enju_fail_task), a review
	// rejected it, or a vote resolved against it. Downstream
	// descendants cascade to SKIPPED. Recovery is operator-
	// driven (enju_invalidate_task). Contrast TaskFailedRetryable.
	TaskFailed TaskState = "failed"
	// TaskFailedRetryable is the NON-terminal "this attempt
	// errored, but it's recoverable" state. Entered ONLY by a
	// compute task whose script exited non-zero (or a transient
	// infra failure) — never by a review reject / vote / explicit
	// enju_fail_task (those stay TaskFailed). Semantics:
	//  - NOT terminal: a run with a failed_retryable task stays
	//   WAITING (blocked on it), never auto-FAILED. The operator
	//   fixes + enju_retry_task, or explicitly terminates.
	//  - Blocks dependents like PENDING (an unsatisfied dep), NOT
	//   like SKIPPED — descendants are "waiting for this to
	//   succeed," not "done, not taken." The fail cascade does
	//   NOT run for this state.
	//  - Counts as "not done" for run-completion (a live blocker).
	//  - Has no open claim (the failed attempt's claim closed);
	//   the reaper ignores it.
	// enju_retry_task re-runs just this task (fresh iteration,
	// script from HEAD by default) without re-running accepted
	// upstream work.
	TaskFailedRetryable TaskState = "failed_retryable"
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
	//  - NOT terminal for run-completion purposes. A run with
	//   parked tasks stays active — they're awaiting
	//   reconciliation, not done.
	//  - NOT in any scheduler state set (ready / claimed /
	//   collecting). Parked tasks are invisible to
	//   enju_list_ready_tasks, the Your Queue view, and the
	//   UpdateReadyTasks sweep.
	//  - NOT in terminal sets (accepted / skipped / failed).
	//   Run completion checks naturally don't count parked
	//   as done.
	//
	// Stale (non-matching-key) parked rows are removed by the
	// reconciliation pass (Phase 2) via the regular
	// subtree-delete machinery. Parked is always a
	// "provisionally preserved" state, never long-lived in a
	// completed run.
	TaskParked TaskState = "parked"
)

// RunState represents the state of a run.
//
// State transitions:
//
//	create → active
//	active → waiting   (no in-flight claim, but non-terminal tasks remain)
//	waiting → active   (a task transitions to ready, e.g. after invalidate or future task-spawn)
//	active|waiting → paused    (explicit enju_pause_run)
//	paused → active|waiting    (explicit enju_resume_run, then re-evaluate)
//	active|waiting → completed   (every task is in {accepted, skipped, failed})
//
// WAITING (Phase 8.1.c — replaced the legacy `idle` value) means
// "no work currently in flight, but the run isn't sealed."
// Distinct from ACTIVE (some claim is being worked) and from
// COMPLETED (all tasks terminal). Phase 8.5 will pair this state
// with a blocked_by JSON field naming the specific bottleneck
// (review awaiting reviewer, dependent artifact still computing,
// etc.) so the operator can see *why* the run is paused for
// outside input rather than just "idle."
//
// Paused freezes the state machine until an operator resumes;
// today the state is observable, claim/submit gating arrives
// with the living-workflow polish.
type RunState string

const (
	RunActive    RunState = "active"
	RunWaiting   RunState = "waiting"
	RunPaused    RunState = "paused"
	RunCompleted RunState = "completed"
	RunFailed    RunState = "failed"
	// RunTerminated is the human-pulled-the-plug terminal
	// state. Distinct from RunFailed so audit / dashboards
	// can distinguish "operator aborted" from "validation
	// said no" — different signals about workflow quality.
	// All non-terminal tasks of the run get marked
	// TaskSkipped with skip_reason="run_terminated"; open
	// claims close with outcome=abandoned. Topic branches
	// stay (immutable git audit). Terminate is irreversible
	// — distinct from pause/resume.
	RunTerminated RunState = "terminated"
)

// TerminalRunStates is the single source of truth for which run
// states are terminal (a run that will never run more tasks).
// Both the terminal-state predicate (runStateTerminal) and the
// archive precondition's "any non-terminal run?" query derive
// from this slice, so adding a future terminal state can't
// silently desync the two.
var TerminalRunStates = []RunState{RunCompleted, RunFailed, RunTerminated}

// IsAlive reports whether a run still owns its (project, branch)
// slot — i.e. is not in a terminal state. The unique-branch index
// keys off the same predicate so two alive runs can't collide on
// one branch even if one of them is idle or paused.
func (s RunState) IsAlive() bool {
	switch s {
	case RunActive, RunWaiting, RunPaused:
		return true
	}
	return false
}

// ProjectRecord is a long-lived project container stored in the database.
// A project holds many runs over time, plus shared artifacts.
type ProjectRecord struct {
	ID     int64
	Name    string
	Description string
	CreatedBy  string // citizen ID
	RemoteURL  string // optional external git remote (push target after each commit)
	// DefaultBranch is the git branch new runs land on when the
	// caller doesn't override with `branch:` at create_run time.
	// Always non-empty after migration — defaults to "main" for
	// legacy rows and new projects unless the caller sets it on
	// create_project / init. Orgs that want Enju activity to
	// stay off their repo's main branch set this to e.g.
	// "enju/work" at project creation time. See
	// docs/runs-and-branches.md for the full rationale.
	DefaultBranch string

	// Reversible archive. Archived hides the project from
	// enju_list_projects' default view; it is NOT deletion — no row
	// removal, no on-disk effect, fully restorable. ArchivedAt/
	// ArchivedBy are last-archive provenance: set on archive and
	// DELIBERATELY KEPT on restore (cheap, and a future
	// irreversible-purge tombstone will want them). Legacy rows
	// scan archived=false / zero time / "". Zero ArchivedAt
	// (time.Time{}) reads as "never archived".
	Archived   bool
	ArchivedAt time.Time
	ArchivedBy string // citizen ID that archived it

	// LastActivityAt is bumped to time.Now() by ApplyPlan at the
	// end of any Plan that emitted an event touching this project
	// (every state-changing mutation hits it). Surfaced on
	// wire.Project so the web-UI's project sort has a freshness
	// signal that beats CreatedAt for long-running projects.
	// Zero (legacy rows pre-first-mutation) — readers floor to
	// CreatedAt.
	LastActivityAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RunRecord is a run stored in the database.
type RunRecord struct {
	ID    int64 // global primary key (auto-increment)
	ProjectID int64 // the long-lived project this run belongs to
	Seq    int  // sequential run number within the project (#1, #2, #3)
	Name   string
	Ref    string // external reference (GitHub issue URL, etc.)
	YAMLData string // raw YAML content
	RepoURL  string
	State   RunState
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
	Params string
	// Branch is the git branch this run commits to. Populated
	// at create_run time from the caller's `branch:` param,
	// falling back to the project's DefaultBranch when the
	// caller doesn't specify one. The serial-runs-per-branch
	// invariant means the coordinator refuses a second active
	// run on the same branch — concurrent runs MUST use
	// distinct branches. See docs/runs-and-branches.md.
	Branch  string
	// Slug is a filesystem-safe identifier derived at
	// create_run time from the template bundle dir (template
	// mode) or the parsed run name (inline YAML). Used to
	// render the self-documenting directory layout
	// enju/runs/{seq}-{slug}/. Empty-string defaults are
	// treated as "run" by the layout helper so old rows still
	// resolve to a valid path. Stamped once at creation and
	// never updated — same stability contract as Branch.
	Slug   string
	// BlockedBy carries a JSON description of why a WAITING run
	// can't make progress. Populated by applyCompleteRun at
	// the moment of the active|whatever → waiting transition;
	// cleared when the run leaves waiting. Empty string for
	// non-WAITING runs (the column is nullable; the scanner
	// folds NULL → ""). Surface readers (enju_run_status)
	// gate on State == RunWaiting before parsing.
	//
	// One of (JSON):
	//   {"kind":"review",       "task":"...","assignee":"...","since":"..."}
	//   {"kind":"human_claim",  "task":"...","assignee":"..."}
	//   {"kind":"artifact",     "task":"...","awaiting_path":"..."}
	//   {"kind":"stuck",        "detail":"..."}
	BlockedBy string
	// SyncStatus carries a JSON description of a run-completion
	// sync that needs operator attention — set when the
	// fat-client's run-branch → base merge hit a conflict and
	// could NOT integrate the run's output into the default
	// branch. Unlike BlockedBy (which is gated on RunWaiting and
	// cleared on every other transition), SyncStatus persists
	// across the terminal-completed state: a run can be
	// `completed` AND still have lost output that never reached
	// base. Empty string = sync clean / not yet attempted /
	// mode:none. Set once by the run_sync_conflict report; not
	// auto-cleared (the operator resolving the merge by hand is
	// out-of-band — a future enju_resolve_sync could clear it).
	//
	// Shape (JSON): {"kind":"conflict","run_branch":"...",
	//   "base_branch":"...","conflict_files":[...],"hint":"..."}
	SyncStatus string
	// SyncModeOverride is the operator-supplied sync mode from the
	// CLI `--sync` flag (or the `sync_mode_override` MCP param).
	// When non-empty, it takes precedence over the workflow YAML's
	// `sync:` block at run-completion time. One of "none", "merge",
	// "push". Empty means "use the YAML's sync: block (default:
	// merge)".
	SyncModeOverride string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskRecord is a task instance stored in the database.
type TaskRecord struct {
	ID     string // full ID (e.g., "endometriosis:foundation")
	RunID  int64
	Seq     int  // sequential number within run (1-based, for quick reference)
	TaskDefID  string // original task ID from YAML
	InstanceKey string // for_each key (e.g., "endometriosis"), empty if no for_each
	// InstanceParams is a JSON-encoded map of for_each variable name ->
	// value for this instance, e.g. `{"gene":"BRCA1","tissue":"breast"}`.
	// Empty string for non-expanded tasks. Stored separately from
	// InstanceKey so iteration metadata can be rendered with real
	// variable names (gene=BRCA1) instead of reverse-parsing the slug,
	// and so underscore-in-value collisions are harmless.
	InstanceParams string
	Ref     string // external reference (URL)
	Action   string // "answer", "contribute", "compute", "review", "vote"
	Prompt   string
	UserPrompt string
	Script   string
	Outputs   string // JSON: map of output name -> description/file/format
	Requirements string // JSON: categorized environment requirements
	ResultType  string
	Timeout   string
	State    TaskState
	ClaimedBy  int64 // citizens.id, 0 if unclaimed
	ClaimedAt  *time.Time
	SubmittedAt *time.Time
	ResultPath string // path in git repo
	// CommitSHA is the git commit that landed this task's result.
	// Populated by the iteration A.2 client-side-writes path;
	// empty for tasks submitted via the legacy coordinator-writes
	// path (which uses the working tree's current state as the
	// implicit version).
	CommitSHA  string
	DependsOn  string // comma-separated list of dependency full IDs
	// ReadsArtifacts and WritesArtifacts are JSON arrays of repo-relative
	// artifact paths (e.g., ["src/analyze.py", "data/genes.csv"]). Reads
	// can be inferred from {{artifact:path}} prompt references; writes
	// must be declared explicitly.
	ReadsArtifacts string
	WritesArtifacts string

	// RunSlug mirrors the enclosing run's Slug, denormalized
	// onto every task row so engine.ComputeResultDir can
	// render enju/runs/{seq}-{slug}/ without a JOIN on every
	// task-response serialization. Stamped at creation time
	// in BuildRunTasks from the caller-supplied slug.
	RunSlug string

	// Assignment and access control. Both optional — the default is
	// open: any registered citizen can claim. When set they narrow who
	// can claim. AssignTo is a JSON array of citizen IDs; RequireRole
	// is a role name checked against citizens.role. See
	// docs/task-assignment.md.
	AssignTo  string
	RequireRole string

	// Review action fields (Phase E). ReviewsTarget is the task def
	// id this review task evaluates, copied from YAML `reviews:`.
	// ReviewDecision is populated on submit for review-action tasks:
	// "approve" or "reject". Both are empty for non-review tasks.
	// On invalidation, ReviewDecision is cleared so a re-run of the
	// review can land a fresh verdict without the old one leaking
	// through.
	ReviewsTarget string
	ReviewDecision ReviewDecision

	// Vote action fields (Phase E.2). VoteOptions is the JSON-
	// encoded list of declared options with their id/label/activates
	// lists, copied from YAML at run-creation time. VoteChoice is
	// the submitted option id, populated on submit and cleared on
	// invalidation so a re-run lands a fresh choice. Citizens,
	// MinQuorum, VoteThreshold, and VoteDeadline are the tally-
	// rule fields parsed from YAML; VoteDeadline is stored as a
	// Go duration string ("2h", "24h"). All zero-value for
	// non-vote tasks.
	VoteOptions  string
	VoteChoice  string
	Citizens   int
	MinQuorum   int
	VoteThreshold string
	VoteDeadline string
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
	// Env is the JSON-encoded map[string]string of task-
	// definition-level environment variables for compute
	// tasks. Empty string for non-compute tasks or compute
	// tasks that didn't declare an env: block. Injected into
	// the compute script's process environment alongside the
	// system ENJU_* vars and run-level ENJU_PARAM_<name>
	// vars — three disjoint namespaces, no override logic.
	Env string

	// Mode controls whether a compute task's enju_execute_task
	// call blocks until the script completes ("sync") or kicks
	// off a detached wrapper and returns immediately ("async").
	// Only populated for action:compute tasks; empty for every
	// other action. Copied from the YAML TaskDef.Mode field at
	// create_run time and resolved via yaml.ResolvedMode at
	// read sites so the default-to-sync rule lives in one place.
	Mode string

	// Container is the OCI image reference for compute tasks
	// that run inside a container (e.g. "alpine:3.19"). Empty
	// for bare-host compute tasks. Copied from the YAML
	// TaskDef.Container field at create_run time so the
	// fat-client executor doesn't need to parse the template
	// snapshot YAML at execute time.
	Container string

	// ContainerRuntime selects the container backend
	// ("docker", "podman", etc.). Empty means the executor
	// uses its configured default. Copied from YAML at
	// create_run time for the same reason as Container.
	ContainerRuntime string

	// Volumes is the JSON-encoded []string of extra host paths
	// bind-mounted into the container, on top of the implicit
	// workspace/scratch/snapshot/shared-root binds. Each entry
	// is a "host[:container[:mode]]" spec with run params
	// already resolved. Empty string for non-container tasks or
	// container tasks that declared no extra volumes. Copied
	// from the YAML TaskDef.Volumes field at create_run time so
	// the fat-client executor doesn't re-parse the snapshot.
	Volumes string

	// Executor selects where a compute task's wrapper runs:
	// "" / "local" (host fork) or "slurm" (sbatch job). Copied
	// from the YAML TaskDef.Executor at create_run time, surfaced
	// on the wire so the fat-client picks the right launcher and
	// the effective-async rule (yaml.ResolvedModeFields) sees it.
	Executor string

	// Resources is the JSON-encoded yaml.Resources SLURM ask
	// (partition/time/cpus/mem/gpus/sbatch_extra). Empty string
	// for non-slurm tasks or a zero ask. Copied from the YAML
	// TaskDef.Resources at create_run time; the fat-client
	// decodes it and hands it to executor.Executor.Submit.
	Resources string

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

	// SpawnedFrom and SpawnTrigger record runtime spawn provenance
	// (living-workflow phase 4). SpawnedFrom is the parent task's
	// full id (empty for tasks authored at run-create time);
	// SpawnTrigger is one of "human", "agent", "template_rule",
	// "auto_triage" describing which mechanism fired the spawn.
	// The detailed audit lives in events as
	// task_spawned; these columns make lineage queryable in a
	// single row.
	SpawnedFrom string
	SpawnTrigger string

	// Review-failure spawn rules (living-workflow phase 4b).
	// Declared in YAML on the dev task; consulted by the engine
	// when a reviewing task rejects this one.
	//
	//  OnReviewReject:     "" (default cascade) | "spawn_remediation"
	//  OnReviewRequestChanges: "" / "continue_iteration" (default) | "spawn_remediation"
	//  RemediationTemplate:  JSON-encoded yaml.RemediationTemplate; empty when no rule applies
	OnReviewReject     string
	OnReviewRequestChanges string
	RemediationTemplate  string

	// Living-workflow phase 4c — auto-triage linkage. When > 0,
	// this task was spawned by the auto-triage hook to fix the
	// project-scoped issue with the given seq. On task accept,
	// the close-on-accept hook transitions that issue to
	// `closed`. 0 on every other task.
	ClosesIssueSeq int

	// Layer-① contract-gate recovery (citizen-task-retryable).
	//
	// VerifyFailCount is the durable per-task count of CONSECUTIVE
	// completed iterations that ended in layer-① non-delivery: the
	// claimant never produced the declared writes_artifacts, so the
	// submission was refused before any reviewer saw it. It is
	// bumped at most once per iteration (see VerifyFailCountedIter)
	// by EITHER the fat-client's ReportCitizenVerifyFail report OR
	// the coordinator reaper observing the lease expire with no
	// submission recorded for that iter_seq — the coordinator is the
	// enforcement boundary, the client only accelerates. Resets to 0
	// on any delivery (submission accepted, invalidate-reopen,
	// retry-reopen) so it measures CONSECUTIVE non-delivery: a task
	// that flaked twice then delivered must not be left one expiry
	// from escalation on some later unrelated reopen. NEVER bumped by
	// request_changes — that path is submitted→accepted→review
	// (layer ① already passed, count already reset at accept).
	VerifyFailCount int

	// VerifyRetryCap is the per-task cap on consecutive layer-①
	// non-delivery iterations before the coordinator parks the task
	// as failed_retryable. 0 means "use the coordinator's global
	// defaultVerifyFailCap const." Populated from the YAML task
	// definition's verify_retry_cap: (and the workflow defaults:
	// verify_retry_cap:) at run-create time. Mirrors how Timeout is
	// resolved: per-task wins over defaults wins over the const.
	VerifyRetryCap int

	// VerifyFailCountedIter is the highest claim iter_seq for which
	// VerifyFailCount has already been incremented. The increment
	// mutation is gated `verify_fail_counted_iter < :iter`, making it
	// idempotent on (task_id, iter_seq) so the client report and the
	// reaper backstop can never double-count the same iteration.
	// Reset to 0 alongside VerifyFailCount on every delivery.
	VerifyFailCountedIter int

	// Retries is the compute auto-retry budget (Snakemake's retries:):
	// the number of EXTRA automatic re-runs the coordinator grants a
	// compute task that fails transiently (kind=compute_error) before
	// parking it failed_retryable. 0 = no auto-retry. Populated from
	// the YAML task def at run-create time. The attempt count is the
	// failing claim's iter_seq (no separate counter column): a failure
	// at iter_seq <= Retries re-admits the task to READY; beyond it
	// parks failed_retryable.
	Retries int

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
	ProjectRoleOwner ProjectRole = "owner"
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
	Role   ProjectRole
	AddedAt  time.Time
	AddedBy  int64 // citizens.id of the adder; 0 for the creator row
}

// IterationRecord is the projection of one attempt at a task —
// living-workflow phase 5. Each row is a task_claims row enriched
// with the per-task seq counter, the claimant's username, and the
// review decision (when applicable). The DB still stores claims
// row-by-row; this struct is what enju_get_task and
// enju_list_iterations surface.
//
// "Iteration" maps to "task_claims row" in v1 — one row per
// fresh claim. Within a claim, request_changes today invalidates
// and re-claims (creating a new iteration); a future
// request_changes-stays-on-same-claim refactor would let one
// iteration carry multiple submissions, at which point the
// CommitSHA field becomes a list.
//
// Outcome values (verbatim from task_claims):
//  - "" (active)
//  - "completed" — submitted and accepted
//  - "invalidated" — cascade-invalidated by an upstream rejection
//  - "released"  — claimant released voluntarily
//  - "timed_out"  — reaper claimed the deadline pass
type IterationRecord struct {
	TaskID     string
	Seq      int  // 1-based, ordered by claimed_at
	// IterSeq is the task_claims.iter_seq column — the
	// accept-cycle counter (Phase 6c). Distinct from Seq
	// (the projection's 1-based row index): multi-citizen
	// tasks have N citizens claiming with the SAME iter_seq
	// for one accept-cycle, so for those tasks Seq counts
	// claim-rows while IterSeq counts cycles. Surfaced in
	// Phase 8.6 so iter_count = COUNT(DISTINCT iter_seq) is
	// computable without a separate query.
	IterSeq    int
	CitizenID   int64 // claimant
	Username    string // resolved at projection time
	ClaimedAt   time.Time
	Deadline    time.Time
	SubmittedAt  *time.Time
	Outcome    ClaimOutcome
	CommitSHA   string // the task's commit at submit time; "" until submitted
	ReviewDecision ReviewDecision // approve | request_changes | reject | comment | "" (no decision yet)
	Option     string // vote choice (vote tasks)
	Model      string // normalized model-name label (per-claim attribution; "" when no LLM)
	// Branch is the iteration-scoped topic branch identifier
	// (living-workflow phase 6a). Format:
	// "<run-slug>/<task_def_id>/iter-<N>". Empty for
	// vote/review tasks (no git artifact) and for pre-phase-6a
	// rows. Phase 6b will use this as the actual git ref the
	// fat client checks out / commits to; phase 6a just records
	// it for audit and forward compat.
	Branch string
}

// IssueRecord is one row in the issues table — a project-level
// structured artifact. Issues outlive runs: filed in one run,
// possibly triaged in a later run, possibly closed by a fix-task
// in a yet-later run. See docs/living-workflow-design-notes.md § 6.
type IssueRecord struct {
	ID       int64
	ProjectID   int64
	Seq      int  // per-project counter — ISSUE-001, ISSUE-002, ...
	Title     string
	Body      string
	Status     IssueStatus  // "open" | "triaged" | "in_progress" | "closed" | "wontfix"
	Severity    IssueSeverity // "low" | "medium" | "high" | "critical"
	FoundInRunID  int64 // 0 if not run-scoped (rare)
	FoundInTaskID string // empty if not task-scoped (e.g. filed against the project as a whole)
	FiledBy    int64 // citizen ID
	FiledAt    time.Time
	TriagedBy   int64   // 0 until triaged
	TriagedAt   *time.Time // nil until triaged
	// ClosedByTaskID has dual semantics depending on Status:
	//  - status=in_progress: the fix task currently working
	//   on this issue (set by MarkIssueInProgress).
	//  - status=closed:   the fix task whose acceptance
	//   resolved this issue (set by CloseIssue, often the
	//   same value MarkIssueInProgress wrote).
	//  - status=open / triaged / wontfix: empty.
	// Column name is historical — "closed by" was accurate
	// pre-phase-4c when issues went open → triaged → closed
	// directly. The phase-4c in_progress status overloaded
	// the field as a "linked fix-task" pointer.
	ClosedByTaskID string
	ClosedAt    *time.Time // nil while open/triaged/in_progress
	UpdatedAt   time.Time
}

// ArtifactRecord is the index row for one mutable file inside a
// project's repository. The file content itself lives only in
// git — this record just tracks who wrote it last and when, for
// provenance and listings.
//
// Keyed by (project_id, branch, path) so runs on isolated
// branches don't stomp on each other's artifact index. A run
// writing artifacts on branch "experiment-2" and another run
// writing the same path on branch "main" produce two rows, each
// pointing at a commit on their own branch.
type ArtifactRecord struct {
	ProjectID int64
	Branch   string // git branch this artifact write lives on
	Path    string // repo-relative path
	LastWriter int64 // citizens.id of the last writer, 0 if never written
	LastTaskID string // fully-qualified task ID that last wrote it
	LastRunID int64 // run that did the last write
	// CommitSHA is the git commit that currently holds this artifact's
	// content. Used by the client-side template resolver (iteration
	// A.2) to read the exact version the index points at rather than
	// whatever happens to be in the working tree right now.
	//
	// Empty when Tracked is false — untracked artifacts are not in
	// git, so there is no SHA to point at. The resolver must handle
	// the empty-SHA case explicitly (stat the working tree, not a
	// historical ref).
	CommitSHA string
	// Tracked reports whether the artifact's bytes live in git
	// (true) or only on disk / shared storage (false). Declared
	// per-entry in YAML via `writes_artifacts: {path:..., track:...}`.
	// Defaults to true — the legacy bare-string form always lands
	// as tracked.
	Tracked  bool
	UpdatedAt time.Time
	CreatedAt time.Time
}

// CitizenRecord is a citizen stored in the database.
//
// Identity is a three-layer model:
//  - ID: internal integer primary key, never surfaced in user-facing output
//  - Username: immutable handle shown everywhere (assign_to, errors,
//   provenance). GitHub-compatible regex, unique.
//  - Name: freely mutable display name ("Tamer Gur"), used in greetings.
type CitizenRecord struct {
	ID       int64
	Username    string
	Name      string
	Email      string
	Role      string // "citizen", "author", "reviewer"
	// A citizen row carries no token. Authentication is the
	// tokens table exclusively (issued, labelled, revocable,
	// multiple per citizen). Use ListTokensByCitizen / the tokens
	// table for anything token-related.
	Score      float64
	TasksCompleted int
	TasksRejected  int
	TasksTimedOut  int
	TasksReleased  int
	TokensContrib  int64
	RegisteredAt  time.Time
	// LastSeen is reserved for a future presence/online-indicator
	// feature. The column + struct field are populated on
	// citizen registration but NOT updated thereafter — the
	// per-API-request UPDATE was removed because nothing in
	// production reads this value. When a future feature needs
	// it, re-add the writes deliberately at that point.
	LastSeen    time.Time
	// Kind is the citizen-kind discriminator: "human" (a person)
	// or "agent" (an unattended citizen owned by a human, with
	// its own token, that claims and executes tasks). A model is
	// NOT a kind — it has no identity and is a label on the work.
	Kind      CitizenKind // empty string = legacy/normalized to Human at read time
	// ParentID is the owner chain for agents. Non-nil for
	// kind='agent' (points at the citizen that owns this agent);
	// nil for humans. Used by enju_my_agents and revocation cascades.
	ParentID    *int64
	// TenantID is the root-owner citizen at the top of the
	// parent_id chain — the tenancy seam. A human root's tenant
	// is itself; an agent's tenant is its owner's tenant. Set at
	// creation and never changed. Populated for every row after
	// creation (nil only on an unsaved in-memory record).
	TenantID    *int64
}

// TokenRecord is one row from the tokens table — an issued bearer
// token with optional label and revocation timestamp. Multiple
// tokens per citizen are allowed (rotation, per-deployment labels).
// Part of the operator/model design — see
// docs/operator-model-design.md.
type TokenRecord struct {
	ID     int64
	CitizenID int64
	Token   string
	Label   string   // "ci-server", "laptop", "" for legacy-migrated tokens have label='legacy'
	IssuedAt  time.Time
	RevokedAt *time.Time // nil = active
	LastUsedAt *time.Time // nil = never used (or last_used_at not yet wired up)
}

// TaskClaimRecord tracks the history of task claims. In multi-
// citizen tasks (citizens > 1), one row per (task, citizen) pair —
// the table functions as the per-citizen submission log. The Option
// column carries the citizen's vote choice for vote-action tasks,
// populated on submit alongside SubmittedAt and outcome.
type TaskClaimRecord struct {
	ID     int64
	TaskID   string
	CitizenID  int64
	ClaimedAt  time.Time
	Deadline  time.Time
	Outcome   ClaimOutcome // empty while open; one of the typed constants once closed
	SubmittedAt *time.Time
	// Option is the citizen's submitted choice on a vote-action
	// task. Empty for non-vote submissions and for claims that
	// haven't submitted yet.
	Option string
	// Model is the normalized model-name label credited for this
	// submission's text — a plain string, not a citizen (a model
	// has no identity). Empty for script tasks and for human
	// submits with no LLM (hand-reviewed work).
	Model string
	// Branch is the per-iteration topic branch this claim writes
	// to (e.g. "myrun/expand/iter-1"). Generated at claim time
	// from (run-slug, task-def-id, prior-claim-count). Empty for
	// vote/review actions and for legacy-migrated rows.
	Branch string
	// CommitSHA is the git SHA of the submission commit that
	// resolved this claim — captured at submit time so future
	// re-iterations don't overwrite this row's history. Empty
	// when the submission produced no commit (untracked-only
	// outputs, vote/review without a content commit) or when
	// the claim hasn't been submitted yet.
	CommitSHA string
	// Decision is the reviewer verdict captured at submit time
	// for action:review claims (approve / request_changes /
	// reject / comment). Empty for non-review claims and for
	// unresolved claims.
	Decision ReviewDecision
	// IterSeq is the iteration counter the apply path stamped
	// on this claim row (Phase 6c). Increments across reopens;
	// stays the same across multiple submissions within one
	// iteration. Surfaced here so the fat-client trailer
	// ('s Enju-Iter-Seq) and audit projections can
	// read it without a second SELECT.
	IterSeq int
}


// ContributionEvent records a single scoreable action by a
// citizen. The events log is append-only — events are never
// deleted, even when the underlying task is invalidated
// (recorded as a separate event). This gives future scoring
// functions a complete audit trail and mirrors the append-
// only git philosophy.
//
// Event types:
//  - task_completed (subtype: action, e.g. "answer")
//  - review_given (subtype: "approve" or "reject")
//  - vote_cast (subtype: the chosen option id)
//  - run_created (subtype: empty)
//  - task_rejected (subtype: empty — task was invalidated)
//  - task_timed_out (subtype: empty)
//  - task_released (subtype: empty)
type ContributionEvent struct {
	ID      int64
	CitizenID  int64
	EventType  string
	EventSubtype string
	TaskID    string
	RunID    int64
	ProjectID  int64
	Metadata   string // JSON blob (tokens, compute time, etc.)
	CreatedAt  time.Time
}
