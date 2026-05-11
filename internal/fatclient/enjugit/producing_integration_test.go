package enjugit

// Real-bare integration tests for Workflow.SubmitTaskResult.
// Uses a live bare git repo + ws.ForProject (a true clone) so
// commits land through the same go-git path production hits.
// The unit-level coverage in producing_test.go uses fake ops;
// this file covers the end-to-end shape of "submit → commit
// lands on bare → re-clone → bytes match."
//
// All four scenarios use BranchOverride="main" to skip topic-
// branch composition — these tests pin the submit primitive's
// behavior, not the topic-branch lifecycle (which has its own
// coverage in claim/submit integration paths).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// TestSubmitTaskResult_HappyPathIntegration covers the happy
// path: clone a fresh project from a bare remote, submit a task
// result with one file, verify the file lands on the remote.
func TestSubmitTaskResult_HappyPathIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	resultDir := resolveTestResultDir(1, "", "hello")
	res, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:hello",
		BranchOverride: "main",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resultDir, "result.md"),
				Content:     []byte("hello world"),
			},
			{
				RepoRelPath: filepath.Join(resultDir, "metadata.json"),
				Content:     []byte(`{"task_def_id":"hello"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("expected non-empty commit SHA")
	}

	// Verify the bare remote now has the new commit by cloning
	// it to a throwaway dir and checking the file is present.
	verifyDir := t.TempDir()
	gittest.Clone(t, verifyDir, bare)
	data, err := os.ReadFile(filepath.Join(verifyDir, resultDir, "result.md"))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

// TestSubmitTaskResult_WithArtifactsIntegration checks that
// artifact paths land under the artifact namespace (artifacts/...)
// AND appear in the commit message body + subject count.
func TestSubmitTaskResult_WithArtifactsIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(43, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	resultDir := resolveTestResultDir(1, "", "write")
	res, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:write",
		BranchOverride: "main",
		Citizen:        Identity{Name: "bob", Email: "bob@example.com"},
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resultDir, "result.md"),
				Content:     []byte("done"),
			},
			{
				RepoRelPath: ArtifactPath("notes/intro.md"),
				Content:     []byte("# Intro\n"),
			},
		},
		ArtifactPaths: []string{"notes/intro.md"},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Verify the commit message on the remote contains the
	// artifact line + count.
	verifyDir := t.TempDir()
	gittest.Clone(t, verifyDir, bare)
	commitMsg := gittest.Run(t, verifyDir, "log", "-1", "--format=%B", res.CommitSHA)
	if !strings.Contains(commitMsg, "Task 1:1:write by @bob") {
		t.Fatalf("commit subject missing standard format: %q", commitMsg)
	}
	// enjugit threads the artifact list through the body line +
	// the Enju-Artifacts trailer (rather than putting "+ N
	// artifact(s)" in the subject like the project package did).
	// Both the body line and the trailer are checked so the
	// scanner round-trip stays covered.
	if !strings.Contains(commitMsg, "Artifacts: notes/intro.md") {
		t.Fatalf("commit body missing artifact list: %q", commitMsg)
	}
	if !strings.Contains(commitMsg, "Enju-Artifacts: notes/intro.md") {
		t.Fatalf("commit message missing Enju-Artifacts trailer: %q", commitMsg)
	}

	// Verify the artifact file is on disk at the expected path.
	data, err := os.ReadFile(filepath.Join(verifyDir, ArtifactPath("notes/intro.md")))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(data) != "# Intro\n" {
		t.Fatalf("unexpected artifact content: %q", string(data))
	}
}

// TestSubmitTaskResult_AuthorAttributionIntegration verifies
// that commits carry the citizen's real name + email as the git
// author when Citizen.Name / Citizen.Email are supplied. Without
// this, every citizen's commits would collapse to a single bot
// identity on GitHub contributor graphs.
//
// Also pins the AI-Model trailer behavior: human commits (no
// ModelName) skip the trailer; AI commits (ModelName set) get
// both `AI-Model:` and `Co-Authored-By:` trailers.
func TestSubmitTaskResult_AuthorAttributionIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(51, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Case 1: explicit author is carried into the commit.
	res, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:alice_task",
		BranchOverride: "main",
		Citizen:        Identity{Name: "Alice Researcher", Email: "alice@example.com"},
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "alice_task"), "result.md"),
				Content:     []byte("alice's result"),
			},
		},
	})
	if err != nil {
		t.Fatalf("submit alice: %v", err)
	}

	verifyDir := t.TempDir()
	gittest.Clone(t, verifyDir, bare)
	authorName := gittest.Run(t, verifyDir, "log", "-1", "--format=%an", res.CommitSHA)
	authorEmail := gittest.Run(t, verifyDir, "log", "-1", "--format=%ae", res.CommitSHA)
	body := gittest.Run(t, verifyDir, "log", "-1", "--format=%B", res.CommitSHA)
	if authorName != "Alice Researcher" {
		t.Errorf("expected author name 'Alice Researcher', got %q", authorName)
	}
	if authorEmail != "alice@example.com" {
		t.Errorf("expected author email 'alice@example.com', got %q", authorEmail)
	}
	// Human commit (no ModelName) should NOT have AI-Model trailer.
	if strings.Contains(body, "AI-Model:") {
		t.Errorf("human commit should not have AI-Model trailer, got: %s", body)
	}

	// Case 2: AI citizen has ModelName — commit gets AI-Model trailer.
	res2, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:bot_task",
		BranchOverride: "main",
		Citizen:        Identity{Name: "Claude Bot", Email: "claude@enju.local"},
		ModelName:      "claude-sonnet-4-20250514",
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "bot_task"), "result.md"),
				Content:     []byte("bot's result"),
			},
		},
	})
	if err != nil {
		t.Fatalf("submit bot: %v", err)
	}

	// Re-clone to pick up the new commit.
	verifyDir2 := t.TempDir()
	gittest.Clone(t, verifyDir2, bare)
	body2 := gittest.Run(t, verifyDir2, "log", "-1", "--format=%B", res2.CommitSHA)
	if !strings.Contains(body2, "AI-Model: claude-sonnet-4-20250514") {
		t.Errorf("AI commit should have AI-Model trailer, got: %s", body2)
	}
	if !strings.Contains(body2, "Co-Authored-By: Claude (claude-sonnet-4-20250514) <noreply@anthropic.com>") {
		t.Errorf("AI commit should have Co-Authored-By trailer, got: %s", body2)
	}
}

// TestForProject_ReopenExistingClone verifies that constructing
// a fresh Workspace and calling ForProject on an id whose clone
// already exists on disk reuses that clone instead of re-cloning
// or erroring. Simulates the common "process restart" recovery
// path: enju mcp goes down, comes back, ForProject(id, "") with
// no remoteURL still works because the on-disk clone is enough.
func TestForProject_ReopenExistingClone(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	wsDir := t.TempDir()
	ws1, _ := NewWorkspace(wsDir, NewProductionConventions(), WithLogger(nullLogger()))
	wf1, err := ws1.ForProject(45, bare)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	projDir := wf1.WorkDir()

	// Re-create the workspace — simulating a process restart.
	// Pass empty remoteURL: the existing on-disk clone's origin
	// is the source of truth.
	ws2, _ := NewWorkspace(wsDir, NewProductionConventions(), WithLogger(nullLogger()))
	wf2, err := ws2.ForProject(45, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if wf2.WorkDir() != projDir {
		t.Fatalf("expected same work dir, got %s vs %s", projDir, wf2.WorkDir())
	}
	if wf2.RemoteURL() != bare {
		t.Fatalf("expected remote URL to be picked up from existing clone, got %q", wf2.RemoteURL())
	}
}
