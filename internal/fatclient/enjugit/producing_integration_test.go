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

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// plumbingHash parses a hex SHA without dragging plumbing into
// the test bodies. Lets verify-step assertions do CommitObject
// lookups by SHA returned from SubmitTaskResult.
func plumbingHash(s string) plumbing.Hash { return plumbing.NewHash(s) }

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
	if _, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
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
	vRepo, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare})
	if err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	commit, err := vRepo.CommitObject(plumbingHash(res.CommitSHA))
	if err != nil {
		t.Fatalf("load commit: %v", err)
	}
	if !strings.Contains(commit.Message, "Task 1:1:write by @bob") {
		t.Fatalf("commit subject missing standard format: %q", commit.Message)
	}
	// enjugit threads the artifact list through the body line +
	// the Enju-Artifacts trailer (rather than putting "+ N
	// artifact(s)" in the subject like the project package did).
	// Both the body line and the trailer are checked so the
	// scanner round-trip stays covered.
	if !strings.Contains(commit.Message, "Artifacts: notes/intro.md") {
		t.Fatalf("commit body missing artifact list: %q", commit.Message)
	}
	if !strings.Contains(commit.Message, "Enju-Artifacts: notes/intro.md") {
		t.Fatalf("commit message missing Enju-Artifacts trailer: %q", commit.Message)
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
	vRepo, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare})
	if err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	c1, err := vRepo.CommitObject(plumbingHash(res.CommitSHA))
	if err != nil {
		t.Fatalf("load alice commit: %v", err)
	}
	if c1.Author.Name != "Alice Researcher" {
		t.Errorf("expected author name 'Alice Researcher', got %q", c1.Author.Name)
	}
	if c1.Author.Email != "alice@example.com" {
		t.Errorf("expected author email 'alice@example.com', got %q", c1.Author.Email)
	}
	// Human commit (no ModelName) should NOT have AI-Model trailer.
	if strings.Contains(c1.Message, "AI-Model:") {
		t.Errorf("human commit should not have AI-Model trailer, got: %s", c1.Message)
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
	vRepo2, err := gogit.PlainClone(verifyDir2, false, &gogit.CloneOptions{URL: bare})
	if err != nil {
		t.Fatalf("verify clone 2: %v", err)
	}
	c2, err := vRepo2.CommitObject(plumbingHash(res2.CommitSHA))
	if err != nil {
		t.Fatalf("load bot commit: %v", err)
	}
	if !strings.Contains(c2.Message, "AI-Model: claude-sonnet-4-20250514") {
		t.Errorf("AI commit should have AI-Model trailer, got: %s", c2.Message)
	}
	if !strings.Contains(c2.Message, "Co-Authored-By: Claude (claude-sonnet-4-20250514) <noreply@anthropic.com>") {
		t.Errorf("AI commit should have Co-Authored-By trailer, got: %s", c2.Message)
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
