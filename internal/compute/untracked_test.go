package compute_test

import (
	"encoding/json"
	"testing"

	"github.com/enju-ai/enju/internal/compute"
)

// TestSpecUntrackedArtifactsJSONRoundTrip confirms the Spec wire
// format carries UntrackedArtifacts alongside WritesArtifacts so
// the detached async-wrapper subprocess picks up the track=false
// paths from the spec file the handler dropped before exec.
func TestSpecUntrackedArtifactsJSONRoundTrip(t *testing.T) {
	orig := compute.Spec{
		TaskID:             "p:1:t",
		WritesArtifacts:    []string{"out/summary.json"},
		UntrackedArtifacts: []string{"out/big.bam"},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back compute.Spec
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.WritesArtifacts) != 1 || back.WritesArtifacts[0] != "out/summary.json" {
		t.Errorf("tracked paths lost: %+v", back.WritesArtifacts)
	}
	if len(back.UntrackedArtifacts) != 1 || back.UntrackedArtifacts[0] != "out/big.bam" {
		t.Errorf("untracked paths lost: %+v", back.UntrackedArtifacts)
	}
}

// TestSpecLegacyWithoutUntrackedArtifacts — a spec file written by
// a pre-untracked enju version has no untracked_artifacts key. The
// decoded spec must tolerate the absence (empty slice, not a
// parse error), so in-flight async wrappers launched by older
// binaries still run to completion.
func TestSpecLegacyWithoutUntrackedArtifacts(t *testing.T) {
	legacy := []byte(`{"task_id":"p:1:t","writes_artifacts":["out/x.md"]}`)
	var spec compute.Spec
	if err := json.Unmarshal(legacy, &spec); err != nil {
		t.Fatalf("legacy spec failed to decode: %v", err)
	}
	if len(spec.UntrackedArtifacts) != 0 {
		t.Errorf("legacy spec should have no untracked entries, got %+v", spec.UntrackedArtifacts)
	}
	if len(spec.WritesArtifacts) != 1 || spec.WritesArtifacts[0] != "out/x.md" {
		t.Errorf("legacy tracked path lost: %+v", spec.WritesArtifacts)
	}
}
