package enjugit

import (
	"strings"
	"testing"
)

func TestResolve_NoTemplateRefs(t *testing.T) {
	wf, _ := makeWorkflow(t)
	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Hello world",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out.Prompt != "Hello world" {
		t.Errorf("plain prompt should pass through, got %q", out.Prompt)
	}
}

func TestResolve_ForEachParamSubstitution(t *testing.T) {
	wf, _ := makeWorkflow(t)
	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Analyze {{gene}} for variants",
		ForEachParams:  map[string]string{"gene": "BRCA1"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(out.Prompt, "BRCA1") {
		t.Errorf("for_each substitution failed, got %q", out.Prompt)
	}
}

func TestResolve_UpstreamContent_SingletonResultMd(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.readContent["abc:runs/1/foo/result.md"] = []byte("upstream output")

	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "Continue from: {{foo.content}}",
		Dependencies: []DependencyRef{{
			TaskDefID:  "foo",
			CommitSHA:  "abc",
			ResultPath: "runs/1/foo",
			State:      "accepted",
		}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(out.Prompt, "upstream output") {
		t.Errorf("expected upstream content inlined, got %q", out.Prompt)
	}
}

func TestResolve_SkippedUpstreamRendersMarker(t *testing.T) {
	wf, _ := makeWorkflow(t)
	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "x={{foo.content}}",
		Dependencies: []DependencyRef{{
			TaskDefID:  "foo",
			ResultPath: "runs/1/foo",
			State:      "skipped",
		}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(out.Prompt, "(skipped)") {
		t.Errorf("expected skipped marker, got %q", out.Prompt)
	}
}

func TestResolve_FailedUpstreamRendersMarker(t *testing.T) {
	wf, _ := makeWorkflow(t)
	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "{{foo.content}}",
		Dependencies: []DependencyRef{{
			TaskDefID:  "foo",
			ResultPath: "runs/1/foo",
			State:      "failed",
		}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(out.Prompt, "(failed)") {
		t.Errorf("expected failed marker, got %q", out.Prompt)
	}
}

func TestResolve_VoteWinningOption(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.readContent["abc:runs/1/vote/result.md"] = []byte("vote done")

	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "winner is {{vote.winning_option}}",
		Dependencies: []DependencyRef{{
			TaskDefID:  "vote",
			CommitSHA:  "abc",
			ResultPath: "runs/1/vote",
			State:      "accepted",
			VoteChoice: "option_a",
		}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(out.Prompt, "option_a") {
		t.Errorf("expected vote choice, got %q", out.Prompt)
	}
}

func TestResolve_FanInAggregation(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.readContent["sha1:runs/1/BRCA1/analyze/result.md"] = []byte("brca1 result")
	fake.readContent["sha2:runs/1/TP53/analyze/result.md"] = []byte("tp53 result")

	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "All findings:\n{{analyze.content}}",
		Dependencies: []DependencyRef{
			{
				TaskDefID:      "analyze",
				InstanceKey:    "BRCA1",
				InstanceParams: map[string]string{"gene": "BRCA1"},
				CommitSHA:      "sha1",
				ResultPath:     "runs/1/BRCA1/analyze",
				State:          "accepted",
			},
			{
				TaskDefID:      "analyze",
				InstanceKey:    "TP53",
				InstanceParams: map[string]string{"gene": "TP53"},
				CommitSHA:      "sha2",
				ResultPath:     "runs/1/TP53/analyze",
				State:          "accepted",
			},
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, want := range []string{"brca1 result", "tp53 result", "iteration: gene=BRCA1", "iteration: gene=TP53"} {
		if !strings.Contains(out.Prompt, want) {
			t.Errorf("fan-in missing %q in output:\n%s", want, out.Prompt)
		}
	}
}

func TestResolve_ArtifactSubstitution(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.readContent["abc:figures/fig1.csv"] = []byte("a,b,c\n1,2,3\n")

	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "data: {{artifact:figures/fig1.csv}}",
		ArtifactReads: []ArtifactRef{
			{Path: "figures/fig1.csv", CommitSHA: "abc"},
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(out.Prompt, "a,b,c") {
		t.Errorf("artifact content missing from prompt, got %q", out.Prompt)
	}
	if got := out.ResolvedArtifacts["figures/fig1.csv"]; !strings.Contains(got, "a,b,c") {
		t.Errorf("ResolvedArtifacts mapping missing entry, got %v", out.ResolvedArtifacts)
	}
}

func TestResolve_MissingArtifactSurfaced(t *testing.T) {
	wf, _ := makeWorkflow(t)
	out, err := wf.Resolve(ResolveInput{
		PromptTemplate: "data: {{artifact:nonexistent.csv}}",
		ArtifactReads: []ArtifactRef{
			{Path: "nonexistent.csv", CommitSHA: "abc"},
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(out.MissingArtifacts) != 1 || out.MissingArtifacts[0] != "nonexistent.csv" {
		t.Errorf("expected MissingArtifacts to include nonexistent.csv, got %v", out.MissingArtifacts)
	}
}

func TestResolve_UserPromptResolves(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.readContent["abc:runs/1/foo/result.md"] = []byte("up")

	out, err := wf.Resolve(ResolveInput{
		PromptTemplate:     "system: {{foo.content}}",
		UserPromptTemplate: "user: {{foo.content}}",
		Dependencies: []DependencyRef{{
			TaskDefID:  "foo",
			CommitSHA:  "abc",
			ResultPath: "runs/1/foo",
			State:      "accepted",
		}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(out.UserPrompt, "up") {
		t.Errorf("user prompt unresolved, got %q", out.UserPrompt)
	}
}

func TestResolve_ArtifactsOnlyUpstreamErrorIsFriendly(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// metadata.json exists but no result.md / result.json / output_files.
	fake.readContent["abc:runs/1/foo/metadata.json"] = []byte("{}")

	_, err := wf.Resolve(ResolveInput{
		PromptTemplate: "{{foo.content}}",
		Dependencies: []DependencyRef{{
			TaskDefID:  "foo",
			CommitSHA:  "abc",
			ResultPath: "runs/1/foo",
			State:      "accepted",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "artifacts-only submit") {
		t.Errorf("expected artifacts-only error, got %v", err)
	}
}

func TestFormatIterationLabel(t *testing.T) {
	got := formatIterationLabel(map[string]string{"gene": "BRCA1", "tissue": "breast"}, "BRCA1-breast")
	if got != "gene=BRCA1, tissue=breast" {
		t.Errorf("expected sorted multi-key label, got %q", got)
	}
	if got := formatIterationLabel(nil, "fallback"); got != "fallback" {
		t.Errorf("empty params should fall back to instance key, got %q", got)
	}
}
