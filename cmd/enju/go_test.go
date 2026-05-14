package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

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

// TestShouldRenderPoll covers the --auto-bots poll-dedup logic.
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
