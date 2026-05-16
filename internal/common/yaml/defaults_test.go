package yaml

// Tests for the workflow-level `defaults:` block — specifically
// the `defaults: { assign_to }` seeding added alongside the YAML
// ergonomics renames. resolveDefaults seeds a task's assign_to
// from the run-level default only when the task left it unset; an
// explicit per-task value (scalar or list) always wins.

import "testing"

func taskByID(p *Run, id string) *TaskDef {
	for i := range p.Tasks {
		if p.Tasks[i].ID == id {
			return &p.Tasks[i]
		}
	}
	return nil
}

// TestDefaultsAssignTo_SeedsUnsetWins covers the three behaviors
// the spec pins: (1) a task with no assign_to is seeded from the
// scalar default; (2) an explicit per-task assign_to overrides
// the default rather than merging; (3) the default itself accepts
// the scalar-or-list yamlStringList shape.
func TestDefaultsAssignTo_SeedsUnsetWins(t *testing.T) {
	src := `
name: defaults-assign-to
version: 1
defaults:
  assign_to: tamer
tasks:
  - id: fetch
    action: compute
    script: scripts/fetch.sh
  - id: review_fetch
    action: review
    reviews: fetch
    prompt: "Check the fetch output."
    assign_to: alice
`
	parsed, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fetch := taskByID(parsed.Run, "fetch")
	if fetch == nil {
		t.Fatal("task 'fetch' missing")
	}
	if len(fetch.AssignTo) != 1 || fetch.AssignTo[0] != "tamer" {
		t.Fatalf("fetch.assign_to: expected [tamer] from default, got %v", fetch.AssignTo)
	}

	rev := taskByID(parsed.Run, "review_fetch")
	if rev == nil {
		t.Fatal("task 'review_fetch' missing")
	}
	if len(rev.AssignTo) != 1 || rev.AssignTo[0] != "alice" {
		t.Fatalf("review_fetch.assign_to: explicit value must win, got %v", rev.AssignTo)
	}
}

// TestDefaultsAssignTo_ListForm confirms the default accepts a
// list (yamlStringList) and seeds every element onto an unset
// task, and that the seeded slice is an independent copy (mutating
// one task's slice must not bleed into another's).
func TestDefaultsAssignTo_ListForm(t *testing.T) {
	src := `
name: defaults-assign-to-list
version: 1
defaults:
  assign_to: [tamer, alice]
tasks:
  - id: a
    action: answer
    prompt: "one"
  - id: b
    action: answer
    prompt: "two"
`
	parsed, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := taskByID(parsed.Run, "a")
	b := taskByID(parsed.Run, "b")
	if a == nil || b == nil {
		t.Fatal("tasks a/b missing")
	}
	for _, tk := range []*TaskDef{a, b} {
		if len(tk.AssignTo) != 2 || tk.AssignTo[0] != "tamer" || tk.AssignTo[1] != "alice" {
			t.Fatalf("%s.assign_to: expected [tamer alice], got %v", tk.ID, tk.AssignTo)
		}
	}
	// Independent backing arrays: mutating a must not touch b.
	a.AssignTo[0] = "mutated"
	if b.AssignTo[0] != "tamer" {
		t.Fatalf("seeded slices alias each other: b.assign_to[0]=%q", b.AssignTo[0])
	}
}

// TestDefaultsAssignTo_NoDefaultNoChange is the negative control:
// without a default, an unset assign_to stays empty (the seeding
// must not invent a value).
func TestDefaultsAssignTo_NoDefaultNoChange(t *testing.T) {
	src := `
name: no-default
version: 1
tasks:
  - id: solo
    action: answer
    prompt: "hi"
`
	parsed, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	solo := taskByID(parsed.Run, "solo")
	if solo == nil {
		t.Fatal("task 'solo' missing")
	}
	if len(solo.AssignTo) != 0 {
		t.Fatalf("solo.assign_to: expected empty (no default), got %v", solo.AssignTo)
	}
}
