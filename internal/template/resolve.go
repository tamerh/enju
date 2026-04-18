// Package template handles prompt template parsing and resolution.
// Templates use {{variable}} syntax:
//   - {{param_name}}            — for_each parameter, resolved at creation time
//   - {{task_id.content}}       — upstream task result, resolved at claim time
//   - {{task_id.field_name}}    — upstream task named output, resolved at claim time
//   - {{artifact:path/to/file}} — current state of a project artifact, resolved at claim time
package template

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Reference pattern: {{word.word}} or {{word}}
var refPattern = regexp.MustCompile(`\{\{(\w+)(?:\.(\w+))?\}\}`)

// Artifact reference pattern: {{artifact:path/to/file}}
// The path can contain alphanumerics, slashes, dots, hyphens and underscores.
var artifactRefPattern = regexp.MustCompile(`\{\{artifact:([A-Za-z0-9_./\-]+)\}\}`)

// Reference represents a parsed template reference.
type Reference struct {
	TaskID string // e.g., "foundation"
	Field  string // e.g., "content", "gene_list", or "" for bare {{param}}
	Raw    string // original match e.g., "{{foundation.content}}"
}

// ExtractReferences finds all {{task_id.field}} references in a prompt.
// Returns only task references (those with a dot), not bare {{param}} references.
func ExtractReferences(prompt string) []Reference {
	matches := refPattern.FindAllStringSubmatch(prompt, -1)
	var refs []Reference
	for _, m := range matches {
		if m[2] != "" {
			// Has a dot: {{task_id.field}} — this is a task reference
			refs = append(refs, Reference{
				TaskID: m[1],
				Field:  m[2],
				Raw:    m[0],
			})
		}
	}
	return refs
}

// InferDependencies extracts unique task IDs referenced in a prompt.
// These are the tasks this prompt depends on.
func InferDependencies(prompt string) []string {
	refs := ExtractReferences(prompt)
	seen := make(map[string]bool)
	var deps []string
	for _, ref := range refs {
		if !seen[ref.TaskID] {
			seen[ref.TaskID] = true
			deps = append(deps, ref.TaskID)
		}
	}
	return deps
}

// ResolveParams replaces {{param_name}} with values from the params map.
// Only replaces bare references (no dot), leaving task references untouched.
func ResolveParams(prompt string, params map[string]string) string {
	return refPattern.ReplaceAllStringFunc(prompt, func(match string) string {
		sub := refPattern.FindStringSubmatch(match)
		if sub[2] == "" {
			// Bare {{param}} — check if it's in params
			if val, ok := params[sub[1]]; ok {
				return val
			}
		}
		// Task reference or unknown param — leave as-is
		return match
	})
}

// ResolveUpstream replaces {{task_id.field}} references with actual upstream results.
// The inputs map is: task_def_id -> result content (raw JSON bytes or parsed map).
func ResolveUpstream(prompt string, inputs map[string]interface{}) string {
	return refPattern.ReplaceAllStringFunc(prompt, func(match string) string {
		sub := refPattern.FindStringSubmatch(match)
		if sub[2] == "" {
			// Bare {{param}} — not an upstream reference, leave as-is
			return match
		}

		taskID := sub[1]
		field := sub[2]

		result, ok := inputs[taskID]
		if !ok {
			return match // upstream not found, leave placeholder
		}

		return extractField(result, field)
	})
}

// extractField gets a specific field from a result.
// Result can be:
//   - map with "content" key (simple result): {{task.content}} returns the content string
//   - map with "content" as object (named outputs): {{task.field}} returns content[field]
func extractField(result interface{}, field string) string {
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return fmt.Sprintf("%v", result)
	}

	content, hasContent := resultMap["content"]

	if field == "content" {
		// {{task.content}} — return the content directly
		switch v := content.(type) {
		case string:
			return v
		case map[string]interface{}:
			// Named outputs stored as object — return JSON representation
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		default:
			return fmt.Sprintf("%v", content)
		}
	}

	if field == "responses" {
		// {{task.responses}} — multi-citizen tasks. Expected
		// shape: [{username, option, content}, ...] populated
		// by the client-side resolver from per-citizen
		// result.md files at the upstream's accepted commit.
		// Render as markdown blocks so the downstream prompt
		// gets human-readable output instead of raw JSON.
		//
		// For for_each upstreams the fan-in aggregator has
		// already rendered a per-iteration block and stashes
		// it here as a string — pass it through unchanged so
		// {{task.responses}} works for both singleton and
		// fan-in cases.
		if responses, ok := resultMap["responses"]; ok {
			if s, ok := responses.(string); ok {
				return s
			}
			return RenderResponsesMarkdown(responses)
		}
		return match(result, field)
	}

	// {{task.field_name}} — look in content object for named output
	if hasContent {
		if contentMap, ok := content.(map[string]interface{}); ok {
			if val, ok := contentMap[field]; ok {
				switch v := val.(type) {
				case string:
					return v
				default:
					b, _ := json.MarshalIndent(v, "", "  ")
					return string(b)
				}
			}
		}
	}

	// Field not found in content — try top-level result
	if val, ok := resultMap[field]; ok {
		return fmt.Sprintf("%v", val)
	}

	return fmt.Sprintf("{{%s.%s}}", taskID(resultMap), field)
}

func match(result interface{}, field string) string {
	return fmt.Sprintf("%v", result)
}

// RenderResponsesMarkdown renders a multi-citizen `responses`
// array as a human-readable markdown block. Expected shape is
// []interface{} of map[string]interface{} entries with keys
// "username", "option", and "content". Missing fields render as
// empty strings; unknown shapes fall back to JSON. Exported so
// the mcpgit fan-in aggregator can pre-render per-iteration
// response blocks when a for_each upstream is consumed via
// {{task.responses}}.
func RenderResponsesMarkdown(responses interface{}) string {
	// Accept both []interface{} (what the JSON-decoded
	// descriptor path produces) and []map[string]interface{}
	// (what the in-Go resolver produces directly). The two
	// paths converge here so template substitution works for
	// both the fat-client resolver and any future caller that
	// builds the input map manually.
	var entries []map[string]interface{}
	switch v := responses.(type) {
	case []map[string]interface{}:
		entries = v
	case []interface{}:
		for _, e := range v {
			if m, ok := e.(map[string]interface{}); ok {
				entries = append(entries, m)
			}
		}
	default:
		b, _ := json.MarshalIndent(responses, "", "  ")
		return string(b)
	}
	var out strings.Builder
	for i, m := range entries {
		username, _ := m["username"].(string)
		option, _ := m["option"].(string)
		content, _ := m["content"].(string)
		if i > 0 {
			out.WriteString("\n\n---\n\n")
		}
		header := "### "
		if username != "" {
			header += "@" + username
		} else {
			header += "(anonymous)"
		}
		if option != "" {
			header += " — " + option
		}
		out.WriteString(header + "\n\n")
		out.WriteString(content)
	}
	return out.String()
}

func taskID(m map[string]interface{}) string {
	if id, ok := m["task_id"]; ok {
		return fmt.Sprintf("%v", id)
	}
	return "unknown"
}

// MergeDependencies combines explicitly declared depends_on with inferred dependencies.
// Explicit deps take precedence; inferred deps are added if not already present.
func MergeDependencies(explicit []string, prompt string) []string {
	inferred := InferDependencies(prompt)

	seen := make(map[string]bool)
	var merged []string

	for _, dep := range explicit {
		if !seen[dep] {
			seen[dep] = true
			merged = append(merged, dep)
		}
	}

	for _, dep := range inferred {
		if !seen[dep] {
			seen[dep] = true
			merged = append(merged, dep)
		}
	}

	return merged
}

// HasUnresolvedReferences checks if a prompt still contains {{}} references.
func HasUnresolvedReferences(prompt string) bool {
	return refPattern.MatchString(prompt)
}

// HasUnresolvedTaskReferences checks if a prompt still has {{task.field}} references.
func HasUnresolvedTaskReferences(prompt string) bool {
	for _, ref := range ExtractReferences(prompt) {
		if ref.Field != "" {
			return true
		}
	}
	return false
}

// ListParams extracts bare {{param}} references (no dot) from a prompt.
func ListParams(prompt string) []string {
	matches := refPattern.FindAllStringSubmatch(prompt, -1)
	seen := make(map[string]bool)
	var params []string
	for _, m := range matches {
		if m[2] == "" && !seen[m[1]] {
			seen[m[1]] = true
			params = append(params, m[1])
		}
	}
	return params
}

// --- Artifact references ---

// InferArtifactReads extracts the unique artifact paths referenced via
// {{artifact:path}} in a prompt. These become implicit reads_artifacts
// declarations on the task.
func InferArtifactReads(prompt string) []string {
	matches := artifactRefPattern.FindAllStringSubmatch(prompt, -1)
	seen := make(map[string]bool)
	var paths []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			paths = append(paths, m[1])
		}
	}
	return paths
}

// MergeArtifactReads combines explicitly declared reads_artifacts with
// paths inferred from {{artifact:path}} references in the prompt.
// Order: explicit first (preserved), then inferred extras.
func MergeArtifactReads(explicit []string, prompt string) []string {
	inferred := InferArtifactReads(prompt)

	seen := make(map[string]bool)
	var merged []string

	for _, p := range explicit {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}
	for _, p := range inferred {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}
	return merged
}

// ResolveArtifacts replaces {{artifact:path}} references with the
// corresponding artifact contents. Paths not found in the map are left
// as-is so the caller can detect unresolved references.
func ResolveArtifacts(prompt string, artifacts map[string]string) string {
	return artifactRefPattern.ReplaceAllStringFunc(prompt, func(match string) string {
		sub := artifactRefPattern.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		path := sub[1]
		if content, ok := artifacts[path]; ok {
			return content
		}
		return match
	})
}
