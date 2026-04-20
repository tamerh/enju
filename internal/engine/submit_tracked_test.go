package engine

import (
	"testing"

	"github.com/enju-ai/enju/internal/store"
)

// TestComputePostSubmitActionsTrackedRouting confirms that
// ComputePostSubmitActions produces artifact mutations with the
// Tracked flag sourced from the task's declared writes_artifacts
// and an empty CommitSHA for untracked entries — regardless of
// what the client sent in req.CommitSHA.
func TestComputePostSubmitActionsTrackedRouting(t *testing.T) {
	task := &store.TaskRecord{
		ID:              "proj:1:align",
		RunID:           10,
		Action:          "compute",
		Citizens:        1,
		ClaimedBy:       42,
		WritesArtifacts: `[{"path":"out/stats.json","track":true},{"path":"out/aligned.bam","track":false}]`,
	}
	run := &store.RunRecord{
		ID:        10,
		ProjectID: 7,
		Branch:    "main",
	}
	req := &SubmitRequest{
		ArtifactsWritten: []string{"out/stats.json", "out/aligned.bam"},
		CommitSHA:        "deadbeef",
	}
	outcome := &SubmissionOutcome{Resolved: true}

	e := New(&mockStore{}, nil)
	actions, err := e.ComputePostSubmitActions(task, run, outcome, req, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions.ArtifactMutations) != 2 {
		t.Fatalf("expected 2 mutations, got %d", len(actions.ArtifactMutations))
	}
	byPath := map[string]store.ArtifactRecord{}
	for _, m := range actions.ArtifactMutations {
		ma, ok := m.(store.MoveArtifact)
		if !ok {
			t.Fatalf("expected MoveArtifact mutation, got %T", m)
		}
		byPath[ma.Artifact.Path] = ma.Artifact
	}
	tracked := byPath["out/stats.json"]
	if !tracked.Tracked {
		t.Errorf("stats.json should be tracked, got %+v", tracked)
	}
	if tracked.CommitSHA != "deadbeef" {
		t.Errorf("tracked artifact must carry client-sent commit SHA, got %q", tracked.CommitSHA)
	}

	untracked := byPath["out/aligned.bam"]
	if untracked.Tracked {
		t.Errorf("aligned.bam should be untracked, got %+v", untracked)
	}
	// Even though the client sent a commit SHA, an untracked
	// artifact must ship with commit_sha="" so the index
	// semantics stay meaningful (commit_sha is a pointer at
	// committed content; untracked content has none).
	if untracked.CommitSHA != "" {
		t.Errorf("untracked artifact must NOT carry a commit SHA, got %q", untracked.CommitSHA)
	}
}

// TestComputePostSubmitActionsLegacyWritesArtifactsAllTracked
// exercises the legacy-JSON read path: pre-untracked DB rows
// stored writes_artifacts as a bare string array. Every
// resulting mutation must land as Tracked=true so old runs
// continue working after the migration.
func TestComputePostSubmitActionsLegacyWritesArtifactsAllTracked(t *testing.T) {
	task := &store.TaskRecord{
		ID:              "proj:1:old",
		RunID:           10,
		Action:          "compute",
		Citizens:        1,
		ClaimedBy:       42,
		WritesArtifacts: `["out/a.md","out/b.md"]`,
	}
	run := &store.RunRecord{ID: 10, ProjectID: 7, Branch: "main"}
	req := &SubmitRequest{
		ArtifactsWritten: []string{"out/a.md", "out/b.md"},
		CommitSHA:        "feedface",
	}
	e := New(&mockStore{}, nil)
	actions, err := e.ComputePostSubmitActions(task, run, &SubmissionOutcome{Resolved: true}, req, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range actions.ArtifactMutations {
		ma := m.(store.MoveArtifact)
		if !ma.Artifact.Tracked {
			t.Errorf("legacy JSON must decode as tracked, got %+v", ma.Artifact)
		}
		if ma.Artifact.CommitSHA != "feedface" {
			t.Errorf("legacy tracked artifact must carry commit SHA, got %q", ma.Artifact.CommitSHA)
		}
	}
}

// TestComputePostSubmitActionsUndeclaredPathDefaultsTracked —
// an artifact path that somehow reached ArtifactsWritten without
// a matching declaration (shouldn't happen post-validation)
// lands as tracked so the behavior stays forward-compatible.
func TestComputePostSubmitActionsUndeclaredPathDefaultsTracked(t *testing.T) {
	task := &store.TaskRecord{
		ID:              "proj:1:x",
		RunID:           10,
		Citizens:        1,
		WritesArtifacts: `[{"path":"out/declared.md","track":false}]`,
	}
	run := &store.RunRecord{ID: 10, ProjectID: 7, Branch: "main"}
	req := &SubmitRequest{
		ArtifactsWritten: []string{"out/undeclared.md"},
		CommitSHA:        "abc",
	}
	e := New(&mockStore{}, nil)
	actions, err := e.ComputePostSubmitActions(task, run, &SubmissionOutcome{Resolved: true}, req, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions.ArtifactMutations) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(actions.ArtifactMutations))
	}
	ma := actions.ArtifactMutations[0].(store.MoveArtifact)
	if !ma.Artifact.Tracked {
		t.Errorf("undeclared path should default to Tracked=true, got %+v", ma.Artifact)
	}
}
