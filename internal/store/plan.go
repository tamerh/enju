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
	Kind    MutationKind    `json:"kind"`
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
			Kind:    m.mutationKind(),
			Payload: payload,
		})
	}
	return json.Marshal(struct {
		Version   string             `json:"version"`
		Mutations []mutationEnvelope `json:"mutations"`
	}{
		Version:   p.Version,
		Mutations: envelopes,
	})
}

// UnmarshalJSON deserializes a Plan, decoding each mutation
// envelope into its concrete type based on the kind tag.
func (p *Plan) UnmarshalJSON(data []byte) error {
	var raw struct {
		Version   string             `json:"version"`
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
	}
	return nil, fmt.Errorf("unknown mutation kind %q", kind)
}

// MutationKind tags each mutation for logging, metrics, and
// the type switch in ApplyPlan.
type MutationKind string

const (
	MutSetTaskState     MutationKind = "set_task_state"
	MutCreateTask       MutationKind = "create_task"
	MutDeleteTask       MutationKind = "delete_task"
	MutCreateRun        MutationKind = "create_run"
	MutSetClaim         MutationKind = "set_claim"
	MutReleaseClaim     MutationKind = "release_claim"
	MutRecordSubmission     MutationKind = "submit_result"
	MutMoveArtifact     MutationKind = "move_artifact"
	MutDeleteArtifact   MutationKind = "delete_artifact"
	MutCreateCitizen    MutationKind = "create_citizen"
	MutUpdateReadyTasks MutationKind = "update_ready_tasks"
	MutCompleteRun      MutationKind = "complete_run"
)

// --- Concrete mutation types ---

// SetTaskState transitions a task to a new state. Used for:
// invalidation (ACCEPTED → READY), cascade (any → PENDING),
// skip-cascade (any → SKIPPED), tally resolution
// (COLLECTING → ACCEPTED). Optional fields carry
// action-specific data (vote choice, review verdict).
type SetTaskState struct {
	TaskID     string
	NewState   TaskState
	ClearClaim bool   // invalidation: clear claimed_by/claimed_at/submitted_at/result_path/commit_sha
	VoteChoice string // vote resolution: the winning option id
	CommitSHA  string // resolution: the commit to record
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
type SetClaim struct {
	TaskID    string
	CitizenID int64
	Deadline  time.Time
}

func (SetClaim) mutationKind() MutationKind { return MutSetClaim }

// ReleaseClaim releases a citizen's claim on a task.
// Removes the task_claims row and (for single-citizen tasks)
// resets the task's claimed_by field.
type ReleaseClaim struct {
	TaskID    string
	CitizenID int64
}

func (ReleaseClaim) mutationKind() MutationKind { return MutReleaseClaim }

// RecordSubmission records a citizen's submission on a task.
// Updates the task_claims row with the result path, commit
// SHA, decision/option, and content. For single-citizen
// tasks also transitions the task state. For multi-citizen
// tasks records the submission but leaves state management
// to subsequent SetTaskState mutations (the engine decides
// whether to resolve based on tally computation).
type RecordSubmission struct {
	TaskID     string
	CitizenID  int64
	ResultPath string
	CommitSHA  string
	Decision   string // review: approve/reject
	VoteChoice string // vote: chosen option id
	Content    string // prose commentary
	TokensUsed int64
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
	Branch    string // branch this artifact row lives on; "" → "main"
	Path      string
}

func (DeleteArtifact) mutationKind() MutationKind { return MutDeleteArtifact }

// CreateCitizen registers a new citizen.
type CreateCitizen struct {
	Citizen CitizenRecord
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
