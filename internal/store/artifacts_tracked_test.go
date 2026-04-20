package store

import (
	"testing"
	"time"
)

// TestApplyMoveArtifactTrackedRoundtrip writes both a tracked and
// an untracked artifact via the plan/apply path and confirms the
// Tracked flag + commit_sha round-trip correctly through GetArtifact
// and ListArtifactsByProject.
func TestApplyMoveArtifactTrackedRoundtrip(t *testing.T) {
	s := newTestStore(t)
	projectID := createTestProject(t, s)

	now := time.Now().UTC().Truncate(time.Second)

	plan := Plan{
		Mutations: []Mutation{
			MoveArtifact{Artifact: ArtifactRecord{
				ProjectID: projectID,
				Branch:    "main",
				Path:      "out/stats.json",
				CommitSHA: "abc123",
				Tracked:   true,
				CreatedAt: now, UpdatedAt: now,
			}},
			MoveArtifact{Artifact: ArtifactRecord{
				ProjectID: projectID,
				Branch:    "main",
				Path:      "out/aligned.bam",
				CommitSHA: "", // untracked — must stay empty
				Tracked:   false,
				CreatedAt: now, UpdatedAt: now,
			}},
		},
	}
	if _, err := s.ApplyPlan(plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	tracked, err := s.GetArtifact(projectID, "main", "out/stats.json")
	if err != nil {
		t.Fatal(err)
	}
	if tracked == nil || !tracked.Tracked || tracked.CommitSHA != "abc123" {
		t.Fatalf("tracked artifact wrong: %+v", tracked)
	}

	untracked, err := s.GetArtifact(projectID, "main", "out/aligned.bam")
	if err != nil {
		t.Fatal(err)
	}
	if untracked == nil || untracked.Tracked {
		t.Fatalf("expected Tracked=false, got %+v", untracked)
	}
	if untracked.CommitSHA != "" {
		t.Fatalf("untracked artifact must have empty commit_sha, got %q", untracked.CommitSHA)
	}

	rows, err := s.ListArtifactsByProject(projectID, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	byPath := map[string]ArtifactRecord{}
	for _, r := range rows {
		byPath[r.Path] = r
	}
	if !byPath["out/stats.json"].Tracked || byPath["out/aligned.bam"].Tracked {
		t.Fatalf("list returned wrong tracked flags: %+v", rows)
	}
}

// TestApplyMoveArtifactFlipsTrackedInPlace covers the ON CONFLICT
// upsert: re-running a task that flipped track:true → false (or
// vice versa) must overwrite the existing row, not accumulate a
// second one.
func TestApplyMoveArtifactFlipsTrackedInPlace(t *testing.T) {
	s := newTestStore(t)
	projectID := createTestProject(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	plan1 := Plan{Mutations: []Mutation{
		MoveArtifact{Artifact: ArtifactRecord{
			ProjectID: projectID, Branch: "main", Path: "out/x",
			CommitSHA: "initial-sha", Tracked: true,
			CreatedAt: now, UpdatedAt: now,
		}},
	}}
	if _, err := s.ApplyPlan(plan1); err != nil {
		t.Fatal(err)
	}

	later := now.Add(time.Second)
	plan2 := Plan{Mutations: []Mutation{
		MoveArtifact{Artifact: ArtifactRecord{
			ProjectID: projectID, Branch: "main", Path: "out/x",
			CommitSHA: "", Tracked: false,
			CreatedAt: later, UpdatedAt: later,
		}},
	}}
	if _, err := s.ApplyPlan(plan2); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetArtifact(projectID, "main", "out/x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tracked {
		t.Fatalf("expected Tracked=false after flip, got %+v", got)
	}
	if got.CommitSHA != "" {
		t.Fatalf("expected commit_sha cleared on flip, got %q", got.CommitSHA)
	}

	rows, _ := s.ListArtifactsByProject(projectID, "main", "")
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (upsert in place), got %d", len(rows))
	}
}

// TestLegacyArtifactsDefaultTracked — the ALTER TABLE migration
// adds tracked with DEFAULT 1, so rows written before the column
// existed should come back as Tracked=true. Simulate that by
// manually inserting a row without the tracked column set.
func TestLegacyArtifactsDefaultTracked(t *testing.T) {
	s := newTestStore(t)
	projectID := createTestProject(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	// Insert without specifying tracked — DEFAULT 1 kicks in.
	_, err := s.db.Exec(
		`INSERT INTO artifacts (project_id, branch, path, last_writer, last_task_id, last_run_id, commit_sha, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, "main", "legacy/artifact.md", 0, "task-1", 0, "sha-legacy", now, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetArtifact(projectID, "main", "legacy/artifact.md")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Tracked {
		t.Fatalf("legacy row should default Tracked=true, got %+v", got)
	}
}
