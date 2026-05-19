package service

// Read-side view methods consumed by in-process UIs (web today,
// future CLI). The 1:1-with-coord shapes (Project, Run, Member)
// are imported from internal/common/wire so coord-side renames
// are a compile-time signal here, not a silent zero-value
// decode. The non-trivial reshapes (TaskSummary's lean
// projection, EventRow's parsed metadata) stay local — they're
// genuinely fat-client view models, not the coord's wire shape.
//
// Methods are read-only — no Plan, no chokepoint. They go through
// the coord HTTP client like every other fat-client read.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// ProjectDetail extends a project with the membership list. One
// method, two coord calls — kept under one round-trip budget for
// the project-detail page.
//
// Embedding wire.Project makes the JSON wire shape flat
// (`{id, name, ..., members}`) — a future `members` field on
// wire.Project would collide with ProjectDetail.Members on
// JSON roundtrip. wire.Project is intentionally lean and won't
// grow such a field; if that constraint changes, switch to
// non-embedded (`Project wire.Project json:"project"`) and
// accept the wire-shape break.
type ProjectDetail struct {
	wire.Project
	Members []wire.Member `json:"members"`
}

// RunDetail extends a run with the task list and a pre-rendered
// Mermaid diagram. Diagram generation is the same pure function
// enju_export_diagram uses, so the diagram in the UI matches the
// one the user gets on the CLI.
type RunDetail struct {
	wire.Run
	Tasks          []TaskSummary `json:"tasks"`
	DiagramMermaid string        `json:"diagram_mermaid"`
}

// TaskSummary is the lean per-task shape for run/inbox views.
// The full TaskResponse on the coord side is ~50 fields; UIs
// rendering a list don't need most of them. Open the task detail
// for the rest. DependsOn is parsed from the coord's
// comma-separated string into a slice for ergonomic consumers.
type TaskSummary struct {
	ID             string   `json:"id"`
	Action         string   `json:"action"`
	State          string   `json:"state"`
	Seq            int      `json:"seq"`
	AssignedTo     []string `json:"assigned_to,omitempty"`
	DependsOn      []string `json:"depends_on,omitempty"`
	ClaimedBy      string   `json:"claimed_by,omitempty"`
	IterationLabel string   `json:"iteration_label,omitempty"`
	// IterCount is the number of distinct iter_seq values the
	// task has been through (Phase 8.6). 1 for single-attempt
	// tasks, > 1 for tasks that bounced through request_changes.
	// UI gates on >1 to surface a "this iterated" indicator.
	IterCount int `json:"iter_count,omitempty"`
}

// ListEventsOpts mirrors the coord /events query string.
//
// TODO(ui-liveness): no `wait=` (long-poll) here. When the UI
// wants live event tail, add a streaming method (SSE or
// long-poll) — wait-style pulls are supervisor work, not view
// work.
type ListEventsOpts struct {
	Since      time.Time
	SinceSeq   int64
	Limit      int
	EventTypes []string
	Citizen    string
	RunSeq     int
}

// EventRow is the parsed event shape — Metadata is already
// json.Unmarshal'd into a map, AssignTo is hoisted out of
// metadata at the coord side. Same enrichment as the existing
// /events endpoint, just typed.
type EventRow struct {
	Seq       int64          `json:"seq"`
	Timestamp time.Time      `json:"ts"`
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype,omitempty"`
	TaskID    string         `json:"task_id,omitempty"`
	Citizen   string         `json:"citizen,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	AssignTo  string         `json:"assign_to,omitempty"`
}

// MaterializedProject is one entry returned from
// ListMaterializedProjects: a project ID that has a local clone
// at Path under the workspace root.
type MaterializedProject struct {
	ProjectID int64
	Path      string
}

// ListProjects returns every project the caller is a member of,
// each with run count. Backed by GET /api/v1/projects.
func (s *FatClient) ListProjects(ctx context.Context) ([]wire.Project, error) {
	data, err := s.coord.Get(ctx, "/api/v1/projects")
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out []wire.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	return out, nil
}

// GetProject returns a single project plus its members.
// Two coord calls — `/projects/{id}` and `/projects/{id}/members`
// — issued sequentially. Switching to parallel is mechanical
// when latency justifies the complexity; for v1 the simple
// shape is fine.
//
// TODO(latency): parallelize the two GETs once a real workload
// shows the sequential cost matters.
func (s *FatClient) GetProject(ctx context.Context, projectID int64) (*ProjectDetail, error) {
	// coord.Get swallows the status and returns 4xx bodies with
	// err==nil; without GetStatus a missing project decodes into
	// a zero-value wire.Project and we'd hand back a non-nil
	// ghost ProjectDetail (blank 200 page). Recover the 404 and
	// guard the error-shaped body like ListRuns does.
	pData, status, err := s.coord.GetStatus(ctx, fmt.Sprintf("/api/v1/projects/%d", projectID))
	if err != nil {
		return nil, err
	}
	if status == 404 { // http.StatusNotFound — literal keeps net/http out of the data layer
		return nil, fmt.Errorf("project %d: %w", projectID, ErrNotFound)
	}
	if msg := coord.ExtractError(pData); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var p wire.Project
	if err := json.Unmarshal(pData, &p); err != nil {
		return nil, fmt.Errorf("decode project: %w", err)
	}

	mData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/members", projectID))
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(mData); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var members []wire.Member
	if err := json.Unmarshal(mData, &members); err != nil {
		return nil, fmt.Errorf("decode members: %w", err)
	}

	return &ProjectDetail{Project: p, Members: members}, nil
}

// ListRuns returns every run for a project. The coord enforces
// project membership; callers without access get a 4xx surfaced
// through the coord client.
func (s *FatClient) ListRuns(ctx context.Context, projectID int64) ([]wire.Run, error) {
	data, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out []wire.Run
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode runs: %w", err)
	}
	return out, nil
}

// GetRun fetches a run plus its tasks plus a pre-rendered
// Mermaid diagram. Three coord touches: run detail, run tasks,
// and the pure-function mermaid render over the JSON we already
// have. No file I/O — the same render that
// enju_export_diagram --format=mermaid commits to disk.
//
// TODO(latency): parallelize the two GETs once a real workload
// shows the sequential cost matters.
func (s *FatClient) GetRun(ctx context.Context, projectID int64, runSeq int) (*RunDetail, error) {
	// ?include=yaml opts into the run's source recipe (a few KB,
	// off the default payload). The web run page renders it
	// beside the DAG; one fetch per page navigation, not a hot
	// poll, so always requesting it here is fine.
	// GetStatus (not Get) so a missing run is a clean 404, not
	// a {"error":...} body that decodes into a zero-value
	// wire.Run and only blows up later as "decode tasks: cannot
	// unmarshal object into []service.taskWire" (leaking the Go
	// type to the browser via the 502 path). Same guard ListRuns
	// already has.
	runData, status, err := s.coord.GetStatus(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d?include=yaml", projectID, runSeq))
	if err != nil {
		return nil, err
	}
	if status == 404 { // http.StatusNotFound — literal keeps net/http out of the data layer
		return nil, fmt.Errorf("run %d:%d: %w", projectID, runSeq, ErrNotFound)
	}
	if msg := coord.ExtractError(runData); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var r wire.Run
	if err := json.Unmarshal(runData, &r); err != nil {
		return nil, fmt.Errorf("decode run: %w", err)
	}

	tasksData, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", projectID, runSeq))
	if err != nil {
		return nil, err
	}
	// Defensive: even though the run GET is now guarded, never
	// feed an error-shaped body into the []taskWire decode.
	if msg := coord.ExtractError(tasksData); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var taskWires []taskWire
	if err := json.Unmarshal(tasksData, &taskWires); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	tasks := make([]TaskSummary, 0, len(taskWires))
	for _, t := range taskWires {
		tasks = append(tasks, t.toSummary())
	}

	return &RunDetail{
		Run:            r,
		Tasks:          tasks,
		DiagramMermaid: format.RenderMermaidBody(runData, tasksData),
	}, nil
}

// ListEvents returns the typed event stream for a project.
// Mirrors the coord /events query parameters (since, since_seq,
// limit, event_types, citizen, run_seq); long-poll wait= is
// not exposed here — wait-style polling is supervisor work,
// not view work.
func (s *FatClient) ListEvents(ctx context.Context, projectID int64, opts ListEventsOpts) ([]EventRow, error) {
	q := url.Values{}
	if !opts.Since.IsZero() {
		q.Set("since", opts.Since.UTC().Format(time.RFC3339))
	}
	if opts.SinceSeq > 0 {
		q.Set("since_seq", strconv.FormatInt(opts.SinceSeq, 10))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if len(opts.EventTypes) > 0 {
		q.Set("event_types", strings.Join(opts.EventTypes, ","))
	}
	if opts.Citizen != "" {
		q.Set("citizen", opts.Citizen)
	}
	if opts.RunSeq > 0 {
		q.Set("run_seq", strconv.Itoa(opts.RunSeq))
	}

	path := fmt.Sprintf("/api/v1/projects/%d/events", projectID)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	data, err := s.coord.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	var raw []eventWire
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode events: %w", err)
	}
	out := make([]EventRow, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.toRow(s.logger))
	}
	return out, nil
}

// ListMaterializedProjects reports projects this fat-client
// knows about. Source order:
//
//  1. Project registry (`~/.enju/projects.json`) when present.
//     Includes externally adopted dirs that aren't discoverable
//     from the filesystem. Stale entries (path no longer
//     exists) are filtered at read time.
//  2. Filesystem walk over the workspace root, used as a
//     fallback when the registry is empty/missing or no
//     registry is configured. Captures standard clones for
//     users on the older "no registry" code path so they don't
//     see an empty UI.
//
// UI cross-project landing pages call this to enumerate
// projects, then GetProject per ID for richer metadata that
// isn't cached in the registry.
func (s *FatClient) ListMaterializedProjects() ([]MaterializedProject, error) {
	if s.projectRegistry == nil {
		return nil, nil
	}
	entries, err := s.projectRegistry.List()
	if err != nil {
		return nil, fmt.Errorf("project registry list: %w", err)
	}
	out := make([]MaterializedProject, 0, len(entries))
	for _, e := range entries {
		out = append(out, MaterializedProject{
			ProjectID: e.ID,
			Path:      e.LocalPath,
		})
	}
	return out, nil
}

// === local wire types ===
//
// taskWire and eventWire stay local because the coord-side
// TaskResponse is genuinely much larger than what the UI needs
// (8 fields vs ~50), and EventRow's metadata-as-map / hoisted
// AssignTo are post-processing the UI cares about. They're
// drift-prone in the same way the moved types were, so the
// follow-up contract test covers them.

type taskWire struct {
	ID             string   `json:"id"`
	Action         string   `json:"action"`
	State          string   `json:"state"`
	Seq            int      `json:"seq"`
	AssignTo       []string `json:"assign_to"`
	DependsOn      string   `json:"depends_on"`
	ClaimedBy      string   `json:"claimed_by"`
	IterationLabel string   `json:"iteration_label"`
	IterCount      int      `json:"iter_count"`
}

func (t taskWire) toSummary() TaskSummary {
	var deps []string
	if t.DependsOn != "" {
		for _, d := range strings.Split(t.DependsOn, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				deps = append(deps, d)
			}
		}
	}
	return TaskSummary{
		ID:             t.ID,
		Action:         t.Action,
		State:          t.State,
		Seq:            t.Seq,
		AssignedTo:     t.AssignTo,
		DependsOn:      deps,
		ClaimedBy:      t.ClaimedBy,
		IterationLabel: t.IterationLabel,
		IterCount:      t.IterCount,
	}
}

type eventWire struct {
	Seq      int64          `json:"seq"`
	Ts       string         `json:"ts"`
	Type     string         `json:"type"`
	Subtype  string         `json:"subtype"`
	TaskID   string         `json:"task_id"`
	Citizen  string         `json:"citizen"`
	Metadata map[string]any `json:"metadata"`
	AssignTo string         `json:"assign_to"`
}

func (e eventWire) toRow(log *slog.Logger) EventRow {
	return EventRow{
		Seq:       e.Seq,
		Timestamp: parseTimeOrLog(e.Ts, time.RFC3339Nano, "event.ts", log),
		Type:      e.Type,
		Subtype:   e.Subtype,
		TaskID:    e.TaskID,
		Citizen:   e.Citizen,
		Metadata:  e.Metadata,
		AssignTo:  e.AssignTo,
	}
}

// parseTimeOrLog parses a timestamp with the given layout. On
// error logs at warn level (so coord-side format drift is
// diagnosable) and returns the zero value. Empty input is not
// an error — coord uses omitempty for absent timestamps.
//
// Only EventRow needs this now (it uses RFC3339Nano with
// post-processed metadata); the wire types in common/wire use
// time.Time directly so encoding/json handles parsing.
func parseTimeOrLog(value, layout, field string, log *slog.Logger) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(layout, value)
	if err != nil && log != nil {
		log.Warn("views: time parse failed",
			"field", field,
			"value", value,
			"layout", layout,
			"error", err,
		)
	}
	return t
}
