package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// TestParseWorkflowForGo_RejectsInvalid pins bug-hunt M4: `enju go`
// validates the workflow BEFORE registering a project, so a YAML
// that fails validation can't leave a phantom project behind. This
// covers the gate (the ordering is structural — parse precedes
// resolveOrRegisterProject in cmdGo).
func TestParseWorkflowForGo_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "enju.yaml")
	// writes path escaping the repo — the tester's escape probe.
	yml := `name: escape
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/escape.sh
    prompt: x
    writes:
      - ../../etc/escaped.txt
`
	if err := os.WriteFile(wf, []byte(yml), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseWorkflowForGo(wf, nil); err == nil {
		t.Fatal("expected validation error for a writes path with '..', got nil")
	}
}

// TestParseWorkflowForGo_AcceptsValid is the no-false-positive twin.
func TestParseWorkflowForGo_AcceptsValid(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "enju.yaml")
	yml := `name: ok
version: 1
tasks:
  - id: t
    action: answer
    prompt: hi
`
	if err := os.WriteFile(wf, []byte(yml), 0644); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseWorkflowForGo(wf, nil)
	if err != nil {
		t.Fatalf("valid workflow rejected: %v", err)
	}
	if parsed == nil || parsed.Run.Name != "ok" {
		t.Fatalf("unexpected parse result: %+v", parsed)
	}
}

// TestZeroAgentWorkflowDowngradesAutoAgents pins the predicate
// behind bug-hunt M1: a workflow with no agents: section yields an
// empty manifest, which cmdGo uses to degrade --auto-agents to a
// plain run instead of hard-failing.
func TestZeroAgentWorkflowDowngradesAutoAgents(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "enju.yaml")
	yml := `name: pure-compute
version: 1
tasks:
  - id: t
    action: compute
    script: scripts/run.sh
    prompt: x
`
	if err := os.WriteFile(wf, []byte(yml), 0644); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseWorkflowForGo(wf, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, perr := bots.FromInlineNode(parsed.Run.Bots)
	if perr != nil {
		t.Fatalf("FromInlineNode: %v", perr)
	}
	if m != nil && len(m.Bots) > 0 {
		t.Fatalf("expected zero agents, got %d", len(m.Bots))
	}
}

// TestPickWorkflowArg exercises Snakemake-style auto-discovery:
// the ./enju.yaml fallback when no positional path is supplied.
// Uses t.Chdir to scope each case so they can't leak into one
// another or other tests.
func TestPickWorkflowArg_ExplicitArgWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "enju.yaml"), []byte("name: t\nversion: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err := pickWorkflowArg([]string{"other.yaml"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "other.yaml" {
		t.Errorf("explicit arg should win even with ./enju.yaml present: got %q", got)
	}
}

func TestPickWorkflowArg_AutoDiscoversCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "enju.yaml"), []byte("name: t\nversion: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err := pickWorkflowArg(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "enju.yaml" {
		t.Errorf("auto-discovery: got %q, want enju.yaml", got)
	}
}

func TestPickWorkflowArg_NoArgsNoCwdFileErrors(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := pickWorkflowArg(nil); err == nil {
		t.Error("expected error when no arg and no ./enju.yaml")
	}
}

func TestPickWorkflowArg_TooManyArgsErrors(t *testing.T) {
	if _, err := pickWorkflowArg([]string{"a.yaml", "b.yaml"}); err == nil {
		t.Error("expected error for 2+ positional args")
	}
}

func TestParseParamsArg(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
		err  bool
	}{
		{"", nil, false},
		{"gene=TP53", map[string]string{"gene": "TP53"}, false},
		{"gene=TP53, effort=high", map[string]string{"gene": "TP53", "effort": "high"}, false},
		{"  gene = TP53  ", map[string]string{"gene": "TP53"}, false},
		{"gene", nil, true},
		{"=value", nil, true},
	}
	for _, c := range cases {
		got, err := parseParamsArg(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseParamsArg(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseParamsArg(%q): unexpected error %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseParamsArg(%q): got %v, want %v", c.in, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parseParamsArg(%q)[%s] = %v, want %v", c.in, k, got[k], v)
			}
		}
	}
}

// TestSplitTopLevelCommas pins the depth/quote-aware splitter:
// plain k=v lists split exactly like strings.Split, but commas
// inside a JSON array/object or a quoted string are NOT split
// points (the bug that made inline list<record> impossible).
func TestSplitTopLevelCommas(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a=1", []string{"a=1"}},
		{"a=1,b=2", []string{"a=1", "b=2"}},
		{`e=[{"s":"a","l":"A"},{"s":"b","l":"B"}],x=1`,
			[]string{`e=[{"s":"a","l":"A"},{"s":"b","l":"B"}]`, "x=1"}},
		{`q={"k":"has,comma"},y=2`, []string{`q={"k":"has,comma"}`, "y=2"}},
		{`s="a,b\"c",z=3`, []string{`s="a,b\"c"`, "z=3"}},
	}
	for _, c := range cases {
		got := splitTopLevelCommas(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitTopLevelCommas(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("splitTopLevelCommas(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestParseParamsArgJSON covers Gap B: a [/{-leading value is
// decoded as JSON (records flow through as typed maps/slices),
// non-JSON values stay raw strings for the declared-type coercer,
// and malformed-but-JSON-shaped values fail loudly rather than
// silently degrading to a string.
func TestParseParamsArgJSON(t *testing.T) {
	got, err := parseParamsArg(`entries=[{"slug":"a","label":"A","question":"Q?"}],effort=high`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s, ok := got["effort"].(string); !ok || s != "high" {
		t.Errorf("effort: got %#v, want string \"high\"", got["effort"])
	}
	list, ok := got["entries"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("entries: got %#v, want a 1-element slice", got["entries"])
	}
	rec, ok := list[0].(map[string]interface{})
	if !ok || rec["slug"] != "a" || rec["label"] != "A" || rec["question"] != "Q?" {
		t.Errorf("entries[0]: got %#v", list[0])
	}
	if _, err := parseParamsArg(`entries=[{"slug":"a"`); err == nil {
		t.Errorf("malformed JSON value: expected error, got nil")
	}
}

// TestLoadParamsFile / TestMergeParams pin the --params-file
// route (the clean path for list<record>) and the inline-wins
// merge order.
func TestLoadParamsFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "p.json")
	if err := os.WriteFile(good, []byte(`{"entries":[{"slug":"a"}],"n":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadParamsFile(good)
	if err != nil {
		t.Fatalf("loadParamsFile: %v", err)
	}
	if _, ok := m["entries"].([]interface{}); !ok {
		t.Errorf("entries not a slice: %#v", m["entries"])
	}
	if n, ok := m["n"].(float64); !ok || n != 3 {
		t.Errorf("n: got %#v, want float64(3) (MCP-shaped numeric)", m["n"])
	}

	bad := filepath.Join(dir, "arr.json")
	_ = os.WriteFile(bad, []byte(`[1,2]`), 0o644)
	if _, err := loadParamsFile(bad); err == nil {
		t.Errorf("top-level array: expected error, got nil")
	}
	if _, err := loadParamsFile(filepath.Join(dir, "nope.json")); err == nil {
		t.Errorf("missing file: expected error, got nil")
	}
}

func TestMergeParams(t *testing.T) {
	if got := mergeParams(nil, nil); got != nil {
		t.Errorf("mergeParams(nil,nil) = %v, want nil", got)
	}
	got := mergeParams(
		map[string]interface{}{"a": 1, "b": 2},
		map[string]interface{}{"b": 99},
	)
	if got["a"] != 1 || got["b"] != 99 {
		t.Errorf("mergeParams: got %v, want a=1 b=99 (hi wins)", got)
	}
}

// TestProjectRootCandidateFindsGit places a workflow under a
// directory tree with .git two levels up; the candidate should
// walk up to the git root rather than picking the workflow's
// immediate parent.
func TestProjectRootCandidateFindsGit(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	got := projectRootCandidate(filepath.Join(deep, "wf.yaml"))
	if got != dir {
		t.Fatalf("expected %s, got %s", dir, got)
	}
}

func TestProjectRootCandidateFallsBackToParent(t *testing.T) {
	dir := t.TempDir()
	got := projectRootCandidate(filepath.Join(dir, "wf.yaml"))
	if got != dir {
		t.Fatalf("expected fallback %s, got %s", dir, got)
	}
}

func TestRunIdentityFromCreateResponse(t *testing.T) {
	data := []byte(`{"seq":3,"id":42,"branch":"foo-3"}`)
	seq, id, err := runIdentityFromCreateResponse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seq != 3 || id != 42 {
		t.Fatalf("got seq=%d id=%d, want 3,42", seq, id)
	}
}

func TestRunIdentityFromCreateResponseHandlesGarbage(t *testing.T) {
	seq, id, err := runIdentityFromCreateResponse([]byte("not json"))
	if err == nil {
		t.Fatalf("expected error for malformed JSON, got seq=%d id=%d", seq, id)
	}
}

// TestResolveOrRegisterProjectAlreadyRegistered covers the
// happy path: a registry entry covers the workflow's
// directory, so resolveOrRegisterProject returns it WITHOUT
// calling CreateProject (which would need a live coord). The
// auto-register branch isn't testable here without an httptest
// coord stub — covered by the manual smoke run against the
// real coord in the merge commit's PR.
//
// FRAGILITY: the service.New(Config{ProjectRegistry: reg})
// construction below is intentionally minimal — no coord,
// no workspace, no logger. Today service.New tolerates this
// because the registry-lookup path doesn't reach into those
// dependencies. If service.New ever starts requiring more
// fields at construction, these tests will surface the
// regression early; the alternative (building a full
// FatClient with httptest coord) is worth the cost only when
// that regression actually fires.
func TestResolveOrRegisterProjectAlreadyRegistered(t *testing.T) {
	projectRoot := t.TempDir()
	workflowPath := filepath.Join(projectRoot, "enju.yaml")
	if err := os.WriteFile(workflowPath, []byte("name: t\nversion: 1\ntasks: []\n"), 0644); err != nil {
		t.Fatal(err)
	}

	regPath := filepath.Join(t.TempDir(), "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{
		ID:          42,
		LocalPath:   projectRoot,
		Name:        "fixture",
		LastTouched: time.Now(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Construct a FatClient with no coord — the registry
	// lookup path doesn't touch coord, so nil is safe for
	// this test slice. WorkspaceRoot="" disables enjugit
	// init; ProjectRegistry attaches our fixture.
	fc := service.New(service.Config{
		ProjectRegistry: reg,
	})
	sess := &cliSession{FC: fc, URL: "http://stub"}

	gotID, gotRoot, err := resolveOrRegisterProject(context.Background(), sess, workflowPath, "", true)
	if err != nil {
		t.Fatalf("resolveOrRegisterProject: %v", err)
	}
	if gotID != 42 {
		t.Errorf("project id: got %d, want 42", gotID)
	}
	if gotRoot != projectRoot {
		t.Errorf("project root: got %s, want %s", gotRoot, projectRoot)
	}
}

// TestResolveOrRegisterProjectNestedPrefersDeeper exercises the
// nested-project case: workflow lives under a deeper registered
// path, so the deeper project (not the outer one) wins. Pure
// registry resolution; no CreateProject call.
func TestResolveOrRegisterProjectNestedPrefersDeeper(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "nested")
	if err := os.MkdirAll(inner, 0755); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(inner, "enju.yaml")
	if err := os.WriteFile(workflowPath, []byte("name: t\nversion: 1\ntasks: []\n"), 0644); err != nil {
		t.Fatal(err)
	}

	regPath := filepath.Join(t.TempDir(), "projects.json")
	reg := projectreg.Open(regPath)
	for _, e := range []projectreg.Entry{
		{ID: 1, LocalPath: outer, Name: "outer", LastTouched: time.Now()},
		{ID: 2, LocalPath: inner, Name: "inner", LastTouched: time.Now()},
	} {
		if err := reg.Upsert(e); err != nil {
			t.Fatalf("upsert %d: %v", e.ID, err)
		}
	}

	fc := service.New(service.Config{ProjectRegistry: reg})
	sess := &cliSession{FC: fc, URL: "http://stub"}

	gotID, _, err := resolveOrRegisterProject(context.Background(), sess, workflowPath, "", true)
	if err != nil {
		t.Fatalf("resolveOrRegisterProject: %v", err)
	}
	if gotID != 2 {
		t.Errorf("expected deeper project id=2, got %d", gotID)
	}
}

func TestRunIdentityFromCreateResponseMissingFields(t *testing.T) {
	// Valid JSON, no seq/id — caller distinguishes "decoded but
	// fields absent" (returns zeros, nil error) from "wire
	// malformed" (returns error). Tests pin the contract so a
	// future refactor doesn't conflate the two.
	seq, id, err := runIdentityFromCreateResponse([]byte(`{"branch":"foo-3"}`))
	if err != nil {
		t.Fatalf("unexpected error for well-formed JSON: %v", err)
	}
	if seq != 0 || id != 0 {
		t.Fatalf("expected zeros for missing fields, got seq=%d id=%d", seq, id)
	}
}

// TestShouldRenderPoll covers the --auto-agents poll-dedup logic.
// A long-running bot turn (5+ minute review) would otherwise
// spam stdout with 150 identical "next gate: <task_id>" lines
// at the 2s poll cadence. Render only when something changed.
func TestShouldRenderPoll(t *testing.T) {
	cases := []struct {
		name        string
		res         *service.ExecuteRunResult
		lastStop    string
		lastBlocker string
		want        bool
	}{
		{
			name: "entries present always renders",
			res: &service.ExecuteRunResult{
				Entries:    []service.ExecuteRunEntry{{TaskID: "1:task_a", Status: "ok"}},
				StopReason: service.StopNoReadyCompute,
			},
			want: true,
		},
		{
			name:     "stop reason changed renders",
			res:      &service.ExecuteRunResult{StopReason: service.StopCitizenTaskReady},
			lastStop: service.StopNoReadyCompute,
			want:     true,
		},
		{
			name: "blocker shifted renders",
			res: &service.ExecuteRunResult{
				StopReason: service.StopCitizenTaskReady,
				Blocker:    &service.ExecuteRunBlocker{TaskID: "1:gate_b"},
			},
			lastStop:    service.StopCitizenTaskReady,
			lastBlocker: "1:gate_a",
			want:        true,
		},
		{
			name: "same stop + same blocker + no entries suppresses",
			res: &service.ExecuteRunResult{
				StopReason: service.StopCitizenTaskReady,
				Blocker:    &service.ExecuteRunBlocker{TaskID: "1:gate_a"},
			},
			lastStop:    service.StopCitizenTaskReady,
			lastBlocker: "1:gate_a",
			want:        false,
		},
		{
			name: "first iteration renders (no prior state)",
			res: &service.ExecuteRunResult{
				StopReason: service.StopCitizenTaskReady,
				Blocker:    &service.ExecuteRunBlocker{TaskID: "1:gate_a"},
			},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRenderPoll(c.res, c.lastStop, c.lastBlocker); got != c.want {
				t.Errorf("shouldRenderPoll: got %v, want %v", got, c.want)
			}
		})
	}
}

// TestValidatePublishFlag pins the allowed publish mode values.
func TestValidatePublishFlag(t *testing.T) {
	for _, v := range []string{"", "none", "local", "push"} {
		if err := validatePublishFlag(v); err != nil {
			t.Errorf("validatePublishFlag(%q): expected nil, got %v", v, err)
		}
	}
	for _, v := range []string{"psh", "PUSH", "Local", "merge", "fast-forward", "auto", "1"} {
		if err := validatePublishFlag(v); err == nil {
			t.Errorf("validatePublishFlag(%q): expected error, got nil", v)
		}
	}
}

func TestNormalizeParallelFlag(t *testing.T) {
	// <1 clamps to serial (1) without erroring — the operator who
	// passes 0 or a negative gets the default behavior, not a hard
	// stop. The unset default (1) and any value up to the cap pass
	// through unchanged.
	for _, in := range []int{-5, 0, 1} {
		got, err := normalizeParallelFlag(in)
		if err != nil {
			t.Errorf("normalizeParallelFlag(%d): unexpected error %v", in, err)
		}
		if in >= 1 && got != in {
			t.Errorf("normalizeParallelFlag(%d): got %d, want %d", in, got, in)
		}
		if in < 1 && got != 1 {
			t.Errorf("normalizeParallelFlag(%d): got %d, want clamp to 1", in, got)
		}
	}
	if got, err := normalizeParallelFlag(service.MaxParallel); err != nil || got != service.MaxParallel {
		t.Errorf("normalizeParallelFlag(%d): got (%d,%v), want (%d,nil) — the cap itself is allowed", service.MaxParallel, got, err, service.MaxParallel)
	}
	// Above the cap is a usage error (returns 0 so cmdGo exits 2),
	// matching the MCP enju_execute_run ceiling rather than silently
	// throttling.
	if _, err := normalizeParallelFlag(service.MaxParallel + 1); err == nil {
		t.Errorf("normalizeParallelFlag(%d): expected an over-cap error, got nil", service.MaxParallel+1)
	}
}
