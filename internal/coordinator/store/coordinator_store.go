package store

import "time"

// CoordinatorStore is the read+ApplyPlan surface that
// coordinator-side packages (service, api, scheduler)
// program against. The single chokepoint invariant —
// "every state-changing write goes through ApplyPlan" —
// is encoded here as a compile-time constraint: the
// interface deliberately does NOT expose direct mutation
// methods, so a service-layer caller cannot accidentally
// bypass the chokepoint.
//
// What's IN:
//
//   - ApplyPlan: the one and only mutation entry point.
//   - All read methods (Get / List / Count / Find /
//     Lookup / Has). The interface is maximalist on
//     reads; any reader used outside this package goes
//     here.
//   - Events()  + RecordContributionEvent: the audit-event
//     emit channel. Events live in a SEPARATE database
//     (events.db) with an async writer that never errors;
//     they are observability, not state. Emitting an event
//     is intentionally NOT a Plan mutation because event
//     loss is acceptable while state loss is not.
// What's OUT (and on purpose):
//
//   - Direct mutation methods (CreateRun, SetCitizenRole,
//     etc.). These all live as Mutation types now;
//     callers route them through ApplyPlan. The
//     compiler enforces the rule by simply not exposing
//     a way to call them.
//   - Lifecycle: Close, AttachEventStore, migrate. These
//     are owned by the construction site (cmd/enju,
//     coordinator startup), which still holds the
//     concrete *Store.
//
// *Store implements this interface by virtue of providing
// every method on it; conversion happens implicitly when
// a *Store is assigned to a CoordinatorStore field.
type CoordinatorStore interface {
	// --- The chokepoint ---
	ApplyPlan(plan Plan) (ApplyResult, error)

	// --- Audit events (separate subsystem) ---
	Events() EventStore
	RecordContributionEvent(e *ContributionEvent) error

	// --- Tasks ---
	GetTask(id string) (*TaskRecord, error)
	GetTaskBySeq(runID string, seq int) (*TaskRecord, error)
	ListTasksByRun(runID int64) ([]TaskRecord, error)
	ListReadyTasks(runID int64) ([]TaskRecord, error)
	ListVoteSubmissions(taskID string) ([]TaskClaimRecord, error)
	HasReviewerOfTarget(runID int64, taskDefID, instanceKey string) (bool, error)
	ListActiveClaims(taskID string) ([]TaskClaimRecord, error)
	EarliestClaimTime(taskID string) (time.Time, error)
	HasActiveClaim(taskID string, citizenID int64) (bool, error)
	CountActiveClaims(taskID string) (int, error)
	ListTaskHistory(taskID string) ([]TaskClaimRecord, error)
	ListTaskIterations(taskID string) ([]IterationRecord, error)
	GetExpiredClaims() ([]TaskClaimRecord, error)
	GetOpenClaimIterSeq(taskID string) (int64, error)
	ListOpenClaimsForCitizen(citizenID int64) ([]TaskClaimRecord, error)

	// --- Runs ---
	GetRun(id int64) (*RunRecord, error)
	GetRunByProjectSeq(projectID int64, seq int) (*RunRecord, error)
	ListRuns() ([]RunRecord, error)
	ListRunsByProject(projectID int64) ([]RunRecord, error)
	ActiveRunOnBranch(projectID int64, branch string) (*RunRecord, error)
	ListRunBranches(projectID int64) ([]string, error)
	GetCycleBudget(runID int64) (used, max int, err error)
	GetAutoTriageTemplate(runID int64) (string, error)
	ListRunsWithAutoTriage(projectID int64) ([]int64, error)
	CountTasksWithDefIDPrefix(runID int64, prefix string) (int, error)

	// --- Projects + members ---
	GetProject(id int64) (*ProjectRecord, error)
	GetProjectByName(name string) (*ProjectRecord, error)
	ListProjects() ([]ProjectRecord, error)
	GetProjectMember(projectID, citizenID int64) (*ProjectMemberRecord, error)
	ListProjectMembers(projectID int64) ([]ProjectMemberRecord, error)
	CountProjectOwners(projectID int64) (int, error)
	CountProjectMembers(projectID int64) (int, error)
	ListProjectsForCitizen(citizenID int64) ([]ProjectRecord, error)
	CountProjectsThisMonth(citizenID int64) (int, error)

	// --- Citizens + tokens ---
	GetCitizen(id int64) (*CitizenRecord, error)
	GetCitizenByUsername(username string) (*CitizenRecord, error)
	GetCitizenByUsernameInTenant(username string, tenantID int64) (*CitizenRecord, error)
	GetCitizenByEmail(email string) (*CitizenRecord, error)
	GetCitizenByToken(token string) (*CitizenRecord, error)
	ListCitizenActiveTasks(citizenID int64) ([]TaskRecord, error)
	ListCitizenCompletedTasks(citizenID int64, limit int) ([]TaskRecord, error)
	ListBotsByParent(parentID int64) ([]CitizenRecord, error)
	ListTokensByCitizen(citizenID int64) ([]TokenRecord, error)
	LookupTokenOwner(token string, tokenID int64) (int64, error)

	// --- Contribution / event reads ---
	GetContributionSummary(citizenID int64) (*ContributionSummary, error)
	CountContributionEvents(citizenID int64) (int, error)
	GetTemplateReuseCount(citizenID int64) (authored int, reused int, err error)
	GetEventMetadataForTask(taskID, eventType string) (string, error)
	ListRunEvents(projectID, runID int64) ([]RunEventRecord, error)
	ListEvents(q EventQuery) ([]RunEventRecord, error)
	GetDownstreamImpact(citizenID int64) (int, int, error)

	// --- Issues ---
	GetIssue(id int64) (*IssueRecord, error)
	GetIssueBySeq(projectID int64, seq int) (*IssueRecord, error)
	ListIssues(f IssueFilter) ([]IssueRecord, error)
	FindOldestOpenIssue(projectID int64) (*IssueRecord, error)

	// --- Artifacts ---
	GetArtifact(projectID int64, branch, path string) (*ArtifactRecord, error)
	ListTasksWritingArtifact(projectID int64, path string, acceptedOnly bool) ([]TaskRecord, error)
	ListArtifactsByProject(projectID int64, branch, pathPrefix string) ([]ArtifactRecord, error)
}

// Compile-time guarantee that *Store satisfies the
// interface. If a method drifts (rename, signature change),
// this catches it at build time instead of at the first
// service call site.
var _ CoordinatorStore = (*Store)(nil)
