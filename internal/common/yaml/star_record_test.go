package yaml

import (
	"strings"
	"testing"
)

// ISSUE-5: `{{param[*].field}}` star expansion on list<record>
// params (and bare `{{param[*]}}` → the record's key: field).
// Exercised at the ParseWithParams level — the real path, real
// RecordFields built by the parser, matching the reporter's
// fan-in reads_artifacts scenario.

const lrpStarYAML = `
name: "fan-in"
version: 1
params:
  - name: samples
    type: list<record>
    required: true
    key: sample_id
    fields:
      sample_id: string
      fastq: string
  - name: names
    type: list<string>
tasks:
  - id: agg
    action: compute
    script: scripts/agg.sh
    prompt: "aggregate"
    reads_artifacts:
      - "q/{{samples[*].sample_id}}/qc.tsv"
      - "c/{{samples[*]}}/pharokka.gff"
      - "n/{{names[*]}}.txt"
`

func parseStar(t *testing.T, reads []string) (*ParsedRun, error) {
	t.Helper()
	params := map[string]interface{}{
		"samples": []interface{}{
			map[string]interface{}{"sample_id": "bc09", "fastq": "a.fq"},
			map[string]interface{}{"sample_id": "bc10", "fastq": "b.fq"},
		},
		"names": []interface{}{"x", "y"},
	}
	return ParseWithParams([]byte(lrpStarYAML), params)
}

func TestStarExpand_ListRecord_FieldAndBareKeyAndStringList(t *testing.T) {
	parsed, err := parseStar(t, nil)
	if err != nil {
		t.Fatalf("ParseWithParams: %v", err)
	}
	got := []string(parsed.Run.Tasks[0].ReadsArtifacts)
	want := []string{
		"q/bc09/qc.tsv", "q/bc10/qc.tsv", // {{samples[*].sample_id}}
		"c/bc09/pharokka.gff", "c/bc10/pharokka.gff", // bare {{samples[*]}} → key: sample_id
		"n/x.txt", "n/y.txt", // list<string> {{names[*]}} unchanged
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("expansion mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestStarExpand_ListRecord_UnknownFieldIsHardError(t *testing.T) {
	y := strings.Replace(lrpStarYAML,
		`"q/{{samples[*].sample_id}}/qc.tsv"`,
		`"q/{{samples[*].sampel_id}}/qc.tsv"`, 1) // typo
	_, err := ParseWithParams([]byte(y), map[string]interface{}{
		"samples": []interface{}{map[string]interface{}{"sample_id": "bc09", "fastq": "a.fq"}},
		"names":   []interface{}{"x"},
	})
	if err == nil {
		t.Fatal("expected a hard error for an unknown record field")
	}
	if !strings.Contains(err.Error(), "sampel_id") ||
		!strings.Contains(err.Error(), "sample_id") || !strings.Contains(err.Error(), "fastq") {
		t.Errorf("error should name the bad field and the declared fields; got: %v", err)
	}
}

// TestStarExpand_ParamsFile_NumericField pins the --params-file
// cross-path. json.Unmarshal into interface{} yields
// []interface{}-of-map[string]interface{} (the SAME shape as
// YAML decode — NOT []map[string]interface{}; the param
// validator enforces []interface{}), with JSON numerics as
// float64. The genuinely-untested bit is the numeric field:
// stringifyParamValue's %v fallback must turn float64(7) into
// "7" (not "" or "7.0") inside a star-expanded artifact path.
func TestStarExpand_ParamsFile_NumericField(t *testing.T) {
	y := `
name: pf
version: 1
params:
  - name: samples
    type: list<record>
    required: true
    key: sample_id
    fields:
      sample_id: string
      count: int
tasks:
  - id: agg
    action: compute
    script: s.sh
    prompt: "x"
    reads_artifacts:
      - "n/{{samples[*].count}}.txt"
      - "k/{{samples[*]}}.flag"
`
	parsed, err := ParseWithParams([]byte(y), map[string]interface{}{
		// Exactly what `json.Unmarshal` of a --params-file
		// produces for a list<record>: []interface{} of
		// map[string]interface{}, numerics as float64.
		"samples": []interface{}{
			map[string]interface{}{"sample_id": "bc09", "count": float64(7)},
			map[string]interface{}{"sample_id": "bc10", "count": float64(12)},
		},
	})
	if err != nil {
		t.Fatalf("ParseWithParams: %v", err)
	}
	got := []string(parsed.Run.Tasks[0].ReadsArtifacts)
	want := []string{
		"n/7.txt", "n/12.txt", // float64 → integer string, not "7.0"
		"k/bc09.flag", "k/bc10.flag", // bare → key (sample_id), []map shape
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("params-file shape mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestStarExpand_FieldOnStringListIsError(t *testing.T) {
	y := strings.Replace(lrpStarYAML,
		`"n/{{names[*]}}.txt"`, `"n/{{names[*].sample_id}}.txt"`, 1)
	_, err := ParseWithParams([]byte(y), map[string]interface{}{
		"samples": []interface{}{map[string]interface{}{"sample_id": "bc09", "fastq": "a.fq"}},
		"names":   []interface{}{"x"},
	})
	if err == nil || !strings.Contains(err.Error(), "list<record>") {
		t.Fatalf("a .field suffix on a list<string> must error mentioning list<record>; got: %v", err)
	}
}
