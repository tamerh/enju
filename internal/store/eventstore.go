package store

// Events — strict-consumer audit ledger.
//
// The EventStore is a separate persistence subsystem from the
// state Store. Architectural contract:
//
//  - Events are a strict consumer of the system, never on the
//   critical path. State mutations (claims, submits, cascades,
//   merges) complete correctly even when the EventStore is
//   unavailable; only the audit log degrades.
//  - The event store has its own database file (events.db) and
//   its own connection pool, completely independent from the
//   state store. Disk corruption, lock pathologies, or runaway
//   writers in events DB cannot affect state DB.
//  - Emission is async and best-effort. Record() never blocks
//   the caller, never returns an error, never propagates an
//   event-store failure to the request path.
//  - Drops are observable (atomic counter, rate-limited log,
//   periodic stats) and gaps are detectable (per-project
//   monotone seq) so "best-effort" is auditable, not invisible.
//  - The subsystem can be killed entirely via config
//   (events.enabled = false). Disabled state is queryable;
//   reads return ErrEventStoreDisabled, which UI surfaces
//   translate to "audit emission disabled by operator." No
//   backfill on resume — gaps stay.
//
// State queries (dashboard, run status, profile metrics) live in
// the state store and never touch events. Audit display
// (enju_show_events, enju_export_run_events) reads events here
// and enriches with Go-side lookups against the state store —
// no SQL JOINs across the two subsystems.

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// MarshalMetadata is the canonical builder for an Event's
// Metadata JSON blob. swept the package off
// `fmt.Sprintf(\`{"k":%q}\`, v)` because Go's %q is *almost*
// JSON-compatible but diverges on a few control characters
// (`\xNN` vs `\uNNNN`) — fine for fixed slug-shaped values
// (action, branch names, hex SHAs) but unsafe for any user-
// supplied free text (issue titles, invalidate reasons,
// reviewer comments) where a stray control byte would produce
// invalid JSON.
//
// Usage at every emit site:
//
//	store.MarshalMetadata(map[string]any{
//	  "iter_seq":  iterSeq,
//	  "commit_sha": sha,
//	  "reviewed":  false,
//	})
//
// Failure is impossible in practice — encoding a flat
// map[string]any of primitives + strings has no error path
// short of cycles, which we don't construct here. We swallow
// the error and return "{}" for the would-never-happen case
// rather than propagate; the caller's emission path doesn't
// have a sensible failure shape.
func MarshalMetadata(kv map[string]any) string {
	if len(kv) == 0 {
		return "{}"
	}
	b, err := json.Marshal(kv)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ErrEventStoreDisabled is returned by EventStore read methods
// when the operator has flipped the kill-switch. UI surfaces
// translate this into a "audit emission disabled" message
// rather than a connection error so operators can disable
// events without making the audit endpoint look broken.
var ErrEventStoreDisabled = errors.New("event store disabled")

// Event is one append-only ledger entry. Self-contained: every
// reader-relevant fact is in either a typed column or the JSON
// metadata blob. Readers never JOIN with state tables — they
// fetch events here and (if they need human-readable names)
// do a lightweight Go-side lookup against the state store.
//
// Seq is the per-project monotone sequence number, claimed
// synchronously at emission time BEFORE the queue decision so
// dropped events still consume a seq — gaps in the seq
// sequence are the audit-detectable signal that the queue
// dropped events under load.
type Event struct {
	ID      int64
	Seq     int64 // monotone within (project_id), gap-detectable
	CitizenID  int64
	EventType  string
	EventSubtype string
	TaskID    string
	RunID    int64
	ProjectID  int64
	Metadata   string // JSON blob, event-type-specific fields
	CreatedAt  time.Time
}

// Stats reports the EventStore's runtime state. Operators read
// it via enju_show_events --status (or equivalent admin tool)
// to see whether drops are happening, how full the queue is,
// and whether the kill-switch is engaged.
//
// All counters are monotone since process start (no decay,
// no reset). Operators correlate against process start time.
type Stats struct {
	Enabled  bool // kill-switch state
	Enqueued  int64 // events successfully placed on the queue
	Persisted int64 // events successfully written to disk
	Dropped  int64 // events dropped (queue full, persist failure)
	QueueDepth int  // current in-flight queue size
}

// EventStore is the persistence layer for the audit ledger.
// One process = one EventStore = one events.db file.
//
// Lifecycle: NewEventStore opens the file and starts the writer
// goroutine. Close drains the queue (bounded by a shutdown
// timeout) and closes the file.
type EventStore interface {
	// Record enqueues an event for async persistence. Never
	// blocks (channel send is non-blocking; full queue → drop
	// with logged warning). Never returns an error to the
	// caller — event-store failures are observability data,
	// not state failures.
	//
	// The event's Seq is assigned by the EventStore (caller
	// must not set it). CreatedAt is set if zero.
	Record(event Event)

	// QueryByRun returns events for a run, ordered by seq.
	// Used by enju_show_events and enju_export_run_events.
	// Returns ErrEventStoreDisabled when the kill-switch is
	// engaged.
	QueryByRun(ctx context.Context, runID int64, since time.Time, limit int) ([]Event, error)

	// QueryByCitizen returns events for a citizen across all
	// projects, ordered by seq. Used by profile/contribution
	// summaries that aren't reformulatable from state-only data.
	QueryByCitizen(ctx context.Context, citizenID int64, limit int) ([]Event, error)

	// Query is the generic projection used by enju_show_events
	// and any consumer that needs filter combinations not
	// covered by the dedicated by-run / by-citizen accessors.
	// All EventQuery fields are optional; an empty filter
	// matches all events. Caller is responsible for normalizing
	// Limit (the EventStore honors whatever bound it receives).
	Query(ctx context.Context, q EventQuery) ([]Event, error)

	// --- Profile-page-shaped aggregate accessors ---
	//
	// The seven methods below are profile-display shortcuts:
	// the existing `enju_my_profile` and friends used to issue
	// these aggregations directly against the state-DB
	// events table, and the migration to a
	// separate event store keeps the same shapes so the UI
	// doesn't have to change. Backends (Postgres, OTLP sink,
	// log-file readers) implementing this interface will
	// either inherit the shape from a shared helper or
	// reimplement them with their native query language.
	//
	// Future consumers (CLI tools, Grafana exporters, custom
	// audit dashboards) should prefer QueryByRun /
	// QueryByCitizen above and aggregate client-side — that
	// path stays stable across backend swaps. The aggregates
	// here are kept for migration parity, not as the
	// recommended consumer shape.
	//
	// A future v2 refactor may extract these
	// into a ProfileQueries helper that wraps any EventStore,
	// shrinking this interface back to Record + raw queries.
	// Tracked as a known cleanup; not blocking v1.

	// CountByCitizenAndType returns counts grouped by
	// (event_type, event_subtype) for a citizen. Profile
	// display.
	CountByCitizenAndType(ctx context.Context, citizenID int64) (map[string]map[string]int, error)

	// SumTokensForCitizen sums the estimated_tokens metadata
	// field for a citizen across all their events. Used by
	// the profile display.
	SumTokensForCitizen(ctx context.Context, citizenID int64) (int64, error)

	// CountDistinctProjectsForCitizen reports how many
	// distinct projects a citizen has events in.
	CountDistinctProjectsForCitizen(ctx context.Context, citizenID int64) (int, error)

	// CountContributionEvents returns the total event count
	// for a citizen.
	CountContributionEvents(ctx context.Context, citizenID int64) (int, error)

	// CountProjectsThisMonth returns the distinct project
	// count for a citizen since the given timestamp.
	CountProjectsThisMonth(ctx context.Context, citizenID int64, since time.Time) (int, error)

	// LatestMetadataForTask returns the metadata of the most
	// recent event of a given type for a task.
	LatestMetadataForTask(ctx context.Context, taskID, eventType string) (string, error)

	// DistinctTaskIDsForCitizenAndType returns the distinct
	// task IDs a citizen has events of a given type for.
	DistinctTaskIDsForCitizenAndType(ctx context.Context, citizenID int64, eventType string) ([]string, error)

	// Stats returns the runtime observability snapshot.
	// Cheap; safe to call frequently.
	Stats() Stats

	// Enabled reports whether the kill-switch is OFF (events
	// flowing) or ON (events disabled, Record is a no-op,
	// reads return ErrEventStoreDisabled).
	//
	// v1 limitation worth noting: the kill-switch is a
	// single global flag — disable means BOTH writes stop
	// AND reads refuse. An operator who wants forensic-mode
	// behavior ("stop accepting new emissions but let me
	// inspect what's already in the log") can't get it
	// without process-level access. If hosted-mode operators
	// hit this workflow in practice, v2 splits this into
	// EmitEnabled / ReadEnabled with separate flags.
	// Tracked; not blocking v1.
	Enabled() bool

	// SetEnabled flips the kill-switch at runtime. Operators
	// use this to disable events if the subsystem is
	// misbehaving without restarting the coordinator. No
	// backfill on re-enable.
	SetEnabled(enabled bool)

	// GapsInProject returns missing seq numbers in the
	// project's persisted event sequence. the
	// audit-detectable signal that drops happened. A return
	// of `[]int64{4, 7, 8}` means seqs 4, 7, and 8 were
	// claimed but never persisted — either dropped by the
	// queue under back-pressure, or lost when the process
	// crashed mid-write. Stats().Dropped distinguishes the
	// two for live operations; post-restart they're
	// indistinguishable (which is acceptable: the gap itself
	// is the signal, not the cause).
	//
	// Returns nil + ErrEventStoreDisabled when the kill-
	// switch is engaged. Empty slice + nil error means "no
	// gaps detected" (audit log is intact for that project).
	GapsInProject(ctx context.Context, projectID int64) ([]int64, error)

	// WaitForDrain blocks up to `timeout` for the writer
	// goroutine to persist everything currently queued, then
	// returns. Used by read paths that need read-after-write
	// consistency on aggregations (profile counters, audit
	// timeline reads). The contract is "wait at most this
	// long" — partial drains are tolerated, the timeout is the
	// cap. Drops still don't block; queue-full events that
	// were never enqueued are not waited on.
	WaitForDrain(timeout time.Duration)

	// Close drains the queue (bounded by an internal
	// shutdown timeout) and closes the file. Idempotent.
	Close() error
}
