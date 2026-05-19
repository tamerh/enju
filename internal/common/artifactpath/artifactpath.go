// Package artifactpath is the single source of truth for
// artifact-path safety. Every path an author can declare
// (writes_artifacts / reads_artifacts) or a citizen can claim to
// have produced (artifacts_written at submit) flows through this
// one core so the rules cannot drift between the surfaces that
// call them:
//
//   - `enju validate`            (parse-time static lint)
//   - `enju go --dry-run`        (parse-time, with params)
//   - `enju_create_run` (MCP)    (engine ValidateRunCreation)
//   - submit_orchestrate         (concrete written paths)
//
// Before this package existed the core lived only in the
// coordinator engine, so `enju validate` was materially weaker
// than the create path it claimed to mirror (it blessed `[*]` on
// a scalar, `../x`, `/etc/passwd`, and reserved-dir writes that
// create then refused — a false green pre-flight).
//
// Deliberately a leaf package: NO internal imports, so both the
// engine and the yaml parser can depend on it without an import
// cycle (internal/common/layout already imports yaml, which ruled
// out hosting this there).
package artifactpath

import (
	"fmt"
	"strings"
)

// ValidateLiteral checks that a CONCRETE artifact path is
// well-formed: non-empty, relative, no traversal, no glob
// metacharacters, not inside a reserved directory.
//
// Use for reads_artifacts entries and the literal paths a citizen
// reports at submit time — both must be commit-resolvable.
func ValidateLiteral(p string) error {
	return core(p, false)
}

// ValidateDeclaration is the writes_artifacts variant: same
// safety floor, but tolerates declaration-only syntax — globs
// (`*`, `?`, `[`) and the trailing-slash directory form. The
// expansion step at submit time re-checks the concrete matches
// with ValidateLiteral.
func ValidateDeclaration(p string) error {
	return core(p, true)
}

// ReservedPrefixes are the top-level directories an artifact must
// never be declared into:
//
//   - ".enju" — Enju's own per-project state + per-task audit
//     (StateDirRoot == ResultDirRoot in internal/common/layout).
//     A write here clobbers the machinery running the workflow.
//   - ".git"  — the operator's object store.
//   - "enju"  — the conventional template-bundle root
//     (layout.DefaultTemplatesDir == "enju/templates"); a write
//     here can corrupt the workflow definitions an in-flight run
//     reads from. Kept reserved even after the visible/hidden
//     ".enju" consolidation precisely because bundles still live
//     under "enju/".
//
// Held as literals (not layout constants) to keep this package a
// dependency-free leaf; these are stable on-disk names.
var ReservedPrefixes = []string{".enju", ".git", "enju"}

func core(p string, allowPatterns bool) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("must be relative (no leading /)")
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("must not contain '..'")
	}
	// Strip the directory marker before running reserved-dir
	// checks. `enju/foo/` is rejected by the same rule that
	// rejects `enju/foo`; the trailing slash is purely a
	// classification hint at the syntactic level.
	check := p
	if allowPatterns && strings.HasSuffix(check, "/") {
		check = strings.TrimSuffix(check, "/")
		if check == "" {
			return fmt.Errorf("path is just a slash")
		}
	}
	if !allowPatterns && strings.ContainsAny(check, "*?[") {
		return fmt.Errorf("must not contain glob metacharacters (this path must be literal)")
	}
	// Normalize backslashes so a Windows-style `.git\objects`
	// can't slip past the forward-slash prefix test.
	norm := strings.ReplaceAll(check, `\`, "/")
	for _, reserved := range ReservedPrefixes {
		if norm == reserved || strings.HasPrefix(norm, reserved+"/") {
			return fmt.Errorf("must not write into %s/ (reserved for Enju state, git internals, or workflow templates)", reserved)
		}
	}
	return nil
}
