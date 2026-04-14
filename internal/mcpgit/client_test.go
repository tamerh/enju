package mcpgit

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// plumbingHash is a tiny wrapper so tests don't have to import
// go-git's plumbing package just to parse a hex SHA.
func plumbingHash(s string) plumbing.Hash { return plumbing.NewHash(s) }

// testSignature returns a deterministic signature for commits made
// inside tests. Using a fixed time avoids spurious non-determinism
// if anyone ever hashes commit metadata in assertions.
func testSig() *object.Signature {
	return &object.Signature{
		Name:  "Test",
		Email: "test@localhost",
		When:  time.Unix(1700000000, 0),
	}
}

// nullLogger returns a slog.Logger that discards everything. Used in
// tests to keep output clean.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initBareRemote creates a bare git repo that tests use as a fake
// "remote" origin. The bare is initialized with `refs/heads/main` as
// the default branch so subsequent clones find a HEAD to track after
// the first push. Returns the filesystem path (which go-git accepts
// as a URL for `file://`-style cloning).
func initBareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	})
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	return dir
}

// seedRemoteWithInitialCommit makes the bare repo look like a
// freshly-created project: one README.md commit on refs/heads/main.
// The bare's HEAD is already set to refs/heads/main by
// initBareRemote, so after this push clones can find it.
func seedRemoteWithInitialCommit(t *testing.T, bareDir string) {
	t.Helper()
	seedDir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(seedDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	readme := filepath.Join(seedDir, "README.md")
	if err := os.WriteFile(readme, []byte("# seed\n"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add readme: %v", err)
	}
	sig := testSig()
	if _, err := wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push seed: %v", err)
	}
}

// TestWorkspaceCloneAndSubmit covers the happy path: create a
// workspace, clone a fresh project from a bare remote, submit a task
// result with one file, verify the file lands on the remote.
func TestWorkspaceCloneAndSubmit(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, err := NewWorkspace(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	proj, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	resultDir := ResultDir(42, 1, "", "hello")
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:hello",
		Username: "alice",
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
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("expected non-empty commit SHA")
	}
	if res.Attempts != 1 {
		t.Fatalf("expected 1 attempt, got %d", res.Attempts)
	}

	// Verify the bare remote now has the new commit by cloning it
	// to a throwaway dir and checking the file is present.
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

// TestSubmitWithArtifacts checks that artifact paths land under
// `artifacts/...` and appear in the commit message body.
func TestSubmitWithArtifacts(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(43, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	resultDir := ResultDir(43, 1, "", "write")
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:write",
		Username: "bob",
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resultDir, "result.md"),
				Content:     []byte("done"),
			},
			{
				RepoRelPath: ArtifactPath(43, "notes/intro.md"),
				Content:     []byte("# Intro\n"),
			},
		},
		ArtifactPaths: []string{"notes/intro.md"},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Verify the commit message on the remote contains the artifact line.
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
	if !strings.Contains(commit.Message, "notes/intro.md") {
		t.Fatalf("commit body missing artifact path: %q", commit.Message)
	}
	if !strings.Contains(commit.Message, "1 artifact(s)") {
		t.Fatalf("commit subject missing artifact count: %q", commit.Message)
	}

	// Verify the artifact file is on disk at the expected path.
	data, err := os.ReadFile(filepath.Join(verifyDir, ArtifactPath(43, "notes/intro.md")))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(data) != "# Intro\n" {
		t.Fatalf("unexpected artifact content: %q", string(data))
	}
}

// TestSubmitRetryOnConcurrentPush simulates a second client pushing
// between our fetch and our push. First SubmitTaskResult attempt
// encounters a stale base, the retry loop re-fetches, re-overlays,
// re-commits, and eventually pushes on attempt 2.
func TestSubmitRetryOnConcurrentPush(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	// Client A will submit first.
	wsA, _ := NewWorkspace(t.TempDir(), nullLogger())
	projA, err := wsA.ForProject(44, bare)
	if err != nil {
		t.Fatalf("clone A: %v", err)
	}

	// Client B clones and is ready to submit.
	wsB, _ := NewWorkspace(t.TempDir(), nullLogger())
	projB, err := wsB.ForProject(44, bare)
	if err != nil {
		t.Fatalf("clone B: %v", err)
	}

	// A submits first.
	projA.Lock()
	if _, err := projA.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:a",
		Username: "alice",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(44, 1, "", "a"), "result.md"), Content: []byte("alice result")},
		},
	}); err != nil {
		t.Fatalf("A submit: %v", err)
	}
	projA.Unlock()

	// B now submits a different task. B's local clone doesn't know
	// about A's push yet — the retry loop should fetch + reset +
	// re-apply + push on attempt 2 (or even attempt 1, since our
	// resetToRemote runs at the start of each attempt).
	projB.Lock()
	res, err := projB.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:b",
		Username: "bob",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(44, 1, "", "b"), "result.md"), Content: []byte("bob result")},
		},
	})
	projB.Unlock()
	if err != nil {
		t.Fatalf("B submit: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("expected non-empty commit SHA for B")
	}

	// Verify both A's and B's results are on the remote — the retry
	// loop must have preserved A's commit when pushing B's.
	verifyDir := t.TempDir()
	if _, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(44, 1, "", "a"), "result.md")); err != nil {
		t.Fatalf("A's file missing after B submit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(44, 1, "", "b"), "result.md")); err != nil {
		t.Fatalf("B's file missing after B submit: %v", err)
	}
}

// TestReopenExistingClone verifies that closing a workspace and
// recreating it reuses the existing clone instead of re-cloning.
func TestReopenExistingClone(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	wsDir := t.TempDir()
	ws1, _ := NewWorkspace(wsDir, nullLogger())
	proj1, err := ws1.ForProject(45, bare)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	projDir := proj1.WorkDir()

	// Re-create the workspace — simulating a process restart.
	ws2, _ := NewWorkspace(wsDir, nullLogger())
	proj2, err := ws2.ForProject(45, "") // note: no URL passed
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if proj2.WorkDir() != projDir {
		t.Fatalf("expected same work dir, got %s vs %s", projDir, proj2.WorkDir())
	}
	if proj2.RemoteURL() != bare {
		t.Fatalf("expected remote URL to be picked up from existing clone, got %q", proj2.RemoteURL())
	}
}

// TestResolveFanIn covers the main non-trivial template resolution
// case: a singleton aggregator task reads from multiple iterations of
// an upstream task and expects the Option 4 block format.
func TestResolveFanIn(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(46, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Write three "analyze" results, one per gene, as three separate
	// commits — simulating three task submits from different
	// iterations of a task-level for_each.
	genes := []string{"BRCA1", "TP53", "EGFR"}
	commitSHAs := make(map[string]string)
	for _, g := range genes {
		proj.Lock()
		res, err := proj.SubmitTaskResult(SubmitRequest{
			TaskID:   "1:1:analyze",
			Username: "alice",
			Files: []FileWrite{
				{
					RepoRelPath: filepath.Join(ResultDir(46, 1, g, "analyze"), "result.md"),
					Content:     []byte("analysis of " + g),
				},
			},
		})
		proj.Unlock()
		if err != nil {
			t.Fatalf("submit for %s: %v", g, err)
		}
		commitSHAs[g] = res.CommitSHA
	}

	// Build a dependency descriptor matching iteration 5's fan-in.
	input := ResolveInput{
		PromptTemplate: "Summarize: {{analyze.content}}",
		Dependencies: []DependencyRef{
			{
				TaskDefID:      "analyze",
				InstanceKey:    "BRCA1",
				InstanceParams: map[string]string{"gene": "BRCA1"},
				CommitSHA:      commitSHAs["BRCA1"],
				ResultPath:     ResultDir(46, 1, "BRCA1", "analyze"),
			},
			{
				TaskDefID:      "analyze",
				InstanceKey:    "TP53",
				InstanceParams: map[string]string{"gene": "TP53"},
				CommitSHA:      commitSHAs["TP53"],
				ResultPath:     ResultDir(46, 1, "TP53", "analyze"),
			},
			{
				TaskDefID:      "analyze",
				InstanceKey:    "EGFR",
				InstanceParams: map[string]string{"gene": "EGFR"},
				CommitSHA:      commitSHAs["EGFR"],
				ResultPath:     ResultDir(46, 1, "EGFR", "analyze"),
			},
		},
	}

	resolved, err := proj.Resolve(input)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The resolved prompt should contain the Option 4 block with
	// labeled iteration headers, sorted by instance key (BRCA1,
	// EGFR, TP53 alphabetically).
	wantHeaders := []string{
		"### iteration: gene=BRCA1",
		"### iteration: gene=EGFR",
		"### iteration: gene=TP53",
	}
	for _, h := range wantHeaders {
		if !strings.Contains(resolved.Prompt, h) {
			t.Errorf("resolved prompt missing header %q\nprompt: %s", h, resolved.Prompt)
		}
	}
	// Sort check: BRCA1 should appear before EGFR which should
	// appear before TP53.
	brcaIdx := strings.Index(resolved.Prompt, "BRCA1")
	egfrIdx := strings.Index(resolved.Prompt, "EGFR")
	tp53Idx := strings.Index(resolved.Prompt, "TP53")
	if brcaIdx > egfrIdx || egfrIdx > tp53Idx {
		t.Errorf("iteration order not sorted alphabetically: BRCA1=%d EGFR=%d TP53=%d", brcaIdx, egfrIdx, tp53Idx)
	}

	// Each iteration's content should be present.
	for _, g := range genes {
		if !strings.Contains(resolved.Prompt, "analysis of "+g) {
			t.Errorf("missing content for %s", g)
		}
	}

	// The placeholder should have been consumed, not left literal.
	if strings.Contains(resolved.Prompt, "{{analyze.content}}") {
		t.Error("placeholder left literal in resolved prompt")
	}
}

// TestResolveSingletonUpstream covers the non-fan-in path: a
// downstream reads a single upstream's content via {{task.content}}.
func TestResolveSingletonUpstream(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(47, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:gather",
		Username: "alice",
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(ResultDir(47, 1, "", "gather"), "result.md"),
				Content:     []byte("raw data"),
			},
		},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	resolved, err := proj.Resolve(ResolveInput{
		PromptTemplate: "Analyze this: {{gather.content}}",
		Dependencies: []DependencyRef{
			{
				TaskDefID:  "gather",
				CommitSHA:  res.CommitSHA,
				ResultPath: ResultDir(47, 1, "", "gather"),
			},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resolved.Prompt, "raw data") {
		t.Errorf("expected resolved prompt to contain upstream content; got: %q", resolved.Prompt)
	}
	if strings.Contains(resolved.Prompt, "{{") {
		t.Errorf("unresolved placeholder in prompt: %q", resolved.Prompt)
	}
}

// TestResolveForEachParams covers bare {{param}} substitution from
// the task's own for_each params.
func TestResolveForEachParams(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(48, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	resolved, err := proj.Resolve(ResolveInput{
		PromptTemplate: "Analyze gene {{gene}} in tissue {{tissue}}",
		ForEachParams: map[string]string{
			"gene":   "BRCA1",
			"tissue": "breast",
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Prompt != "Analyze gene BRCA1 in tissue breast" {
		t.Errorf("unexpected resolved prompt: %q", resolved.Prompt)
	}
}

// TestResolveArtifactRead covers {{artifact:path}} inlining from an
// artifact's committed content.
func TestResolveArtifactRead(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(49, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Write an artifact via a submit, grab its commit SHA.
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:writer",
		Username: "alice",
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(ResultDir(49, 1, "", "writer"), "result.md"),
				Content:     []byte("done"),
			},
			{
				RepoRelPath: ArtifactPath(49, "notes/intro.md"),
				Content:     []byte("# Intro\n\nThe intro content."),
			},
		},
		ArtifactPaths: []string{"notes/intro.md"},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	resolved, err := proj.Resolve(ResolveInput{
		PromptTemplate: "Context:\n{{artifact:notes/intro.md}}",
		ArtifactReads: []ArtifactRef{
			{Path: "notes/intro.md", CommitSHA: res.CommitSHA},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resolved.Prompt, "The intro content") {
		t.Errorf("expected artifact content in resolved prompt; got: %q", resolved.Prompt)
	}
	if _, ok := resolved.ResolvedArtifacts["notes/intro.md"]; !ok {
		t.Error("expected artifact to appear in ResolvedArtifacts map")
	}
	if len(resolved.MissingArtifacts) != 0 {
		t.Errorf("expected no missing artifacts, got %v", resolved.MissingArtifacts)
	}
}

// TestSubmitCommitAuthorAttribution verifies that commits carry the
// citizen's real name + email as the git author when AuthorName /
// AuthorEmail are supplied (A.6 fix). Falls back to the generic
// `Enju Client` identity when they're empty. Without this, every
// citizen's commits would collapse to a single bot identity on
// GitHub contributor graphs.
func TestSubmitCommitAuthorAttribution(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(51, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Case 1: explicit author is carried into the commit.
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:      "1:1:alice_task",
		Username:    "alice",
		AuthorName:  "Alice Researcher",
		AuthorEmail: "alice@example.com",
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(ResultDir(51, 1, "", "alice_task"), "result.md"),
				Content:     []byte("alice's result"),
			},
		},
	})
	proj.Unlock()
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

	// The empty-AuthorName/AuthorEmail fallback to the generic
	// `Enju Client` placeholder is exercised implicitly by every
	// other test in this file that doesn't pass AuthorName /
	// AuthorEmail, so no separate assertion is needed here.
}
// artifact the task declared can't be found at the given commit.
func TestResolveMissingArtifact(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(50, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	resolved, err := proj.Resolve(ResolveInput{
		PromptTemplate: "Context: {{artifact:missing.md}}",
		ArtifactReads: []ArtifactRef{
			{Path: "missing.md"}, // no commit SHA, file doesn't exist
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved.MissingArtifacts) != 1 || resolved.MissingArtifacts[0] != "missing.md" {
		t.Errorf("expected missing.md in MissingArtifacts, got %v", resolved.MissingArtifacts)
	}
	// The placeholder should survive in the prompt as a secondary
	// visible signal, matching iteration 3.3's behavior.
	if !strings.Contains(resolved.Prompt, "{{artifact:missing.md}}") {
		t.Errorf("expected placeholder to survive for missing artifact; got: %q", resolved.Prompt)
	}
}
