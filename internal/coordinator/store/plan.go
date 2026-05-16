package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// Plan is the output of every engine computation. It's an
// ordered list of Mutations that the coordinator validates
// against current DB state and applies in a single
// transaction. If any mutation fails validation, the entire
// plan is rolled back — no partial commits.
//
// Plans are serializable (for the /apply endpoint) and
// snapshot-testable (compare the plan output against a golden
// file in a unit test). The engine produces plans; the
// coordinator consumes them. Neither side does both.
type Plan struct {
	// Version is the engine version that produced this plan.
	// The coordinator rejects plans from mismatched versions
	// so a stale client can't submit plans the coordinator
	// doesn't understand.
	Version string

	// Mutations is the ordered list of primitive state
	// changes to apply. The coordinator walks them in order
	// inside one transaction, validating each against the
	// current DB state before applying.
	Mutations []Mutation
}

// AppendCascade appends an UpdateReadyTasks mutation for the
// given run. Convention: every service method that mutates
// task state ends its final plan with this so the readiness
// cascade fires inside the same transaction as the state
// change. Forgetting to call it leaves downstream tasks stuck
// in PENDING — a loud functional bug that integration tests
// catch immediately.
//
// Returns p so call sites can chain:
//
//	c.Store.ApplyPlan(plan.AppendCascade(task.RunID))
//
// This replaces the old pattern of `s.UpdateReadyTasks(runID)`
// called separately after `ApplyPlan` succeeded — that pattern
// scattered cascade firing across two write paths and was the
// root cause of intermittent task_ready emission gaps.
//
// Multi-plan exception: a service operation that runs several
// ApplyPlan calls in sequence (e.g. service/submit.go's
// review-resolve → invalidate → remediation chain) intentionally
// omits AppendCascade on the intermediate plans and fires ONE
// consolidated cascade at the end. This avoids emitting
// task_ready events for tasks that briefly looked ready between
// plans but were resolved further down the chain. The
// final-cascade-only pattern is correct under that pattern; the
// invariant becomes "every service operation ends with a
// cascade" rather than "every plan ends with a cascade." If you
// see a service method whose final plan lacks AppendCascade,
// that's the bug you're looking for.
func (p Plan) AppendCascade(runID int64) Plan {
	p.Mutations = append(p.Mutations, UpdateReadyTasks{RunID: runID})
	return p
}

// Mutation is a single primitive state change. The set of
// mutation kinds is small (~10) and stable — new features
// add computation in the engine, not new mutation kinds in
// the coordinator.
//
// Implemented as a Go interface so each kind carries only
// the fields it needs. The coordinator's ApplyPlan does a
// type switch over the concrete types.
type Mutation interface {
	mutationKind() MutationKind
}

// --- JSON serialization for the /apply endpoint ---

// mutationEnvelope wraps a Mutation with its kind tag for
// JSON round-tripping. Without it, json.Unmarshal doesn't
// know which concrete type to decode into.
type mutationEnvelope struct {
	Kind  MutationKind  `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// MarshalJSON serializes a Plan by wrapping each Mutation in
// an envelope with a kind discriminator.
func (p Plan) MarshalJSON() ([]byte, error) {
	envelopes := make([]mutationEnvelope, 0, len(p.Mutations))
	for _, m := range p.Mutations {
		payload, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		envelopes = append(envelopes, mutationEnvelope{
			Kind:  m.mutationKind(),
			Payload: payload,
		})
	}
	return json.Marshal(struct {
		Version  string       `json:"version"`
		Mutations []mutationEnvelope `json:"mutations"`
	}{
		Version:  p.Version,
		Mutations: envelopes,
	})
}

// UnmarshalJSON deserializes a Plan, decoding each mutation
// envelope into its concrete type based on the kind tag.
func (p *Plan) UnmarshalJSON(data []byte) error {
	var raw struct {
		Version  string       `json:"version"`
		Mutations []mutationEnvelope `json:"mutations"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Version = raw.Version
	p.Mutations = make([]Mutation, 0, len(raw.Mutations))
	for i, env := range raw.Mutations {
		m, err := decodeMutation(env.Kind, env.Payload)
		if err != nil {
			return fmt.Errorf("mutation[%d] (%s): %w", i, env.Kind, err)
		}
		p.Mutations = append(p.Mutations, m)
	}
	return nil
}

func decodeMutation(kind MutationKind, data json.RawMessage) (Mutation, error) {
	switch kind {
	case MutSetTaskState:
		var m SetTaskState
		return m, json.Unmarshal(data, &m)
	case MutCreateTask:
		var m CreateTask
		return m, json.Unmarshal(data, &m)
	case MutDeleteTask:
		var m DeleteTask
		return m, json.Unmarshal(data, &m)
	case MutCreateRun:
		var m CreateRun
		return m, json.Unmarshal(data, &m)
	case MutSetClaim:
		var m SetClaim
		return m, json.Unmarshal(data, &m)
	case MutReleaseClaim:
		var m ReleaseClaim
		return m, json.Unmarshal(data, &m)
	case MutExpireClaim:
		var m ExpireClaim
		return m, json.Unmarshal(data, &m)
	case MutSetClaimDeadline:
		var m SetClaimDeadline
		return m, json.Unmarshal(data, &m)
	case MutRecordSubmission:
		var m RecordSubmission
		return m, json.Unmarshal(data, &m)
	case MutMoveArtifact:
		var m MoveArtifact
		return m, json.Unmarshal(data, &m)
	case MutDeleteArtifact:
		var m DeleteArtifact
		return m, json.Unmarshal(data, &m)
	case MutCreateCitizen:
		var m CreateCitizen
		return m, json.Unmarshal(data, &m)
	case MutUpdateReadyTasks:
		var m UpdateReadyTasks
		return m, json.Unmarshal(data, &m)
	case MutCompleteRun:
		var m CompleteRun
		return m, json.Unmarshal(data, &m)
	case MutEmitEvent:
		var m EmitEvent
		return m, json.Unmarshal(data, &m)
	case MutCreateProject:
		var m CreateProject
		return m, json.Unmarshal(data, &m)
	case MutSetProjectDefaultBranch:
		var m SetProjectDefaultBranch
		return m, json.Unmarshal(data, &m)
	case MutSetProjectRemoteURL:
		var m SetProjectRemoteURL
		return m, json.Unmarshal(data, &m)
	case MutAddProjectMember:
		var m AddProjectMember
		return m, json.Unmarshal(data, &m)
	case MutRemoveProjectMember:
		var m RemoveProjectMember
		return m, json.Unmarshal(data, &m)
	case MutSetProjectMemberRole:
		var m SetProjectMemberRole
		return m, json.Unmarshal(data, &m)
	case MutMarkOpenClaimsInvalidated:
		var m MarkOpenClaimsInvalidated
		return m, json.Unmarshal(data, &m)
	case MutMarkOpenClaimsFailed:
		var m MarkOpenClaimsFailed
		return m, json.Unmarshal(data, &m)
	case MutMarkLatestClaimOutcome:
		var m MarkLatestClaimOutcome
		return m, json.Unmarshal(data, &m)
	case MutPauseRun:
		var m PauseRun
		return m, json.Unmarshal(data, &m)
	case MutResumeRun:
		var m ResumeRun
		return m, json.Unmarshal(data, &m)
	case MutTerminateRun:
		var m TerminateRun
		return m, json.Unmarshal(data, &m)
	case MutCreateIssue:
		var m CreateIssue
		return m, json.Unmarshal(data, &m)
	case MutTriageIssue:
		var m TriageIssue
		return m, json.Unmarshal(data, &m)
	case MutMarkIssueInProgress:
		var m MarkIssueInProgress
		return m, json.Unmarshal(data, &m)
	case MutCloseIssue:
		var m CloseIssue
		return m, json.Unmarshal(data, &m)
	case MutSpawnTask:
		var m SpawnTask
		return m, json.Unmarshal(data, &m)
	case MutSetCycleBudgetMax:
		var m SetCycleBudgetMax
		return m, json.Unmarshal(data, &m)
	case MutSetCitizenRole:
		var m SetCitizenRole
		return m, json.Unmarshal(data, &m)
	case MutUpdateCitizenProfile:
		var m UpdateCitizenProfile
		return m, json.Unmarshal(data, &m)
	case MutIssueToken:
		var m IssueToken
		return m, json.Unmarshal(data, &m)
	case MutRevokeToken:
		var m RevokeToken
		return m, json.Unmarshal(data, &m)
	case MutRevokeTokenByValue:
		var m RevokeTokenByValue
		return m, json.Unmarshal(data, &m)
	case MutSetAutoTriageTemplate:
		var m SetAutoTriageTemplate
		return m, json.Unmarshal(data, &m)
	}
	return nil, fmt.Errorf("unknown mutation kind %q", kind)
}

// MutationKind tags each mutation for logging, metrics, and
// the type switch in ApplyPlan.
type MutationKind string

const (
	MutSetTaskState   MutationKind = "set_task_state"
	MutCreateTask    MutationKind = "create_task"
	MutDeleteTask    MutationKind = "delete_task"
	MutCreateRun    MutationKind = "create_run"
	MutSetClaim     MutationKind = "set_claim"
	MutReleaseClaim   MutationKind = "release_claim"
	MutExpireClaim   MutationKind = "expire_claim"
	MutSetClaimDeadline MutationKind = "set_claim_deadline"
	MutRecordSubmission   MutationKind = "submit_result"
	MutMoveArtifact   MutationKind = "move_artifact"
	MutDeleteArtifact  MutationKind = "delete_artifact"
	MutCreateCitizen  MutationKind = "create_citizen"
	MutUpdateReadyTasks MutationKind = "update_ready_tasks"
	MutCompleteRun   MutationKind = "complete_run"
	MutEmitEvent     MutationKind = "emit_event"

	MutCreateProject           MutationKind = "create_project"
	MutSetProjectDefaultBranch MutationKind = "set_project_default_branch"
	MutSetProjectRemoteURL     MutationKind = "set_project_remote_url"
	MutAddProjectMember        MutationKind = "add_project_member"
	MutRemoveProjectMember     MutationKind = "remove_project_member"
	MutSetProjectMemberRole    MutationKind = "set_project_member_role"

	MutMarkOpenClaimsInvalidated MutationKind = "mark_open_claims_invalidated"
	MutMarkOpenClaimsFailed      MutationKind = "mark_open_claims_failed"
	MutMarkLatestClaimOutcome    MutationKind = "mark_latest_claim_outcome"

	MutPauseRun     MutationKind = "pause_run"
	MutResumeRun    MutationKind = "resume_run"
	MutTerminateRun MutationKind = "terminate_run"

	MutCreateIssue         MutationKind = "create_issue"
	MutTriageIssue         MutationKind = "triage_issue"
	MutMarkIssueInProgress MutationKind = "mark_issue_in_progress"
	MutCloseIssue          MutationKind = "close_issue"

	MutSpawnTask         MutationKind = "spawn_task"
	MutSetCycleBudgetMax MutationKind = "set_cycle_budget_max"

	MutSetCitizenRole       MutationKind = "set_citizen_role"
	MutUpdateCitizenProfile MutationKind = "update_citizen_profile"
	MutIssueToken           MutationKind = "issue_token"
	MutRevokeToken          MutationKind = "revoke_token"
	MutRevokeTokenByValue   MutationKind = "revoke_token_by_value"

	MutSetAutoTriageTemplate MutationKind = "set_auto_triage_template"
)

// AllMutationKinds enumerates every supported MutationKind.
// Maintain in lockstep with the const block above and
// decodeMutation: adding a new mutation requires (1) a const
// here, (2) an entry in this slice, (3) a decoder case,
// (4) a dispatcher case in apply.go, (5) the apply handler.
// TestAllMutationKindsHaveDecoder catches drift between the
// slice and decoder; the dispatcher panic catches a missing
// case at runtime.
var AllMutationKinds = []MutationKind{
	MutSetTaskState,
	MutCreateTask,
	MutDeleteTask,
	MutCreateRun,
	MutSetClaim,
	MutReleaseClaim,
	MutExpireClaim,
	MutSetClaimDeadline,
	MutRecordSubmission,
	MutMoveArtifact,
	MutDeleteArtifact,
	MutCreateCitizen,
	MutUpdateReadyTasks,
	MutCompleteRun,
	MutEmitEvent,
	MutCreateProject,
	MutSetProjectDefaultBranch,
	MutSetProjectRemoteURL,
	MutAddProjectMember,
	MutRemoveProjectMember,
	MutSetProjectMemberRole,
	MutMarkOpenClaimsInvalidated,
	MutMarkOpenClaimsFailed,
	MutMarkLatestClaimOutcome,
	MutPauseRun,
	MutResumeRun,
	MutTerminateRun,
	MutCreateIssue,
	MutTriageIssue,
	MutMarkIssueInProgress,
	MutCloseIssue,
	MutSpawnTask,
	MutSetCycleBudgetMax,
	MutSetCitizenRole,
	MutUpdateCitizenProfile,
	MutIssueToken,
	MutRevokeToken,
	MutRevokeTokenByValue,
	MutSetAutoTriageTemplate,
}

// --- Concrete mutation types ---

// SetTaskState transitions a task to a new state. Used for:
// invalidation (ACCEPTED → READY), cascade (any → PENDING),
// skip-cascade (any → SKIPPED), tally resolution
// (COLLECTING → ACCEPTED). Optional fields carry
// action-specific data (vote choice, review verdict).
type SetTaskState struct {
	TaskID   string
	NewState  TaskState
	ClearClaim bool  // invalidation: clear claimed_by/claimed_at/submitted_at/result_path/commit_sha
	VoteChoice string // vote resolution: the winning option id
	CommitSHA string // resolution: the commit to record
	FailReason string // fail: reason for failure
	SkipReason string // skip (upstream-failure cascade): e.g. "upstream failed: 1:4:write_data"
	// ParkedFromState carries the state to stash when
	// transitioning to TaskParked (partial re-materialization
	// Phase 1). The apply layer writes it to
	// `parked_from_state` when NewState == TaskParked.
	//
	// Kept on SetTaskState rather than a dedicated mutation
	// type so parking stays a single row UPDATE that also
	// preserves claimed_by / commit_sha / review_decision /
	// vote_choice / task_claims — everything that isn't the
	// state column keeps its value so restore is lossless.
	// ClearClaim must be false when parking (otherwise ballots
	// and result commits would be wiped).
	ParkedFromState TaskState
	// NewDependsOn, when non-nil, overwrites the task's
	// depends_on column in the same transaction as the state
	// flip. Nil = leave depends_on untouched. Used by the J.2
	// partial re-materialization singleton-reopen path, where
	// re-accepting with a different instance set requires
	// rewriting a transitively-deferred singleton's fan-in
	// edges at the same moment its state resets to PENDING.
	//
	// Empty-but-not-nil slice → depends_on is cleared (empty
	// string). Non-nil pointer + non-empty slice →
	// comma-joined into the column. The pointer shape makes
	// "don't touch" vs "clear" unambiguous.
	NewDependsOn *[]string
}

func (SetTaskState) mutationKind() MutationKind { return MutSetTaskState }

// CreateTask inserts a new task row. Used by run creation
// and dynamic for_each materialization.
type CreateTask struct {
	Task TaskRecord
}

func (CreateTask) mutationKind() MutationKind { return MutCreateTask }

// DeleteTask removes a task row and its associated
// task_claims rows. Used by dynamic for_each
// dematerialization on invalidation.
type DeleteTask struct {
	TaskID string
}

func (DeleteTask) mutationKind() MutationKind { return MutDeleteTask }

// CreateRun inserts a new run row. The run's per-project
// sequence number is computed by the coordinator at apply
// time (not by the engine) because it requires an atomic
// increment.
type CreateRun struct {
	Run RunRecord
}

func (CreateRun) mutationKind() MutationKind { return MutCreateRun }

// SetClaim records a citizen claiming a task. Inserts a
// task_claims row and updates the task's claimed_by/
// claimed_at fields. For multi-citizen tasks, each citizen
// gets one slot; the coordinator validates slot availability
// at apply time.
//
// ModelID attributes the claim to a model citizen
// when known at claim time. Optional for humans (a hand-review
// has no model); REQUIRED for bot operators — applySetClaim
// rejects bot claims with model_id=NULL because bots can't
// think on their own. The constraint is enforced at apply
// time, not in SQLite, since CHECK can't cross-table-reference.
type SetClaim struct {
	TaskID  string
	CitizenID int64
	Deadline time.Time
	ModelID  *int64
}

func (SetClaim) mutationKind() MutationKind { return MutSetClaim }

// ReleaseClaim releases a citizen's claim on a task voluntarily
// (user-initiated `enju_release_task`). Resets the task to READY
// and marks the open claim row outcome=released. The claim row
// is preserved as audit history (who held it, when, why it
// ended).
type ReleaseClaim struct {
	TaskID  string
	CitizenID int64
}

func (ReleaseClaim) mutationKind() MutationKind { return MutReleaseClaim }

// ExpireClaim expires a citizen's claim on a task because the
// deadline passed. Resets the task to READY, marks the open
// claim row outcome=timed_out, increments the citizen's
// timeout counter and recomputes their score. Used by the
// scheduler/reaper.
//
// Distinct from ReleaseClaim because (a) the outcome string
// differs ("timed_out" vs "released") and (b) the citizen
// stats penalty only applies to involuntary timeouts.
type ExpireClaim struct {
	TaskID  string
	CitizenID int64
}

func (ExpireClaim) mutationKind() MutationKind { return MutExpireClaim }

// SetClaimDeadline re-anchors the deadline of a task's open claim
// (outcome IS NULL) to a fresh value, without touching claimant,
// state, or claim history. Used by the CLAIMED → RUNNING
// transition so the lease covers the task's actual execution
// (which begins at RUNNING) instead of the claim-time guess: the
// claim deadline alone reaped long legitimate work (a 3-hour
// assembly, or a first-run multi-GB image pull) the same as a
// dead worker, because nothing distinguished "still running" from
// "claimed and died." No-op when there is no open claim.
type SetClaimDeadline struct {
	TaskID   string
	Deadline time.Time
}

func (SetClaimDeadline) mutationKind() MutationKind { return MutSetClaimDeadline }

// RecordSubmission records a citizen's submission on a task.
// Updates the task_claims row with the result path, commit
// SHA, decision/option, and content. For single-citizen
// tasks also transitions the task state. For multi-citizen
// tasks records the submission but leaves state management
// to subsequent SetTaskState mutations (the engine decides
// whether to resolve based on tally computation).
type RecordSubmission struct {
	TaskID   string
	CitizenID int64
	ResultPath string
	CommitSHA string
	Decision  string // review: approve/reject
	VoteChoice string // vote: chosen option id
	TokensUsed int64
	// EstimatedTokens carries a (prompt+content)/4 estimate
	// for events.estimated_tokens metadata. rewires
	// the token-tracking emission from engine/submit.go to
	// applyRecordSubmission, so the engine now passes its
	// computed estimate through the mutation rather than
	// embedding it in a metadata blob. Profile counters
	// (SUM(json_extract(metadata, '$.estimated_tokens')))
	// stay populated.
	EstimatedTokens int64
	// ModelID attributes the submission to a model
	// citizen. Optional for human operators (a hand-review has
	// no model); REQUIRED for bot operators — applyRecordSubmission
	// rejects bot submissions with model_id=NULL. The constraint
	// is enforced at apply time (SQLite CHECK can't cross-table).
	ModelID *int64
}

func (RecordSubmission) mutationKind() MutationKind { return MutRecordSubmission }

// MoveArtifact upserts an artifact index row to point at a
// new (or restored) writer. Used by submission (new write)
// and invalidation rollback (re-point to prior writer).
type MoveArtifact struct {
	Artifact ArtifactRecord
}

func (MoveArtifact) mutationKind() MutationKind { return MutMoveArtifact }

// DeleteArtifact removes an artifact index row. Used by
// invalidation rollback when no prior writer exists.
type DeleteArtifact struct {
	ProjectID int64
	Branch  string // branch this artifact row lives on; "" → "main"
	Path   string
}

func (DeleteArtifact) mutationKind() MutationKind { return MutDeleteArtifact }

// CreateCitizen registers a new citizen plus its initial
// token. TokenLabel is the optional label for the auto-issued
// token row (e.g. "primary", "rotation-2026-05"). Empty label
// matches the historical default for unlabeled tokens.
type CreateCitizen struct {
	Citizen    CitizenRecord
	TokenLabel string
}

func (CreateCitizen) mutationKind() MutationKind { return MutCreateCitizen }

// UpdateReadyTasks sweeps a run's tasks and promotes
// eligible PENDING tasks to READY (all dependencies
// satisfied). This is a compound operation — it reads +
// writes — but it's atomic and self-contained, so it
// lives as a single mutation rather than N individual
// SetTaskState mutations (the eligible set depends on the
// state at sweep time, not at plan-compute time).
type UpdateReadyTasks struct {
	RunID int64
}

func (UpdateReadyTasks) mutationKind() MutationKind { return MutUpdateReadyTasks }

// CompleteRun checks whether all tasks in a run are in
// terminal states (accepted/skipped) and transitions the
// run to COMPLETED if so. Like UpdateReadyTasks, this is
// a read-then-write that must run inside the apply
// transaction.
type CompleteRun struct {
	RunID int64
}

func (CompleteRun) mutationKind() MutationKind { return MutCompleteRun }

// EmitEvent stages an event with no associated state change.
// Used for purely informational signals — cascade_fired,
// branch_merged, and similar — that need to ride the same
// post-commit drain as state-coupled events without coupling
// to a state mutation.
//
// Why this exists: the EventSink contract says every event
// flows through ApplyPlan. Without this mutation, callers
// emitting metadata-only events would either need a sibling
// state mutation to piggy-back on (awkward and brittle) or an
// out-of-band Store.Events().Record path (defeats the
// chokepoint). EmitEvent keeps the rule simple: build a Plan,
// call ApplyPlan, every event in the plan fires post-commit
// or none of them do.
//
// The handler is a one-liner: sink.Emit(m.Event). The whole
// type exists for the contract, not the behavior.
type EmitEvent struct {
	Event Event
}

func (EmitEvent) mutationKind() MutationKind { return MutEmitEvent }

// CreateProject inserts a new long-lived project. The new
// project's id is returned via ApplyResult.ProjectID.
type CreateProject struct {
	Project ProjectRecord
}

func (CreateProject) mutationKind() MutationKind { return MutCreateProject }

// SetProjectDefaultBranch updates the project's default branch.
// Empty string defaults to "main" (column never left blank).
type SetProjectDefaultBranch struct {
	ProjectID int64
	Branch    string
}

func (SetProjectDefaultBranch) mutationKind() MutationKind { return MutSetProjectDefaultBranch }

// SetProjectRemoteURL updates (or clears, with empty string)
// the project's external git remote URL.
type SetProjectRemoteURL struct {
	ProjectID int64
	RemoteURL string
}

func (SetProjectRemoteURL) mutationKind() MutationKind { return MutSetProjectRemoteURL }

// AddProjectMember inserts a (project, citizen, role) row.
// AddedBy is the citizens.id of the adder, or 0 for the
// creator self-add row.
type AddProjectMember struct {
	ProjectID int64
	CitizenID int64
	Role      ProjectRole
	AddedBy   int64
}

func (AddProjectMember) mutationKind() MutationKind { return MutAddProjectMember }

// RemoveProjectMember deletes the membership row. No-op if
// the citizen is not a member. Caller is responsible for
// invariant checks (last-owner refusal).
type RemoveProjectMember struct {
	ProjectID int64
	CitizenID int64
}

func (RemoveProjectMember) mutationKind() MutationKind { return MutRemoveProjectMember }

// SetProjectMemberRole updates a citizen's role within a
// project. No-op if the citizen is not a member.
type SetProjectMemberRole struct {
	ProjectID int64
	CitizenID int64
	Role      ProjectRole
}

func (SetProjectMemberRole) mutationKind() MutationKind { return MutSetProjectMemberRole }

// MarkOpenClaimsInvalidated closes any open (outcome IS NULL)
// claim rows for TaskID with outcome='invalidated'. Used by
// cascade callers (invalidate, fail-cascade) when a downstream
// descendant was claimed-but-not-yet-terminal and its iteration
// is being thrown away as collateral damage. Idempotent.
type MarkOpenClaimsInvalidated struct {
	TaskID string
}

func (MarkOpenClaimsInvalidated) mutationKind() MutationKind { return MutMarkOpenClaimsInvalidated }

// MarkOpenClaimsFailed closes any open (outcome IS NULL) claim
// rows for TaskID with outcome='failed'. Sibling of
// MarkOpenClaimsInvalidated, but for a different cause: the
// attempt's compute script errored on its own merits (not
// collateral, not a reviewer verdict). Closing the iteration
// here is load-bearing — it's what makes the failed attempt an
// auditable closed iteration AND lets the next enju_retry_task
// re-claim advance iter_seq instead of reusing the dead row.
// Idempotent (no-op when there is no open claim).
type MarkOpenClaimsFailed struct {
	TaskID string
}

func (MarkOpenClaimsFailed) mutationKind() MutationKind { return MutMarkOpenClaimsFailed }

// MarkLatestClaimOutcome sets the outcome of the most recent
// claim row for TaskID to Outcome, regardless of its current
// value (NULL or any prior terminal). Used by Phase 6c cascade
// paths where the claim might be either open (reviewed task
// awaiting verdict) or already terminal (operator invalidating
// long after the fact). Outcome must be in validRelabelOutcomes.
type MarkLatestClaimOutcome struct {
	TaskID  string
	Outcome ClaimOutcome
}

func (MarkLatestClaimOutcome) mutationKind() MutationKind { return MutMarkLatestClaimOutcome }

// PauseRun moves a run from active|idle into paused. Refuses
// if the run is already terminal (completed / failed). Idempotent
// on already-paused runs (no event emitted, no state change).
// CitizenID attributes the operator action.
type PauseRun struct {
	RunID     int64
	CitizenID int64
}

func (PauseRun) mutationKind() MutationKind { return MutPauseRun }

// ResumeRun lifts a paused run back to active. The caller
// typically follows with a CompleteRun mutation in the same
// plan to let the task-graph evaluator pick the right end
// state (active / idle / completed). Refuses if the run is
// terminal. No-op if already non-paused.
type ResumeRun struct {
	RunID     int64
	CitizenID int64
}

func (ResumeRun) mutationKind() MutationKind { return MutResumeRun }

// TerminateRun is the human-pulled-the-plug operation: moves a
// run to the terminal "terminated" state, cascade-skips every
// non-terminal task with skip_reason="run_terminated", and
// closes every open claim with outcome=abandoned. Topic
// branches stay (immutable git audit). Refuses if the run is
// already terminal. Reason is optional, capped to ReasonMaxLen
// bytes by the service layer before reaching this mutation.
//
// Distinct from PauseRun (reversible, no task cascade) and
// FailRun (system-said-no semantics). Use this when an operator
// gives up on a run before natural completion — bot stuck in a
// request_changes loop, requirements changed mid-run, design
// flaw discovered. Audit signals "operator aborted" rather than
// "validation failed" — different workflow-quality stories.
type TerminateRun struct {
	RunID     int64
	CitizenID int64
	Reason    string
}

func (TerminateRun) mutationKind() MutationKind { return MutTerminateRun }

// CreateIssue inserts a new issue. Returns the new issue's
// (id, seq) via ApplyResult.IssueID and ApplyResult.IssueSeq.
type CreateIssue struct {
	Issue IssueRecord
}

func (CreateIssue) mutationKind() MutationKind { return MutCreateIssue }

// TriageIssue moves open → triaged with optional severity update.
// Refuses on non-open issues so a closed/wontfix issue can't be
// revived without an explicit re-open path.
type TriageIssue struct {
	IssueID   int64
	CitizenID int64
	Severity  IssueSeverity // optional override; empty keeps current
}

func (TriageIssue) mutationKind() MutationKind { return MutTriageIssue }

// MarkIssueInProgress moves open|triaged → in_progress and
// links the issue to the fix task.
type MarkIssueInProgress struct {
	IssueID   int64
	CitizenID int64
	FixTaskID string
}

func (MarkIssueInProgress) mutationKind() MutationKind { return MutMarkIssueInProgress }

// CloseIssue moves open|triaged|in_progress → closed/wontfix.
// ClosedByTaskID is optional — empty for manual close without a
// fix (duplicate, wontfix).
type CloseIssue struct {
	IssueID        int64
	CitizenID      int64
	Status         IssueStatus // must be IssueStatusClosed or IssueStatusWontfix
	ClosedByTaskID string
}

func (CloseIssue) mutationKind() MutationKind { return MutCloseIssue }

// SpawnTask creates a new task in an existing run, enforcing
// the per-run cycle budget. Refuses on terminal/paused runs.
// On budget exhaustion, pauses the run and emits
// cycle_budget_exhausted (the SetTaskState/PauseRun follow-up
// is folded into the same handler so the audit captures the
// exhaustion as a single transactional event). The new task's
// fully-qualified id is returned via ApplyResult.SpawnedTaskID.
type SpawnTask struct {
	Spec SpawnSpec
}

func (SpawnTask) mutationKind() MutationKind { return MutSpawnTask }

// SetCycleBudgetMax bumps the per-run cycle budget cap. Refuses
// if the new max is below current used. Idempotent (no-op on
// equal values, no event emitted). Used to give a paused-by-
// exhaustion run room to keep going.
type SetCycleBudgetMax struct {
	RunID     int64
	CitizenID int64
	NewMax    int
}

func (SetCycleBudgetMax) mutationKind() MutationKind { return MutSetCycleBudgetMax }

// SetCitizenRole updates a citizen's global role
// (citizen / author / reviewer). Privileged operation —
// admin-only in production.
type SetCitizenRole struct {
	CitizenID int64
	Role      string
}

func (SetCitizenRole) mutationKind() MutationKind { return MutSetCitizenRole }

// UpdateCitizenProfile updates a citizen's name/email. Username
// is intentionally immutable. Pass nil for fields you don't
// want to change. Email uniqueness is checked at apply time.
type UpdateCitizenProfile struct {
	CitizenID int64
	Name      *string
	Email     *string
}

func (UpdateCitizenProfile) mutationKind() MutationKind { return MutUpdateCitizenProfile }

// IssueToken creates a new bearer-token row for a citizen.
// Multiple active tokens per citizen are allowed (rotation,
// per-deployment labels). The new token's id is returned via
// ApplyResult.TokenID.
type IssueToken struct {
	CitizenID int64
	Token     string
	Label     string
}

func (IssueToken) mutationKind() MutationKind { return MutIssueToken }

// RevokeToken marks a token as revoked by row id. Idempotent
// (double-revoke is a no-op). The row is preserved for audit.
type RevokeToken struct {
	TokenID int64
}

func (RevokeToken) mutationKind() MutationKind { return MutRevokeToken }

// RevokeTokenByValue is the same as RevokeToken but keyed by
// the token string instead of the row id. Convenience for
// CLI/API callers that hold the token but not its row id.
type RevokeTokenByValue struct {
	Token string
}

func (RevokeTokenByValue) mutationKind() MutationKind { return MutRevokeTokenByValue }

// SetAutoTriageTemplate persists the run-level auto-triage
// rule (JSON-encoded RemediationTemplate). Empty TemplateJSON
// clears the rule. Used by the create-run path to plumb the
// rule through the chokepoint instead of a side-channel write.
type SetAutoTriageTemplate struct {
	RunID        int64
	TemplateJSON string
}

func (SetAutoTriageTemplate) mutationKind() MutationKind { return MutSetAutoTriageTemplate }
