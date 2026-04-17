package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// --- Artifact helpers ---

// validateArtifactPath enforces the rules the client's write helper
// also applies:
//   - non-empty
//   - relative (no leading slash)
//   - no .. traversal
//   - no .git escape hatch
//   - doesn't end with /
//
// Path is the user-facing form (without the "artifacts/" prefix).
// The coordinator uses this in the submit-report validator to
// sanity-check reported artifact paths before updating the index.
func validateArtifactPath(p string) error {
	if p == "" {
		return fmt.Errorf("path is empty")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be relative")
	}
	if strings.HasSuffix(p, "/") {
		return fmt.Errorf("path must not end with /")
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned != p {
		return fmt.Errorf("path is not in canonical form (got %q, want %q)", p, cleaned)
	}
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.Contains(cleaned, "/../") {
		return fmt.Errorf("path traversal not allowed")
	}
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return fmt.Errorf(".git is reserved")
	}
	if cleaned == ".enju" || strings.HasPrefix(cleaned, ".enju/") {
		return fmt.Errorf(".enju is reserved for Enju state")
	}
	if strings.HasPrefix(cleaned, "enju_templates/") {
		return fmt.Errorf("enju_templates is reserved for templates")
	}
	return nil
}

// marshalStringSlice serializes a []string for storage in a TEXT
// column. Empty/nil slices become "" (so the DEFAULT '' constraint
// holds).
func marshalStringSlice(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	b, err := json.Marshal(xs)
	if err != nil {
		return ""
	}
	return string(b)
}

// marshalVoteOptions serializes a YAML vote options list into the
// JSON form stored in tasks.vote_options. Keeping this helper in
// the api package (not yaml) avoids a circular dep — the store
// treats the column as opaque JSON.
func marshalVoteOptions(options []enjuYaml.VoteOption) string {
	if len(options) == 0 {
		return ""
	}
	// Round-trip through an anonymous struct with lowercase JSON
	// tags so the stored shape matches what the router's submit
	// handler decodes. yaml.VoteOption's field tags are yaml:...,
	// not json:..., so re-shaping here is simpler than adding
	// json tags upstream.
	type wire struct {
		ID        string   `json:"id"`
		Label     string   `json:"label,omitempty"`
		Activates []string `json:"activates,omitempty"`
	}
	out := make([]wire, len(options))
	for i, o := range options {
		out[i] = wire{ID: o.ID, Label: o.Label, Activates: o.Activates}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// unmarshalStringSlice parses the storage form back to a slice. An
// empty string yields nil (no entries).
func unmarshalStringSlice(s string) []string {
	if s == "" {
		return nil
	}
	var xs []string
	if err := json.Unmarshal([]byte(s), &xs); err != nil {
		return nil
	}
	return xs
}

// formatIterationLabel renders a task's iteration context as
// "key1=val1, key2=val2" using the persisted instance_params JSON
// when available, falling back to the raw instance key slug for
// rows that predate the instance_params column. Keys are sorted so
// the output is deterministic. Used by toTaskResponse to populate
// the iteration_label field.
func formatIterationLabel(instanceParams, instanceKey string) string {
	if instanceParams == "" {
		return instanceKey
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(instanceParams), &m); err != nil || len(m) == 0 {
		return instanceKey
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}
