package yaml

import (
	"strings"
	"testing"
)

// TestParseContainerOnComputeAccepted — canonical happy path.
// An image reference on a compute task survives parse; the
// validator doesn't second-guess the grammar (Docker's CLI
// arbitrates at pull time).
func TestParseContainerOnComputeAccepted(t *testing.T) {
	cases := []string{
		"biocontainers/samtools:1.18",
		"ghcr.io/org/tool:v1.2.3",
		"registry.example.com:5000/org/img:tag",
		"alpine",
		"alpine:3.19",
		"image@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd",
	}
	for _, ref := range cases {
		yaml := `
name: "container happy path"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    container: ` + ref + `
    prompt: "Run it"
`
		parsed, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatalf("unexpected parse failure for %q: %v", ref, err)
		}
		if got := parsed.Run.Tasks[0].Container; got != ref {
			t.Errorf("container not preserved: got %q want %q", got, ref)
		}
	}
}

// TestParseContainerOnNonComputeRejected — container:
// on any non-compute action is a parse-time error. The
// concept doesn't apply (answer / review / vote have no
// script to run inside anything).
func TestParseContainerOnNonComputeRejected(t *testing.T) {
	for _, action := range []string{"answer", "contribute", "review", "vote"} {
		body := `
name: "container on wrong action"
version: 1
tasks:
  - id: t
    action: ` + action + `
    container: alpine:3.19
    prompt: "x"
`
		// review + vote need extra fields to be structurally
		// valid; stub them.
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
		if !strings.Contains(err.Error(), "container:") {
			t.Errorf("action=%s: error should cite container: field, got %q", action, err)
		}
	}
}

// TestParseContainerWithWhitespaceRejected — a template-author
// mistake (e.g. copied a shell invocation) must surface loudly
// rather than get passed to docker as a weird image ref.
func TestParseContainerWithWhitespaceRejected(t *testing.T) {
	bad := []string{
		"alpine 3.19",
		"alpine\t3.19",
		"\nimage:tag",
		"image:tag ",
	}
	for _, ref := range bad {
		yaml := `
name: "container whitespace"
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    container: "` + ref + `"
    prompt: "x"
`
		_, err := Parse([]byte(yaml))
		if err == nil {
			t.Errorf("whitespace %q: expected parse error, got nil", ref)
			continue
		}
		if !strings.Contains(err.Error(), "whitespace") {
			t.Errorf("whitespace %q: error should mention whitespace, got %q", ref, err)
		}
	}
}

// TestParseContainerOmittedNoValidation — empty container:
// is the default (run on host); not an error.
func TestParseContainerOmittedNoValidation(t *testing.T) {
	yaml := `
name: "container omitted"
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
	if got := parsed.Run.Tasks[0].Container; got != "" {
		t.Errorf("expected empty container default, got %q", got)
	}
}

// TestParseContainerSurvivesForEachInstanceExpansion — like
// script: and env:, the container reference is literal (not
// a template string). It copies unchanged onto every for_each
// instance.
func TestParseContainerSurvivesForEachInstanceExpansion(t *testing.T) {
	yaml := `
name: "container with for_each"
version: 1
tasks:
  - id: analyze
    for_each:
      sample: [alpha, beta]
    action: compute
    script: scripts/run.sh
    container: biocontainers/samtools:1.18
    prompt: "Analyze {{sample}}"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// ExpandedTasks is keyed by for_each instance key
	// ("alpha", "beta"). Walk the whole map and count the
	// `analyze` instances; each must carry the container
	// reference unchanged.
	var count int
	for _, instances := range parsed.ExpandedTasks {
		for _, ti := range instances {
			if ti.ID != "analyze" {
				continue
			}
			count++
			if ti.Container != "biocontainers/samtools:1.18" {
				t.Errorf("instance %s: container not threaded, got %q", ti.InstanceKey, ti.Container)
			}
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 analyze instances, got %d", count)
	}
}
