package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

// handleShowEvents is the read-only projection over
// events — the JSONL-shaped event log view.
// Distinct from /events (run-scoped) and from
// enju_export_run_events (which writes git-tracked snapshots).
// This endpoint is for ad-hoc queries: "what happened in this
// project / run / by this citizen, of these types, since when."
//
// Query params: run_seq (optional, narrows to one run),
// citizen (username, optional), event_types (comma-separated),
// since (RFC3339), limit (default 100, max 1000).
//
// Living-workflow phase 2 — see docs/living-workflow-design-notes.md
// § "The event log is the central data primitive."
//
// Long-poll mode (?wait=30s) is the substrate for the
// notification subsystem (docs/notifications.md). Subscribe-then-
// query inside longPollEvents closes the missed-event race; see
// the EventStore.WaitForEvent contract for details.

// longPollEvents is the read loop with optional blocking. When
// waitDuration <= 0 it's a single ListEvents call. When > 0 it
// loops:
//
//  1. Subscribe to the EventStore notifier (BEFORE querying so
//   any persist between subscribe and query is observed on
//   the next iteration's query, not missed).
//  2. Query the database with the caller's filter.
//  3. If results are non-empty → return.
//  4. Otherwise wait on (notifier OR remaining-time OR
//   request-context-cancelled), then loop.
//
// Returns empty slice + nil on timeout or context cancel — the
// caller treats both as "no events," which is the correct wire
// shape for long-poll. ctx errors don't propagate up; the
// response just becomes empty.
// eventRowFromStore renders a store event into the wire shape
// shared by /events and /runs/{seq}/events. Centralizes the
// metadata-parsing + assign_to hoist so both endpoints stay in
// sync. The hoist promotes assign_to (a metadata field) to a
// top-level wire field so notify-supervisor predicates can read
// it like citizen/task_id without parsing metadata themselves.
// Future "X is the load-bearing match key" fields (project_owner,
// parent_id) plug in here.
func eventRowFromStore(e store.RunEventRecord) map[string]interface{} {
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

func (s *Server) longPollEvents(ctx context.Context, q store.EventQuery, waitDuration time.Duration) ([]store.RunEventRecord, error) {
	if waitDuration <= 0 {
		// Read-after-write consistency for one-shot queries:
		// give the async writer a brief window to drain in-
		// flight events. Without this, an assistant calling
		// enju_recent_events immediately after a submit can
		// miss the event still in the writer's queue. Matches
		// the budget used by aggregation reads in the store
		// package (eventDrainBudget). Long-poll mode skips this
		// because the subscribe-then-query loop catches new
		// events directly via the broadcast channel.
		const oneShotReadDrainBudget = 100 * time.Millisecond
		s.store.Events().WaitForDrain(oneShotReadDrainBudget)
		return s.store.ListEvents(q)
	}
	deadline := time.Now().Add(waitDuration)
	for {
		// Subscribe BEFORE querying. If a persist races between
		// these two lines, ListEvents sees the committed event
		// and we return immediately; if the persist happens
		// after both, the channel is observed closed and the
		// next iteration catches it.
		waitCh := s.store.Events().WaitForEvent()

		events, err := s.store.ListEvents(q)
		if err != nil {
			return nil, err
		}
		if len(events) > 0 {
			return events, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return events, nil // empty + timed out
		}
		select {
		case <-waitCh:
			// New event landed; loop and re-query.
		case <-ctx.Done():
			return events, nil // client gone; empty response is fine
		case <-time.After(remaining):
			return events, nil // wait elapsed
		}
	}
}

func (s *Server) handleShowEvents(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	// kill-switch UX. When the EventStore is
	// disabled, ListEvents returns nil/nil so the response
	// is an empty array — indistinguishable from "no events
	// match" in the wire shape. Stamp a response header so
	// the MCP tool layer can prepend an explicit warning.
	// Header (not body) keeps the JSON array contract intact
	// for direct REST consumers.
	if !s.store.Events().Enabled() {
		w.Header().Set("X-Enju-Audit-Disabled", "true")
	}

	q := store.EventQuery{ProjectID: projectID}

	if rs := r.URL.Query().Get("run_seq"); rs != "" {
		seq, err := strconv.Atoi(rs)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_seq")
			return
		}
		run, err := s.store.GetRunByProjectSeq(projectID, seq)
		if err != nil || run == nil {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		q.RunID = run.ID
	}
	if u := r.URL.Query().Get("citizen"); u != "" {
		c, err := s.store.GetCitizenByUsername(u)
		if err == nil && c != nil {
			q.CitizenID = c.ID
		} else {
			// Unknown username → empty result, not an error.
			writeJSON(w, http.StatusOK, []map[string]interface{}{})
			return
		}
	}
	if et := r.URL.Query().Get("event_types"); et != "" {
		q.EventTypes = strings.Split(et, ",")
	}
	if since := r.URL.Query().Get("since"); since != "" {
		ts, err := time.Parse(time.RFC3339, since)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since (expected RFC3339): "+err.Error())
			return
		}
		q.Since = ts
	}
	if seqParam := r.URL.Query().Get("since_seq"); seqParam != "" {
		// Streaming-cursor variant of `since`. Strict `>` filter on
		// the per-project monotone seq. Notify uses this — eliminates
		// the +1ns cursor dance the timestamp filter requires.
		n, err := strconv.ParseInt(seqParam, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid since_seq (expected non-negative int)")
			return
		}
		q.SinceSeq = n
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		q.Limit = n
	}

	// Long-poll: ?wait=30s holds the connection until either
	// (a) matching events arrive, (b) the wait elapses, or
	// (c) the client disconnects. Used by `enju notify` and
	// the future `enju_recent_events` polling tool to react
	// to events without busy-polling the database.
	//
	// Bound: min(longPollHardCap, httpTimeout - longPollMargin).
	// The HTTP middleware cap is the real ceiling — wait beyond
	// it and the middleware fires a 503 before the handler can
	// respond, leaving the client with a useless error. Subtract
	// a margin for the response write to flush before the
	// middleware's deadline. wait <= 0 → immediate return,
	// legacy shape.
	const (
		longPollHardCap = 60 * time.Second
		longPollMargin  = 5 * time.Second
	)
	httpTimeout := s.httpRequestTimeout
	if httpTimeout <= 0 {
		httpTimeout = 30 * time.Second
	}
	longPollMax := longPollHardCap
	if safe := httpTimeout - longPollMargin; safe < longPollMax {
		longPollMax = safe
	}
	if longPollMax < 0 {
		longPollMax = 0
	}
	var waitDuration time.Duration
	if waitParam := r.URL.Query().Get("wait"); waitParam != "" {
		d, err := time.ParseDuration(waitParam)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid wait (expected duration like 30s)")
			return
		}
		if d < 0 {
			d = 0
		}
		if d > longPollMax {
			d = longPollMax
		}
		waitDuration = d
	}

	events, err := s.longPollEvents(r.Context(), q, waitDuration)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing events: "+err.Error())
		return
	}

	out := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		out = append(out, eventRowFromStore(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEventsStatus returns the EventStore's runtime state:
// enabled flag + Stats() snapshot. Read-only — the kill-switch
// is flipped by editing enju.conf and SIGHUP, not via HTTP.
// Useful for monitoring "are events landing? are we dropping?"
// without grepping logs.
func (s *Server) handleEventsStatus(w http.ResponseWriter, r *http.Request) {
	es := s.store.Events()
	stats := es.Stats()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":        stats.Enabled,
		"enqueued":       stats.Enqueued,
		"persisted":      stats.Persisted,
		"dropped":        stats.Dropped,
		"queue_depth":    stats.QueueDepth,
		"queue_capacity": stats.QueueCapacity,
	})
}
