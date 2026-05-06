package yaml

// Pattern detection + matching helpers for writes_artifacts paths.
//
// Three forms, all syntactic. The path field of a WriteArtifact
// drives the interpretation; no separate kind enum, no parser
// ambiguity:
//
//   - Literal:   `src/server.go`   — exact path
//   - Glob:      `src/api/*.go`    — has *, ?, or [
//   - Directory: `src/api/`        — trailing /
//
// IsGlob and IsDir are pure string predicates so callers (parse-
// time validators, coord-side pattern matchers, FS-walking
// expanders) all classify paths the same way without a second
// source of truth.
//
// MatchesPattern is the coord-side validation primitive: a
// submitted path is allowed iff it matches some declared
// pattern in the task's writes_artifacts. Coord never touches
// the FS — it can only string-match — so this function does
// the trickle of glob + dir-prefix logic without any disk
// access.

import (
	"path/filepath"
	"strings"
)

// IsGlob reports whether path contains glob metacharacters
// recognized by filepath.Match: `*`, `?`, or `[`. The fourth
// metacharacter `\\` (Windows escape) is not surfaced because
// repo-relative paths in YAML are POSIX-style by Enju
// convention.
func IsGlob(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

// IsDir reports whether path declares a directory — i.e. ends
// with `/`. The trailing slash is the syntactic marker, not a
// stat() check, so a declaration can be classified at parse
// time without touching disk.
func IsDir(path string) bool {
	return strings.HasSuffix(path, "/") && path != "/"
}

// MatchesPattern returns true when submittedPath would be
// covered by the declared pattern. Used coord-side to validate
// that every entry in `artifacts_written` corresponds to some
// writes_artifacts declaration.
//
// Three branches mirror the three forms:
//
//   - Literal: byte-for-byte equality.
//   - Glob:    filepath.Match against the declared pattern.
//             A pattern with `**` is NOT supported (filepath.Match
//             doesn't grok it); use a directory declaration when
//             you want recursive coverage.
//   - Dir:     submittedPath is under declaredPattern (string
//             prefix on the slash-terminated form). No `..`
//             escape check here — that's the submit-validator's
//             job at a different layer (ValidateArtifactPath).
//
// Symmetry with expandGlob in expand_fs.go is load-bearing:
// the coord here decides whether a submitted path is allowed,
// the client there decides which paths to submit. Both must
// classify the same path identically against the same pattern,
// or coord rejects something the client sent. They both use
// filepath.Match in the glob branch for that reason —
// optimizing one without the other would break submission.
//
// MatchesPattern operates on slash-separated paths, the
// canonical form Enju uses on the wire. Callers pass POSIX
// paths even on Windows; the wrapper that builds the workdir
// paths converts via filepath.ToSlash before reaching this
// function.
func MatchesPattern(submittedPath, declaredPattern string) bool {
	if declaredPattern == "" {
		return false
	}
	switch {
	case IsDir(declaredPattern):
		return strings.HasPrefix(submittedPath, declaredPattern)
	case IsGlob(declaredPattern):
		ok, err := filepath.Match(declaredPattern, submittedPath)
		return err == nil && ok
	default:
		return submittedPath == declaredPattern
	}
}

// MatchesAnyPattern is the bulk version: returns true if
// submittedPath matches any entry in declared. Used by the
// coord-side submit_orchestrate validator that walks every
// artifacts_written path against the task's full declaration.
func (w WriteArtifacts) MatchesAnyPattern(submittedPath string) bool {
	for _, e := range w {
		if MatchesPattern(submittedPath, e.Path) {
			return true
		}
	}
	return false
}
