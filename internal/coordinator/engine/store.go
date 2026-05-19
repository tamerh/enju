// Package engine contains the pure-computation core of the Enju
// coordinator. It reads state via the ReadStore interface,
// performs computations (tally evaluation, cascade walks,
// dynamic for_each expansion, run creation), and emits Plans —
// ordered lists of Mutations that describe what the coordinator
// should write. The engine never writes state itself.
//
// This separation exists so the same computation code can run
// in two deployment shapes:
//
//   - Networked mode: MCP client → engine.Compute*() → POST
//     the Plan to a shared coordinator → coordinator validates
//     and commits inside a transaction.
//   - Local-only mode: MCP client → engine.Compute*() →
//     embedded SQLite applies the Plan directly. No server,
//     no network hop.
//
// The coordinator is the authority on state transitions. Even
// when the client carries the engine, the coordinator validates
// every mutation against current DB state before committing.
// A stale or buggy plan gets rejected, not silently applied.
package engine

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReadStore is the read-only view of the store that the engine
// uses to inspect current state before computing a plan. The
// engine imports this interface, not the concrete *store.Store
// — so engine functions can be unit-tested against a mock
// without touching SQLite.
//
// Every method here has a matching method on *store.Store.
// The interface exists purely for testability and to enforce
// the "engine reads but never writes" contract at the type
// level.
type ReadStore interface {
	// Tasks
	GetTask(id string) (*store.TaskRecord, error)
	ListTasksByRun(runID int64) ([]store.TaskRecord, error)

	// Runs
	GetRun(id int64) (*store.RunRecord, error)
	GetRunByProjectSeq(projectID int64, seq int) (*store.RunRecord, error)

	// Projects
	GetProject(id int64) (*store.ProjectRecord, error)

	// Vote/review submissions (task_claims rows)
	ListVoteSubmissions(taskID string) ([]store.TaskClaimRecord, error)
	ListActiveClaims(taskID string) ([]store.TaskClaimRecord, error)
	EarliestClaimTime(taskID string) (time.Time, error)
	HasActiveClaim(taskID string, citizenID int64) (bool, error)
	CountActiveClaims(taskID string) (int, error)
	// GetOpenClaimIterSeq returns the iter_seq of the most recent
	// open claim for the task (0 if none). Used by the
	// single-citizen superseded-claim guard in ValidateSubmitRequest.
	GetOpenClaimIterSeq(taskID string) (int64, error)

	// Citizens
	GetCitizen(id int64) (*store.CitizenRecord, error)
	GetCitizenByUsername(username string) (*store.CitizenRecord, error)

	// Artifacts — keyed by (project, branch, path) so runs on
	// isolated branches see their own rows.
	GetArtifact(projectID int64, branch, path string) (*store.ArtifactRecord, error)
	ListTasksWritingArtifact(projectID int64, path string, acceptedOnly bool) ([]store.TaskRecord, error)
}
