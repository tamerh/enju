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

// TestParseContainerRuntimeDockerAccepted — explicit
// container_runtime: docker round-trips unchanged.
func TestParseContainerRuntimeDockerAccepted(t *testing.T) {
	yaml := `
name: "runtime docker accepted"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    container: alpine:3.19
    container_runtime: docker
    prompt: "Run"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
	if got := parsed.Run.Tasks[0].ContainerRuntime; got != "docker" {
		t.Errorf("container_runtime not preserved: got %q", got)
	}
}

// TestParseContainerRuntimeEmptyAccepted — omitted field is
// the common case; must parse cleanly.
func TestParseContainerRuntimeEmptyAccepted(t *testing.T) {
	yaml := `
name: "runtime omitted"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    container: alpine:3.19
    prompt: "Run"
`
	if _, err := Parse([]byte(yaml)); err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
}

// TestParseContainerRuntimeApptainerAccepted — apptainer is
// the second runtime alongside docker. Value round-trips
// unchanged onto the parsed TaskDef.
func TestParseContainerRuntimeApptainerAccepted(t *testing.T) {
	yaml := `
name: "runtime apptainer"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    container: docker://alpine:latest
    container_runtime: apptainer
    prompt: "Run"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
	if got := parsed.Run.Tasks[0].ContainerRuntime; got != "apptainer" {
		t.Errorf("container_runtime not preserved: got %q", got)
	}
}

// TestParseContainerRuntimeSingularityNormalizedToApptainer
// — singularity is a YAML-side alias only. Internal storage
// (and logs, error messages, etc.) collapses to "apptainer"
// at parse time so downstream code has one runtime name to
// reason about.
func TestParseContainerRuntimeSingularityNormalizedToApptainer(t *testing.T) {
	yaml := `
name: "runtime singularity alias"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    container: docker://alpine:latest
    container_runtime: singularity
    prompt: "Run"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
	if got := parsed.Run.Tasks[0].ContainerRuntime; got != "apptainer" {
		t.Errorf("singularity should be rewritten to apptainer, got %q", got)
	}
}

// TestParseContainerRuntimeUnknownRejected — anything outside
// the {docker, apptainer, singularity} set is a parse error
// that names the value and lists the valid options. Don't
// silently fall back to docker.
func TestParseContainerRuntimeUnknownRejected(t *testing.T) {
	for _, rt := range []string{"podman", "nerdctl", "rkt", "containerd"} {
		yaml := `
name: "runtime unsupported"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    container: alpine:3.19
    container_runtime: ` + rt + `
    prompt: "Run"
`
		_, err := Parse([]byte(yaml))
		if err == nil {
			t.Errorf("container_runtime=%q: expected parse error, got nil", rt)
			continue
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Errorf("container_runtime=%q: error should say 'not supported', got: %v", rt, err)
		}
		if !strings.Contains(err.Error(), rt) {
			t.Errorf("container_runtime=%q: error should name the value, got: %v", rt, err)
		}
	}
}

// TestParseContainerRuntimeWithoutContainerRejected — a
// runtime selector with no image to run is almost always a
// template-author mistake (image removed, runtime left
// behind; or runtime declared before the image was decided).
// Surface at parse time rather than letting the wrapper hit
// a more confusing failure later.
func TestParseContainerRuntimeWithoutContainerRejected(t *testing.T) {
	for _, rt := range []string{"docker", "apptainer", "singularity"} {
		yaml := `
name: "runtime without image"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    container_runtime: ` + rt + `
    prompt: "Run"
`
		_, err := Parse([]byte(yaml))
		if err == nil {
			t.Errorf("container_runtime=%q without container: expected parse error, got nil", rt)
			continue
		}
		if !strings.Contains(err.Error(), "without container") {
			t.Errorf("container_runtime=%q: error should mention missing container:, got: %v", rt, err)
		}
	}
}

// TestParseContainerRuntimeOnNonComputeRejected — field is
// only meaningful on compute tasks; declaring it elsewhere
// is a template-author mistake worth catching.
func TestParseContainerRuntimeOnNonComputeRejected(t *testing.T) {
	yaml := `
name: "runtime on non-compute"
version: 1
tasks:
  - id: run
    action: answer
    container_runtime: docker
    prompt: "Run"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error for container_runtime on answer task")
	}
	if !strings.Contains(err.Error(), "compute") {
		t.Errorf("error should mention action:compute, got: %v", err)
	}
}

// TestParseExecutorLocalAccepted — reserved executor field
// accepts "local" (current behavior) + empty.
func TestParseExecutorLocalAccepted(t *testing.T) {
	yaml := `
name: "executor local"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    executor: local
    prompt: "Run"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
	if got := parsed.Run.Tasks[0].Executor; got != "local" {
		t.Errorf("executor not preserved: got %q", got)
	}
}

// TestParseExecutorRemoteRejected — the still-roadmap
// executors (k8s, aws-batch, gcp-batch) get a "not yet
// supported" rejection that names the value and points at
// the roadmap. slurm is NO LONGER here: it ships in this
// change and has its own positive test below.
func TestParseExecutorRemoteRejected(t *testing.T) {
	for _, exec := range []string{"k8s", "kubernetes", "aws-batch", "gcp-batch"} {
		yaml := `
name: "executor unsupported"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    executor: ` + exec + `
    prompt: "Run"
`
		_, err := Parse([]byte(yaml))
		if err == nil {
			t.Errorf("executor=%q: expected parse error, got nil", exec)
			continue
		}
		if !strings.Contains(err.Error(), "not yet supported") {
			t.Errorf("executor=%q: error should say 'not yet supported', got: %v", exec, err)
		}
		if !strings.Contains(err.Error(), exec) {
			t.Errorf("executor=%q: error should name the value, got: %v", exec, err)
		}
	}
}

// TestParseExecutorSlurmAccepted — slurm now parses (the
// executor-abstraction change). resources: is optional;
// when present its shape is validated separately.
func TestParseExecutorSlurmAccepted(t *testing.T) {
	yaml := `
name: "executor slurm"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.sh
    executor: slurm
    resources:
      partition: gpu
      time: "02:00:00"
      cpus: 8
      mem: 32G
      gpus: 1
      sbatch_extra:
        - "--account=lab123"
    prompt: "Run"
`
	parsed, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected parse failure: %v", err)
	}
	tk := parsed.Run.Tasks[0]
	if tk.Executor != "slurm" {
		t.Errorf("executor not preserved: got %q", tk.Executor)
	}
	if tk.Resources.Partition != "gpu" || tk.Resources.CPUs != 8 ||
		tk.Resources.Mem != "32G" || tk.Resources.GPUs != 1 ||
		len(tk.Resources.SbatchExtra) != 1 {
		t.Errorf("resources not parsed as expected: %+v", tk.Resources)
	}
}

// TestParseExecutorOnNonComputeRejected — same shape as
// the container_runtime guard.
func TestParseExecutorOnNonComputeRejected(t *testing.T) {
	yaml := `
name: "executor on non-compute"
version: 1
tasks:
  - id: run
    action: answer
    executor: local
    prompt: "Run"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error for executor on answer task")
	}
	if !strings.Contains(err.Error(), "compute") {
		t.Errorf("error should mention action:compute, got: %v", err)
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

// TestValidateTaskResourcesRejections pins all four guard
// branches of validateTaskResources. The guard's whole point is
// catching the author who flips executor: slurm→local (or off)
// and leaves a resources: block, or types a nonsensical knob —
// so every rejection path gets a case, not just the happy path.
//
// Note the reachability subtlety: validateTaskExecutor runs
// before validateTaskResources in the loop, so the "resources
// on a non-compute task" branch is only reachable when NO
// executor is set (executor: slurm on a non-compute task is
// rejected earlier by validateTaskExecutor). The cases below
// are constructed to actually exercise each branch.
func TestValidateTaskResourcesRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "resources with executor local",
			body: `
name: r
version: 1
tasks:
  - id: t
    action: compute
    script: s.sh
    executor: local
    resources: { partition: gpu }
    prompt: x
`,
			want: "resources: is set but executor is",
		},
		{
			name: "resources with no executor (defaults local)",
			body: `
name: r
version: 1
tasks:
  - id: t
    action: compute
    script: s.sh
    resources: { cpus: 4 }
    prompt: x
`,
			want: "resources: is set but executor is",
		},
		{
			name: "resources on non-compute task",
			body: `
name: r
version: 1
tasks:
  - id: t
    action: answer
    resources: { partition: gpu }
    prompt: x
`,
			want: "resources: is only valid on action: compute",
		},
		{
			name: "negative cpus",
			body: `
name: r
version: 1
tasks:
  - id: t
    action: compute
    script: s.sh
    executor: slurm
    resources: { cpus: -1 }
    prompt: x
`,
			want: "resources.cpus -1 is negative",
		},
		{
			name: "negative gpus",
			body: `
name: r
version: 1
tasks:
  - id: t
    action: compute
    script: s.sh
    executor: slurm
    resources: { gpus: -2 }
    prompt: x
`,
			want: "resources.gpus -2 is negative",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.body))
			if err == nil {
				t.Fatalf("expected parse error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should contain %q, got: %v", c.want, err)
			}
		})
	}
}

// TestParseWithParamsResolvesResources closes the
// substituteResourcesInPlace coverage gap: parameterizing the
// string SLURM knobs + sbatch_extra is a documented author
// feature (sibling of volumes/writes_artifacts substitution).
func TestParseWithParamsResolvesResources(t *testing.T) {
	yaml := []byte(`
name: "resources param resolution"
version: 1
params:
  - name: part
    type: string
    required: true
    description: "SLURM partition"
  - name: acct
    type: string
    required: true
    description: "billing account"
tasks:
  - id: align
    action: compute
    script: scripts/align.sh
    executor: slurm
    resources:
      partition: "{{part}}"
      time: "01:00:00"
      mem: "16G"
      sbatch_extra:
        - "--account={{acct}}"
        - "--constraint=a100"
    prompt: "Run"
`)
	parsed, err := ParseWithParams(yaml, map[string]interface{}{
		"part": "gpu-a100",
		"acct": "lab-42",
	})
	if err != nil {
		t.Fatalf("ParseWithParams failed: %v", err)
	}
	r := parsed.Run.Tasks[0].Resources
	if r.Partition != "gpu-a100" {
		t.Errorf("partition: got %q, want gpu-a100", r.Partition)
	}
	if len(r.SbatchExtra) != 2 || r.SbatchExtra[0] != "--account=lab-42" {
		t.Errorf("sbatch_extra not resolved: %v", r.SbatchExtra)
	}
	// Non-param fields untouched; no stray {{ }} anywhere.
	if r.Time != "01:00:00" || r.Mem != "16G" {
		t.Errorf("non-param knobs altered: %+v", r)
	}
	for _, s := range append([]string{r.Partition, r.Time, r.Mem}, r.SbatchExtra...) {
		if strings.Contains(s, "{{") {
			t.Errorf("unresolved param ref left in %q", s)
		}
	}
}
