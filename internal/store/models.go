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

// ProblemState represents the state of a problem.
type ProblemState string

const (
	ProblemActive    ProblemState = "active"
	ProblemCompleted ProblemState = "completed"
	ProblemFailed    ProblemState = "failed"
)

// ProblemRecord is a problem stored in the database.
type ProblemRecord struct {
	ID        string
	Name      string
	YAMLData  string // raw YAML content
	RepoURL   string
	State     ProblemState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskRecord is a task instance stored in the database.
type TaskRecord struct {
	ID          string // full ID (e.g., "endometriosis:foundation")
	ProblemID   string
	TaskDefID   string // original task ID from YAML
	InstanceKey string // for_each key (e.g., "endometriosis"), empty if no for_each
	Type        string // "llm_prompt" or "script"
	Mode        string // "autonomous" or "assisted"
	Prompt      string
	UserPrompt  string
	Script      string
	ResultType  string
	Timeout     string
	State       TaskState
	ClaimedBy   string
	ClaimedAt   *time.Time
	SubmittedAt *time.Time
	ResultPath  string // path in git repo
	DependsOn   string // comma-separated list of dependency full IDs
	CreatedAt   time.Time
}

// ParticipantRecord is a participant stored in the database.
type ParticipantRecord struct {
	ID              string
	Name            string
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
	ParticipantID string
	ClaimedAt     time.Time
	Deadline      time.Time
	Outcome       string // "completed", "timed_out", "released", "rejected"
	SubmittedAt   *time.Time
}
