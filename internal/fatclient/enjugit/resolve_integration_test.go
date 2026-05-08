package enjugit

// Real-bare integration tests for Workflow.Resolve. Uses a live
// bare git repo + ws.ForProject (a true clone) so Resolve reads
// committed bytes through the same go-git path production hits.
// The unit-level coverage in resolve_test.go uses fake ops; this
// file covers the end-to-end shape of "commit lands → Resolve
// reads it back via CommitSHA."

import (
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// resolveTestResultDir composes the canonical task-result path
// the resolver expects. instanceKey appended under task-def dir
// when non-empty; matches the layout the coordinator emits.
func resolveTestResultDir(runSeq int, instanceKey, taskDefID string) string {
	base := filepath.Join("enju", "runs",
		intToString(runSeq), taskDefID)
	if instanceKey != "" {
		return filepath.Join(base, instanceKey)
	}
	return base
}

// intToString avoids pulling fmt into helper-heavy tests.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// seedResultCommit lands a single result.md under
// enju/runs/{seq}/{taskDef}[/{instanceKey}]/ and returns the
// commit SHA. Wraps CommitArbitraryFiles so the resolve tests
// don't have to spell out the SubmitRequest/topic-branch dance
// just to seed a commit they'll read back through Resolve.
func seedResultCommit(t *testing.T, wf *Workflow, runSeq int, instanceKey, taskDef string, body []byte) string {
	t.Helper()
	resultDir := resolveTestResultDir(runSeq, instanceKey, taskDef)
	res, err := wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(resultDir, "result.md"), Content: body},
		},
		Subject:     "seed result for " + taskDef,
		AuthorName:  "T",
		AuthorEmail: "t@x",
	})
	if err != nil {
		t.Fatalf("seedResultCommit %s/%s: %v", taskDef, instanceKey, err)
	}
	return res.CommitSHA
}

// TestResolve_FanInIntegration covers the main non-trivial template
// resolution case end-to-end: a singleton aggregator task reads
// from multiple iterations of an upstream task and expects the
// Option 4 block format. Real bare → real clone → real commits →
// Resolve reads via go-git.
func TestResolve_FanInIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(46, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Commit three "analyze" results, one per gene, as three
	// separate commits — simulating three submits from different
	// iterations of a task-level for_each.
	genes := []string{"BRCA1", "TP53", "EGFR"}
	commitSHAs := make(map[string]string, len(genes))
	for _, g := range genes {
		commitSHAs[g] = seedResultCommit(t, wf, 1, g, "analyze",
			[]byte("analysis of "+g))
	}

	input := ResolveInput{
		PromptTemplate: "Summarize: {{analyze.content}}",
		Dependencies: []DependencyRef{
			{
				TaskDefID:      "analyze",
				InstanceKey:    "BRCA1",
				InstanceParams: map[string]string{"gene": "BRCA1"},
				CommitSHA:      commitSHAs["BRCA1"],
				ResultPath:     resolveTestResultDir(1, "BRCA1", "analyze"),
			},
			{
				TaskDefID:      "analyze",
				InstanceKey:    "TP53",
				InstanceParams: map[string]string{"gene": "TP53"},
				CommitSHA:      commitSHAs["TP53"],
				ResultPath:     resolveTestResultDir(1, "TP53", "analyze"),
			},
			{
				TaskDefID:      "analyze",
				InstanceKey:    "EGFR",
				InstanceParams: map[string]string{"gene": "EGFR"},
				CommitSHA:      commitSHAs["EGFR"],
				ResultPath:     resolveTestResultDir(1, "EGFR", "analyze"),
			},
		},
	}

	resolved, err := wf.Resolve(input)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The resolved prompt should contain the Option 4 block with
	// labeled iteration headers, sorted by instance key (BRCA1,
	// EGFR, TP53 alphabetically).
	wantHeaders := []string{
		"### iteration: gene=BRCA1",
		"### iteration: gene=EGFR",
		"### iteration: gene=TP53",
	}
	for _, h := range wantHeaders {
		if !strings.Contains(resolved.Prompt, h) {
			t.Errorf("resolved prompt missing header %q\nprompt: %s", h, resolved.Prompt)
		}
	}
	brcaIdx := strings.Index(resolved.Prompt, "BRCA1")
	egfrIdx := strings.Index(resolved.Prompt, "EGFR")
	tp53Idx := strings.Index(resolved.Prompt, "TP53")
	if brcaIdx > egfrIdx || egfrIdx > tp53Idx {
		t.Errorf("iteration order not sorted alphabetically: BRCA1=%d EGFR=%d TP53=%d", brcaIdx, egfrIdx, tp53Idx)
	}
	for _, g := range genes {
		if !strings.Contains(resolved.Prompt, "analysis of "+g) {
			t.Errorf("missing content for %s", g)
		}
	}
	if strings.Contains(resolved.Prompt, "{{analyze.content}}") {
		t.Error("placeholder left literal in resolved prompt")
	}
}

// TestResolve_SingletonUpstreamIntegration covers the non-fan-in
// path: a downstream reads a single upstream's content via
// {{task.content}}.
func TestResolve_SingletonUpstreamIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(47, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	commitSHA := seedResultCommit(t, wf, 1, "", "gather", []byte("raw data"))

	resolved, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Analyze this: {{gather.content}}",
		Dependencies: []DependencyRef{
			{
				TaskDefID:  "gather",
				CommitSHA:  commitSHA,
				ResultPath: resolveTestResultDir(1, "", "gather"),
			},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resolved.Prompt, "raw data") {
		t.Errorf("expected resolved prompt to contain upstream content; got: %q", resolved.Prompt)
	}
	if strings.Contains(resolved.Prompt, "{{") {
		t.Errorf("unresolved placeholder in prompt: %q", resolved.Prompt)
	}
}

// TestResolve_WinningOptionIntegration covers Phase E.2's
// {{task.winning_option}} accessor: an upstream vote task's
// VoteChoice gets surfaced on the dependency ref, the resolver
// attaches it to the result map, and the template substitution
// hits the top-level field lookup in extractField.
func TestResolve_WinningOptionIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(90, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// The voter's commentary is the result.md; the winning option
	// id rides along on the DependencyRef (mirroring what the
	// coordinator would populate from tasks.vote_choice).
	commitSHA := seedResultCommit(t, wf, 1, "", "pick_db",
		[]byte("DuckDB fits the workload best."))

	resolved, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Rationale for {{pick_db.winning_option}}: {{pick_db.content}}",
		Dependencies: []DependencyRef{
			{
				TaskDefID:  "pick_db",
				CommitSHA:  commitSHA,
				ResultPath: resolveTestResultDir(1, "", "pick_db"),
				VoteChoice: "duckdb",
			},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resolved.Prompt, "duckdb") {
		t.Errorf("expected winning_option to resolve to 'duckdb', got: %q", resolved.Prompt)
	}
	if !strings.Contains(resolved.Prompt, "DuckDB fits the workload best") {
		t.Errorf("expected commentary content to still resolve via {{task.content}}, got: %q", resolved.Prompt)
	}
	if strings.Contains(resolved.Prompt, "{{") {
		t.Errorf("unresolved placeholder: %q", resolved.Prompt)
	}
}

// TestResolve_ForEachParamsIntegration covers bare {{param}}
// substitution from the task's own for_each params over a real
// clone. (Doesn't actually need a remote-side commit — the
// substitution is in-memory — but the integration angle is
// "Resolve over a Workflow obtained from ForProject" instead of
// the fake-ops path.)
func TestResolve_ForEachParamsIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(48, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	resolved, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Analyze gene {{gene}} in tissue {{tissue}}",
		ForEachParams: map[string]string{
			"gene":   "BRCA1",
			"tissue": "breast",
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Prompt != "Analyze gene BRCA1 in tissue breast" {
		t.Errorf("unexpected resolved prompt: %q", resolved.Prompt)
	}
}

// TestResolve_ArtifactReadIntegration covers {{artifact:path}}
// inlining from an artifact's committed content. Seeds the
// artifact via a commit + reads it back via Resolve at that SHA.
func TestResolve_ArtifactReadIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(49, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	res, err := wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "writer"), "result.md"),
				Content:     []byte("done"),
			},
			{
				RepoRelPath: ArtifactPath("notes/intro.md"),
				Content:     []byte("# Intro\n\nThe intro content."),
			},
		},
		Subject:     "seed writer + artifact",
		AuthorName:  "T",
		AuthorEmail: "t@x",
	})
	if err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	resolved, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Context:\n{{artifact:notes/intro.md}}",
		ArtifactReads: []ArtifactRef{
			{Path: "notes/intro.md", CommitSHA: res.CommitSHA},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resolved.Prompt, "The intro content") {
		t.Errorf("expected artifact content in resolved prompt; got: %q", resolved.Prompt)
	}
	if _, ok := resolved.ResolvedArtifacts["notes/intro.md"]; !ok {
		t.Error("expected artifact to appear in ResolvedArtifacts map")
	}
	if len(resolved.MissingArtifacts) != 0 {
		t.Errorf("expected no missing artifacts, got %v", resolved.MissingArtifacts)
	}
}

// TestResolve_MissingArtifactIntegration covers the surface where
// a declared artifact can't be found at the given commit. The
// placeholder must survive in the prompt as a secondary visible
// signal, and the path goes into MissingArtifacts.
func TestResolve_MissingArtifactIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(50, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	resolved, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Context: {{artifact:missing.md}}",
		ArtifactReads: []ArtifactRef{
			{Path: "missing.md"}, // no commit SHA, file doesn't exist
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved.MissingArtifacts) != 1 || resolved.MissingArtifacts[0] != "missing.md" {
		t.Errorf("expected missing.md in MissingArtifacts, got %v", resolved.MissingArtifacts)
	}
	if !strings.Contains(resolved.Prompt, "{{artifact:missing.md}}") {
		t.Errorf("expected placeholder to survive for missing artifact; got: %q", resolved.Prompt)
	}
}

// _ keeps the gogit import live in case future resolve tests need
// to verify commit graph state directly. Without it, removing the
// import here forces editing the seed helper which already uses
// CommitArbitraryFiles' result. Cheap insurance for the next
// scenario port.
var _ = gogit.PlainClone
