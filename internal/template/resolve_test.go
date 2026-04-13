package template

import (
	"sort"
	"testing"
)

func TestExtractReferences(t *testing.T) {
	prompt := "Use {{foundation.content}} and {{genes.gene_list}} to analyze {{disease}}"
	refs := ExtractReferences(prompt)

	// Should only find task references (with dot), not bare {{disease}}
	if len(refs) != 2 {
		t.Fatalf("expected 2 references, got %d: %v", len(refs), refs)
	}
	if refs[0].TaskID != "foundation" || refs[0].Field != "content" {
		t.Fatalf("expected foundation.content, got %s.%s", refs[0].TaskID, refs[0].Field)
	}
	if refs[1].TaskID != "genes" || refs[1].Field != "gene_list" {
		t.Fatalf("expected genes.gene_list, got %s.%s", refs[1].TaskID, refs[1].Field)
	}
}

func TestInferDependencies(t *testing.T) {
	prompt := `Analyze using:
		{{foundation.content}}
		{{genes.gene_list}}
		{{genes.protein_families}}
		{{expression.content}}`

	deps := InferDependencies(prompt)
	sort.Strings(deps)

	expected := []string{"expression", "foundation", "genes"}
	if len(deps) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, deps)
	}
	for i, d := range expected {
		if deps[i] != d {
			t.Fatalf("expected %v, got %v", expected, deps)
		}
	}
}

func TestInferNoDeps(t *testing.T) {
	prompt := "Analyze {{disease}} for drug targets"
	deps := InferDependencies(prompt)
	if len(deps) != 0 {
		t.Fatalf("expected no dependencies, got %v", deps)
	}
}

func TestResolveParams(t *testing.T) {
	prompt := "Analyze {{disease}} in {{tissue}} context"
	params := map[string]string{
		"disease": "endometriosis",
		"tissue":  "uterine",
	}

	resolved := ResolveParams(prompt, params)
	expected := "Analyze endometriosis in uterine context"
	if resolved != expected {
		t.Fatalf("expected %q, got %q", expected, resolved)
	}
}

func TestResolveParamsLeavesTaskRefs(t *testing.T) {
	prompt := "Analyze {{disease}} using {{foundation.content}}"
	params := map[string]string{"disease": "PCOS"}

	resolved := ResolveParams(prompt, params)
	expected := "Analyze PCOS using {{foundation.content}}"
	if resolved != expected {
		t.Fatalf("expected %q, got %q", expected, resolved)
	}
}

func TestResolveUpstreamSimple(t *testing.T) {
	prompt := "Based on: {{research.content}}"
	inputs := map[string]interface{}{
		"research": map[string]interface{}{
			"content": "The research found three key genes.",
			"task_id": "research",
		},
	}

	resolved := ResolveUpstream(prompt, inputs)
	expected := "Based on: The research found three key genes."
	if resolved != expected {
		t.Fatalf("expected %q, got %q", expected, resolved)
	}
}

func TestResolveUpstreamNamedOutput(t *testing.T) {
	prompt := "Genes: {{analysis.gene_list}}\nPathways: {{analysis.pathways}}"
	inputs := map[string]interface{}{
		"analysis": map[string]interface{}{
			"content": map[string]interface{}{
				"gene_list": "BRCA1, TP53, EGFR",
				"pathways":  "KEGG:hsa04110, KEGG:hsa04115",
			},
			"task_id": "analysis",
		},
	}

	resolved := ResolveUpstream(prompt, inputs)
	if !contains(resolved, "BRCA1, TP53, EGFR") {
		t.Fatalf("expected gene_list resolved, got %q", resolved)
	}
	if !contains(resolved, "KEGG:hsa04110") {
		t.Fatalf("expected pathways resolved, got %q", resolved)
	}
}

func TestResolveUpstreamMissing(t *testing.T) {
	prompt := "Based on: {{missing_task.content}}"
	inputs := map[string]interface{}{}

	resolved := ResolveUpstream(prompt, inputs)
	// Should leave placeholder when upstream not found
	if resolved != prompt {
		t.Fatalf("expected unresolved placeholder, got %q", resolved)
	}
}

func TestResolveParamsLeavesBareUnknown(t *testing.T) {
	prompt := "Hello {{name}}, analyze {{unknown_param}}"
	params := map[string]string{"name": "tamer"}

	resolved := ResolveParams(prompt, params)
	expected := "Hello tamer, analyze {{unknown_param}}"
	if resolved != expected {
		t.Fatalf("expected %q, got %q", expected, resolved)
	}
}

func TestMergeDependencies(t *testing.T) {
	explicit := []string{"task_a"}
	prompt := "Use {{task_a.content}} and {{task_b.content}} and {{task_c.output}}"

	merged := MergeDependencies(explicit, prompt)
	sort.Strings(merged)

	expected := []string{"task_a", "task_b", "task_c"}
	if len(merged) != 3 {
		t.Fatalf("expected %v, got %v", expected, merged)
	}
	for i, d := range expected {
		if merged[i] != d {
			t.Fatalf("expected %v, got %v", expected, merged)
		}
	}
}

func TestMergeDependenciesNoDuplicates(t *testing.T) {
	explicit := []string{"foundation", "genes"}
	prompt := "Use {{foundation.content}} and {{genes.content}}"

	merged := MergeDependencies(explicit, prompt)
	if len(merged) != 2 {
		t.Fatalf("expected 2 deps (no duplicates), got %v", merged)
	}
}

func TestListParams(t *testing.T) {
	prompt := "Analyze {{disease}} in {{tissue}} using {{foundation.content}}"
	params := ListParams(prompt)
	sort.Strings(params)

	expected := []string{"disease", "tissue"}
	if len(params) != 2 {
		t.Fatalf("expected %v, got %v", expected, params)
	}
}

func TestFullResolveFlow(t *testing.T) {
	// Simulate the full flow: params first, then upstream
	prompt := "Analyze {{disease}} using genes: {{foundation.content}}"

	// Step 1: resolve for_each params at creation time
	step1 := ResolveParams(prompt, map[string]string{"disease": "endometriosis"})
	expected1 := "Analyze endometriosis using genes: {{foundation.content}}"
	if step1 != expected1 {
		t.Fatalf("step1: expected %q, got %q", expected1, step1)
	}

	// Step 2: resolve upstream at claim time
	inputs := map[string]interface{}{
		"foundation": map[string]interface{}{
			"content": "BRCA1, TP53",
			"task_id": "foundation",
		},
	}
	step2 := ResolveUpstream(step1, inputs)
	expected2 := "Analyze endometriosis using genes: BRCA1, TP53"
	if step2 != expected2 {
		t.Fatalf("step2: expected %q, got %q", expected2, step2)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
