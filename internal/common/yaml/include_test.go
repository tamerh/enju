package yaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// memFS is an in-memory reader for the resolver tests: path → bytes.
func memFS(files map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		b, ok := files[p]
		if !ok {
			return nil, &notFound{p}
		}
		return []byte(b), nil
	}
}

type notFound struct{ p string }

func (e *notFound) Error() string { return "no such file: " + e.p }

func TestFlattenIncludes_NoIncludeIsByteIdentical(t *testing.T) {
	src := "name: Solo\nversion: 1\ntasks:\n  - id: a\n    action: compute\n    script: a.sh\n"
	out, err := FlattenIncludes("wf/enju.yaml", memFS(map[string]string{"wf/enju.yaml": src}))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if string(out) != src {
		t.Fatalf("no-include file must pass through byte-identical.\n got: %q\nwant: %q", out, src)
	}
}

func TestFlattenIncludes_FlatMerge(t *testing.T) {
	files := map[string]string{
		"wf/enju.yaml": `name: Big
version: 1
params:
  - name: items
    type: list<string>
include:
  - frag/ingest.yaml
  - frag/analyze.yaml
tasks:
  - id: publish
    action: answer
    prompt: "summarize"
`,
		"wf/frag/ingest.yaml": `tasks:
  - id: ingest
    action: compute
    script: scripts/ingest.sh
bots:
  - name: dev-bot
    handler: claude
`,
		"wf/frag/analyze.yaml": `tasks:
  - id: analyze
    action: compute
    script: scripts/analyze.sh
`,
	}
	out, err := FlattenIncludes("wf/enju.yaml", memFS(files))
	if err != nil {
		t.Fatalf("FlattenIncludes: %v", err)
	}
	parsed, err := Parse(out)
	if err != nil {
		t.Fatalf("flattened doc must parse: %v\n---\n%s", err, out)
	}
	gotIDs := []string{}
	for _, tk := range parsed.Run.Tasks {
		gotIDs = append(gotIDs, tk.ID)
	}
	want := []string{"ingest", "analyze", "publish"} // includes first (in list order), then entry-local
	if strings.Join(gotIDs, ",") != strings.Join(want, ",") {
		t.Errorf("task order: got %v, want %v", gotIDs, want)
	}
	if parsed.Run.Name != "Big" || len(parsed.Run.Params) != 1 {
		t.Errorf("entry singletons/params lost: name=%q params=%d", parsed.Run.Name, len(parsed.Run.Params))
	}
	if !strings.HasPrefix(string(out), "# Flattened by enju from:") {
		t.Errorf("missing provenance header:\n%s", out)
	}
}

func TestFlattenIncludes_DuplicateTaskIDIsHardError(t *testing.T) {
	files := map[string]string{
		"wf/enju.yaml":     "name: D\nversion: 1\ninclude:\n  - a.yaml\n  - b.yaml\n",
		"wf/a.yaml":        "tasks:\n  - id: dup\n    action: compute\n    script: x.sh\n",
		"wf/b.yaml":        "tasks:\n  - id: dup\n    action: compute\n    script: y.sh\n",
	}
	_, err := FlattenIncludes("wf/enju.yaml", memFS(files))
	if err == nil || !strings.Contains(err.Error(), "duplicate task") {
		t.Fatalf("want duplicate-task error naming both files, got: %v", err)
	}
	if !strings.Contains(err.Error(), "wf/a.yaml") || !strings.Contains(err.Error(), "wf/b.yaml") {
		t.Errorf("error should name both source files, got: %v", err)
	}
}

func TestFlattenIncludes_FragmentSettingSingletonIsError(t *testing.T) {
	files := map[string]string{
		"wf/enju.yaml": "name: E\nversion: 1\ninclude:\n  - frag.yaml\n",
		"wf/frag.yaml": "name: SneakyFragment\ntasks:\n  - id: t\n    action: compute\n    script: t.sh\n",
	}
	_, err := FlattenIncludes("wf/enju.yaml", memFS(files))
	if err == nil || !strings.Contains(err.Error(), "may only set") {
		t.Fatalf("a fragment setting a singleton must error, got: %v", err)
	}
}

func TestFlattenIncludes_CycleDetected(t *testing.T) {
	files := map[string]string{
		"wf/enju.yaml": "name: C\nversion: 1\ninclude:\n  - a.yaml\n",
		"wf/a.yaml":    "include:\n  - b.yaml\n",
		"wf/b.yaml":    "include:\n  - a.yaml\n",
	}
	_, err := FlattenIncludes("wf/enju.yaml", memFS(files))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got: %v", err)
	}
}

func TestFlattenIncludes_EscapeBundleRejected(t *testing.T) {
	files := map[string]string{
		"wf/enju.yaml":  "name: X\nversion: 1\ninclude:\n  - ../secrets/base.yaml\n",
		"secrets/base.yaml": "tasks:\n  - id: t\n    action: compute\n    script: t.sh\n",
	}
	_, err := FlattenIncludes("wf/enju.yaml", memFS(files))
	if err == nil || !strings.Contains(err.Error(), "escapes the workflow directory") {
		t.Fatalf("escaping include must be rejected, got: %v", err)
	}
}

// TestFlattenFile_SymlinkEscapeRejected pins the decision on review
// point 1: the FS-backed reader resolves symlinks, so an include
// that is lexically in-bundle but resolves outside it via a symlink
// is rejected on the validate/--dry-run path — keeping it honest
// about what the run-create path (which can't follow the link)
// would actually flatten.
func TestFlattenFile_SymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "outside", "secret.yaml"),
		"tasks:\n  - id: x\n    action: compute\n    script: s.sh\n")
	mustWrite(t, filepath.Join(root, "bundle", "enju.yaml"),
		"name: S\nversion: 1\ninclude:\n  - link.yaml\n")
	// link.yaml is lexically inside bundle/, but resolves outside.
	if err := os.Symlink(filepath.Join("..", "outside", "secret.yaml"),
		filepath.Join(root, "bundle", "link.yaml")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := FlattenFile(filepath.Join(root, "bundle", "enju.yaml")); err == nil ||
		!strings.Contains(err.Error(), "outside the workflow directory") {
		t.Fatalf("symlink-escaping include must be rejected by FlattenFile, got: %v", err)
	}

	// Positive control: a genuine in-bundle include still flattens
	// through the same confined reader.
	mustWrite(t, filepath.Join(root, "bundle", "parts", "t.yaml"),
		"tasks:\n  - id: ok\n    action: compute\n    script: s.sh\n")
	mustWrite(t, filepath.Join(root, "bundle", "ok.yaml"),
		"name: S\nversion: 1\ninclude:\n  - parts/t.yaml\n")
	out, err := FlattenFile(filepath.Join(root, "bundle", "ok.yaml"))
	if err != nil {
		t.Fatalf("in-bundle include via FlattenFile: %v", err)
	}
	if p, _ := Parse(out); p == nil || len(p.Run.Tasks) != 1 || p.Run.Tasks[0].ID != "ok" {
		t.Errorf("in-bundle include did not flatten: %s", out)
	}
}

func TestFlattenIncludes_NestedIncludeAndMissingFile(t *testing.T) {
	ok := map[string]string{
		"wf/enju.yaml": "name: N\nversion: 1\ninclude:\n  - mid.yaml\n",
		"wf/mid.yaml":  "include:\n  - leaf.yaml\n",
		"wf/leaf.yaml": "tasks:\n  - id: leaf\n    action: compute\n    script: l.sh\n",
	}
	out, err := FlattenIncludes("wf/enju.yaml", memFS(ok))
	if err != nil {
		t.Fatalf("nested include: %v", err)
	}
	if p, _ := Parse(out); p == nil || len(p.Run.Tasks) != 1 || p.Run.Tasks[0].ID != "leaf" {
		t.Errorf("nested include did not flatten leaf task: %s", out)
	}

	bad := map[string]string{"wf/enju.yaml": "name: M\nversion: 1\ninclude:\n  - gone.yaml\n"}
	if _, err := FlattenIncludes("wf/enju.yaml", memFS(bad)); err == nil {
		t.Fatal("missing include file must error")
	}
}
