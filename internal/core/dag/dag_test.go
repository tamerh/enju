package dag

import (
	"sort"
	"testing"
)

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}

func TestAddNodeAndEdge(t *testing.T) {
	g := New()

	if err := g.AddNode("a", "llm_prompt", nil); err != nil {
		t.Fatal(err)
	}
	if err := g.AddNode("b", "llm_prompt", nil); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge("a", "b"); err != nil {
		t.Fatal(err)
	}

	if g.NodeCount() != 2 {
		t.Fatalf("expected 2 nodes, got %d", g.NodeCount())
	}

	// Duplicate node
	if err := g.AddNode("a", "llm_prompt", nil); err == nil {
		t.Fatal("expected error for duplicate node")
	}

	// Edge to non-existent node
	if err := g.AddEdge("a", "z"); err == nil {
		t.Fatal("expected error for non-existent node")
	}
}

func TestCycleDetection(t *testing.T) {
	g := New()
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)

	g.AddEdge("a", "b")
	g.AddEdge("b", "c")

	// c -> a would create a cycle
	if err := g.AddEdge("c", "a"); err == nil {
		t.Fatal("expected cycle error")
	}

	// Self-loop
	if err := g.AddEdge("a", "a"); err == nil {
		t.Fatal("expected cycle error for self-loop")
	}
}

func TestRootsAndLeaves(t *testing.T) {
	g := New()
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)
	g.AddNode("d", "llm_prompt", nil)

	// a -> b -> d
	// a -> c -> d
	g.AddEdge("a", "b")
	g.AddEdge("a", "c")
	g.AddEdge("b", "d")
	g.AddEdge("c", "d")

	roots := sorted(g.Roots())
	if len(roots) != 1 || roots[0] != "a" {
		t.Fatalf("expected roots [a], got %v", roots)
	}

	leaves := sorted(g.Leaves())
	if len(leaves) != 1 || leaves[0] != "d" {
		t.Fatalf("expected leaves [d], got %v", leaves)
	}
}

func TestMultipleRootsAndLeaves(t *testing.T) {
	g := New()
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)
	g.AddNode("d", "llm_prompt", nil)

	// a -> c
	// b -> d
	// Two independent branches
	g.AddEdge("a", "c")
	g.AddEdge("b", "d")

	roots := sorted(g.Roots())
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots, got %v", roots)
	}

	leaves := sorted(g.Leaves())
	if len(leaves) != 2 {
		t.Fatalf("expected 2 leaves, got %v", leaves)
	}
}

func TestParentsAndChildren(t *testing.T) {
	g := New()
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)

	g.AddEdge("a", "c")
	g.AddEdge("b", "c")

	parents := sorted(g.Parents("c"))
	if len(parents) != 2 || parents[0] != "a" || parents[1] != "b" {
		t.Fatalf("expected parents [a, b], got %v", parents)
	}

	children := sorted(g.Children("a"))
	if len(children) != 1 || children[0] != "c" {
		t.Fatalf("expected children [c], got %v", children)
	}
}

func TestDescendants(t *testing.T) {
	g := New()
	// a -> b -> d -> e
	//   -> c ->
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)
	g.AddNode("d", "llm_prompt", nil)
	g.AddNode("e", "llm_prompt", nil)

	g.AddEdge("a", "b")
	g.AddEdge("a", "c")
	g.AddEdge("b", "d")
	g.AddEdge("c", "d")
	g.AddEdge("d", "e")

	desc := sorted(g.Descendants("a"))
	expected := []string{"b", "c", "d", "e"}
	if len(desc) != len(expected) {
		t.Fatalf("expected descendants %v, got %v", expected, desc)
	}
	for i, v := range expected {
		if desc[i] != v {
			t.Fatalf("expected descendants %v, got %v", expected, desc)
		}
	}

	// Descendants of d should be just e
	descD := g.Descendants("d")
	if len(descD) != 1 || descD[0] != "e" {
		t.Fatalf("expected descendants of d [e], got %v", descD)
	}

	// Descendants of e (leaf) should be empty
	descE := g.Descendants("e")
	if len(descE) != 0 {
		t.Fatalf("expected no descendants of e, got %v", descE)
	}
}

func TestAncestors(t *testing.T) {
	g := New()
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)
	g.AddNode("d", "llm_prompt", nil)

	g.AddEdge("a", "b")
	g.AddEdge("b", "c")
	g.AddEdge("a", "d")
	g.AddEdge("d", "c")

	anc := sorted(g.Ancestors("c"))
	expected := []string{"a", "b", "d"}
	if len(anc) != len(expected) {
		t.Fatalf("expected ancestors %v, got %v", expected, anc)
	}

	// Ancestors of root should be empty
	ancA := g.Ancestors("a")
	if len(ancA) != 0 {
		t.Fatalf("expected no ancestors of a, got %v", ancA)
	}
}

func TestTopologicalSort(t *testing.T) {
	g := New()
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)
	g.AddNode("d", "llm_prompt", nil)

	g.AddEdge("a", "b")
	g.AddEdge("a", "c")
	g.AddEdge("b", "d")
	g.AddEdge("c", "d")

	order, err := g.TopologicalSort()
	if err != nil {
		t.Fatal(err)
	}

	if len(order) != 4 {
		t.Fatalf("expected 4 nodes in sort, got %d", len(order))
	}

	// a must come before b and c; b and c must come before d
	pos := make(map[string]int)
	for i, id := range order {
		pos[id] = i
	}

	if pos["a"] >= pos["b"] || pos["a"] >= pos["c"] {
		t.Fatalf("a must come before b and c: %v", order)
	}
	if pos["b"] >= pos["d"] || pos["c"] >= pos["d"] {
		t.Fatalf("b and c must come before d: %v", order)
	}
}

func TestReadyNodes(t *testing.T) {
	g := New()
	g.AddNode("a", "llm_prompt", nil)
	g.AddNode("b", "llm_prompt", nil)
	g.AddNode("c", "llm_prompt", nil)
	g.AddNode("d", "llm_prompt", nil)

	g.AddEdge("a", "b")
	g.AddEdge("a", "c")
	g.AddEdge("b", "d")
	g.AddEdge("c", "d")

	// Nothing completed — only root is ready
	ready := sorted(g.ReadyNodes(map[string]bool{}))
	if len(ready) != 1 || ready[0] != "a" {
		t.Fatalf("expected [a] ready, got %v", ready)
	}

	// a completed — b and c become ready
	ready = sorted(g.ReadyNodes(map[string]bool{"a": true}))
	if len(ready) != 2 || ready[0] != "b" || ready[1] != "c" {
		t.Fatalf("expected [b, c] ready, got %v", ready)
	}

	// a and b completed — only c is ready (d needs both b and c)
	ready = sorted(g.ReadyNodes(map[string]bool{"a": true, "b": true}))
	if len(ready) != 1 || ready[0] != "c" {
		t.Fatalf("expected [c] ready, got %v", ready)
	}

	// a, b, c completed — d becomes ready
	ready = sorted(g.ReadyNodes(map[string]bool{"a": true, "b": true, "c": true}))
	if len(ready) != 1 || ready[0] != "d" {
		t.Fatalf("expected [d] ready, got %v", ready)
	}

	// All completed — nothing ready
	ready = g.ReadyNodes(map[string]bool{"a": true, "b": true, "c": true, "d": true})
	if len(ready) != 0 {
		t.Fatalf("expected nothing ready, got %v", ready)
	}
}

func TestDiseaseAnalysisDAG(t *testing.T) {
	// Simulate the actual disease analysis DAG from our architecture docs
	g := New()

	// Tier 1
	g.AddNode("foundation", "llm_prompt", nil)

	// Tier 2 — all depend on foundation, parallel to each other
	g.AddNode("genes_proteins", "llm_prompt", nil)
	g.AddNode("expression", "llm_prompt", nil)
	g.AddNode("structure", "llm_prompt", nil)
	g.AddNode("interactions", "llm_prompt", nil)

	g.AddEdge("foundation", "genes_proteins")
	g.AddEdge("genes_proteins", "expression")
	g.AddEdge("genes_proteins", "structure")
	g.AddEdge("genes_proteins", "interactions")

	// Tier 3 — depends on all of tier 2
	g.AddNode("drug_targets", "llm_prompt", nil)
	g.AddEdge("expression", "drug_targets")
	g.AddEdge("structure", "drug_targets")
	g.AddEdge("interactions", "drug_targets")

	// Final synthesis
	g.AddNode("synthesis", "llm_prompt", nil)
	g.AddEdge("drug_targets", "synthesis")

	// Validate
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}

	// Check structure
	if g.NodeCount() != 7 {
		t.Fatalf("expected 7 nodes, got %d", g.NodeCount())
	}

	roots := g.Roots()
	if len(roots) != 1 || roots[0] != "foundation" {
		t.Fatalf("expected single root 'foundation', got %v", roots)
	}

	leaves := g.Leaves()
	if len(leaves) != 1 || leaves[0] != "synthesis" {
		t.Fatalf("expected single leaf 'synthesis', got %v", leaves)
	}

	// Simulate execution order
	completed := map[string]bool{}

	// Step 1: foundation is ready
	ready := g.ReadyNodes(completed)
	if len(ready) != 1 || ready[0] != "foundation" {
		t.Fatalf("step 1: expected [foundation], got %v", ready)
	}
	completed["foundation"] = true

	// Step 2: genes_proteins is ready
	ready = g.ReadyNodes(completed)
	if len(ready) != 1 || ready[0] != "genes_proteins" {
		t.Fatalf("step 2: expected [genes_proteins], got %v", ready)
	}
	completed["genes_proteins"] = true

	// Step 3: expression, structure, interactions are all ready (parallel!)
	ready = sorted(g.ReadyNodes(completed))
	if len(ready) != 3 {
		t.Fatalf("step 3: expected 3 parallel tasks, got %v", ready)
	}
	completed["expression"] = true
	completed["structure"] = true
	completed["interactions"] = true

	// Step 4: drug_targets is ready
	ready = g.ReadyNodes(completed)
	if len(ready) != 1 || ready[0] != "drug_targets" {
		t.Fatalf("step 4: expected [drug_targets], got %v", ready)
	}
	completed["drug_targets"] = true

	// Step 5: synthesis is ready
	ready = g.ReadyNodes(completed)
	if len(ready) != 1 || ready[0] != "synthesis" {
		t.Fatalf("step 5: expected [synthesis], got %v", ready)
	}

	// Test cascade invalidation from genes_proteins
	desc := sorted(g.Descendants("genes_proteins"))
	expected := []string{"drug_targets", "expression", "interactions", "structure", "synthesis"}
	if len(desc) != len(expected) {
		t.Fatalf("cascade: expected %v, got %v", expected, desc)
	}
	for i, v := range expected {
		if desc[i] != v {
			t.Fatalf("cascade: expected %v, got %v", expected, desc)
		}
	}
}
