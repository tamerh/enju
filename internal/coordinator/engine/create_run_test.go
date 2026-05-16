package engine

import (
	"strings"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// TestValidateRunCreation_RecordStarMixedWithScalarParams pins the
// ISSUE-5-revised cross-path a tester reported (against a stale
// pre-9b41b07 binary): a reads_artifacts entry that mixes plain
// scalar params with a list<record> star-ref, under run-level
// for_each + aggregates. The hypothesized failure was the glob
// check firing on the partially-substituted "...[*]..." between a
// scalar phase and a star phase. It does NOT fire on current code
// — substituteParamsInPlace star-expands the {{name[*].field}}
// token first, scalar substitution runs after, and
// ValidateRunCreation sees only fully-resolved literal paths — but
// the mixed + for_each + aggregates combination is otherwise
// untested, so this guards against a future substitute/expand
// reordering regression.
func TestValidateRunCreation_RecordStarMixedWithScalarParams(t *testing.T) {
	const y = `
name: mixed
version: 1
params:
  - name: outdir
    type: string
  - name: project
    type: string
  - name: samples
    type: list<record>
    required: true
    key: sample_id
    fields:
      sample_id: string
for_each:
  sample: "{{samples}}"
tasks:
  - id: qc
    action: compute
    script: x.sh
    prompt: "qc {{sample.sample_id}}"
    writes:
      - "{{outdir}}/{{project}}/qc/{{sample.sample_id}}/stats.txt"
  - id: agg
    action: compute
    collects: qc
    script: y.sh
    prompt: "aggregate"
    reads:
      - "{{outdir}}/{{project}}/qc/{{samples[*].sample_id}}/stats.txt"
    writes:
      - "{{outdir}}/{{project}}/summary.txt"
`
	parsed, err := enjuYaml.ParseWithParams([]byte(y), map[string]interface{}{
		"outdir":  "results",
		"project": "phage_demo",
		"samples": []interface{}{
			map[string]interface{}{"sample_id": "bc09"},
			map[string]interface{}{"sample_id": "bc10"},
		},
	})
	if err != nil {
		t.Fatalf("ParseWithParams: %v", err)
	}

	// The exact failure point the tester reported.
	if err := New(&mockStore{}, nil).ValidateRunCreation(parsed); err != nil {
		t.Fatalf("ValidateRunCreation rejected a mixed scalar+record-star path: %v", err)
	}

	// Every instance's reads must be fully resolved — scalar
	// params substituted AND the record star-ref expanded per
	// record, no leftover {{...}} / [*] (exactly what
	// ValidateRunCreation's literal check rejects).
	want := map[string]bool{
		"results/phage_demo/qc/bc09/stats.txt": false,
		"results/phage_demo/qc/bc10/stats.txt": false,
	}
	for _, insts := range parsed.ExpandedTasks {
		for _, ti := range insts {
			for _, r := range ti.ReadsArtifacts {
				if strings.ContainsAny(r, "{[*") {
					t.Errorf("reads not fully resolved (task %q): %q", ti.ID, r)
				}
				if _, ok := want[r]; ok {
					want[r] = true
				}
			}
		}
	}
	for path, seen := range want {
		if !seen {
			t.Errorf("expected resolved aggregator read %q to appear", path)
		}
	}
}
