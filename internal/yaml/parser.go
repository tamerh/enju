// Package yaml parses Cedar problem definition files.
package yaml

import (
	"fmt"
	"os"
	"strings"

	"github.com/enju-ai/enju/internal/dag"
	yamlv3 "gopkg.in/yaml.v3"
)

// Problem is the top-level structure of a problem.yaml file.
type Problem struct {
	Name    string            `yaml:"name"`
	Version int               `yaml:"version"`
	ForEach map[string][]string `yaml:"for_each,omitempty"`
	Defaults TaskDefaults      `yaml:"defaults,omitempty"`
	Tasks   []TaskDef         `yaml:"tasks"`
}

// TaskDefaults holds default values for all tasks.
type TaskDefaults struct {
	Timeout string `yaml:"timeout,omitempty"` // e.g., "30m", "2h"
}

// TaskDef is a single task definition from the YAML.
type TaskDef struct {
	ID         string            `yaml:"id"`
	Type       string            `yaml:"type"`                  // "llm_prompt" or "script"
	Mode       string            `yaml:"mode,omitempty"`        // "autonomous" (default) or "assisted"
	DependsOn  []string          `yaml:"depends_on,omitempty"`
	Prompt     string            `yaml:"prompt,omitempty"`
	UserPrompt string            `yaml:"user_prompt,omitempty"` // for assisted mode
	Script     string            `yaml:"script,omitempty"`
	ScriptSource string          `yaml:"script_source,omitempty"` // "predefined" or "participant"
	ResultType string            `yaml:"result_type,omitempty"`   // "text" (default), "json", "file"
	Timeout    string            `yaml:"timeout,omitempty"`
	Gather     bool              `yaml:"gather,omitempty"`        // collect all for_each instances
	Outputs    map[string]string `yaml:"outputs,omitempty"`
	Config     map[string]interface{} `yaml:"config,omitempty"`
}

// ParsedProblem is the result of parsing and validating a problem file.
// It contains the original definition plus the constructed DAG.
type ParsedProblem struct {
	Problem  *Problem
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

// ParseFile reads and parses a problem YAML file.
func ParseFile(path string) (*ParsedProblem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	return Parse(data)
}

// Parse parses problem YAML bytes.
func Parse(data []byte) (*ParsedProblem, error) {
	var prob Problem
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

// validate checks the problem definition for errors.
func validate(p *Problem) error {
	if p.Name == "" {
		return fmt.Errorf("problem name is required")
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("at least one task is required")
	}

	ids := make(map[string]bool)
	for _, t := range p.Tasks {
		if t.ID == "" {
			return fmt.Errorf("task ID is required")
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate task ID %q", t.ID)
		}
		ids[t.ID] = true

		if t.Type == "" {
			return fmt.Errorf("task %q: type is required", t.ID)
		}
		if t.Type != "llm_prompt" && t.Type != "script" {
			return fmt.Errorf("task %q: invalid type %q (must be 'llm_prompt' or 'script')", t.ID, t.Type)
		}

		if t.Type == "llm_prompt" && t.Prompt == "" {
			return fmt.Errorf("task %q: prompt is required for llm_prompt tasks", t.ID)
		}
		if t.Type == "script" && t.Script == "" {
			return fmt.Errorf("task %q: script is required for script tasks", t.ID)
		}

		if t.Mode != "" && t.Mode != "autonomous" && t.Mode != "assisted" {
			return fmt.Errorf("task %q: invalid mode %q (must be 'autonomous' or 'assisted')", t.ID, t.Mode)
		}

		if t.ResultType != "" && t.ResultType != "text" && t.ResultType != "json" && t.ResultType != "file" {
			return fmt.Errorf("task %q: invalid result_type %q", t.ID, t.ResultType)
		}

		// Check depends_on references exist
		for _, dep := range t.DependsOn {
			if !ids[dep] {
				// Could be defined later — we'll check again after all tasks are parsed
			}
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
func build(p *Problem) (*ParsedProblem, error) {
	// Determine instance keys from for_each
	instances := expandForEach(p.ForEach)

	result := &ParsedProblem{
		Problem:       p,
		DAG:           dag.New(),
		ExpandedTasks: make(map[string][]TaskInstance),
	}

	// Build task instances and DAG nodes for each instance
	for _, inst := range instances {
		var taskInstances []TaskInstance

		for _, taskDef := range p.Tasks {
			fullID := MakeFullID(inst.key, taskDef.ID)

			ti := TaskInstance{
				TaskDef:     taskDef,
				InstanceKey: inst.key,
				Params:      inst.params,
				FullID:      fullID,
			}
			taskInstances = append(taskInstances, ti)

			// Add node to DAG
			data := map[string]interface{}{
				"instance_key": inst.key,
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(fullID, taskDef.Type, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", fullID, err)
			}
		}

		// Add edges within this instance
		for _, taskDef := range p.Tasks {
			childID := MakeFullID(inst.key, taskDef.ID)
			for _, dep := range taskDef.DependsOn {
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
