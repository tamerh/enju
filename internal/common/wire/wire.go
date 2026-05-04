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

import "time"

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
	Slug            string    `json:"slug,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	SourcePath      string    `json:"source_path,omitempty"`
	SourceCommitSHA string    `json:"source_commit_sha,omitempty"`
	Warnings        []string  `json:"warnings,omitempty"`
}

// Member is the JSON shape for one project membership row.
type Member struct {
	Username string    `json:"username"`
	Name     string    `json:"name,omitempty"`
	Role     string    `json:"role"`
	AddedAt  time.Time `json:"added_at"`
	AddedBy  string    `json:"added_by,omitempty"`
}
