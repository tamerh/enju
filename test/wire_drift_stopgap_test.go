package test

// Runtime stopgap for the wire-drift problem documented in
// TODO.md ("Web-UI prep follow-ups"). The simple wire types —
// Project / Run / Member — moved to internal/common/wire so
// drift on those is a compile error. Task and Event are still
// represented by separate structs on the two sides
// (fatclient/service.taskWire ↔ coord-side TaskResponse, and
// fatclient/service.eventWire ↔ the inline map written by
// eventRowFromStore in coord/api/events.go). Drift on those is
// silent at compile time.
//
// This test plugs the gap by spinning up a real coordinator,
// creating a project + run + a draft task + emitting submit /
// completion events, then calling FatClient.ListProjects /
// GetRun / ListEvents directly and asserting the high-signal
// fields actually populated. A coord-side rename of
// `assign_to`, `task_id`, `seq`, etc. would produce zero
// values here and fail loudly.
//
// Doesn't replace the reflective contract test — only catches
// the fields the assertions exercise — but it costs ~40 lines
// and exercises the actual decode path against a real coord,
// which is more honest than reflection alone.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

func TestFatClientViewMethodsAgainstRealCoord(t *testing.T) {
	h := newMCPHarness(t, "DriftAuthor")
	projectID := h.createTestProject()

	// h.createTestProject() inserts the project under a default
	// "harness" citizen for backwards compat with older flat
	// tests. DriftAuthor needs explicit membership to see the
	// project via ListProjects (which filters by membership);
	// add the row directly through the store.
	authorCitizen, err := h.store.GetCitizenByUsername(h.username)
	if err != nil || authorCitizen == nil {
		t.Fatalf("lookup author: %v", err)
	}
	if _, err := h.store.ApplyPlan(store.Plan{Mutations: []store.Mutation{
		store.AddProjectMember{
			ProjectID: projectID,
			CitizenID: authorCitizen.ID,
			Role:      store.ProjectRoleOwner,
			AddedBy:   authorCitizen.ID,
		},
	}}); err != nil {
		t.Fatalf("AddProjectMember: %v", err)
	}

	yaml := `name: "drift smoke"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write something."
    assign_to:
      - ` + h.username + `
`
	h.mcpCreateRunInline(t, projectID, yaml)

	// Drive a claim + submit so events accrete and the task
	// has a non-trivial state for the assertions below.
	h.mcpClaimOK(t, "draft")
	h.mcpSubmitText(t, "draft", "drift-smoke-content")

	// Build a FatClient that talks to the same coord URL the
	// harness's MCP client uses. Mirrors the production wiring
	// in mcphandlers.New, minus the workspace + registry (this
	// test is read-only against coord).
	citizen, err := h.store.GetCitizenByUsername(h.username)
	if err != nil || citizen == nil {
		t.Fatalf("lookup citizen: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := service.New(service.Config{
		Coord: coord.New(coord.Config{
			BaseURL:   h.url,
			Username:  h.username,
			AuthToken: citizen.Token,
			Logger:    logger,
		}),
		Logger: logger,
	})
	ctx := context.Background()

	// ListProjects — exercises wire.Project decode path.
	projects, err := fc.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) == 0 {
		t.Fatalf("ListProjects returned empty; expected the test project")
	}
	var found bool
	for _, p := range projects {
		if p.ID == projectID {
			found = true
			if p.Name == "" {
				t.Errorf("project %d has empty Name — wire shape drift on `name`?", p.ID)
			}
			if p.CreatedAt.IsZero() {
				t.Errorf("project %d has zero CreatedAt — wire shape drift on `created_at`?", p.ID)
			}
		}
	}
	if !found {
		t.Errorf("project ID %d missing from ListProjects result", projectID)
	}

	// GetRun — exercises wire.Run + the LOCAL taskWire decode
	// path. The local taskWire is the load-bearing one for
	// drift detection; if coord renames a TaskResponse field
	// we read here, AssignedTo / Action / State below come
	// back zero.
	run, err := fc.GetRun(ctx, projectID, 1)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Seq != 1 {
		t.Errorf("run Seq mismatch: got %d, want 1", run.Seq)
	}
	if run.State == "" {
		t.Errorf("run State empty — wire shape drift on `state`?")
	}
	if run.CreatedAt.IsZero() {
		t.Errorf("run CreatedAt zero — wire shape drift on `created_at`?")
	}
	if run.DiagramMermaid == "" {
		t.Errorf("DiagramMermaid empty — render path or task-list decode broken")
	}
	if len(run.Tasks) == 0 {
		t.Fatalf("run Tasks empty — taskWire decode broken")
	}
	var draft *service.TaskSummary
	for i := range run.Tasks {
		if run.Tasks[i].Action == "answer" {
			draft = &run.Tasks[i]
			break
		}
	}
	if draft == nil {
		t.Fatalf("draft task missing from GetRun response")
	}
	if draft.ID == "" {
		t.Errorf("task.ID empty — drift on `id`?")
	}
	if draft.State == "" {
		t.Errorf("task.State empty — drift on `state`?")
	}
	if len(draft.AssignedTo) == 0 || draft.AssignedTo[0] != h.username {
		t.Errorf("task.AssignedTo mismatch: got %v, want [%q] — drift on `assign_to`?",
			draft.AssignedTo, h.username)
	}

	// ListEvents — exercises the local eventWire decode path.
	// At least one task_submitted event should exist after the
	// submit above; assert its high-signal fields populate.
	events, err := fc.ListEvents(ctx, projectID, service.ListEventsOpts{Limit: 100})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("ListEvents returned empty; expected at least one submit event")
	}
	var sawSubmit bool
	for _, e := range events {
		if e.Seq == 0 {
			t.Errorf("event has zero Seq — drift on `seq`?")
		}
		if e.Type == "" {
			t.Errorf("event has empty Type — drift on `type`?")
		}
		if e.Timestamp.IsZero() {
			t.Errorf("event has zero Timestamp — drift on `ts`?")
		}
		if e.Type == "task_submitted" {
			sawSubmit = true
			if e.TaskID == "" {
				t.Errorf("task_submitted event has empty TaskID — drift on `task_id`?")
			}
			if e.Citizen == "" {
				t.Errorf("task_submitted event has empty Citizen — drift on `citizen`?")
			}
		}
	}
	if !sawSubmit {
		t.Errorf("no task_submitted event found in %d events; emission or type-name drift", len(events))
	}
}
