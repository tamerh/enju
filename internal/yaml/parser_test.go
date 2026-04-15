package yaml

import (
	"sort"
	"testing"
)

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}

func TestParseSimple(t *testing.T) {
	yamlData := []byte(`
name: "Test Run"
version: 1
tasks:
  - id: step1
    action: answer
    prompt: "Do step 1"
  - id: step2
    action: answer
    depends_on: [step1]
    prompt: "Do step 2 with {{step1.content}}"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Run.Name != "Test Run" {
		t.Fatalf("expected name 'Test Run', got %q", parsed.Run.Name)
	}
	if parsed.DAG.NodeCount() != 2 {
		t.Fatalf("expected 2 nodes, got %d", parsed.DAG.NodeCount())
	}

	roots := parsed.DAG.Roots()
	if len(roots) != 1 || roots[0] != "step1" {
		t.Fatalf("expected root [step1], got %v", roots)
	}
}

func TestParseForEach(t *testing.T) {
	yamlData := []byte(`
name: "Disease Analysis"
version: 1
for_each:
  disease: [endometriosis, PCOS, adenomyosis]
tasks:
  - id: foundation
    action: answer
    prompt: "Analyze {{disease}}"
  - id: synthesis
    action: answer
    depends_on: [foundation]
    prompt: "Synthesize {{foundation.content}}"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	// 3 diseases × 2 tasks = 6 nodes
	if parsed.DAG.NodeCount() != 6 {
		t.Fatalf("expected 6 nodes, got %d", parsed.DAG.NodeCount())
	}

	// 3 instances
	if len(parsed.ExpandedTasks) != 3 {
		t.Fatalf("expected 3 instances, got %d", len(parsed.ExpandedTasks))
	}

	// Check that each instance has proper full IDs
	for _, inst := range parsed.ExpandedTasks["endometriosis"] {
		if inst.InstanceKey != "endometriosis" {
			t.Fatalf("expected instance key 'endometriosis', got %q", inst.InstanceKey)
		}
		if inst.Params["disease"] != "endometriosis" {
			t.Fatalf("expected param disease=endometriosis, got %q", inst.Params["disease"])
		}
	}

	// Check DAG structure: endometriosis:foundation -> endometriosis:synthesis
	children := parsed.DAG.Children("endometriosis:foundation")
	if len(children) != 1 || children[0] != "endometriosis:synthesis" {
		t.Fatalf("expected children [endometriosis:synthesis], got %v", children)
	}

	// Instances should be independent — no edges between diseases
	descEndo := parsed.DAG.Descendants("endometriosis:foundation")
	sort.Strings(descEndo)
	if len(descEndo) != 1 || descEndo[0] != "endometriosis:synthesis" {
		t.Fatalf("endometriosis descendants should only include endometriosis tasks, got %v", descEndo)
	}

	// 3 roots (one per disease)
	roots := parsed.DAG.Roots()
	if len(roots) != 3 {
		t.Fatalf("expected 3 roots, got %v", roots)
	}
}

func TestParseBranchingDAG(t *testing.T) {
	yamlData := []byte(`
name: "Branching Test"
version: 1
tasks:
  - id: root
    action: answer
    prompt: "Start"
  - id: branch_a
    action: answer
    depends_on: [root]
    prompt: "Branch A"
  - id: branch_b
    action: answer
    depends_on: [root]
    prompt: "Branch B"
  - id: merge
    action: answer
    depends_on: [branch_a, branch_b]
    prompt: "Merge {{branch_a.content}} and {{branch_b.content}}"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	if parsed.DAG.NodeCount() != 4 {
		t.Fatalf("expected 4 nodes, got %d", parsed.DAG.NodeCount())
	}

	// root -> branch_a, branch_b -> merge
	ready := parsed.DAG.ReadyNodes(map[string]bool{})
	if len(ready) != 1 || ready[0] != "root" {
		t.Fatalf("expected [root] ready, got %v", ready)
	}

	ready = parsed.DAG.ReadyNodes(map[string]bool{"root": true})
	sort.Strings(ready)
	if len(ready) != 2 || ready[0] != "branch_a" || ready[1] != "branch_b" {
		t.Fatalf("expected [branch_a, branch_b] ready, got %v", ready)
	}

	// merge needs both branches
	ready = parsed.DAG.ReadyNodes(map[string]bool{"root": true, "branch_a": true})
	if len(ready) != 1 || ready[0] != "branch_b" {
		t.Fatalf("expected [branch_b] ready (merge still blocked), got %v", ready)
	}

	ready = parsed.DAG.ReadyNodes(map[string]bool{"root": true, "branch_a": true, "branch_b": true})
	if len(ready) != 1 || ready[0] != "merge" {
		t.Fatalf("expected [merge] ready, got %v", ready)
	}
}

func TestParseScriptTask(t *testing.T) {
	yamlData := []byte(`
name: "Script Test"
version: 1
tasks:
  - id: fetch_data
    action: compute
    script: scripts/fetch.py
    script_source: predefined
  - id: analyze
    action: answer
    depends_on: [fetch_data]
    prompt: "Analyze {{fetch_data.content}}"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	if parsed.DAG.NodeCount() != 2 {
		t.Fatalf("expected 2 nodes, got %d", parsed.DAG.NodeCount())
	}
}

func TestParseContributeAction(t *testing.T) {
	yamlData := []byte(`
name: "Deliberation"
version: 1
tasks:
  - id: gather_position
    action: contribute
    user_prompt: "What is your position?"
    prompt: "Structure this input: {{user_input}}"
  - id: synthesize
    action: answer
    prompt: "Synthesize: {{gather_position.content}}"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	tasks := parsed.ExpandedTasks[""]
	if tasks[0].Action != "contribute" {
		t.Fatalf("expected contribute action, got %q", tasks[0].Action)
	}
	if tasks[0].UserPrompt == "" {
		t.Fatal("expected user_prompt to be set")
	}
}

func TestInferredDependencies(t *testing.T) {
	// No depends_on — dependencies inferred from template references
	yamlData := []byte(`
name: "Inferred Deps"
version: 1
tasks:
  - id: research
    action: answer
    prompt: "Research the topic"
  - id: analysis
    action: answer
    prompt: "Analyze: {{research.content}}"
  - id: synthesis
    action: answer
    prompt: |
      Combine research and analysis:
      Research: {{research.content}}
      Analysis: {{analysis.content}}
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	// research has no deps — should be root
	roots := parsed.DAG.Roots()
	if len(roots) != 1 || roots[0] != "research" {
		t.Fatalf("expected root [research], got %v", roots)
	}

	// analysis depends on research (inferred)
	parents := parsed.DAG.Parents("analysis")
	if len(parents) != 1 || parents[0] != "research" {
		t.Fatalf("expected analysis parents [research], got %v", parents)
	}

	// synthesis depends on both research and analysis (inferred)
	synthParents := sorted(parsed.DAG.Parents("synthesis"))
	if len(synthParents) != 2 || synthParents[0] != "analysis" || synthParents[1] != "research" {
		t.Fatalf("expected synthesis parents [analysis, research], got %v", synthParents)
	}

	// Verify execution order works
	ready := parsed.DAG.ReadyNodes(map[string]bool{})
	if len(ready) != 1 || ready[0] != "research" {
		t.Fatalf("expected [research] ready, got %v", ready)
	}

	ready = parsed.DAG.ReadyNodes(map[string]bool{"research": true})
	if len(ready) != 1 || ready[0] != "analysis" {
		t.Fatalf("expected [analysis] ready, got %v", ready)
	}

	// synthesis needs both
	ready = parsed.DAG.ReadyNodes(map[string]bool{"research": true, "analysis": true})
	if len(ready) != 1 || ready[0] != "synthesis" {
		t.Fatalf("expected [synthesis] ready, got %v", ready)
	}
}

func TestForEachParamResolution(t *testing.T) {
	yamlData := []byte(`
name: "Param Resolution"
version: 1
for_each:
  disease: [endometriosis, PCOS]
tasks:
  - id: analyze
    action: answer
    prompt: "Analyze {{disease}} for drug targets"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	// Check that {{disease}} was resolved at creation time
	for _, tasks := range parsed.ExpandedTasks {
		for _, task := range tasks {
			if task.InstanceKey == "endometriosis" {
				if task.Prompt != "Analyze endometriosis for drug targets" {
					t.Fatalf("expected resolved prompt for endometriosis, got %q", task.Prompt)
				}
			}
			if task.InstanceKey == "PCOS" {
				if task.Prompt != "Analyze PCOS for drug targets" {
					t.Fatalf("expected resolved prompt for PCOS, got %q", task.Prompt)
				}
			}
		}
	}
}

func TestInferredDepsWithForEach(t *testing.T) {
	yamlData := []byte(`
name: "Inferred + ForEach"
version: 1
for_each:
  disease: [endo, PCOS]
tasks:
  - id: foundation
    action: answer
    prompt: "Analyze {{disease}}"
  - id: genes
    action: answer
    prompt: "Map genes for {{disease}} using {{foundation.content}}"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	// 2 diseases × 2 tasks = 4 nodes
	if parsed.DAG.NodeCount() != 4 {
		t.Fatalf("expected 4 nodes, got %d", parsed.DAG.NodeCount())
	}

	// endo:genes depends on endo:foundation (inferred + scoped to instance)
	parents := parsed.DAG.Parents("endo:genes")
	if len(parents) != 1 || parents[0] != "endo:foundation" {
		t.Fatalf("expected [endo:foundation], got %v", parents)
	}

	// Check prompts are resolved
	for _, task := range parsed.ExpandedTasks["endo"] {
		if task.ID == "foundation" && task.Prompt != "Analyze endo" {
			t.Fatalf("expected resolved prompt, got %q", task.Prompt)
		}
		if task.ID == "genes" {
			// {{disease}} resolved, {{foundation.content}} left for claim time
			expected := "Map genes for endo using {{foundation.content}}"
			if task.Prompt != expected {
				t.Fatalf("expected %q, got %q", expected, task.Prompt)
			}
		}
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		err  string
	}{
		{
			name: "missing name",
			yaml: `tasks: [{id: a, action: answer, prompt: "x"}]`,
			err:  "name is required",
		},
		{
			name: "no tasks",
			yaml: `name: "test"`,
			err:  "at least one task",
		},
		{
			name: "missing task id",
			yaml: `name: "test"
tasks: [{action: answer, prompt: "x"}]`,
			err: "task ID is required",
		},
		{
			name: "duplicate task id",
			yaml: `name: "test"
tasks:
  - {id: a, action: answer, prompt: "x"}
  - {id: a, action: answer, prompt: "y"}`,
			err: "duplicate task ID",
		},
		{
			name: "invalid action",
			yaml: `name: "test"
tasks: [{id: a, action: invalid, prompt: "x"}]`,
			err: "invalid action",
		},
		{
			name: "missing prompt",
			yaml: `name: "test"
tasks: [{id: a, action: answer}]`,
			err: "prompt is required",
		},
		{
			name: "missing script",
			yaml: `name: "test"
tasks: [{id: a, action: compute}]`,
			err: "script is required",
		},
		{
			name: "bad dependency",
			yaml: `name: "test"
tasks:
  - {id: a, action: answer, prompt: "x", depends_on: [nonexistent]}`,
			err: "does not exist",
		},
		{
			name: "cycle",
			yaml: `name: "test"
tasks:
  - {id: a, action: answer, prompt: "x", depends_on: [b]}
  - {id: b, action: answer, prompt: "y", depends_on: [a]}`,
			err: "cycle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.err)
			}
			if !contains(err.Error(), tt.err) {
				t.Fatalf("expected error containing %q, got %q", tt.err, err.Error())
			}
		})
	}
}

// --- Task-level for_each (iteration 5) ---

func TestParseTaskLevelForEach(t *testing.T) {
	yamlData := []byte(`
name: "Gene Analysis"
version: 1
tasks:
  - id: analyze
    action: answer
    for_each:
      gene: [BRCA1, TP53, EGFR]
    prompt: "Analyze {{gene}}."
  - id: report
    action: answer
    prompt: "Summarize findings: {{analyze.content}}"
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	// 3 analyze instances + 1 report = 4 nodes
	if parsed.DAG.NodeCount() != 4 {
		t.Fatalf("expected 4 nodes, got %d", parsed.DAG.NodeCount())
	}

	// report is a singleton under key ""
	singletons := parsed.ExpandedTasks[""]
	if len(singletons) != 1 || singletons[0].TaskDef.ID != "report" {
		t.Fatalf("expected singleton 'report', got %v", singletons)
	}
	report := singletons[0]

	// report must depend on all 3 analyze instances (fan-in)
	if len(report.DependsOn) != 3 {
		t.Fatalf("expected report to depend on 3 analyze instances, got %v", report.DependsOn)
	}
	depSet := map[string]bool{}
	for _, d := range report.DependsOn {
		depSet[d] = true
	}
	for _, gene := range []string{"BRCA1", "TP53", "EGFR"} {
		want := "" + gene + ":analyze"
		if !depSet[want] {
			t.Fatalf("expected report to depend on %q, got %v", want, report.DependsOn)
		}
	}

	// analyze instances are expanded with their own prompt substitution
	for _, gene := range []string{"BRCA1", "TP53", "EGFR"} {
		list := parsed.ExpandedTasks[gene]
		if len(list) != 1 {
			t.Fatalf("expected 1 task under iteration %s, got %d", gene, len(list))
		}
		if list[0].Prompt != "Analyze "+gene+"." {
			t.Fatalf("expected prompt 'Analyze %s.', got %q", gene, list[0].Prompt)
		}
	}
}

func TestParseTaskLevelForEachFanOut(t *testing.T) {
	yamlData := []byte(`
name: "Fan-out from singleton"
version: 1
tasks:
  - id: setup
    action: answer
    prompt: "Prepare the context."
  - id: analyze
    action: answer
    for_each:
      gene: [BRCA1, TP53]
    prompt: "Using {{setup.content}}, analyze {{gene}}."
`)

	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}

	// Every analyze iteration should depend on the singleton setup
	for _, gene := range []string{"BRCA1", "TP53"} {
		inst := parsed.ExpandedTasks[gene][0]
		if len(inst.DependsOn) != 1 || inst.DependsOn[0] != "setup" {
			t.Fatalf("expected %s:analyze to depend on [setup], got %v", gene, inst.DependsOn)
		}
	}
}

func TestParseRejectsRunLevelAndTaskLevelBoth(t *testing.T) {
	yamlData := []byte(`
name: "Mixed"
version: 1
for_each:
  name: [A, B]
tasks:
  - id: greet
    action: answer
    for_each:
      role: [admin, user]
    prompt: "Hi {{name}} {{role}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting run-level + task-level for_each together")
	}
}

func TestParseRejectsDifferingTaskLevelForEach(t *testing.T) {
	yamlData := []byte(`
name: "Conflicting task-level for_each"
version: 1
tasks:
  - id: a
    action: answer
    for_each:
      x: [1, 2]
    prompt: "A {{x}}"
  - id: b
    action: answer
    for_each:
      x: [1, 2, 3]
    prompt: "B {{x}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error for differing task-level for_each groups")
	}
}

// --- Strict for_each validation (iteration 5 bugs 2/3/4) ---

func TestParseRejectsEmptyForEachList(t *testing.T) {
	yamlData := []byte(`
name: "Empty list"
version: 1
for_each:
  name: []
tasks:
  - id: greet
    action: answer
    prompt: "Hi {{name}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting empty for_each list")
	}
}

func TestParseRejectsUndefinedTemplateVariable(t *testing.T) {
	yamlData := []byte(`
name: "Typo"
version: 1
for_each:
  name: [Alice]
tasks:
  - id: greet
    action: answer
    prompt: "Hi {{name}} — also mention {{oops}}."
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting undefined variable {{oops}}")
	}
}

func TestParseRejectsUnusedForEachVariable(t *testing.T) {
	yamlData := []byte(`
name: "Unused variable"
version: 1
for_each:
  name: [Alice, Bob]
  unused: [x, y]
tasks:
  - id: greet
    action: answer
    prompt: "Hi {{name}}."
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting unused for_each variable 'unused'")
	}
}

func TestParseRejectsUnknownTaskReference(t *testing.T) {
	yamlData := []byte(`
name: "Bad task ref"
version: 1
tasks:
  - id: one
    action: answer
    prompt: "See {{missing.content}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting unknown task id in {{missing.content}}")
	}
}

// TestParseReviewAction covers the Phase E review-action validator:
// required reviews: field, target-must-exist, self-reference
// rejection, auto-inserted dep edge, and the non-review task
// rejecting a stray reviews: field.
func TestParseReviewAction(t *testing.T) {
	t.Run("happy path auto-inserts dep", func(t *testing.T) {
		parsed, err := Parse([]byte(`
name: "Review happy"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write a summary."
  - id: check
    action: review
    reviews: draft
    prompt: "Approve or reject."
`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		// Review task's depends_on should include the target even
		// though the YAML didn't list it.
		var checkDeps []string
		for _, tk := range parsed.Run.Tasks {
			if tk.ID == "check" {
				checkDeps = tk.DependsOn
				break
			}
		}
		found := false
		for _, d := range checkDeps {
			if d == "draft" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected parser to auto-insert 'draft' into check.depends_on, got %v", checkDeps)
		}
	})

	t.Run("missing reviews field", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Review missing"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: check
    action: review
    prompt: "Check it."
`))
		if err == nil {
			t.Fatal("expected error for missing reviews: field")
		}
		if !contains(err.Error(), "reviews:") {
			t.Errorf("error should mention reviews:, got %q", err.Error())
		}
	})

	t.Run("self reference rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Review self"
version: 1
tasks:
  - id: check
    action: review
    reviews: check
    prompt: "Review myself."
`))
		if err == nil {
			t.Fatal("expected error for reviews: self")
		}
		if !contains(err.Error(), "itself") {
			t.Errorf("error should mention self-reference, got %q", err.Error())
		}
	})

	t.Run("dangling reviews target", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Review dangling"
version: 1
tasks:
  - id: check
    action: review
    reviews: nonexistent
    prompt: "Review a ghost."
`))
		if err == nil {
			t.Fatal("expected error for dangling reviews target")
		}
		if !contains(err.Error(), "nonexistent") {
			t.Errorf("error should name the dangling target, got %q", err.Error())
		}
	})

	t.Run("reviews on non-review action rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Reviews on answer"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: check
    action: answer
    reviews: draft
    prompt: "Do a thing."
`))
		if err == nil {
			t.Fatal("expected error for reviews: on non-review task")
		}
		if !contains(err.Error(), "review") {
			t.Errorf("error should mention review-only, got %q", err.Error())
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
