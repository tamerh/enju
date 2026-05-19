// Package wire holds the JSON shapes shared between the
// coordinator (which encodes them on the way out of the HTTP
// API) and the fat-client (which decodes them on the way in).
// One source of truth makes coord-side field renames a
// compile-time break on the fat-client instead of a silent
// zero-value-after-decode.
//
// Boundary note: the architecture rule earlier read "no wire
// concerns" for `internal/common`. The intent was "no transport
// package imports" (mcp-go, chi, etc.) — JSON tags don't pull
// any of those in; `encoding/json` is std lib. This package
// stays inside that intent.
//
// What lives here vs. elsewhere:
//   - Here: shapes that BOTH sides exchange verbatim (project,
//     run, member listings).
//   - In `coordinator/service`: shapes the coord computes for
//     its own consumers but the fat-client doesn't decode (run
//     status header, cost summary).
//   - In `fatclient/service`: view models that post-process the
//     wire (TaskSummary's lean projection, EventRow's parsed
//     metadata) — not 1:1 with coord JSON.
//
// Migration policy for the legacy aliases (e.g.
// `coordinator/service.ProjectResponse = wire.Project`,
// `MemberResponse = wire.Member`): the aliases are transitional.
// Existing call sites continue working unchanged; new code and
// any file being touched for other reasons should rename the
// reference to the canonical wire.X form. The aliases are
// expected to fade out without a flag-day rename.
package wire

import (
	"strings"
	"time"
)

// Project is the JSON shape for one project.
//
// Field names + JSON tags are load-bearing: the coord-side
// formatters in internal/common/format consume these key names,
// and the fat-client decodes them into the same struct. Rename
// a field here and both sides break, which is the point —
// drift becomes a compile-time signal instead of a silent
// runtime miss.
type Project struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	RemoteURL     string    `json:"remote_url,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	RunCount      int       `json:"run_count"`
	CreatedAt     time.Time `json:"created_at"`

	// LastPushAt / LastPushError are populated client-side by
	// DecorateProjectListWithPushStatus (in fatclient/service/
	// project_ops.go) before format.ProjectList consumes the
	// JSON. Coord never sets these — both fields stay
	// zero-valued in coord-side encodings, omitempty drops them
	// from the wire. They live on this type so the format
	// renderer's expectation is part of the wire contract,
	// drift becomes a compile-time signal, and the decorator
	// has a typed home rather than poking string keys into a
	// generic map.
	LastPushAt    time.Time `json:"last_push_at,omitempty"`
	LastPushError string    `json:"last_push_error,omitempty"`
}

// Run is the JSON shape for one run. Same drift-protection
// argument as Project.
type Run struct {
	ID              int64     `json:"id"`
	ProjectID       int64     `json:"project_id,omitempty"`
	Seq             int       `json:"seq"`
	Name            string    `json:"name"`
	State           string    `json:"state"`
	TaskCount       int       `json:"task_count"`
	Branch          string    `json:"branch,omitempty"`
	// DefaultBranch is the project's default branch at the time
	// the run was created — fat-clients use it as the fork-base
	// when EnsureRunBranch needs to materialize a brand-new run
	// branch. Sent on create/get; empty on older coordinators.
	DefaultBranch   string    `json:"default_branch,omitempty"`
	Slug            string    `json:"slug,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	SourcePath      string    `json:"source_path,omitempty"`
	SourceCommitSHA string    `json:"source_commit_sha,omitempty"`
	Warnings        []string  `json:"warnings,omitempty"`
	// BlockedBy is the JSON-encoded blocker description for a
	// WAITING run (see store.BlockedBy + ParseBlockedBy).
	// Empty for non-WAITING runs OR for older coordinators.
	// Surface readers (enju_run_status renderer) check
	// state==waiting before parsing.
	BlockedBy string `json:"blocked_by,omitempty"`
	// SyncStatus is the JSON-encoded run-completion sync
	// annotation (see store.SyncStatus + ParseSyncStatus). Set
	// when the run-branch → default-branch merge hit a content
	// conflict, so the run's output never reached the default
	// branch. UNLIKE BlockedBy this is NOT gated on a run state
	// — a `completed` run can still carry it (that's exactly the
	// silent-data-loss the flag exists to surface). Empty for a
	// clean sync / mode:none / older coordinators.
	SyncStatus string `json:"sync_status,omitempty"`
	// YAML is the raw source recipe (pre-param-substitution)
	// the run was created from. Opt-in only: the coordinator
	// returns it solely on GET .../runs/{seq}?include=yaml, so
	// it is empty on the default run payload and on older
	// coordinators. Populated for every run (inline or
	// template) when requested. Surfaced read-only on the run
	// page beside the DAG.
	YAML string `json:"yaml,omitempty"`
}

// Member is the JSON shape for one project membership row.
type Member struct {
	Username string    `json:"username"`
	Name     string    `json:"name,omitempty"`
	Role     string    `json:"role"`
	AddedAt  time.Time `json:"added_at"`
	AddedBy  string    `json:"added_by,omitempty"`
}

// Iteration is the JSON shape for one iteration of a task —
// one row in task_claims. Used by the iterations endpoint
// (GET /api/v1/tasks/{id}/iterations) and consumed by both
// the format renderer and the web UI's task-history panel.
type Iteration struct {
	Seq            int        `json:"seq"`
	Citizen        string     `json:"citizen"`
	Outcome        string     `json:"outcome"`
	ClaimedAt      time.Time  `json:"claimed_at"`
	// SubmittedAt is a pointer so omitempty actually drops it
	// from the wire when the iteration is still open.
	// time.Time's zero value would otherwise marshal as
	// "0001-01-01T00:00:00Z" — a wire regression vs the
	// previous string-with-omitempty shape.
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	CommitSHA      string     `json:"commit_sha,omitempty"`
	Branch         string     `json:"branch,omitempty"`
	ReviewDecision string     `json:"review_decision,omitempty"`
	Option         string     `json:"option,omitempty"`
	Model          string     `json:"model,omitempty"`
	// DurationMS is the elapsed wall-clock between claim and
	// submission, in milliseconds. Phase 8.6 surface — lets
	// run_status / web UI render "5m" / "2h" without doing
	// the time arithmetic on the client. Zero for iterations
	// that haven't submitted yet (claim still open).
	DurationMS int64 `json:"duration_ms,omitempty"`
}

// IsTerminalRunState returns true if the given state string (case-insensitive)
// represents a run that has finished its lifecycle.
func IsTerminalRunState(s string) bool {
	switch strings.ToLower(s) {
	case "completed", "failed", "aborted", "terminated":
		return true
	}
	return false
}
