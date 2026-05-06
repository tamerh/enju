package compute_test

import (
	"encoding/json"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/compute"
)

// TestSpecWritesArtifactsJSONRoundTrip pins the wire format
// for the detached async-wrapper subprocess: the handler
// serializes Spec to a temp file, the wrapper reads it back,
// and every WriteArtifact field (Path, Track, Optional) must
// survive the round trip. The wrapper drives both the commit
// step (Track=true entries) and the .gitignore + index-row
// management (Track=false entries) off this single field.
func TestSpecWritesArtifactsJSONRoundTrip(t *testing.T) {
	orig := compute.Spec{
		TaskID: "p:1:t",
		WritesArtifacts: enjuYaml.WriteArtifacts{
			{Path: "out/summary.json", Track: true},
			{Path: "out/big.bam", Track: false},
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back compute.Spec
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.WritesArtifacts) != 2 {
		t.Fatalf("declarations lost: %+v", back.WritesArtifacts)
	}
	tracked := back.WritesArtifacts.TrackedPaths()
	untracked := back.WritesArtifacts.UntrackedPaths()
	if len(tracked) != 1 || tracked[0] != "out/summary.json" {
		t.Errorf("tracked: got %v, want [out/summary.json]", tracked)
	}
	if len(untracked) != 1 || untracked[0] != "out/big.bam" {
		t.Errorf("untracked: got %v, want [out/big.bam]", untracked)
	}
}

// TestSpecBareStringListDecodesAsAllTracked pins the
// dual-shape JSON tolerance: writes_artifacts may arrive as
// a bare []string (every entry implicitly Track=true) or as
// the typed object form. The bare shape is what the
// coordinator's writes_artifacts column held before the
// track flag landed; rows from that era must still parse so
// wrappers driven from old DB rows produce correct commits.
func TestSpecBareStringListDecodesAsAllTracked(t *testing.T) {
	body := []byte(`{"task_id":"p:1:t","writes_artifacts":["out/x.md","out/y.md"]}`)
	var spec compute.Spec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(spec.WritesArtifacts) != 2 {
		t.Fatalf("entries lost: %+v", spec.WritesArtifacts)
	}
	tracked := spec.WritesArtifacts.TrackedPaths()
	if len(tracked) != 2 || tracked[0] != "out/x.md" || tracked[1] != "out/y.md" {
		t.Errorf("bare []string should default Track=true: %v", tracked)
	}
}

// TestSpecUnknownFieldsSilentlyIgnored — Spec holds only the
// fields the wrapper consumes. A JSON document with extra
// keys (e.g., `untracked_artifacts` from an unrelated codebase
// shape) decodes the recognized fields and drops the rest.
// Keeping this test pins encoding/json's default behavior so
// any future switch to DisallowUnknownFields would surface
// here loudly rather than as a mysterious wrapper failure.
func TestSpecUnknownFieldsSilentlyIgnored(t *testing.T) {
	body := []byte(`{"task_id":"p:1:t","writes_artifacts":["out/x.md"],"untracked_artifacts":["out/big.bam"]}`)
	var spec compute.Spec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(spec.WritesArtifacts) != 1 || spec.WritesArtifacts[0].Path != "out/x.md" {
		t.Errorf("writes_artifacts didn't decode: %+v", spec.WritesArtifacts)
	}
}
