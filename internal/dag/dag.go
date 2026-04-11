// Package dag implements a directed acyclic graph for task dependency resolution.
// Inspired by Terraform's internal/dag — custom implementation, no external dependencies.
package dag

import (
	"fmt"
	"sync"
)

// DAG represents a directed acyclic graph of task nodes.
type DAG struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	edges    map[string]map[string]bool // parent -> children
	inEdges  map[string]map[string]bool // child -> parents (reverse index)
}

// Node represents a task in the DAG.
type Node struct {
	ID       string
	TaskType string // "llm_prompt" or "script"
	Data     map[string]interface{}
}

// New creates an empty DAG.
func New() *DAG {
	return &DAG{
		nodes:   make(map[string]*Node),
		edges:   make(map[string]map[string]bool),
		inEdges: make(map[string]map[string]bool),
	}
}

// AddNode adds a node to the graph. Returns error if node already exists.
func (d *DAG) AddNode(id, taskType string, data map[string]interface{}) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.nodes[id]; exists {
		return fmt.Errorf("node %q already exists", id)
	}

	d.nodes[id] = &Node{
		ID:       id,
		TaskType: taskType,
		Data:     data,
	}
	d.edges[id] = make(map[string]bool)
	d.inEdges[id] = make(map[string]bool)
	return nil
}

// AddEdge adds a directed edge from parent to child (parent must complete before child).
// Returns error if it would create a cycle.
func (d *DAG) AddEdge(parentID, childID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.nodes[parentID]; !ok {
		return fmt.Errorf("parent node %q does not exist", parentID)
	}
	if _, ok := d.nodes[childID]; !ok {
		return fmt.Errorf("child node %q does not exist", childID)
	}

	// Check if adding this edge would create a cycle.
	// A cycle exists if child can already reach parent.
	if d.canReach(childID, parentID) {
		return fmt.Errorf("edge %s -> %s would create a cycle", parentID, childID)
	}

	d.edges[parentID][childID] = true
	d.inEdges[childID][parentID] = true
	return nil
}

// GetNode returns a node by ID.
func (d *DAG) GetNode(id string) (*Node, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n, ok := d.nodes[id]
	return n, ok
}

// Roots returns all nodes with no incoming edges (no dependencies).
func (d *DAG) Roots() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var roots []string
	for id := range d.nodes {
		if len(d.inEdges[id]) == 0 {
			roots = append(roots, id)
		}
	}
	return roots
}

// Leaves returns all nodes with no outgoing edges (final outputs).
func (d *DAG) Leaves() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var leaves []string
	for id := range d.nodes {
		if len(d.edges[id]) == 0 {
			leaves = append(leaves, id)
		}
	}
	return leaves
}

// Parents returns the direct parents (dependencies) of a node.
func (d *DAG) Parents(id string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var parents []string
	for p := range d.inEdges[id] {
		parents = append(parents, p)
	}
	return parents
}

// Children returns the direct children (dependents) of a node.
func (d *DAG) Children(id string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var children []string
	for c := range d.edges[id] {
		children = append(children, c)
	}
	return children
}

// Descendants returns ALL downstream nodes reachable from the given node (BFS).
// Used for cascade invalidation.
func (d *DAG) Descendants(id string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	visited := make(map[string]bool)
	queue := []string{id}
	var result []string

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for child := range d.edges[current] {
			if !visited[child] {
				visited[child] = true
				result = append(result, child)
				queue = append(queue, child)
			}
		}
	}
	return result
}

// Ancestors returns ALL upstream nodes that can reach the given node (BFS on reverse edges).
func (d *DAG) Ancestors(id string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	visited := make(map[string]bool)
	queue := []string{id}
	var result []string

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for parent := range d.inEdges[current] {
			if !visited[parent] {
				visited[parent] = true
				result = append(result, parent)
				queue = append(queue, parent)
			}
		}
	}
	return result
}

// TopologicalSort returns nodes in dependency order (parents before children).
// Returns error if the graph contains a cycle (should not happen if AddEdge validates).
func (d *DAG) TopologicalSort() ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Kahn's algorithm
	inDegree := make(map[string]int)
	for id := range d.nodes {
		inDegree[id] = len(d.inEdges[id])
	}

	var queue []string
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		sorted = append(sorted, current)

		for child := range d.edges[current] {
			inDegree[child]--
			if inDegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}

	if len(sorted) != len(d.nodes) {
		return nil, fmt.Errorf("graph contains a cycle")
	}

	return sorted, nil
}

// ReadyNodes returns nodes whose ALL parents are in the given "completed" set.
// This is the core scheduling query: "what can run next?"
func (d *DAG) ReadyNodes(completed map[string]bool) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var ready []string
	for id := range d.nodes {
		if completed[id] {
			continue // already done
		}

		allParentsDone := true
		for parent := range d.inEdges[id] {
			if !completed[parent] {
				allParentsDone = false
				break
			}
		}

		if allParentsDone {
			ready = append(ready, id)
		}
	}
	return ready
}

// NodeCount returns the number of nodes in the graph.
func (d *DAG) NodeCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.nodes)
}

// Validate checks the DAG for structural issues.
func (d *DAG) Validate() error {
	// Check for cycles via topological sort
	if _, err := d.TopologicalSort(); err != nil {
		return err
	}

	// Check for dangling edges (shouldn't happen with AddEdge validation, but defensive)
	d.mu.RLock()
	defer d.mu.RUnlock()
	for parent, children := range d.edges {
		if _, ok := d.nodes[parent]; !ok {
			return fmt.Errorf("edge references non-existent node %q", parent)
		}
		for child := range children {
			if _, ok := d.nodes[child]; !ok {
				return fmt.Errorf("edge references non-existent node %q", child)
			}
		}
	}

	return nil
}

// canReach checks if source can reach target via BFS (caller must hold lock).
func (d *DAG) canReach(source, target string) bool {
	visited := make(map[string]bool)
	queue := []string{source}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if current == target {
			return true
		}

		if visited[current] {
			continue
		}
		visited[current] = true

		for child := range d.edges[current] {
			queue = append(queue, child)
		}
	}
	return false
}
