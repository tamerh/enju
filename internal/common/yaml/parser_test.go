package yaml

import (
	"sort"
	"strings"
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

// TestParseRejectsMultiReviewerPerTask pins the phase 6b.2
// parse-time gate that two distinct review tasks targeting the
// same upstream are rejected. The auto-merge model can't FF-merge
// two divergent review topics that share an upstream — the
// second approve would refuse, after the YAML author has already
// committed to the topology. Surfacing the error at parse time
// gives them a clear path forward (citizens: N for quorum, or
// sequential review stages).
func TestParseRejectsMultiReviewerPerTask(t *testing.T) {
	yamlData := []byte(`
name: "Two reviewers"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "write"
  - id: gate_a
    action: review
    reviews: draft
    prompt: "review a"
  - id: gate_b
    action: review
    reviews: draft
    prompt: "review b"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting two reviewers of the same target")
	}
	for _, want := range []string{"gate_b", "gate_a", "draft", "multi-reviewer-per-task"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err.Error())
		}
	}
}

// TestParseAcceptsReviewOfReviewSequentialChain confirms that
// the multi-reviewer-per-task gate doesn't reject sequential
// review stages where review_b reviews review_a (different
// targets). That's the workaround the gate's error message
// suggests, so it must remain valid YAML.
func TestParseAcceptsReviewOfReviewSequentialChain(t *testing.T) {
	yamlData := []byte(`
name: "Sequential reviews"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "write"
  - id: gate_a
    action: review
    reviews: draft
    prompt: "first review"
  - id: gate_b
    action: review
    reviews: gate_a
    prompt: "review the review"
`)
	if _, err := Parse(yamlData); err != nil {
		t.Fatalf("sequential review chain should parse: %v", err)
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

// TestParseVoteAction covers the Phase E.2 vote-action validator
// and dep-edge auto-insertion.
func TestParseVoteAction(t *testing.T) {
	t.Run("happy path auto-inserts reverse dep", func(t *testing.T) {
		parsed, err := Parse([]byte(`
name: "Vote happy"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "Pick a direction."
    options:
      - id: py
        label: "Python"
        activates: [build_py]
      - id: rs
        label: "Rust"
        activates: [build_rs]
  - id: build_py
    action: answer
    prompt: "Build with Python."
  - id: build_rs
    action: answer
    prompt: "Build with Rust."
`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		// Both build tasks should now depend on the vote task
		// even though the YAML didn't list that edge explicitly.
		for _, want := range []string{"build_py", "build_rs"} {
			var deps []string
			for _, tk := range parsed.Run.Tasks {
				if tk.ID == want {
					deps = tk.DependsOn
					break
				}
			}
			found := false
			for _, d := range deps {
				if d == "pick" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %s to auto-depend on pick, got depends_on=%v", want, deps)
			}
		}
	})

	t.Run("fewer than 2 options rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "One option"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    options:
      - {id: only}
`))
		if err == nil {
			t.Fatal("expected error for single-option vote")
		}
		if !contains(err.Error(), "at least 2") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate option ids rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Dup"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    options:
      - {id: a, label: A}
      - {id: a, label: B}
`))
		if err == nil {
			t.Fatal("expected error for duplicate option ids")
		}
	})

	t.Run("unknown activates target rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Bad activates"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    options:
      - {id: a, activates: [nonexistent]}
      - {id: b, label: B}
`))
		if err == nil {
			t.Fatal("expected error for dangling activates target")
		}
		if !contains(err.Error(), "nonexistent") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("citizens greater than 1 accepted", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Multi voter"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    citizens: 3
    threshold: majority
    options:
      - {id: a}
      - {id: b}
`))
		if err != nil {
			t.Fatalf("citizens:3 should parse now that session 2a landed: %v", err)
		}
	})

	t.Run("min_quorum exceeds citizens rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Bad quorum"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    citizens: 3
    min_quorum: 5
    options:
      - {id: a}
      - {id: b}
`))
		if err == nil {
			t.Fatal("expected error for min_quorum > citizens")
		}
		if !contains(err.Error(), "min_quorum") {
			t.Errorf("error should mention min_quorum, got: %v", err)
		}
	})

	t.Run("options forbidden on non-vote action", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Stray options"
version: 1
tasks:
  - id: step
    action: answer
    prompt: "x"
    options:
      - {id: a}
      - {id: b}
`))
		if err == nil {
			t.Fatal("expected error for options on non-vote task")
		}
	})

	t.Run("threshold parsed", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "With threshold"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    threshold: majority
    options:
      - {id: a}
      - {id: b}
`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
	})

	t.Run("invalid threshold rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Bad threshold"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    threshold: largestMinority
    options:
      - {id: a}
      - {id: b}
`))
		if err == nil {
			t.Fatal("expected error for unknown threshold")
		}
	})

	t.Run("percent threshold parsed", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Percent"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    threshold: "percent:60"
    options:
      - {id: a}
      - {id: b}
`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
	})

	t.Run("deadline parses as duration", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Deadline"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    deadline: 2h
    options:
      - {id: a}
      - {id: b}
`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
	})

	t.Run("invalid deadline rejected", func(t *testing.T) {
		_, err := Parse([]byte(`
name: "Bad deadline"
version: 1
tasks:
  - id: pick
    action: vote
    prompt: "x"
    deadline: "1 fortnight"
    options:
      - {id: a}
      - {id: b}
`))
		if err == nil {
			t.Fatal("expected error for unparseable deadline")
		}
	})
}

// TestParseReviewWithNoConsumersWarning — a review on a task
// that has no downstream consumers is not an error, but the
// parser emits a non-fatal warning so authors see the "runs
// but gates nothing" situation when they create the run.
func TestParseReviewWithNoConsumersWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Orphan review"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write a thing."
  - id: check
    action: review
    reviews: draft
    prompt: "Review it."
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Warnings) == 0 {
		t.Fatal("expected a warning for review with no downstream consumers")
	}
	found := false
	for _, w := range parsed.Warnings {
		if contains(w, "has no downstream consumers") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'no downstream consumers' warning, got: %v", parsed.Warnings)
	}
}

// TestParseReviewWithConsumerNoWarning — the same task with a
// downstream that references {{draft.content}} should NOT emit
// the warning.
func TestParseReviewWithConsumerNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Good review"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write a thing."
  - id: check
    action: review
    reviews: draft
    prompt: "Review it."
  - id: publish
    action: answer
    prompt: "Publish {{draft.content}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "no downstream consumers") {
			t.Errorf("unexpected orphan-review warning: %s", w)
		}
	}
}

// TestParseComputeTaskNoDepsWarns — a compute task with no
// visible upstream linkage (no task-field refs, no reads,
// no depends_on) AND a downstream consumer trips the
// structural lint. The warning tells the author to declare
// the upstream edge explicitly; non-fatal.
//
// Standalone (no consumer) compute tasks are exempt — see
// TestParseComputeLeafNoConsumersNoWarning for the reviewer-
// reported false-positive that suppression closes.
func TestParseComputeTaskNoDepsWarns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute no deps"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/run.py
    prompt: "Run the script"

  - id: downstream
    action: answer
    depends_on: [gen]
    prompt: "Consumes whatever gen produced."
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\" has no declared dependencies") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected compute-no-deps warning for gen; got: %v", parsed.Warnings)
	}
}

// TestParseComputeLeafNoConsumersNoWarning — a standalone
// compute task with no upstream deps AND no downstream
// consumers (no task depends on it, no task references it,
// no task reads its artifacts) is a safe "one-shot" use
// case. The stealth-reader warning is speculative there:
// nothing cascades if the task does happen to read
// external state, and prototype / test templates commonly
// look exactly like this.
//
// Regression guard for a reviewer-reported false positive
// that fired on every standalone compute task in the test
// templates.
func TestParseComputeLeafNoConsumersNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "standalone compute"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/run.py
    prompt: "Run the standalone script."
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"run\"") {
			t.Errorf("leaf compute task with no consumers should not warn, got: %s", w)
		}
	}
}

// TestParseComputeWithDownstreamConsumerStillWarns — the
// hidden-stealth-reader risk is real when another task
// depends on this one (cascading failure). Keep the warning.
func TestParseComputeWithDownstreamConsumerStillWarns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "compute with downstream"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/run.py
    prompt: "Run it"

  - id: consume
    action: answer
    prompt: "Summarize: {{gen.content}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning (downstream exists), got: %v", parsed.Warnings)
	}
}

// TestParseComputeWithDependsOnConsumerStillWarns — consumer
// declared via depends_on (not a prompt ref).
func TestParseComputeWithDependsOnConsumerStillWarns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "compute with depends_on consumer"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/run.py
    prompt: "Run it"

  - id: consume
    action: answer
    depends_on: [gen]
    prompt: "After gen runs."
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning (depends_on consumer exists), got: %v", parsed.Warnings)
	}
}

// TestParseComputeWithArtifactConsumerStillWarns — consumer
// declared via reads_artifacts matching the compute task's
// writes_artifacts path.
func TestParseComputeWithArtifactConsumerStillWarns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "compute with artifact consumer"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/run.py
    writes_artifacts:
      - out/data.txt
    prompt: "Produce data"

  - id: consume
    action: answer
    reads_artifacts:
      - out/data.txt
    prompt: "Read: {{artifact:out/data.txt}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The gen task has writes_artifacts → that counts as a
	// declared output, but no upstream deps. The consume
	// task reads via an artifact ref, so gen has a consumer.
	// The stealth-reader warning should still fire — even
	// though reads_artifacts is empty on `gen` (the checked
	// condition), the path-match existence means consumers
	// exist, so the conservative "might cascade" case holds.
	found := false
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning (artifact-path consumer exists), got: %v", parsed.Warnings)
	}
}

// TestParseComputeReviewTargetStillWarns — a review task
// targeting the compute task counts as a consumer (cascade
// via review approval/rejection).
func TestParseComputeReviewTargetStillWarns(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "compute reviewed"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/run.py
    prompt: "Run it"

  - id: check
    action: review
    reviews: gen
    prompt: "Is this OK?"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning (review target is a consumer), got: %v", parsed.Warnings)
	}
}

// TestParseComputeWithDependsOnNoWarning — same compute task
// with an explicit depends_on declaration silences the lint.
func TestParseComputeWithDependsOnNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute with depends_on"
version: 1
tasks:
  - id: source
    action: answer
    prompt: "Produce source data."
  - id: gen
    action: compute
    script: scripts/run.py
    depends_on: [source]
    prompt: "Run the script"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			t.Errorf("unexpected compute-no-deps warning with depends_on set: %s", w)
		}
	}
}

// TestParseComputeWithPromptRefNoWarning — {{task.field}} in
// the prompt is an implicit dep declaration and also silences
// the lint.
func TestParseComputeWithPromptRefNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute with prompt ref"
version: 1
tasks:
  - id: source
    action: answer
    prompt: "Produce source data."
  - id: gen
    action: compute
    script: scripts/run.py
    prompt: "Process: {{source.content}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			t.Errorf("unexpected compute-no-deps warning with prompt ref: %s", w)
		}
	}
}

// TestParseComputeWithReadsArtifactNoWarning — reads_artifacts
// is the third dep-declaration form and also silences the lint.
func TestParseComputeWithReadsArtifactNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute with reads_artifacts"
version: 1
tasks:
  - id: gen
    action: compute
    script: scripts/run.py
    reads_artifacts: [data/input.csv]
    prompt: "Run on input.csv"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			t.Errorf("unexpected compute-no-deps warning with reads_artifacts: %s", w)
		}
	}
}

// TestParseComputeParamRefSuppressesWarning — `{{paramname}}`
// in the prompt signals the task is parameterized by run
// context (via ENJU_PARAM_* / context.json). Scripts that
// reach run context explicitly aren't the class of compute
// task the lint was designed to catch (stealth-readers of
// peer outputs), so the warning is suppressed.
//
// Tester report 2026-04-19: a review_claude compute task
// read $SOURCE_REPO from run params but still tripped the
// warning. Suppress on visible param refs clears the false
// positive.
func TestParseComputeParamRefSuppressesWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute with param only"
version: 1
params:
  - name: target
    type: string
    required: true
tasks:
  - id: gen
    action: compute
    script: scripts/run.py
    prompt: "Run for {{target}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "compute task \"gen\"") {
			t.Errorf("unexpected compute-no-deps warning — visible {{param}} ref should suppress; got: %v", parsed.Warnings)
		}
	}
}

// TestParseWarnOnComputeContentRefWithArtifacts is the
// regression guard for the "{{X.content}} on a compute task
// that writes artifacts" footgun — .content is the script's
// stdout, not the file bytes, but it's an easy mistake to make.
// The lint fires on run creation so the author catches it
// before any citizen claims a downstream.
func TestParseWarnOnComputeContentRefWithArtifacts(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute content-ref lint"
version: 1
tasks:
  - id: aggregate
    action: compute
    script: scripts/aggregate.sh
    writes_artifacts:
      - out/totals.tsv
    prompt: "Aggregate the data"

  - id: summarize
    action: answer
    prompt: "Summarize: {{aggregate.content}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got string
	for _, w := range parsed.Warnings {
		if contains(w, "{{aggregate.content}}") {
			got = w
			break
		}
	}
	if got == "" {
		t.Fatalf("expected compute-content warning, got: %v", parsed.Warnings)
	}
	// Warning must name the artifact and the replacement syntax
	// so the author can fix without cross-referencing docs.
	for _, want := range []string{"out/totals.tsv", "{{artifact:"} {
		if !contains(got, want) {
			t.Errorf("warning missing %q: %s", want, got)
		}
	}
}

// TestParseComputeContentRefSuppressedByArtifactRef — if the
// author ALREADY uses {{artifact:<path>}} for the same
// producer's output, they clearly know the distinction.
// Suppress the warning so a prompt that legitimately wants
// both stdout (status line) + artifact (bytes) doesn't
// nag the author.
func TestParseComputeContentRefSuppressedByArtifactRef(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute content+artifact both"
version: 1
tasks:
  - id: aggregate
    action: compute
    script: scripts/aggregate.sh
    writes_artifacts:
      - out/totals.tsv
    prompt: "Aggregate the data"

  - id: summarize
    action: answer
    prompt: |
      Status line from aggregate: {{aggregate.content}}
      Actual data: {{artifact:out/totals.tsv}}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "{{aggregate.content}}") {
			t.Errorf("warning should be suppressed when {{artifact:...}} is also present: %s", w)
		}
	}
}

// TestParseComputeContentRefNoArtifactsNoWarning — if the
// producer compute task has no writes_artifacts, stdout IS the
// canonical output. {{X.content}} is correct, no warning.
func TestParseComputeContentRefNoArtifactsNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute no artifacts"
version: 1
tasks:
  - id: compute_only
    action: compute
    script: scripts/run.sh
    prompt: "Run it"

  - id: consume
    action: answer
    prompt: "Read: {{compute_only.content}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "{{compute_only.content}}") {
			t.Errorf("unexpected warning for stdout-only compute producer: %s", w)
		}
	}
}

// TestParseNonComputeContentRefNoWarning — {{X.content}} on
// answer / contribute / review / vote producers is exactly the
// right thing. Only compute tasks have the stdout-vs-artifact
// split that creates the footgun.
func TestParseNonComputeContentRefNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Answer content ref"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write a paragraph."

  - id: review
    action: answer
    prompt: "Critique: {{draft.content}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "{{draft.content}}") {
			t.Errorf("unexpected warning for answer-producer content ref: %s", w)
		}
	}
}

// TestParseComputeFieldRefNoWarning — {{X.field}} for a
// declared named output is the correct pattern and should not
// trip the stdout-vs-artifact lint.
func TestParseComputeFieldRefNoWarning(t *testing.T) {
	parsed, err := Parse([]byte(`
name: "Compute field ref"
version: 1
tasks:
  - id: analyze
    action: compute
    script: scripts/analyze.sh
    outputs:
      gene_list:
        description: "list of genes"
    writes_artifacts:
      - out/genes.tsv
    prompt: "Analyze"

  - id: consume
    action: answer
    prompt: "Genes: {{analyze.gene_list}}"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range parsed.Warnings {
		if contains(w, "{{analyze.") {
			t.Errorf("unexpected warning for named-field ref: %s", w)
		}
	}
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

// TestParseRunLevelParams covers the new top-level params: block
// added in Phase H.1. A run can declare parameters that get
// substituted into {{param}} references at submission time. The
// same YAML file is both runnable and reusable as a template —
// location (under templates/) signals intent, not schema.
func TestParseRunLevelParams(t *testing.T) {
	yamlData := []byte(`
name: "GWAS recipe"
description: "Analyze GWAS summary stats for a disease."
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "The disease to analyze (e.g. endometriosis, PCOS)"
  - name: tissue
    type: string
    default: "whole blood"
    description: "Primary tissue for expression analysis"
tasks:
  - id: gwas
    action: answer
    prompt: "Analyze GWAS data for {{disease}} in {{tissue}}"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if parsed.Run.Description == "" {
		t.Error("expected Description to be populated")
	}
	if len(parsed.Run.Params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(parsed.Run.Params))
	}
	disease := parsed.Run.Params[0]
	if disease.Name != "disease" || disease.Type != "string" || !disease.Required {
		t.Errorf("disease param wrong: %+v", disease)
	}
	tissue := parsed.Run.Params[1]
	if tissue.Default != "whole blood" {
		t.Errorf("tissue default wrong: %+v", tissue.Default)
	}
}

// TestParseRejectsDuplicateParamName — uniqueness is a hard
// parser error, not a warning. Two params with the same name
// would make the substitution order-dependent.
func TestParseRejectsDuplicateParamName(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "first"
  - name: disease
    type: string
    required: true
    description: "second"
tasks:
  - id: t
    action: answer
    prompt: "x {{disease}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
	if !searchString(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

// TestParseRejectsUnknownParamType — strict type vocabulary;
// unknown types fail fast so authors notice the typo.
func TestParseRejectsUnknownParamType(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
params:
  - name: count
    type: integer
    required: true
    description: "count"
tasks:
  - id: t
    action: answer
    prompt: "x {{count}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected invalid-type error, got nil")
	}
	if !searchString(err.Error(), "invalid type") {
		t.Errorf("expected invalid-type error, got: %v", err)
	}
}

// TestParseRejectsRequiredWithDefault — required + default is a
// contradiction. If there's a default, it's not required.
func TestParseRejectsRequiredWithDefault(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
    default: "pcos"
    description: "disease"
tasks:
  - id: t
    action: answer
    prompt: "x {{disease}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected required+default error, got nil")
	}
	if !searchString(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got: %v", err)
	}
}

// TestParseRejectsBadDefaultType — default values get type-
// checked at parse time.
func TestParseRejectsBadDefaultType(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
params:
  - name: count
    type: int
    default: "not a number"
    description: "count"
tasks:
  - id: t
    action: answer
    prompt: "x {{count}}"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected type-mismatch error, got nil")
	}
	if !searchString(err.Error(), "whole number") {
		t.Errorf("expected whole-number error, got: %v", err)
	}
}

// TestParseWithParamsHappyPath — ParseWithParams substitutes
// supplied values into task prompts. Required params are
// provided; optional params fall back to their declared
// default. After substitution, no {{param}} placeholders
// remain in the resolved prompt.
func TestParseWithParamsHappyPath(t *testing.T) {
	yamlData := []byte(`
name: "GWAS recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "The disease to analyze"
  - name: tissue
    type: string
    default: "whole blood"
    description: "Primary tissue"
tasks:
  - id: gwas
    action: answer
    prompt: "Analyze GWAS data for {{disease}} in {{tissue}}"
`)
	parsed, err := ParseWithParams(yamlData, map[string]interface{}{
		"disease": "PCOS",
	})
	if err != nil {
		t.Fatalf("ParseWithParams failed: %v", err)
	}
	got := parsed.Run.Tasks[0].Prompt
	want := "Analyze GWAS data for PCOS in whole blood"
	if got != want {
		t.Errorf("prompt substitution wrong\n  got:  %q\n  want: %q", got, want)
	}
}

// TestParseWithParamsMissingRequired — omitting a required
// param produces a natural-language error that the LLM can
// turn into a follow-up question for the user.
func TestParseWithParamsMissingRequired(t *testing.T) {
	yamlData := []byte(`
name: "GWAS recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "The disease to analyze (e.g. endometriosis, PCOS)"
tasks:
  - id: gwas
    action: answer
    prompt: "Analyze {{disease}}"
`)
	_, err := ParseWithParams(yamlData, map[string]interface{}{})
	if err == nil {
		t.Fatal("expected missing-required error, got nil")
	}
	if !searchString(err.Error(), "missing required parameter") {
		t.Errorf("expected 'missing required parameter', got: %v", err)
	}
	// The description must appear — that's the whole point of
	// the natural-language phrasing.
	if !searchString(err.Error(), "The disease to analyze") {
		t.Errorf("expected description in error, got: %v", err)
	}
}

// TestParseWithParamsUnknownName — a typo'd param name errors
// as "unknown parameter" rather than masking the typo as a
// missing-required error on the correctly-named one.
func TestParseWithParamsUnknownName(t *testing.T) {
	yamlData := []byte(`
name: "Recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "d"
tasks:
  - id: t
    action: answer
    prompt: "x {{disease}}"
`)
	_, err := ParseWithParams(yamlData, map[string]interface{}{
		"diesase": "PCOS",
	})
	if err == nil {
		t.Fatal("expected unknown-param error, got nil")
	}
	if !searchString(err.Error(), "unknown parameter") {
		t.Errorf("expected 'unknown parameter', got: %v", err)
	}
}

// TestParseWithParamsTypeMismatch — a supplied value of the
// wrong type errors with a param-named message.
func TestParseWithParamsTypeMismatch(t *testing.T) {
	yamlData := []byte(`
name: "Recipe"
version: 1
params:
  - name: count
    type: int
    required: true
    description: "how many"
tasks:
  - id: t
    action: answer
    prompt: "x {{count}}"
`)
	_, err := ParseWithParams(yamlData, map[string]interface{}{
		"count": "not a number",
	})
	if err == nil {
		t.Fatal("expected type-mismatch error, got nil")
	}
	if !searchString(err.Error(), "count") {
		t.Errorf("expected error to mention 'count', got: %v", err)
	}
}

// TestParseWithParamsListSubstitution — list<string> params
// are substituted as one value per line (readable in an LLM
// prompt).
func TestParseWithParamsListSubstitution(t *testing.T) {
	yamlData := []byte(`
name: "Recipe"
version: 1
params:
  - name: genes
    type: list<string>
    required: true
    description: "Genes to analyze"
tasks:
  - id: t
    action: answer
    prompt: "Analyze:\n{{genes}}"
`)
	parsed, err := ParseWithParams(yamlData, map[string]interface{}{
		"genes": []interface{}{"BRCA1", "TP53", "EGFR"},
	})
	if err != nil {
		t.Fatalf("ParseWithParams failed: %v", err)
	}
	got := parsed.Run.Tasks[0].Prompt
	want := "Analyze:\nBRCA1\nTP53\nEGFR"
	if got != want {
		t.Errorf("list substitution wrong\n  got:  %q\n  want: %q", got, want)
	}
}

// TestParseStarRefExpandsListFields verifies `{{param[*]}}`
// duplicates a list element per value in a list<string> param.
// Applies to writes_artifacts, reads_artifacts, assign_to,
// depends_on — the four list-valued fields a template author
// would want to scale from N=1 to N=many without enumerating
// every path.
func TestParseStarRefExpandsListFields(t *testing.T) {
	yamlData := []byte(`
name: "star expansion"
version: 1
params:
  - name: items
    type: list<string>
    required: true
tasks:
  - id: seed
    action: answer
    writes_artifacts: ["state/items/{{items[*]}}.json"]
    prompt: "Emit {{items}} items."
  - id: consume
    action: answer
    depends_on: [seed]
    reads_artifacts: ["state/items/{{items[*]}}.json"]
    prompt: "Consume {{items}}."
`)
	parsed, err := ParseWithParams(yamlData, map[string]interface{}{
		"items": []interface{}{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("ParseWithParams: %v", err)
	}
	seed := parsed.Run.Tasks[0]
	if got := seed.WritesArtifacts.Paths(); len(got) != 3 || got[0] != "state/items/a.json" || got[2] != "state/items/c.json" {
		t.Fatalf("writes_artifacts expansion wrong: %+v", got)
	}
	consume := parsed.Run.Tasks[1]
	if got := []string(consume.ReadsArtifacts); len(got) != 3 || got[1] != "state/items/b.json" {
		t.Fatalf("reads_artifacts expansion wrong: %+v", got)
	}
}

// TestParseStarRefRejectsNonListParam — a typo that points
// `[*]` at a scalar param fails the parse loudly rather than
// leaving the literal placeholder in the artifact path (which
// would blow up at validate time with a cryptic
// "malformed path" error).
func TestParseStarRefRejectsNonListParam(t *testing.T) {
	yamlData := []byte(`
name: "star on scalar"
version: 1
params:
  - name: name
    type: string
    required: true
tasks:
  - id: t
    action: answer
    writes_artifacts: ["state/{{name[*]}}.json"]
    prompt: "x"
`)
	_, err := ParseWithParams(yamlData, map[string]interface{}{"name": "x"})
	if err == nil {
		t.Fatal("expected error on [*] against non-list param, got nil")
	}
	if !strings.Contains(err.Error(), "list<string>") {
		t.Errorf("expected list<string> hint in error, got: %v", err)
	}
}

// TestParseStarRefRejectsMultipleInOneElement — two `[*]`
// refs in one element would imply a cartesian product and
// silent blowup. Reject up front; if someone needs cross
// products, add explicit syntax later.
func TestParseStarRefRejectsMultipleInOneElement(t *testing.T) {
	yamlData := []byte(`
name: "two stars"
version: 1
params:
  - name: a
    type: list<string>
    required: true
  - name: b
    type: list<string>
    required: true
tasks:
  - id: t
    action: answer
    writes_artifacts: ["{{a[*]}}/{{b[*]}}.json"]
    prompt: "x"
`)
	_, err := ParseWithParams(yamlData, map[string]interface{}{
		"a": []interface{}{"1", "2"},
		"b": []interface{}{"x", "y"},
	})
	if err == nil {
		t.Fatal("expected error on multiple [*] refs, got nil")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("expected 'multiple' in error, got: %v", err)
	}
}

// TestParseWithParamsRejectsExtraForPlainRun — passing params
// to a run that declares none is a mismatch, not a silent
// accept. Surfaces path/template mixups.
func TestParseWithParamsRejectsExtraForPlainRun(t *testing.T) {
	yamlData := []byte(`
name: "Plain run"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "Hello"
`)
	_, err := ParseWithParams(yamlData, map[string]interface{}{
		"disease": "PCOS",
	})
	if err == nil {
		t.Fatal("expected 'does not declare any parameters' error, got nil")
	}
	if !searchString(err.Error(), "does not declare") {
		t.Errorf("expected 'does not declare' error, got: %v", err)
	}
}

// TestParseLeavesParamPlaceholdersWhenCalledDirectly — plain
// Parse() does not substitute. Leaves {{param}} refs literal
// so a linter or description tool can inspect a template
// without supplying values.
func TestParseLeavesParamPlaceholdersWhenCalledDirectly(t *testing.T) {
	yamlData := []byte(`
name: "Recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
    description: "d"
tasks:
  - id: t
    action: answer
    prompt: "Analyze {{disease}}"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if !searchString(parsed.Run.Tasks[0].Prompt, "{{disease}}") {
		t.Errorf("expected placeholder to remain literal, got: %q", parsed.Run.Tasks[0].Prompt)
	}
}

// TestParseMissingDescriptionWarning — missing description is
// non-fatal but emits a warning. The LLM needs prose to turn
// the param into a user-facing question.
// TestParseDynamicForEach — Phase J.1 core parse path.
// A task declares for_each whose list comes from an
// upstream task's list<string> output. The parser accepts
// it, produces zero instances for the deferred task, and
// records deferred metadata naming the upstream + field.
// Downstream singletons that depend on the deferred task
// are transitively deferred.
func TestParseDynamicForEach(t *testing.T) {
	yamlData := []byte(`
name: "Per-gene analysis"
version: 1
tasks:
  - id: discover_genes
    action: answer
    prompt: "List 3 candidate genes for endometriosis, one per line."
    outputs:
      gene_symbols:
        description: "Gene symbols to analyze."
        format: list<string>

  - id: analyze_gene
    action: answer
    for_each:
      gene: "{{discover_genes.gene_symbols}}"
    prompt: "Analyze gene {{gene}}."

  - id: synthesize
    action: answer
    prompt: "Combine findings: {{analyze_gene.content}}"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	// The discover_genes singleton should materialize. The
	// analyze_gene def should NOT — it's dynamic. synthesize
	// should also be deferred because it consumes a deferred
	// upstream.
	singletonIDs := make(map[string]bool)
	for _, tasks := range parsed.ExpandedTasks {
		for _, ti := range tasks {
			singletonIDs[ti.TaskDef.ID] = true
		}
	}
	if !singletonIDs["discover_genes"] {
		t.Error("expected discover_genes to materialize")
	}
	if singletonIDs["analyze_gene"] {
		t.Error("expected analyze_gene to be deferred (no instances)")
	}
	if singletonIDs["synthesize"] {
		t.Error("expected synthesize to be transitively deferred")
	}

	// DeferredTaskDefs should contain both analyze_gene
	// (directly dynamic) and synthesize (transitive).
	deferredByID := make(map[string]DeferredTaskDef)
	for _, d := range parsed.DeferredTaskDefs {
		deferredByID[d.TaskDefID] = d
	}
	analyze, ok := deferredByID["analyze_gene"]
	if !ok {
		t.Fatal("expected analyze_gene in DeferredTaskDefs")
	}
	if analyze.TransitivelyDeferred {
		t.Error("analyze_gene is directly dynamic, not transitive")
	}
	ref, ok := analyze.ForEachRefs["gene"]
	if !ok {
		t.Fatal("expected analyze_gene to record a ForEachRef for 'gene'")
	}
	if ref.TaskID != "discover_genes" || ref.Field != "gene_symbols" {
		t.Errorf("wrong ForEachRef: %+v", ref)
	}

	synth, ok := deferredByID["synthesize"]
	if !ok {
		t.Fatal("expected synthesize in DeferredTaskDefs")
	}
	if !synth.TransitivelyDeferred {
		t.Error("synthesize should be marked transitively deferred")
	}
}

// TestParseDynamicForEachRejectsUnknownUpstream — a
// {{undefined.field}} reference fails at parse time with
// a clear "unknown upstream task" error.
func TestParseDynamicForEachRejectsUnknownUpstream(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
tasks:
  - id: analyze
    action: answer
    for_each:
      item: "{{missing.items}}"
    prompt: "Analyze {{item}}."
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected unknown-upstream error, got nil")
	}
	if !searchString(err.Error(), "unknown upstream task") {
		t.Errorf("expected 'unknown upstream task' error, got: %v", err)
	}
}

// TestParseDynamicForEachRejectsMissingField — the upstream
// exists but doesn't declare the referenced output field.
func TestParseDynamicForEachRejectsMissingField(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
tasks:
  - id: produce
    action: answer
    prompt: "x"
    outputs:
      other_field:
        format: list<string>

  - id: analyze
    action: answer
    for_each:
      item: "{{produce.items}}"
    prompt: "Analyze {{item}}."
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected missing-field error, got nil")
	}
	if !searchString(err.Error(), "does not declare an output field") {
		t.Errorf("expected 'does not declare an output field' error, got: %v", err)
	}
}

// TestParseDynamicForEachRejectsNonListOutput — the
// referenced field exists but isn't declared as
// list<string>. Dynamic for_each needs a typed iterable
// source.
func TestParseDynamicForEachRejectsNonListOutput(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
tasks:
  - id: produce
    action: answer
    prompt: "x"
    outputs:
      summary:
        format: text

  - id: analyze
    action: answer
    for_each:
      item: "{{produce.summary}}"
    prompt: "Analyze {{item}}."
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected non-list-output error, got nil")
	}
	if !searchString(err.Error(), "list<string>") {
		t.Errorf("expected 'list<string>' error, got: %v", err)
	}
}

// TestParseDynamicForEachRejectsSelfReference — a task
// can't fan out over its own output.
func TestParseDynamicForEachRejectsSelfReference(t *testing.T) {
	yamlData := []byte(`
name: "Bad recipe"
version: 1
tasks:
  - id: analyze
    action: answer
    for_each:
      item: "{{analyze.items}}"
    outputs:
      items:
        format: list<string>
    prompt: "Analyze {{item}}."
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected self-reference error, got nil")
	}
	if !searchString(err.Error(), "references itself") {
		t.Errorf("expected 'references itself' error, got: %v", err)
	}
}

// TestParseDynamicForEachPerInstanceChain — two tasks share
// the same dynamic for_each reference (the canonical
// per-instance review pattern). Both are deferred and
// treated as the same iteration dimension.
func TestParseDynamicForEachPerInstanceChain(t *testing.T) {
	yamlData := []byte(`
name: "Per-gene analysis + review"
version: 1
tasks:
  - id: discover
    action: answer
    prompt: "List genes"
    outputs:
      genes:
        format: list<string>

  - id: analyze
    action: answer
    for_each:
      gene: "{{discover.genes}}"
    prompt: "Analyze {{gene}}."

  - id: review
    action: review
    reviews: analyze
    for_each:
      gene: "{{discover.genes}}"
    prompt: "Review the analysis of {{gene}}."
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	deferredByID := make(map[string]bool)
	for _, d := range parsed.DeferredTaskDefs {
		deferredByID[d.TaskDefID] = true
	}
	if !deferredByID["analyze"] || !deferredByID["review"] {
		t.Errorf("expected both analyze and review deferred, got: %v", deferredByID)
	}
}

func TestParseMissingDescriptionWarning(t *testing.T) {
	yamlData := []byte(`
name: "Recipe"
version: 1
params:
  - name: disease
    type: string
    required: true
tasks:
  - id: t
    action: answer
    prompt: "x {{disease}}"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	foundWarning := false
	for _, w := range parsed.Warnings {
		if searchString(w, "no description") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected 'no description' warning, got: %v", parsed.Warnings)
	}
}

func TestParseRemediationRule(t *testing.T) {
	yamlData := []byte(`
name: "remediation rule test"
version: 1
tasks:
  - id: develop_x
    action: answer
    prompt: "build it"
    on_review_reject: spawn_remediation
    remediation_template:
      action: answer
      prompt: "Address: {{review.feedback}}"
  - id: review_x
    action: review
    reviews: develop_x
    depends_on: [develop_x]
    prompt: "check it"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}
	var dev *TaskDef
	for i := range parsed.Run.Tasks {
		if parsed.Run.Tasks[i].ID == "develop_x" {
			dev = &parsed.Run.Tasks[i]
			break
		}
	}
	if dev == nil {
		t.Fatal("develop_x task not found in parsed tasks")
	}
	if dev.OnReviewReject != "spawn_remediation" {
		t.Fatalf("on_review_reject mismatch: %q", dev.OnReviewReject)
	}
	if dev.RemediationTemplate == nil {
		t.Fatal("expected RemediationTemplate to be parsed")
	}
	if dev.RemediationTemplate.Action != "answer" {
		t.Fatalf("remediation action: %q", dev.RemediationTemplate.Action)
	}
	if dev.RemediationTemplate.Prompt != "Address: {{review.feedback}}" {
		t.Fatalf("remediation prompt: %q", dev.RemediationTemplate.Prompt)
	}
}

func TestParseAutoTriage(t *testing.T) {
	yamlData := []byte(`
name: "auto-triage parse test"
version: 1
auto_triage:
  action: answer
  prompt: "Fix issue {{issue.id}}: {{issue.title}}"
  assign_to: [bot-fixer]
  require_role: "developer"
tasks:
  - id: develop
    action: answer
    prompt: "Build it"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}
	at := parsed.Run.AutoTriage
	if at == nil {
		t.Fatal("expected Run.AutoTriage to be parsed, got nil")
	}
	if at.Action != "answer" {
		t.Fatalf("action: %q", at.Action)
	}
	if at.Prompt != "Fix issue {{issue.id}}: {{issue.title}}" {
		t.Fatalf("prompt: %q", at.Prompt)
	}
	if len(at.AssignTo) != 1 || at.AssignTo[0] != "bot-fixer" {
		t.Fatalf("assign_to: %v", at.AssignTo)
	}
	if at.RequireRole != "developer" {
		t.Fatalf("require_role: %q", at.RequireRole)
	}
}

func TestParseAutoTriageOmitted(t *testing.T) {
	// Static workflow without auto_triage — Run.AutoTriage is nil.
	yamlData := []byte(`
name: "no auto_triage"
version: 1
tasks:
  - id: x
    action: answer
    prompt: "y"
`)
	parsed, err := Parse(yamlData)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Run.AutoTriage != nil {
		t.Fatalf("expected nil AutoTriage, got %+v", parsed.Run.AutoTriage)
	}
}

// TestParseRejectsAuthorWrittenMergeResolve pins the parallel-
// merge phase 3 contract that `action: merge_resolve` cannot
// appear in user-authored YAML — it's auto-spawned by coord on
// non-FF merge conflicts. Without this gate a typo'd or copy-
// pasted manifest would parse, then spawn weirdly because
// generateIterationBranch skips topic-branch flow assuming the
// operator has already done the merge externally.
func TestParseRejectsAuthorWrittenMergeResolve(t *testing.T) {
	yamlData := []byte(`
name: "Hand-written merge_resolve"
version: 1
tasks:
  - id: bogus
    action: merge_resolve
    prompt: "shouldn't be here"
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting hand-written action: merge_resolve")
	}
	for _, want := range []string{"merge_resolve", "system-only", "bogus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err.Error())
		}
	}
}

// TestParseRejectsParallelSiblingsWithOverlappingWrites pins the
// parallel-merge phase 4 lint: two tasks with no transitive dep
// edge that both declare the SAME literal writes_artifacts path
// are flagged at parse time. Under parallel execution their
// commits would conflict at auto-merge time, spawning a
// merge_resolve task — surfacing the issue at parse time gives
// the YAML author a clean rewrite path instead.
func TestParseRejectsParallelSiblingsWithOverlappingWrites(t *testing.T) {
	yamlData := []byte(`
name: "Overlapping siblings"
version: 1
tasks:
  - id: alpha
    action: answer
    prompt: "alpha"
    writes_artifacts:
      - path: shared/notes.md
  - id: beta
    action: answer
    prompt: "beta"
    writes_artifacts:
      - path: shared/notes.md
`)
	_, err := Parse(yamlData)
	if err == nil {
		t.Fatal("expected error rejecting parallel siblings with overlapping writes")
	}
	for _, want := range []string{"alpha", "beta", "shared/notes.md", "no dep edge"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err.Error())
		}
	}
}

// TestParseAcceptsSerialOverlappingWrites confirms the lint only
// fires on PARALLEL siblings — when one task depends on the
// other (transitively or directly), they're serialized by the
// DAG and the second commit naturally lands on top of the first.
// Same path on both is fine because it's just an overwrite, not
// a merge conflict.
func TestParseAcceptsSerialOverlappingWrites(t *testing.T) {
	yamlData := []byte(`
name: "Serial overlap"
version: 1
tasks:
  - id: alpha
    action: answer
    prompt: "alpha"
    writes_artifacts:
      - path: shared/notes.md
  - id: beta
    action: answer
    prompt: "beta"
    depends_on: [alpha]
    writes_artifacts:
      - path: shared/notes.md
`)
	if _, err := Parse(yamlData); err != nil {
		t.Fatalf("serial overlap should parse: %v", err)
	}
}

// TestParseAcceptsParallelDisjointWrites covers the happy
// parallel case: two siblings with no dep edge but DIFFERENT
// paths. No conflict possible at merge time; lint stays quiet.
func TestParseAcceptsParallelDisjointWrites(t *testing.T) {
	yamlData := []byte(`
name: "Parallel disjoint"
version: 1
tasks:
  - id: alpha
    action: answer
    prompt: "alpha"
    writes_artifacts:
      - path: out/alpha.md
  - id: beta
    action: answer
    prompt: "beta"
    writes_artifacts:
      - path: out/beta.md
`)
	if _, err := Parse(yamlData); err != nil {
		t.Fatalf("parallel disjoint writes should parse: %v", err)
	}
}

// TestParseAcceptsParallelSiblingsWithTemplatedOverlap pins the
// scope limitation: the lint is literal-path only, so siblings
// that both write to "out/{{instance}}.md" pass parse-time
// (different instance keys produce disjoint paths at
// materialization). Glob/template-aware overlap detection is a
// follow-up; today's lint shouldn't false-positive on these.
func TestParseAcceptsParallelSiblingsWithTemplatedOverlap(t *testing.T) {
	yamlData := []byte(`
name: "Templated overlap"
version: 1
for_each:
  case: [a, b]
tasks:
  - id: alpha
    action: answer
    prompt: "alpha {{case}}"
    writes_artifacts:
      - path: "out/{{case}}/alpha.md"
  - id: beta
    action: answer
    prompt: "beta {{case}}"
    writes_artifacts:
      - path: "out/{{case}}/beta.md"
`)
	if _, err := Parse(yamlData); err != nil {
		t.Fatalf("templated paths should parse: %v", err)
	}
}
