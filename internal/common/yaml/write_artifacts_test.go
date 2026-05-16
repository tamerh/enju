package yaml

import (
	"encoding/json"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// TestWriteArtifactsYAMLShorthand exercises the bare-string form.
// Every entry must round-trip with Track defaulting to true.
func TestWriteArtifactsYAMLShorthand(t *testing.T) {
	src := `writes:
  - out/stats.json
  - out/summary.md`
	var td TaskDef
	if err := yamlv3.Unmarshal([]byte(src), &td); err != nil {
		t.Fatal(err)
	}
	if len(td.WritesArtifacts) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(td.WritesArtifacts))
	}
	for _, e := range td.WritesArtifacts {
		if !e.Track {
			t.Errorf("shorthand entry %q should default Track=true, got %+v", e.Path, e)
		}
	}
	if td.WritesArtifacts[0].Path != "out/stats.json" || td.WritesArtifacts[1].Path != "out/summary.md" {
		t.Fatalf("paths wrong: %+v", td.WritesArtifacts)
	}
}

// TestWriteArtifactsYAMLObjectForm covers the explicit object
// form, including the Track=false opt-out and the implicit
// Track=true default when the key is omitted.
func TestWriteArtifactsYAMLObjectForm(t *testing.T) {
	src := `writes:
  - path: out/aligned.bam
    track: false
  - path: out/stats.json
    track: true
  - path: out/inferred.md`
	var td TaskDef
	if err := yamlv3.Unmarshal([]byte(src), &td); err != nil {
		t.Fatal(err)
	}
	if len(td.WritesArtifacts) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(td.WritesArtifacts))
	}
	if td.WritesArtifacts[0].Path != "out/aligned.bam" || td.WritesArtifacts[0].Track {
		t.Errorf("track:false entry wrong: %+v", td.WritesArtifacts[0])
	}
	if !td.WritesArtifacts[1].Track {
		t.Errorf("track:true entry wrong: %+v", td.WritesArtifacts[1])
	}
	// Omitting track: in object form still defaults to tracked.
	if !td.WritesArtifacts[2].Track {
		t.Errorf("omitted-track entry should default to Track=true, got %+v", td.WritesArtifacts[2])
	}
}

// TestWriteArtifactsYAMLMixedList confirms that shorthand and
// object form can mix in the same list.
func TestWriteArtifactsYAMLMixedList(t *testing.T) {
	src := `writes:
  - out/stats.json
  - path: out/aligned.bam
    track: false`
	var td TaskDef
	if err := yamlv3.Unmarshal([]byte(src), &td); err != nil {
		t.Fatal(err)
	}
	if !td.WritesArtifacts[0].Track || td.WritesArtifacts[1].Track {
		t.Fatalf("mixed-list tracks wrong: %+v", td.WritesArtifacts)
	}
}

// TestWriteArtifactsYAMLMalformedTrack — a non-bool track value
// must surface as a schema error, not silently decode to false.
func TestWriteArtifactsYAMLMalformedTrack(t *testing.T) {
	src := `writes:
  - path: out/x
    track: yes-please`
	var td TaskDef
	err := yamlv3.Unmarshal([]byte(src), &td)
	if err == nil {
		t.Fatal("expected YAML parse error on non-bool track, got nil")
	}
}

// TestWriteArtifactsYAMLInvalidShape — a sequence/scalar hybrid
// (e.g. a list inside the entry) should also reject.
func TestWriteArtifactsYAMLInvalidShape(t *testing.T) {
	src := `writes:
  - - nested
    - list`
	var td TaskDef
	err := yamlv3.Unmarshal([]byte(src), &td)
	if err == nil {
		t.Fatal("expected YAML parse error on sequence entry, got nil")
	}
	if !strings.Contains(err.Error(), "writes entry") {
		t.Errorf("expected error to cite writes, got: %v", err)
	}
}

// TestWriteArtifactsJSONLegacyShape covers the zero-migration
// back-compat contract: pre-untracked DB rows stored as a bare
// []string must decode with Track=true on every entry.
func TestWriteArtifactsJSONLegacyShape(t *testing.T) {
	legacy := []byte(`["out/a.md","out/b.md"]`)
	var w WriteArtifacts
	if err := json.Unmarshal(legacy, &w); err != nil {
		t.Fatal(err)
	}
	if len(w) != 2 {
		t.Fatalf("got %d entries, want 2", len(w))
	}
	for i, e := range w {
		if !e.Track {
			t.Errorf("legacy entry %d should be tracked: %+v", i, e)
		}
	}
	if w[0].Path != "out/a.md" || w[1].Path != "out/b.md" {
		t.Fatalf("legacy paths wrong: %+v", w)
	}
}

// TestWriteArtifactsJSONObjectShape covers the current stored
// form, including omitted `track` defaulting to true.
func TestWriteArtifactsJSONObjectShape(t *testing.T) {
	current := []byte(`[{"path":"out/a.md","track":true},{"path":"out/b.md","track":false},{"path":"out/c.md"}]`)
	var w WriteArtifacts
	if err := json.Unmarshal(current, &w); err != nil {
		t.Fatal(err)
	}
	if len(w) != 3 {
		t.Fatalf("got %d entries, want 3", len(w))
	}
	if !w[0].Track || w[1].Track || !w[2].Track {
		t.Fatalf("track flags wrong: %+v", w)
	}
}

// TestWriteArtifactsJSONRoundTrip — marshal then parse preserves
// both path and track fields for every entry.
func TestWriteArtifactsJSONRoundTrip(t *testing.T) {
	orig := WriteArtifacts{
		{Path: "out/a.md", Track: true},
		{Path: "out/b.bam", Track: false},
	}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back WriteArtifacts
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0] != orig[0] || back[1] != orig[1] {
		t.Fatalf("round-trip lost fidelity: got %+v, want %+v", back, orig)
	}
}

// TestWriteArtifactsJSONEmpty — null and empty array both
// decode to nil so the DB default stays clean.
func TestWriteArtifactsJSONEmpty(t *testing.T) {
	cases := [][]byte{[]byte(`null`), []byte(``), []byte(`[]`)}
	for _, raw := range cases {
		var w WriteArtifacts
		if err := json.Unmarshal(raw, &w); err != nil && len(raw) > 0 {
			// Empty bytes aren't valid JSON — skip.
			if string(raw) == "" {
				continue
			}
			t.Errorf("unexpected error for %q: %v", raw, err)
		}
		if len(w) != 0 {
			t.Errorf("expected empty decode for %q, got %+v", raw, w)
		}
	}
}

// TestResolveWriteArtifactsSubstitutesPath drives the per-instance
// substitution helper used by build.go when a for_each instance
// materializes. Track flag is a literal YAML bool — substitution
// only touches Path.
func TestResolveWriteArtifactsSubstitutesPath(t *testing.T) {
	src := WriteArtifacts{
		{Path: "summaries/{{stem}}.md", Track: true},
		{Path: "scratch/{{stem}}.bam", Track: false},
	}
	out := ResolveWriteArtifacts(src, map[string]string{"stem": "alpha"})
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out))
	}
	if out[0].Path != "summaries/alpha.md" || !out[0].Track {
		t.Errorf("entry 0 wrong: %+v", out[0])
	}
	if out[1].Path != "scratch/alpha.bam" || out[1].Track {
		t.Errorf("entry 1 wrong: %+v", out[1])
	}
	// Source untouched — helper must allocate a fresh slice
	// so sibling for_each instances don't share backing storage.
	if src[0].Path != "summaries/{{stem}}.md" {
		t.Errorf("source was mutated: %+v", src)
	}
}

func TestResolveWriteArtifactsEmpty(t *testing.T) {
	if got := ResolveWriteArtifacts(nil, nil); got != nil {
		t.Fatalf("expected nil for nil input, got %+v", got)
	}
	if got := ResolveWriteArtifacts(WriteArtifacts{}, map[string]string{"x": "y"}); got != nil {
		t.Fatalf("expected nil for empty slice, got %+v", got)
	}
}

func TestWriteArtifactsHelpers(t *testing.T) {
	w := WriteArtifacts{
		{Path: "a", Track: true},
		{Path: "b", Track: false},
		{Path: "c", Track: true},
	}
	if got, want := strings.Join(w.Paths(), ","), "a,b,c"; got != want {
		t.Errorf("Paths() = %q, want %q", got, want)
	}
	if got, want := strings.Join(w.TrackedPaths(), ","), "a,c"; got != want {
		t.Errorf("TrackedPaths() = %q, want %q", got, want)
	}
	if got, want := strings.Join(w.UntrackedPaths(), ","), "b"; got != want {
		t.Errorf("UntrackedPaths() = %q, want %q", got, want)
	}

	var empty WriteArtifacts
	if empty.Paths() != nil || empty.TrackedPaths() != nil || empty.UntrackedPaths() != nil {
		t.Error("empty slice helpers should return nil")
	}
}

// TestWriteArtifactsYAMLPatternForms confirms the YAML parser
// preserves directory and glob path syntax verbatim — they
// reach the WriteArtifact.Path field as-is, ready for the
// expand step at submit time. Pre-pattern YAML callers that
// still write literals get unchanged behavior.
func TestWriteArtifactsYAMLPatternForms(t *testing.T) {
	src := `writes:
  - src/api/
  - src/handlers/*.go
  - cmd/*/main.go
  - go.mod`
	var td TaskDef
	if err := yamlv3.Unmarshal([]byte(src), &td); err != nil {
		t.Fatal(err)
	}
	want := []string{"src/api/", "src/handlers/*.go", "cmd/*/main.go", "go.mod"}
	if len(td.WritesArtifacts) != len(want) {
		t.Fatalf("entry count: got %d, want %d", len(td.WritesArtifacts), len(want))
	}
	for i, w := range want {
		if td.WritesArtifacts[i].Path != w {
			t.Errorf("entry %d: path = %q, want %q", i, td.WritesArtifacts[i].Path, w)
		}
		if !td.WritesArtifacts[i].Track {
			t.Errorf("entry %d: shorthand should default Track=true; got %+v", i, td.WritesArtifacts[i])
		}
	}
}

// TestWriteArtifactsYAMLOptionalRoundTrip pins both YAML
// shapes for the optional flag: object-form with `optional:
// true` parses as Optional=true; absent key parses as
// Optional=false. The shorthand bare-string form has no
// way to express optional — that's by design (bare paths
// declare required outputs).
func TestWriteArtifactsYAMLOptionalRoundTrip(t *testing.T) {
	src := `writes:
  - path: src/server.go
  - path: src/go.sum
    optional: true
  - path: out/big.bam
    track: false
    optional: true`
	var td TaskDef
	if err := yamlv3.Unmarshal([]byte(src), &td); err != nil {
		t.Fatal(err)
	}
	if len(td.WritesArtifacts) != 3 {
		t.Fatalf("entries: got %d, want 3", len(td.WritesArtifacts))
	}
	if td.WritesArtifacts[0].Optional {
		t.Errorf("absent optional: should be false, got %+v", td.WritesArtifacts[0])
	}
	if !td.WritesArtifacts[1].Optional || !td.WritesArtifacts[1].Track {
		t.Errorf("optional+default-track: %+v", td.WritesArtifacts[1])
	}
	if !td.WritesArtifacts[2].Optional || td.WritesArtifacts[2].Track {
		t.Errorf("optional+track:false: %+v", td.WritesArtifacts[2])
	}

	// Round-trip via JSON (the wire format the wrapper subprocess sees).
	encoded, err := json.Marshal(td.WritesArtifacts)
	if err != nil {
		t.Fatal(err)
	}
	var back WriteArtifacts
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	for i, e := range back {
		if e.Path != td.WritesArtifacts[i].Path ||
			e.Track != td.WritesArtifacts[i].Track ||
			e.Optional != td.WritesArtifacts[i].Optional {
			t.Errorf("round-trip lost data at %d: got %+v, want %+v",
				i, e, td.WritesArtifacts[i])
		}
	}
}
