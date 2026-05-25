package yaml

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --- Single-struct JSON decode (Track defaults to true) ---

// TestWriteArtifact_JSONObjectFormDefaultsTrackTrue pins the
// asymmetry fix: object-form JSON without `track:` decodes
// the same way the YAML object form does — Track=true. Before
// the WriteArtifact-level UnmarshalJSON, Go's default decode
// would have produced Track=false (zero value), silently
// flipping a tracked declaration to untracked.
//
// Bare-string JSON is also accepted here for symmetry with
// the YAML scalar form.
func TestWriteArtifact_JSONObjectFormDefaultsTrackTrue(t *testing.T) {
	var w WriteArtifact
	if err := json.Unmarshal([]byte(`{"path":"src/x.go"}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.Path != "src/x.go" || !w.Track {
		t.Errorf("object form without track: got %+v, want {Path:src/x.go Track:true}", w)
	}

	// Explicit track:false still wins.
	var w2 WriteArtifact
	if err := json.Unmarshal([]byte(`{"path":"out/big.bam","track":false}`), &w2); err != nil {
		t.Fatal(err)
	}
	if w2.Track {
		t.Errorf("explicit track:false should stick: %+v", w2)
	}

	// Bare string accepted as scalar shorthand.
	var w3 WriteArtifact
	if err := json.Unmarshal([]byte(`"src/server.go"`), &w3); err != nil {
		t.Fatal(err)
	}
	if w3.Path != "src/server.go" || !w3.Track {
		t.Errorf("bare-string single-struct decode: got %+v", w3)
	}

	// Optional flag round-trips.
	var w4 WriteArtifact
	if err := json.Unmarshal([]byte(`{"path":"src/go.sum","optional":true}`), &w4); err != nil {
		t.Fatal(err)
	}
	if !w4.Track || !w4.Optional {
		t.Errorf("optional with default-track: got %+v", w4)
	}
}

// TestWriteArtifact_PublishDefaultsTrue pins the deliverable-filter
// lever: publish defaults true (omitting it = current behavior, the
// artifact lands on the deliverable), and publish:false sticks for a
// tracked intermediate. Covered across the single-struct, bare-string,
// and slice decoders since each pre-sets the default independently.
func TestWriteArtifact_PublishDefaultsTrue(t *testing.T) {
	// Single struct, object form, no publish: → true.
	var w WriteArtifact
	if err := json.Unmarshal([]byte(`{"path":"deliverable.md"}`), &w); err != nil {
		t.Fatal(err)
	}
	if !w.Publish {
		t.Errorf("object form without publish: want Publish:true, got %+v", w)
	}
	// Explicit publish:false sticks (tracked-but-not-published).
	var w2 WriteArtifact
	if err := json.Unmarshal([]byte(`{"path":"sections/intermediate.md","publish":false}`), &w2); err != nil {
		t.Fatal(err)
	}
	if w2.Publish {
		t.Errorf("explicit publish:false should stick: %+v", w2)
	}
	// Bare string → published true.
	var w3 WriteArtifact
	if err := json.Unmarshal([]byte(`"out/x.md"`), &w3); err != nil {
		t.Fatal(err)
	}
	if !w3.Publish {
		t.Errorf("bare-string: want Publish:true, got %+v", w3)
	}
	// Slice decoder (used when re-reading task.WritesArtifacts at submit):
	// omitted publish → true; explicit false → false.
	var ws WriteArtifacts
	if err := json.Unmarshal([]byte(`[{"path":"a.md"},{"path":"b.md","publish":false},"c.md"]`), &ws); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a.md": true, "b.md": false, "c.md": true}
	for _, e := range ws {
		if e.Publish != want[e.Path] {
			t.Errorf("slice decode %s: Publish=%v, want %v", e.Path, e.Publish, want[e.Path])
		}
	}
}

// --- Pure pattern-detection helpers (no FS work) ---

func TestIsGlob(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"src/server.go", false},
		{"src/api/", false},
		{"src/api/*.go", true},
		{"cmd/?/main.go", true},
		{"src/[abc].go", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsGlob(tc.path); got != tc.want {
			t.Errorf("IsGlob(%q): got %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestIsDir(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"src/api/", true},
		{"src/api", false},
		{"src/server.go", false},
		{"/", false}, // edge: bare slash isn't a useful directory marker
		{"", false},
	}
	for _, tc := range cases {
		if got := IsDir(tc.path); got != tc.want {
			t.Errorf("IsDir(%q): got %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMatchesPattern(t *testing.T) {
	cases := []struct {
		submitted string
		declared  string
		want      bool
		note      string
	}{
		// Literal
		{"src/server.go", "src/server.go", true, "literal exact match"},
		{"src/server.go", "src/other.go", false, "literal non-match"},
		// Glob
		{"src/api/server.go", "src/api/*.go", true, "glob: matches"},
		{"src/api/sub/x.go", "src/api/*.go", false, "glob: non-recursive (subdir)"},
		{"src/main.go", "cmd/*/main.go", false, "glob: doesn't match outside"},
		{"cmd/server/main.go", "cmd/*/main.go", true, "glob: nested wildcard"},
		// Directory
		{"src/api/server.go", "src/api/", true, "dir: file inside"},
		{"src/api/sub/x.go", "src/api/", true, "dir: file in subdir (recursive coverage)"},
		{"src/other.go", "src/api/", false, "dir: outside"},
		{"src/api", "src/api/", false, "dir: not a file under (no trailing slash on submitted)"},
		// Empty / edge
		{"x", "", false, "empty pattern matches nothing"},
	}
	for _, tc := range cases {
		got := MatchesPattern(tc.submitted, tc.declared)
		if got != tc.want {
			t.Errorf("MatchesPattern(%q, %q): got %v, want %v — %s",
				tc.submitted, tc.declared, got, tc.want, tc.note)
		}
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	w := WriteArtifacts{
		{Path: "src/api/", Track: true},
		{Path: "tests/*.go", Track: true},
		{Path: "go.mod", Track: true},
	}
	cases := []struct {
		path string
		want bool
	}{
		{"src/api/server.go", true},  // dir match
		{"src/api/sub/x.go", true},   // dir match (recursive)
		{"tests/api_test.go", true},  // glob match
		{"go.mod", true},             // literal match
		{"go.sum", false},            // not declared
		{"src/server.go", false},     // outside dir match
		{"tests/sub/foo_test.go", false}, // glob doesn't recurse
	}
	for _, tc := range cases {
		if got := w.MatchesAnyPattern(tc.path); got != tc.want {
			t.Errorf("MatchesAnyPattern(%q): got %v, want %v", tc.path, got, tc.want)
		}
	}
}

// --- ExpandAgainstWorkdir ---

// makeFiles drops a tree of zero-byte regular files into root,
// creating parent dirs as needed. Used to assemble synthetic
// working trees for expansion tests.
func makeFiles(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func paths(entries []WriteArtifact) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	sort.Strings(out)
	return out
}

func TestExpand_Literal_PresentAndMissing(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, "src/server.go")

	w := WriteArtifacts{
		{Path: "src/server.go", Track: true},
		{Path: "src/missing.go", Track: true},
	}
	entries, missing, err := w.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got, want := paths(entries), []string{"src/server.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("entries: got %v, want %v", got, want)
	}
	if got, want := missing, []string{"src/missing.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("missing: got %v, want %v", got, want)
	}
}

func TestExpand_Optional_MissingIsSilent(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, "src/server.go")

	w := WriteArtifacts{
		{Path: "src/server.go", Track: true},
		{Path: "src/go.sum", Track: true, Optional: true},
	}
	entries, missing, err := w.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("optional missing should not appear in missing list; got %v", missing)
	}
	if got, want := paths(entries), []string{"src/server.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("entries: got %v, want %v", got, want)
	}
}

func TestExpand_Glob_MatchesAndZero(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, "src/api/server.go", "src/api/middleware.go", "src/api/handler_test.go", "src/api/notes.md")

	w := WriteArtifacts{
		{Path: "src/api/*.go", Track: true},
	}
	entries, missing, err := w.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing: got %v, want []", missing)
	}
	want := []string{"src/api/handler_test.go", "src/api/middleware.go", "src/api/server.go"}
	if got := paths(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("entries: got %v, want %v", got, want)
	}

	// Zero matches: report as missing unless Optional.
	w2 := WriteArtifacts{
		{Path: "src/api/*.py", Track: true},
	}
	_, miss2, _ := w2.ExpandAgainstWorkdir(dir)
	if len(miss2) != 1 || miss2[0] != "src/api/*.py" {
		t.Errorf("zero-match required glob should be missing; got %v", miss2)
	}

	w3 := WriteArtifacts{
		{Path: "src/api/*.py", Track: true, Optional: true},
	}
	_, miss3, _ := w3.ExpandAgainstWorkdir(dir)
	if len(miss3) != 0 {
		t.Errorf("zero-match optional glob should be silent; got %v", miss3)
	}
}

func TestExpand_Directory_RecursiveAndZero(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir,
		"src/api/server.go",
		"src/api/middleware.go",
		"src/api/handlers/users.go",
		"src/api/handlers/posts.go",
		"src/api/handlers/admin/keys.go",
	)

	w := WriteArtifacts{
		{Path: "src/api/", Track: true},
	}
	entries, missing, err := w.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing: %v", missing)
	}
	want := []string{
		"src/api/handlers/admin/keys.go",
		"src/api/handlers/posts.go",
		"src/api/handlers/users.go",
		"src/api/middleware.go",
		"src/api/server.go",
	}
	if got := paths(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("entries: got %v, want %v", got, want)
	}

	// Empty directory → required missing.
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	w2 := WriteArtifacts{{Path: "empty/", Track: true}}
	_, miss2, _ := w2.ExpandAgainstWorkdir(dir)
	if len(miss2) != 1 || miss2[0] != "empty/" {
		t.Errorf("empty dir without optional should be missing; got %v", miss2)
	}
}

func TestExpand_DirectorySkipsInfraDirs(t *testing.T) {
	// A declared `enju/` shouldn't sweep .bare.git/ or .clone/
	// even though they live under enju/. (Real users almost
	// never declare `enju/` but the safety guard is load-
	// bearing for when they do.)
	dir := t.TempDir()
	makeFiles(t, dir,
		"enju/templates/x.yaml",
		"enju/.bare.git/HEAD",
		"enju/.bare.git/objects/00/aabbcc",
		"enju/.clone/.git/config",
		"enju/.clone/file.go",
	)
	// Note: `enju/` is rejected by ValidateArtifactDeclaration,
	// but the expander itself should still be safe in case
	// validation is bypassed (e.g. tests, programmatic
	// construction). Simulate by walking a sibling dir that
	// happens to host the infra names.
	makeFiles(t, dir, "data/.git/HEAD", "data/notes.md", "data/sub/x.go")

	w := WriteArtifacts{{Path: "data/", Track: true}}
	entries, missing, err := w.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing: %v", missing)
	}
	got := paths(entries)
	for _, p := range got {
		if strings.Contains(p, ".git/") {
			t.Errorf(".git/ should be skipped; got entry %q", p)
		}
	}
	want := []string{"data/notes.md", "data/sub/x.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries: got %v, want %v", got, want)
	}
}

func TestExpand_Dedupes_AcrossDeclarations(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, "src/server.go", "src/handler.go")

	// Both declarations cover server.go. Expect ONE entry, not two.
	w := WriteArtifacts{
		{Path: "src/server.go", Track: true},
		{Path: "src/*.go", Track: true},
	}
	entries, _, err := w.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{"src/handler.go", "src/server.go"}
	if got := paths(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("dedupe failed: got %v, want %v", got, want)
	}
}

func TestExpand_LiteralPointingAtDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, "src/api/server.go") // creates src/api/

	// `src/api` (no trailing slash) is a literal that resolves
	// to a directory — almost certainly user intent confusion.
	// Surface a clear error rather than committing nothing.
	w := WriteArtifacts{{Path: "src/api", Track: true}}
	_, _, err := w.ExpandAgainstWorkdir(dir)
	if err == nil {
		t.Fatal("expected error for literal pointing at dir, got nil")
	}
	if !strings.Contains(err.Error(), "src/api") || !strings.Contains(err.Error(), "/") {
		t.Errorf("error should hint at trailing-/ form: %v", err)
	}
}

// TestExpand_OptionalSurvivesResolveWriteArtifacts pins the
// regression: ResolveWriteArtifacts is called when a task is
// instantiated (per-instance copy of the source declarations).
// A pre-fix version reconstructed each entry with explicit
// {Path, Track} fields and silently dropped Optional, so a
// task that declared `{path: go.sum, optional: true}` arrived
// at expansion time as Optional=false and tripped the
// "missing required" check.
func TestExpand_OptionalSurvivesResolveWriteArtifacts(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, "smoke/a.md")

	source := WriteArtifacts{
		{Path: "smoke/a.md", Track: true},
		{Path: "smoke/never-exists.md", Track: true, Optional: true},
	}
	// Resolve with empty params (no substitution) — exercises
	// the per-instance materialization path that was dropping
	// Optional.
	resolved := ResolveWriteArtifacts(source, map[string]string{})
	for i, e := range resolved {
		if e.Optional != source[i].Optional {
			t.Errorf("Resolve dropped Optional at %d: got %+v, want %+v", i, e, source[i])
		}
	}

	_, missing, err := resolved.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("optional-missing path landed in missing list: %v", missing)
	}
}

// TestExpand_OptionalSurvivesStarRefExpansion mirrors the
// ResolveWriteArtifacts case for the `[*]` list-expansion
// path: a `{path: foo/{{items[*]}}, optional: true}` entry
// expanded over a list parameter must produce N entries that
// ALL carry Optional=true. Pre-fix the star-expansion loop
// dropped Optional silently.
func TestExpand_OptionalSurvivesStarRefExpansion(t *testing.T) {
	source := WriteArtifacts{
		{Path: "out/{{items[*]}}.lock", Track: true, Optional: true},
	}
	merged := map[string]interface{}{"items": []interface{}{"a", "b", "c"}}
	expanded, err := expandStarRefsInWrites(source, merged, nil, "task.writes_artifacts")
	if err != nil {
		t.Fatalf("expandStarRefsInWrites: %v", err)
	}
	if len(expanded) != 3 {
		t.Fatalf("expansion count: got %d, want 3", len(expanded))
	}
	for _, e := range expanded {
		if !e.Optional {
			t.Errorf("star-expanded entry should carry Optional=true: %+v", e)
		}
	}
}

// TestExpand_OptionalSurvivesJSONRoundTrip pins the storage
// path: the coord serializes WriteArtifacts to JSON for the
// tasks.writes_artifacts column, the client deserializes on
// claim. Optional must survive that round trip — in particular
// the `omitempty` tag must not strip a true value (it strips
// only the zero value, but it's worth pinning explicitly).
func TestExpand_OptionalSurvivesJSONRoundTrip(t *testing.T) {
	source := WriteArtifacts{
		{Path: "smoke/a.md", Track: true},
		{Path: "smoke/never-exists.md", Track: true, Optional: true},
		{Path: "out/big.bam", Track: false, Optional: true},
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var back WriteArtifacts
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 {
		t.Fatalf("entries lost: %d", len(back))
	}
	if back[1].Optional != true || back[2].Optional != true {
		t.Errorf("optional flag lost across JSON: %+v", back)
	}
	if back[0].Optional != false {
		t.Errorf("absent optional should stay false: %+v", back[0])
	}
}

func TestExpand_MixedTrackAndOptional(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, "src/server.go", "build/output.bin")

	w := WriteArtifacts{
		{Path: "src/server.go", Track: true},
		{Path: "build/output.bin", Track: false},                // untracked binary
		{Path: "src/go.sum", Track: true, Optional: true},       // optional, missing
		{Path: "build/optional.dat", Track: false, Optional: true}, // optional untracked, missing
	}
	entries, missing, err := w.ExpandAgainstWorkdir(dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing: %v", missing)
	}
	gotEntries := make(map[string]bool)
	for _, e := range entries {
		gotEntries[e.Path] = e.Track
	}
	if gotEntries["src/server.go"] != true {
		t.Errorf("src/server.go should be tracked, got %v", gotEntries["src/server.go"])
	}
	if gotEntries["build/output.bin"] != false {
		t.Errorf("build/output.bin should be untracked, got %v", gotEntries["build/output.bin"])
	}
	if _, present := gotEntries["src/go.sum"]; present {
		t.Error("optional missing src/go.sum should NOT appear in entries")
	}
}
