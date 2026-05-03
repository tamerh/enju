package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// EventListParams bundles the filter knobs both event-listing
// tools (enju_show_events, enju_recent_events, GET /events)
// support. Defaults: zero values mean "no filter."
type EventListParams struct {
	ProjectID  int64
	RunSeq     int    // 0 = no run filter; service resolves seq → run_id
	Citizen    string // "" = no citizen filter; unknown citizen → empty result
	EventTypes string // CSV; "" = no type filter
	Since      string // RFC3339; "" = no since filter
	SinceSeq   int64  // strict-> seq filter; notify uses this
	Limit      int
}

// ListEvents reads the project event log with the given
// filters, drained for read-after-write consistency. Returns
// the JSON-shape map slice both formatters
// (format.EventListJSONL + format.EventListRecent) consume.
//
// Membership-gated through the project. Returns ErrNotMember
// for non-members, ErrNotFound when run_seq names a missing
// run.
//
// Unknown citizen → empty result (matches the legacy HTTP
// behaviour that writes `[]` rather than 404).
func ListEvents(s *store.Store, caller *store.CitizenRecord, p EventListParams) ([]map[string]interface{}, error) {
	if !CanReadProject(s, p.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	q := store.EventQuery{ProjectID: p.ProjectID}
	if p.RunSeq != 0 {
		run, err := s.GetRunByProjectSeq(p.ProjectID, p.RunSeq)
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, ErrNotFound
		}
		q.RunID = run.ID
	}
	if p.Citizen != "" {
		c, err := s.GetCitizenByUsername(p.Citizen)
		if err != nil || c == nil {
			// Unknown user → empty list, not an error.
			return []map[string]interface{}{}, nil
		}
		q.CitizenID = c.ID
	}
	if p.EventTypes != "" {
		q.EventTypes = strings.Split(p.EventTypes, ",")
	}
	if p.Since != "" {
		ts, err := time.Parse(time.RFC3339, p.Since)
		if err != nil {
			return nil, err
		}
		q.Since = ts
	}
	if p.SinceSeq > 0 {
		q.SinceSeq = p.SinceSeq
	}
	if p.Limit > 0 {
		q.Limit = p.Limit
	}

	// Read-after-write consistency. Same budget as the legacy
	// HTTP one-shot path so a recent_events call right after a
	// submit doesn't miss the event still in the writer's queue.
	const oneShotReadDrainBudget = 100 * time.Millisecond
	s.Events().WaitForDrain(oneShotReadDrainBudget)

	events, err := s.ListEvents(q)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		out = append(out, eventRowMap(e))
	}
	return out, nil
}

// eventRowMap is the wire-shape projection of one event row,
// used by both formatters. Pulled into the service package so
// REST + MCP + future Web UI all serialize identically.
func eventRowMap(e store.RunEventRecord) map[string]interface{} {
	row := map[string]interface{}{
		"seq":  e.Seq,
		"ts":   e.Timestamp.UTC().Format(time.RFC3339Nano),
		"type": e.Type,
	}
	if e.Subtype != "" {
		row["subtype"] = e.Subtype
	}
	if e.TaskID != "" {
		row["task_id"] = e.TaskID
	}
	if e.Citizen != "" {
		row["citizen"] = e.Citizen
	}
	if e.Metadata != "" {
		var md interface{}
		if json.Unmarshal([]byte(e.Metadata), &md) == nil {
			row["metadata"] = md
			if mdMap, ok := md.(map[string]interface{}); ok {
				if at, ok := mdMap["assign_to"].(string); ok && at != "" {
					row["assign_to"] = at
				}
			}
		} else {
			row["metadata"] = e.Metadata
		}
	}
	return row
}

// EventsStatusResponse is the wire shape for the
// enju_events_status admin tool. Mirrors store.Stats with JSON
// keys format.EventsStatus consumes.
type EventsStatusResponse struct {
	Enabled       bool  `json:"enabled"`
	Enqueued      int64 `json:"enqueued"`
	Persisted     int64 `json:"persisted"`
	Dropped       int64 `json:"dropped"`
	QueueDepth    int   `json:"queue_depth"`
	QueueCapacity int   `json:"queue_capacity"`
}

// GetEventsStatus returns the EventStore's runtime stats. No
// project scope; caller must be authenticated (admin-style
// tool). Read-only — operators flip the kill-switch via
// enju.conf + SIGHUP.
func GetEventsStatus(s *store.Store, caller *store.CitizenRecord) EventsStatusResponse {
	stats := s.Events().Stats()
	return EventsStatusResponse{
		Enabled:       stats.Enabled,
		Enqueued:      stats.Enqueued,
		Persisted:     stats.Persisted,
		Dropped:       stats.Dropped,
		QueueDepth:    stats.QueueDepth,
		QueueCapacity: stats.QueueCapacity,
	}
}
