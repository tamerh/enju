package engine

// Pattern-matching validation in ValidateSubmitRequest.
// Phase B of writes_artifacts pattern support: a submitted
// path must match SOME declared pattern (literal, glob, or
// directory). Pre-fix this was an exact-string lookup —
// glob declarations would always reject submitted matches.

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// declaresArtifacts builds a TaskRecord with writesArtifacts
// JSON pre-populated. Tests pass the declared shape they're
// validating; the WriteArtifacts unmarshaler accepts both the
// legacy []string and the current object form.
func declaresArtifacts(t *testing.T, jsonDecl string) *store.TaskRecord {
	t.Helper()
	return &store.TaskRecord{
		ID:              "proj:1:task",
		RunID:           10,
		Action:          "compute",
		Citizens:        1,
		ClaimedBy:       42,
		WritesArtifacts: jsonDecl,
	}
}

func runRecord() *store.RunRecord {
	return &store.RunRecord{ID: 10, ProjectID: 7, Branch: "main"}
}

// TestValidateSubmit_DirectoryPatternMatchesNestedFiles pins
// the directory-form contract: declaring `src/api/` covers
// every concrete file the citizen submits under that prefix,
// at any depth.
func TestValidateSubmit_DirectoryPatternMatchesNestedFiles(t *testing.T) {
	task := declaresArtifacts(t, `[{"path":"src/api/","track":true}]`)
	req := &SubmitRequest{
		ArtifactsWritten: []string{
			"src/api/server.go",
			"src/api/middleware.go",
			"src/api/handlers/users.go",
		},
		ResultPath: "enju/runs/1-task/task",
	}
	e := New(&mockStore{}, nil)
	_, _, _, _, err := e.ValidateSubmitRequest(task, runRecord(), req)
	// The submit may fail downstream for unrelated reasons (we
	// haven't set up a full claim/run record in this minimal
	// fixture). What matters is that the artifact-validation
	// stage doesn't reject a directory-covered path.
	if err != nil && strings.Contains(err.Error(), "not covered by writes_artifacts") {
		t.Fatalf("directory pattern should cover nested file: %v", err)
	}
}

// TestValidateSubmit_GlobPatternMatchesShallowFiles pins the
// glob-form contract: `src/api/*.go` covers any .go file
// directly under src/api/ but NOT nested subdirs (Go's
// filepath.Match is non-recursive).
func TestValidateSubmit_GlobPatternMatchesShallowFiles(t *testing.T) {
	task := declaresArtifacts(t, `[{"path":"src/api/*.go","track":true}]`)
	req := &SubmitRequest{
		ArtifactsWritten: []string{"src/api/server.go", "src/api/middleware.go"},
		ResultPath:       "enju/runs/1-task/task",
	}
	e := New(&mockStore{}, nil)
	_, _, _, _, err := e.ValidateSubmitRequest(task, runRecord(), req)
	if err != nil && strings.Contains(err.Error(), "not covered by writes_artifacts") {
		t.Fatalf("glob pattern should cover shallow .go files: %v", err)
	}

	// Subdirectory file should NOT match a non-recursive glob.
	reqRecursive := &SubmitRequest{
		ArtifactsWritten: []string{"src/api/sub/x.go"},
		ResultPath:       "enju/runs/1-task/task",
	}
	_, _, _, _, err = e.ValidateSubmitRequest(task, runRecord(), reqRecursive)
	if err == nil || !strings.Contains(err.Error(), "not covered by writes_artifacts") {
		t.Fatalf("glob shouldn't recurse; expected coverage error, got %v", err)
	}
}

// TestValidateSubmit_LiteralPatternIsExact preserves today's
// strict behavior: a literal declaration matches exactly that
// one path, nothing else. Pins backwards-compat against the
// new pattern-matcher.
func TestValidateSubmit_LiteralPatternIsExact(t *testing.T) {
	task := declaresArtifacts(t, `[{"path":"src/server.go","track":true}]`)
	req := &SubmitRequest{
		ArtifactsWritten: []string{"src/server.go"},
		ResultPath:       "enju/runs/1-task/task",
	}
	e := New(&mockStore{}, nil)
	_, _, _, _, err := e.ValidateSubmitRequest(task, runRecord(), req)
	if err != nil && strings.Contains(err.Error(), "not covered by writes_artifacts") {
		t.Fatalf("literal exact match should pass: %v", err)
	}

	reqWrong := &SubmitRequest{
		ArtifactsWritten: []string{"src/other.go"},
		ResultPath:       "enju/runs/1-task/task",
	}
	_, _, _, _, err = e.ValidateSubmitRequest(task, runRecord(), reqWrong)
	if err == nil || !strings.Contains(err.Error(), "not covered by writes_artifacts") {
		t.Fatalf("literal pattern should reject non-matching path; got %v", err)
	}
}

// TestValidateSubmit_RejectsPathOutsideAnyPattern guards the
// non-permissive default: a path that matches NOTHING is
// rejected with the "not covered" error message.
func TestValidateSubmit_RejectsPathOutsideAnyPattern(t *testing.T) {
	task := declaresArtifacts(t, `[{"path":"src/api/","track":true},{"path":"go.mod","track":true}]`)
	req := &SubmitRequest{
		ArtifactsWritten: []string{"docs/notes.md"},
		ResultPath:       "enju/runs/1-task/task",
	}
	e := New(&mockStore{}, nil)
	_, _, _, _, err := e.ValidateSubmitRequest(task, runRecord(), req)
	if err == nil {
		t.Fatal("expected error for path outside all declared patterns")
	}
	if !strings.Contains(err.Error(), "not covered") {
		t.Errorf("error should explain coverage: %v", err)
	}
	if !strings.Contains(err.Error(), "docs/notes.md") {
		t.Errorf("error should name the offending path: %v", err)
	}
}
