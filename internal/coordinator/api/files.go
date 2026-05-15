package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/service"
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
	if cleaned == "enju" || strings.HasPrefix(cleaned, "enju/") {
		return fmt.Errorf("enju is reserved for Enju state")
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
//
// VoteOption now carries json: tags (lowercase id/label/activates),
// so a direct json.Marshal IS the canonical wire shape — the same
// shape engine/marshalVoteOptions, the submit handler, tally, and
// the bot daemon's parseVoteOptions all read. The previous
// re-shape-through-an-anonymous-wire-struct dance existed only
// because the tags were missing; with them, one Marshal = one
// shape, no second code path to drift.
func marshalVoteOptions(options []enjuYaml.VoteOption) string {
	if len(options) == 0 {
		return ""
	}
	b, err := json.Marshal(options)
	if err != nil {
		return ""
	}
	return string(b)
}

// Thin in-package wrappers that delegate to the canonical
// helpers in service. Kept so existing api call sites keep
// reading the same way they always did; new code should call
// service directly.

func unmarshalStringSlice(s string) []string                { return service.UnmarshalStringSlice(s) }
func unmarshalWriteArtifacts(s string) enjuYaml.WriteArtifacts { return service.UnmarshalWriteArtifacts(s) }
func unmarshalStringMapField(s string) map[string]string    { return service.UnmarshalStringMapField(s) }
func formatIterationLabel(ip, ik string) string             { return service.FormatIterationLabel(ip, ik) }
