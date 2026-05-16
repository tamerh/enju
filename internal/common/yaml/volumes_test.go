package yaml

import (
	"strings"
	"testing"
)

// TestParseVolumesOnComputeContainerAccepted — the canonical
// happy path. volumes: on a containerized compute task survives
// parse intact, including the bare-host, host:container, and
// host:container:mode forms.
func TestParseVolumesOnComputeContainerAccepted(t *testing.T) {
	yaml := `
name: "volumes accepted"
version: 1
tasks:
  - id: nanoplot
    action: compute
    script: scripts/run.sh
    container: staphb/nanoplot:latest
    volumes:
      - /data/refs
      - /data/raw:/inputs
      - /data/db:/db:ro
    prompt: "Run"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
	got := parsed.Run.Tasks[0].Volumes
	want := []string{"/data/refs", "/data/raw:/inputs", "/data/db:/db:ro"}
	if len(got) != len(want) {
		t.Fatalf("volumes count: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("volumes[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseVolumesWithoutContainerRejected — a bare-host script
// already sees the host filesystem, so volumes: without a
// container: image is meaningless and almost certainly an
// author mistake. Mirrors the container_runtime-without-
// container guard.
func TestParseVolumesWithoutContainerRejected(t *testing.T) {
	yaml := `
name: "volumes without container"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    volumes:
      - /data/refs
    prompt: "x"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "without container:") {
		t.Errorf("error should explain the missing container:, got %q", err)
	}
}

// TestParseVolumesOnNonComputeRejected — volumes: is anchored
// to action: compute like container:/env:/executor:.
func TestParseVolumesOnNonComputeRejected(t *testing.T) {
	for _, action := range []string{"answer", "contribute", "review", "vote"} {
		body := `
name: "volumes on wrong action"
version: 1
tasks:
  - id: t
    action: ` + action + `
    volumes:
      - /data/refs
    prompt: "x"
`
		if action == "review" {
			body += "    reviews: upstream\n"
		}
		if action == "vote" {
			body += "    options:\n      - id: yes\n      - id: no\n"
		}
		_, err := Parse([]byte(body))
		if err == nil {
			t.Errorf("action=%s: expected parse error, got nil", action)
			continue
		}
		if !strings.Contains(err.Error(), "volumes:") {
			t.Errorf("action=%s: error should cite volumes: field, got %q", action, err)
		}
	}
}

// TestParseVolumesWhitespaceRejected — a stray space almost
// always means the author fat-fingered a docker invocation;
// surface it rather than hand a malformed bind to the runtime.
func TestParseVolumesWhitespaceRejected(t *testing.T) {
	yaml := `
name: "volumes whitespace"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    container: alpine:3.19
    volumes:
      - "/data/a -v /data/b"
    prompt: "x"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("error should cite whitespace, got %q", err)
	}
}

// TestParseVolumesInvalidModeRejected — a literal third segment
// must be ro or rw. (Unresolved {{param}} modes are tolerated;
// that's covered by the param-resolution test below.)
func TestParseVolumesInvalidModeRejected(t *testing.T) {
	yaml := `
name: "volumes bad mode"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    container: alpine:3.19
    volumes:
      - /data/db:/db:readonly
    prompt: "x"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid mode") {
		t.Errorf("error should cite invalid mode, got %q", err)
	}
}

// TestParseVolumesTooManySegmentsRejected — a fourth ':'
// segment is not a recognized form.
func TestParseVolumesTooManySegmentsRejected(t *testing.T) {
	yaml := `
name: "volumes too many segments"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    container: alpine:3.19
    volumes:
      - /a:/b:ro:extra
    prompt: "x"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "too many") {
		t.Errorf("error should cite too many segments, got %q", err)
	}
}

// TestParseVolumesOmittedNoValidation — no volumes: block means
// the validator never runs (an unrelated compute task without a
// container must still parse cleanly).
func TestParseVolumesOmittedNoValidation(t *testing.T) {
	yaml := `
name: "no volumes"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    prompt: "x"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
	if len(parsed.Run.Tasks[0].Volumes) != 0 {
		t.Errorf("expected no volumes, got %v", parsed.Run.Tasks[0].Volumes)
	}
}

// TestParseWithParamsResolvesVolumes — the core ask of ISSUE-4.
// Volume specs carry {{param}} refs so a workflow author can
// declare exactly which external paths a task needs without
// hardcoding machine-specific locations. Validation runs before
// substitution, so the raw {{...}} tokens must pass the shape
// check; ParseWithParams then resolves them like every other
// param-bearing field.
func TestParseWithParamsResolvesVolumes(t *testing.T) {
	yaml := []byte(`
name: "volumes param resolution"
version: 1
params:
  - name: raw_data_root
    type: string
    required: true
    description: "Host path to raw reads"
  - name: checkv_db
    type: string
    required: true
    description: "Host path to the CheckV database"
tasks:
  - id: nanoplot_raw
    action: compute
    script: scripts/nanoplot.sh
    container: staphb/nanoplot:latest
    volumes:
      - "{{raw_data_root}}:{{raw_data_root}}"
      - "{{checkv_db}}:{{checkv_db}}:ro"
    prompt: "Run NanoPlot"
`)
	parsed, err := ParseWithParams(yaml, map[string]interface{}{
		"raw_data_root": "/data/raw-data/phage_demo",
		"checkv_db":     "/data/databases/checkv",
	})
	if err != nil {
		t.Fatalf("ParseWithParams failed: %v", err)
	}
	got := parsed.Run.Tasks[0].Volumes
	want := []string{
		"/data/raw-data/phage_demo:/data/raw-data/phage_demo",
		"/data/databases/checkv:/data/databases/checkv:ro",
	}
	if len(got) != len(want) {
		t.Fatalf("volumes: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("volumes[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
	for _, v := range got {
		if strings.Contains(v, "{{") {
			t.Errorf("unresolved param ref left in volume %q", v)
		}
	}
}
