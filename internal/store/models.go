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

// ProjectState represents the state of a project.
type ProjectState string

const (
	ProjectActive    ProjectState = "active"
	ProjectCompleted ProjectState = "completed"
	ProjectFailed    ProjectState = "failed"
)

// ProjectRecord is a project stored in the database.
type ProjectRecord struct {
	ID        int64
	Name      string
	Ref       string // external reference (GitHub issue URL, etc.)
	YAMLData  string // raw YAML content
	RepoURL   string
	State     ProjectState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskRecord is a task instance stored in the database.
type TaskRecord struct {
	ID          string // full ID (e.g., "endometriosis:foundation")
	ProjectID   int64
	Seq         int    // sequential number within project (1-based, for quick reference)
	TaskDefID   string // original task ID from YAML
	InstanceKey string // for_each key (e.g., "endometriosis"), empty if no for_each
	Ref         string // external reference (URL)
	Action      string // "answer", "contribute", "compute", "review", "vote"
	Prompt      string
	UserPrompt  string
	Script      string
	Outputs     string // JSON: map of output name -> description
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

// CitizenRecord is a citizen stored in the database.
type CitizenRecord struct {
	ID              string
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
	CitizenID string
	ClaimedAt     time.Time
	Deadline      time.Time
	Outcome       string // "completed", "timed_out", "released", "rejected"
	SubmittedAt   *time.Time
}
