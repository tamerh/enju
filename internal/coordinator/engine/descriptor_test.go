package engine

import (
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

func TestBuildInputsDescriptorBasic(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"1:1:step1": {
				ID: "1:1:step1", TaskDefID: "step1",
				State: store.TaskAccepted, CommitSHA: "abc123",
				ResultPath: "projects/1/runs/1/step1",
			},
		},
		projects: map[int64]*store.ProjectRecord{
			1: {ID: 1, RemoteURL: "git@example.com:repo.git"},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID:        "1:1:step2",
		TaskDefID: "step2",
		Prompt:    "Use {{step1.content}}",
		DependsOn: "1:1:step1",
	}
	run := &store.RunRecord{ProjectID: 1, Seq: 1}

	desc, err := e.BuildInputsDescriptor(task, run)
	if err != nil {
		t.Fatal(err)
	}
	if desc.ProjectRemoteURL != "git@example.com:repo.git" {
		t.Errorf("remote URL: %q", desc.ProjectRemoteURL)
	}
	if len(desc.Dependencies) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(desc.Dependencies))
	}
	dep := desc.Dependencies[0]
	if dep["commit_sha"] != "abc123" {
		t.Errorf("commit_sha: %v", dep["commit_sha"])
	}
}

func TestBuildInputsDescriptorAnonymized(t *testing.T) {
	ms := &mockStore{
		tasks: map[string]*store.TaskRecord{
			"1:1:vote": {
				ID: "1:1:vote", TaskDefID: "vote",
				State: store.TaskAccepted, Citizens: 3,
				Anonymize: true,
			},
		},
		submissions: map[string][]store.TaskClaimRecord{
			"1:1:vote": {
				{CitizenID: 10, Option: "approve", CommitSHA: "sha10"},
				{CitizenID: 20, Option: "reject", CommitSHA: "sha20"},
			},
		},
		citizens: map[int64]*store.CitizenRecord{
			10: {Username: "alice"},
			20: {Username: "bob"},
		},
		projects: map[int64]*store.ProjectRecord{
			1: {ID: 1},
		},
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID:        "1:1:synth",
		DependsOn: "1:1:vote",
	}
	run := &store.RunRecord{ProjectID: 1, Seq: 1}

	desc, err := e.BuildInputsDescriptor(task, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Dependencies) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(desc.Dependencies))
	}
	responses, ok := desc.Dependencies[0]["responses"].([]map[string]interface{})
	if !ok || len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %v", desc.Dependencies[0]["responses"])
	}
	// Anonymized: should be citizen-1, citizen-2, not alice, bob.
	if responses[0]["username"] != "citizen-1" {
		t.Errorf("expected citizen-1, got %v", responses[0]["username"])
	}
	if responses[1]["username"] != "citizen-2" {
		t.Errorf("expected citizen-2, got %v", responses[1]["username"])
	}
}

func TestBuildInputsDescriptorMissingArtifact(t *testing.T) {
	ms := &mockStore{
		projects: map[int64]*store.ProjectRecord{1: {ID: 1}},
		// No artifacts configured — simulates a deleted index row.
	}
	e := New(ms, nil)
	task := &store.TaskRecord{
		ID:             "1:1:reader",
		ReadsArtifacts: `["notes/intro.md"]`,
	}
	run := &store.RunRecord{ProjectID: 1, Seq: 1}

	desc, err := e.BuildInputsDescriptor(task, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.ArtifactReads) != 1 {
		t.Fatalf("expected 1 artifact read, got %d", len(desc.ArtifactReads))
	}
	// Missing artifact should have empty commit_sha.
	if desc.ArtifactReads[0]["commit_sha"] != "" {
		t.Errorf("expected empty commit_sha for missing artifact, got %v", desc.ArtifactReads[0]["commit_sha"])
	}
}
