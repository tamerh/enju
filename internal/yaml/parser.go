// Package yaml parses Cedar run definition files.
package yaml

import (
	"fmt"
	"os"
	"strings"

	"github.com/enju-ai/enju/internal/dag"
	"github.com/enju-ai/enju/internal/template"
	yamlv3 "gopkg.in/yaml.v3"
)

// Run is the top-level structure of a run.yaml file.
type Run struct {
	Name         string                 `yaml:"name"`
	Version      int                    `yaml:"version"`
	Ref          string                 `yaml:"ref,omitempty"`
	ForEach      map[string][]string    `yaml:"for_each,omitempty"`
	Defaults     TaskDefaults           `yaml:"defaults,omitempty"`
	Requirements map[string]interface{} `yaml:"requirements,omitempty"` // project-level requirements, inherited by tasks
	Tasks        []TaskDef              `yaml:"tasks"`
}

// TaskDefaults holds default values for all tasks.
type TaskDefaults struct {
	Timeout string `yaml:"timeout,omitempty"` // e.g., "30m", "2h"
}

// OutputSpec describes a single named output.
// Supports two YAML formats:
//   outputs:
//     name: "Description"                              # simple string
//   outputs:
//     name:                                            # object form
//       description: "Description"
//       file: "result.csv"                             # optional file
//       format: csv                                    # optional format
type OutputSpec struct {
	Description string `yaml:"description,omitempty"`
	File        string `yaml:"file,omitempty"`
	Format      string `yaml:"format,omitempty"`
}

// UnmarshalYAML supports both string and object forms.
func (o *OutputSpec) UnmarshalYAML(value *yamlv3.Node) error {
	// Try string form first
	if value.Kind == yamlv3.ScalarNode {
		o.Description = value.Value
		return nil
	}
	// Object form
	type alias OutputSpec
	var a alias
	if err := value.Decode(&a); err != nil {
		return err
	}
	*o = OutputSpec(a)
	return nil
}

// TaskDef is a single task definition from the YAML.
type TaskDef struct {
	ID         string            `yaml:"id"`
	Action     string            `yaml:"action"`                  // "answer", "contribute", "compute", "review", "vote"
	Ref        string            `yaml:"ref,omitempty"`
	DependsOn  []string          `yaml:"depends_on,omitempty"`
	Prompt     string            `yaml:"prompt,omitempty"`
	UserPrompt string            `yaml:"user_prompt,omitempty"`
	Script     string            `yaml:"script,omitempty"`
	ScriptSource string          `yaml:"script_source,omitempty"`
	ResultType string            `yaml:"result_type,omitempty"`
	Timeout    string            `yaml:"timeout,omitempty"`
	Gather     bool              `yaml:"gather,omitempty"`
	Outputs      map[string]OutputSpec  `yaml:"outputs,omitempty"`
	Requirements map[string]interface{} `yaml:"requirements,omitempty"` // task-level requirements (replaces project-level)
	Config       map[string]interface{} `yaml:"config,omitempty"`
}

// ParsedRun is the result of parsing and validating a run file.
// It contains the original definition plus the constructed DAG.
type ParsedRun struct {
	Run  *Run
	DAG      *dag.DAG
	// ExpandedTasks maps instance_key -> []TaskInstance for for_each expansion.
	// If no for_each, there's a single instance with key "".
	ExpandedTasks map[string][]TaskInstance
}

// TaskInstance is a concrete task instance after for_each expansion.
type TaskInstance struct {
	TaskDef
	InstanceKey string            // e.g., "endometriosis"
	Params      map[string]string // resolved for_each parameters
	FullID      string            // e.g., "endometriosis:foundation" or just "foundation"
}

// ParseFile reads and parses a run YAML file.
func ParseFile(path string) (*ParsedRun, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	return Parse(data)
}

// Parse parses run YAML bytes.
func Parse(data []byte) (*ParsedRun, error) {
	var prob Run
	if err := yamlv3.Unmarshal(data, &prob); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	if err := validate(&prob); err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}

	parsed, err := build(&prob)
	if err != nil {
		return nil, fmt.Errorf("building DAG: %w", err)
	}

	return parsed, nil
}

// resolveAction sets default action if not specified.
func resolveAction(t *TaskDef) {
	if t.Action == "" {
		t.Action = "answer"
	}
}

// validate checks the run definition for errors.
func validate(p *Run) error {
	if p.Name == "" {
		return fmt.Errorf("run name is required")
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("at least one task is required")
	}

	validActions := map[string]bool{
		"answer": true, "contribute": true, "compute": true,
		"review": true, "vote": true,
	}

	ids := make(map[string]bool)
	for i := range p.Tasks {
		t := &p.Tasks[i]

		if t.ID == "" {
			return fmt.Errorf("task ID is required")
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate task ID %q", t.ID)
		}
		ids[t.ID] = true

		// Set default action
		resolveAction(t)

		// Validate action
		if !validActions[t.Action] {
			return fmt.Errorf("task %q: invalid action %q (must be answer, contribute, compute, review, or vote)", t.ID, t.Action)
		}

		// Validate required fields based on action
		switch t.Action {
		case "answer", "contribute", "review":
			if t.Prompt == "" {
				return fmt.Errorf("task %q: prompt is required for %s action", t.ID, t.Action)
			}
		case "compute":
			if t.Script == "" {
				return fmt.Errorf("task %q: script is required for compute action", t.ID)
			}
		}

		if t.ResultType != "" && t.ResultType != "text" && t.ResultType != "json" && t.ResultType != "file" {
			return fmt.Errorf("task %q: invalid result_type %q", t.ID, t.ResultType)
		}
	}

	// Second pass: verify all depends_on references exist
	for _, t := range p.Tasks {
		for _, dep := range t.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("task %q depends on %q which does not exist", t.ID, dep)
			}
		}
	}

	return nil
}

// build constructs the DAG and expands for_each parameters.
func build(p *Run) (*ParsedRun, error) {
	// Determine instance keys from for_each
	instances := expandForEach(p.ForEach)

	result := &ParsedRun{
		Run:       p,
		DAG:           dag.New(),
		ExpandedTasks: make(map[string][]TaskInstance),
	}

	// Collect task IDs for dependency validation
	taskIDs := make(map[string]bool)
	for _, t := range p.Tasks {
		taskIDs[t.ID] = true
	}

	// Build task instances and DAG nodes for each instance
	for _, inst := range instances {
		var taskInstances []TaskInstance

		for _, taskDef := range p.Tasks {
			fullID := MakeFullID(inst.key, taskDef.ID)

			// Resolve for_each params in prompt at creation time
			resolvedPrompt := template.ResolveParams(taskDef.Prompt, inst.params)
			resolvedUserPrompt := template.ResolveParams(taskDef.UserPrompt, inst.params)

			// Infer dependencies from template references, merge with explicit depends_on
			allDeps := template.MergeDependencies(taskDef.DependsOn, taskDef.Prompt)

			// Validate inferred deps exist
			for _, dep := range allDeps {
				if !taskIDs[dep] {
					return nil, fmt.Errorf("task %q references %q which does not exist", taskDef.ID, dep)
				}
			}

			ti := TaskInstance{
				TaskDef:     taskDef,
				InstanceKey: inst.key,
				Params:      inst.params,
				FullID:      fullID,
			}
			// Override prompt with resolved version
			ti.Prompt = resolvedPrompt
			ti.UserPrompt = resolvedUserPrompt
			// Store merged dependencies
			ti.DependsOn = allDeps
			// Inherit run requirements if task doesn't have its own
			if ti.Requirements == nil {
				ti.Requirements = p.Requirements
			}

			taskInstances = append(taskInstances, ti)

			// Add node to DAG
			data := map[string]interface{}{
				"instance_key": inst.key,
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(fullID, taskDef.Action, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", fullID, err)
			}
		}

		// Add edges from merged dependencies (explicit + inferred)
		for _, ti := range taskInstances {
			childID := ti.FullID
			for _, dep := range ti.DependsOn {
				parentID := MakeFullID(inst.key, dep)
				if err := result.DAG.AddEdge(parentID, childID); err != nil {
					return nil, fmt.Errorf("adding edge %s -> %s: %w", parentID, childID, err)
				}
			}
		}

		result.ExpandedTasks[inst.key] = taskInstances
	}

	// Validate the constructed DAG
	if err := result.DAG.Validate(); err != nil {
		return nil, fmt.Errorf("DAG validation: %w", err)
	}

	return result, nil
}

type forEachInstance struct {
	key    string
	params map[string]string
}

// expandForEach generates all combinations of for_each parameters.
// For now, supports single for_each variable (most common case).
// Multi-variable cartesian product can be added later.
func expandForEach(forEach map[string][]string) []forEachInstance {
	if len(forEach) == 0 {
		// No expansion — single instance with empty key
		return []forEachInstance{{key: "", params: map[string]string{}}}
	}

	// Single variable expansion (most common: for_each: disease: [...])
	if len(forEach) == 1 {
		for varName, values := range forEach {
			instances := make([]forEachInstance, 0, len(values))
			for _, val := range values {
				instances = append(instances, forEachInstance{
					key:    val,
					params: map[string]string{varName: val},
				})
			}
			return instances
		}
	}

	// Multi-variable: cartesian product
	// For now, concatenate all variable expansions
	// TODO: implement proper cartesian product if needed
	var keys []string
	var vals [][]string
	for k, v := range forEach {
		keys = append(keys, k)
		vals = append(vals, v)
	}

	return cartesianProduct(keys, vals)
}

func cartesianProduct(keys []string, vals [][]string) []forEachInstance {
	if len(keys) == 0 {
		return []forEachInstance{{key: "", params: map[string]string{}}}
	}

	var result []forEachInstance
	var generate func(depth int, current map[string]string)
	generate = func(depth int, current map[string]string) {
		if depth == len(keys) {
			// Build instance key from all param values
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, current[k])
			}
			key := strings.Join(parts, "_")
			params := make(map[string]string)
			for k, v := range current {
				params[k] = v
			}
			result = append(result, forEachInstance{key: key, params: params})
			return
		}

		for _, v := range vals[depth] {
			current[keys[depth]] = v
			generate(depth+1, current)
		}
	}
	generate(0, make(map[string]string))
	return result
}

// MakeFullID constructs a full task ID from instance key and task ID.
func MakeFullID(instanceKey, taskID string) string {
	if instanceKey == "" {
		return taskID
	}
	return instanceKey + ":" + taskID
}
