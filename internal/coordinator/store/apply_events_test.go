package store

import (
	"strings"
	"testing"
	"time"
)

// TestAllMutationKindsHaveDecoder pins the registry-decoder
// contract: every entry in AllMutationKinds must have a case
// in decodeMutation. The runtime panic in applyPlanOnce's
// "default:" arm would also catch a missing dispatcher case,
// but a test catches both directions pre-deploy with a clear
// error.
//
// Mirrors fatclient/mcphandlers TestEveryRegisteredToolHasHandler.
func TestAllMutationKindsHaveDecoder(t *testing.T) {
	for _, kind := range AllMutationKinds {
		m, err := decodeMutation(kind, []byte(`{}`))
		if err != nil && !strings.Contains(err.Error(), "unknown mutation kind") {
			// Empty payload may fail field validation for
			// some types — that's fine, we only care that
			// the decoder recognized the kind.
			continue
		}
		if m == nil {
			t.Errorf("mutation kind %q has no decoder case in decodeMutation", kind)
		}
	}
}

// TestNoOrphanMutationKinds walks the symmetric direction:
// every kind decodeMutation handles must appear in
// AllMutationKinds. Catches the case where someone adds a
// decoder case but forgets to extend the slice.
//
// Implementation: probe a curated list of "kind names that
// shouldn't decode" and assert decodeMutation returns the
// "unknown mutation kind" error. If a future contributor
// adds a decoder case for one of these without updating the
// slice, the test fails. We trust the visible enumeration
// in decodeMutation covers the rest.
func TestNoOrphanMutationKinds_Spot(t *testing.T) {
	for _, fakeKind := range []MutationKind{
		"definitely_not_a_real_mutation",
		"future_mutation_xyzzy",
	} {
		_, err := decodeMutation(fakeKind, []byte(`{}`))
		if err == nil || !strings.Contains(err.Error(), "unknown mutation kind") {
			t.Errorf("expected decodeMutation(%q) to error with 'unknown mutation kind', got %v", fakeKind, err)
		}
	}
}

// TestBuildTaskReadyEvents_TableDriven covers the pure
// fan-out logic exhaustively: empty input, single/multi
// assignee, unassigned-task fallback, parents snapshot
// embedded across multiple assignees, skipped-parent empty
// commit_sha preservation, and multi-task fan-out
// independence. Complements the existing TestBuildTaskReadyEvents
// which exercises the basic 3-case shape; this expands to
// the parents axis the reviewer flagged as untested.
func TestBuildTaskReadyEvents_TableDriven(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		readied   []ReadiedTask
		wantCount int
		// per-event assertions: index → (key, substring expected
		// in metadata JSON)
		assertions map[int]map[string]string
	}{
		{
			name:      "empty input → no events",
			readied:   nil,
			wantCount: 0,
		},
		{
			name: "unassigned task → one event with empty assign_to",
			readied: []ReadiedTask{{
				TaskID: "1:1:t", Action: "answer",
				RunID: 7, ProjectID: 3,
			}},
			wantCount: 1,
			assertions: map[int]map[string]string{
				0: {"assign_to": `"assign_to":""`},
			},
		},
		{
			name: "single assignee → one event",
			readied: []ReadiedTask{{
				TaskID: "1:1:t", Action: "review",
				Assignees: []string{"alice"},
				RunID:     7, ProjectID: 3,
			}},
			wantCount: 1,
			assertions: map[int]map[string]string{
				0: {"assign_to": `"assign_to":"alice"`},
			},
		},
		{
			name: "multi-assignee → one event per assignee",
			readied: []ReadiedTask{{
				TaskID: "1:1:t", Action: "review",
				Assignees: []string{"alice", "bob", "carol"},
				RunID:     7, ProjectID: 3,
			}},
			wantCount: 3,
			assertions: map[int]map[string]string{
				0: {"assign_to": `"assign_to":"alice"`},
				1: {"assign_to": `"assign_to":"bob"`},
				2: {"assign_to": `"assign_to":"carol"`},
			},
		},
		{
			name: "parents present → embedded in each assignee event",
			readied: []ReadiedTask{{
				TaskID: "1:1:downstream", Action: "answer",
				Assignees: []string{"alice"},
				RunID:     7, ProjectID: 3,
				Parents: []ReadiedParent{
					{TaskID: "1:1:up", Action: "answer", CommitSHA: "abc123", ResultDir: "enju/runs/1-r/answer/up"},
				},
			}},
			wantCount: 1,
			assertions: map[int]map[string]string{
				0: {
					"parents-key":     `"parents":`,
					"parent-task":     `"task_id":"1:1:up"`,
					"parent-commit":   `"commit_sha":"abc123"`,
					"parent-resultd":  `"result_dir":"enju/runs/1-r/answer/up"`,
				},
			},
		},
		{
			name: "skipped parent → empty commit_sha preserved (not omitted)",
			readied: []ReadiedTask{{
				TaskID: "1:1:downstream", Action: "answer",
				RunID: 7, ProjectID: 3,
				Parents: []ReadiedParent{
					{TaskID: "1:1:skipped", Action: "answer", CommitSHA: ""},
				},
			}},
			wantCount: 1,
			assertions: map[int]map[string]string{
				0: {"empty-commit": `"commit_sha":""`},
			},
		},
		{
			name: "multiple readied tasks fan out independently",
			readied: []ReadiedTask{
				{TaskID: "a", Action: "answer", Assignees: []string{"alice"}, RunID: 1, ProjectID: 1},
				{TaskID: "b", Action: "review", Assignees: []string{"bob", "carol"}, RunID: 1, ProjectID: 1},
			},
			wantCount: 3, // 1 + 2
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildTaskReadyEvents(tc.readied, now)
			if len(got) != tc.wantCount {
				t.Fatalf("event count: got %d, want %d", len(got), tc.wantCount)
			}
			for i, evt := range got {
				if evt.EventType != "task_ready" {
					t.Errorf("event[%d].EventType = %q, want task_ready", i, evt.EventType)
				}
				if !evt.CreatedAt.Equal(now) {
					t.Errorf("event[%d].CreatedAt = %v, want %v", i, evt.CreatedAt, now)
				}
				if checks, ok := tc.assertions[i]; ok {
					meta := string(evt.Metadata)
					for label, want := range checks {
						if !strings.Contains(meta, want) {
							t.Errorf("event[%d] %s: metadata missing %q\nfull: %s", i, label, want, meta)
						}
					}
				}
			}
		})
	}
}
