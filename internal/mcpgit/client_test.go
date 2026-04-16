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

// TestResolveWinningOption covers Phase E.2's
// {{task.winning_option}} accessor: an upstream vote task's
// VoteChoice gets surfaced on the dependency ref, the resolver
// attaches it to the result map, and the template substitution
// hits the top-level field lookup in extractField.
func TestResolveWinningOption(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(90, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Seed a vote task's "accepted" commit: the voter's
	// commentary is the result.md; the winning option id rides
	// along on the DependencyRef (mirroring what the coordinator
	// would populate from tasks.vote_choice).
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:pick_db",
		Username: "alice",
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(ResultDir(90, 1, "", "pick_db"), "result.md"),
				Content:     []byte("DuckDB fits the workload best."),
			},
		},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	resolved, err := proj.Resolve(ResolveInput{
		PromptTemplate: "Rationale for {{pick_db.winning_option}}: {{pick_db.content}}",
		Dependencies: []DependencyRef{
			{
				TaskDefID:  "pick_db",
				CommitSHA:  res.CommitSHA,
				ResultPath: ResultDir(90, 1, "", "pick_db"),
				VoteChoice: "duckdb",
			},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resolved.Prompt, "duckdb") {
		t.Errorf("expected winning_option to resolve to 'duckdb', got: %q", resolved.Prompt)
	}
	if !strings.Contains(resolved.Prompt, "DuckDB fits the workload best") {
		t.Errorf("expected commentary content to still resolve via {{task.content}}, got: %q", resolved.Prompt)
	}
	if strings.Contains(resolved.Prompt, "{{") {
		t.Errorf("unresolved placeholder: %q", resolved.Prompt)
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
	// Human commit (no ModelName) should NOT have AI-Model trailer.
	if strings.Contains(c1.Message, "AI-Model:") {
		t.Errorf("human commit should not have AI-Model trailer, got: %s", c1.Message)
	}

	// Case 2: AI citizen has ModelName — commit gets AI-Model trailer.
	proj.Lock()
	res2, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:      "1:1:bot_task",
		Username:    "claude-bot",
		AuthorName:  "Claude Bot",
		AuthorEmail: "claude@enju.local",
		ModelName:   "claude-sonnet-4-20250514",
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(ResultDir(51, 1, "", "bot_task"), "result.md"),
				Content:     []byte("bot's result"),
			},
		},
	})
	proj.Unlock()
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

// TestPushForceOverwritesDivergedRemote covers the force-push
// recovery path used by the explicit force-sync MCP tool. We simulate
// a diverged remote by pointing two independently-seeded clients at
// the same bare repo, then verify that PushForce from the second
// client overwrites the first client's commit.
func TestPushForceOverwritesDivergedRemote(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	// Client A writes and pushes normally to bare.
	wsA, _ := NewWorkspace(t.TempDir(), nullLogger())
	projA, err := wsA.ForProject(60, bare)
	if err != nil {
		t.Fatalf("clone A: %v", err)
	}
	projA.Lock()
	if _, err := projA.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:a",
		Username: "alice",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(60, 1, "", "a"), "result.md"), Content: []byte("alice v1")},
		},
	}); err != nil {
		t.Fatalf("A submit: %v", err)
	}
	projA.Unlock()

	// Client B starts on an unrelated bare (same seed, different
	// history). Write + commit locally so HEAD is a real commit.
	bareB := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bareB)
	wsB, _ := NewWorkspace(t.TempDir(), nullLogger())
	projB, err := wsB.ForProject(60, bareB)
	if err != nil {
		t.Fatalf("clone B: %v", err)
	}
	projB.Lock()
	if _, err := projB.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:b",
		Username: "bob",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(60, 1, "", "b"), "result.md"), Content: []byte("bob v1")},
		},
	}); err != nil {
		t.Fatalf("B initial submit: %v", err)
	}
	projB.Unlock()

	// Repoint B at A's bare. Normal Push should fail (divergent
	// histories), PushForce should win.
	if err := projB.repo.DeleteRemote("origin"); err != nil {
		t.Fatalf("delete origin: %v", err)
	}
	if _, err := projB.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bare},
	}); err != nil {
		t.Fatalf("recreate origin: %v", err)
	}
	projB.remoteURL = bare

	projB.Lock()
	if err := projB.Push(); err == nil {
		t.Fatal("expected normal Push to fail against diverged remote")
	}
	if err := projB.PushForce(); err != nil {
		t.Fatalf("PushForce: %v", err)
	}
	projB.Unlock()

	verifyDir := t.TempDir()
	if _, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(60, 1, "", "a"), "result.md")); !os.IsNotExist(err) {
		t.Errorf("expected A's file to be gone after force push, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(60, 1, "", "b"), "result.md")); err != nil {
		t.Errorf("expected B's file on remote after force push: %v", err)
	}
}

// TestSubmitRetryExhaustionNamesStep verifies that when retries are
// exhausted, the final error names which step (sync/commit/push)
// failed last — not just the raw underlying error. Covers the B1a
// retry labeling improvement.
func TestSubmitRetryExhaustionNamesStep(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(61, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Point the project at a bogus remote so every fetch fails —
	// exercises the "sync" failure path inside the retry loop.
	if err := proj.repo.DeleteRemote("origin"); err != nil {
		t.Fatalf("delete origin: %v", err)
	}
	bogus := filepath.Join(t.TempDir(), "nonexistent.git")
	if _, err := proj.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bogus},
	}); err != nil {
		t.Fatalf("recreate origin: %v", err)
	}
	proj.remoteURL = bogus

	proj.Lock()
	_, err = proj.SubmitTaskResult(SubmitRequest{
		TaskID:     "1:1:x",
		Username:   "alice",
		MaxRetries: 2,
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(61, 1, "", "x"), "result.md"), Content: []byte("data")},
		},
	})
	proj.Unlock()
	if err == nil {
		t.Fatal("expected submit to fail against bogus remote")
	}
	msg := err.Error()
	if !strings.Contains(msg, "submit failed after 2 attempts") {
		t.Errorf("expected attempt count in error, got: %q", msg)
	}
	if !strings.Contains(msg, "sync step") {
		t.Errorf("expected 'sync step' label in error, got: %q", msg)
	}
}

// TestFriendlyGitErrorHints covers the auth/network hint helper
// added in B1a — each branch of the pattern match should produce a
// distinguishable hint when given a representative error string.
func TestFriendlyGitErrorHints(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantHint string
	}{
		{"ssh", errStr("ssh: handshake failed: no supported methods"), "SSH agent"},
		{"publickey", errStr("publickey denied"), "SSH agent"},
		{"https 401", errStr("authentication required: HTTP 401"), "credential helper"},
		{"403", errStr("remote: HTTP 403 forbidden"), "credential helper"},
		{"non-ff", errStr("non-fast-forward update rejected"), "enju_project_sync"},
		{"dns", errStr("dial tcp: lookup git.example: no such host"), "network/DNS"},
		{"timeout", errStr("i/o timeout on fetch"), "network/DNS"},
		{"not found", errStr("repository not found"), "verify the remote URL"},
	}
	// Local path variant — same underlying error, different hint.
	t.Run("local path not found", func(t *testing.T) {
		got := friendlyGitError("clone", "/tmp/does-not-exist.git", errStr("repository not found"))
		if got == nil {
			t.Fatal("nil error")
		}
		if !strings.Contains(got.Error(), "valid bare repository") {
			t.Errorf("expected local-path hint, got: %q", got.Error())
		}
		if strings.Contains(got.Error(), "your account has access") {
			t.Errorf("local-path error should NOT include credentials hint, got: %q", got.Error())
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := friendlyGitError("push", "git@example:foo.git", tc.err)
			if got == nil {
				t.Fatalf("nil error")
			}
			if !strings.Contains(got.Error(), tc.wantHint) {
				t.Errorf("expected hint containing %q, got: %q", tc.wantHint, got.Error())
			}
			if !strings.Contains(got.Error(), "push") {
				t.Errorf("expected op name 'push' in message, got: %q", got.Error())
			}
		})
	}

	// Unclassified errors pass through without a hint suffix.
	plain := friendlyGitError("clone", "", errStr("some random non-matching failure"))
	if strings.Contains(plain.Error(), "hint:") {
		t.Errorf("unclassified error should not carry a hint, got: %q", plain.Error())
	}
}

// TestCrossWorkspaceFlockSerialization verifies that two Workspace
// instances pointed at the same root dir (simulating two MCP
// processes running against the same ~/.enju/workspaces) serialize
// their Project.Lock() calls via the on-disk flock. The second
// Lock must block until the first Unlock happens.
func TestCrossWorkspaceFlockSerialization(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	sharedRoot := t.TempDir()

	wsA, _ := NewWorkspace(sharedRoot, nullLogger())
	projA, err := wsA.ForProject(80, bare)
	if err != nil {
		t.Fatalf("wsA ForProject: %v", err)
	}

	wsB, _ := NewWorkspace(sharedRoot, nullLogger())
	projB, err := wsB.ForProject(80, bare)
	if err != nil {
		t.Fatalf("wsB ForProject: %v", err)
	}

	// Sanity: different in-process handles (each workspace has its
	// own clients map), but pointing at the same clone on disk.
	if projA == projB {
		t.Fatal("expected distinct Project instances across Workspaces")
	}
	if projA.WorkDir() != projB.WorkDir() {
		t.Fatalf("expected same work dir across Workspaces, got %q vs %q",
			projA.WorkDir(), projB.WorkDir())
	}

	// A locks first.
	projA.Lock()

	// B tries to lock — should block until A unlocks. Run it in a
	// goroutine and observe it's still waiting after a short
	// moment.
	done := make(chan struct{})
	go func() {
		projB.Lock()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("projB.Lock() returned while projA was still holding the lock")
	case <-time.After(50 * time.Millisecond):
		// Expected: B is blocked on A.
	}

	projA.Unlock()

	select {
	case <-done:
		// Good: B acquired once A released.
	case <-time.After(2 * time.Second):
		t.Fatal("projB.Lock() never returned after projA.Unlock()")
	}
	projB.Unlock()
}

// TestLeaveProjectRemovesClone verifies that LeaveProject drops the
// cached handle and wipes the on-disk clone, and that a subsequent
// ForProject call re-clones from the remote cleanly.
func TestLeaveProjectRemovesClone(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	wsDir := t.TempDir()
	ws, _ := NewWorkspace(wsDir, nullLogger())
	proj, err := ws.ForProject(70, bare)
	if err != nil {
		t.Fatalf("first clone: %v", err)
	}
	workDir := proj.WorkDir()
	if _, err := os.Stat(workDir); err != nil {
		t.Fatalf("expected clone dir to exist: %v", err)
	}
	if !ws.HasLocalClone(70) {
		t.Fatal("expected HasLocalClone to report true before leave")
	}

	if err := ws.LeaveProject(70); err != nil {
		t.Fatalf("LeaveProject: %v", err)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("expected clone dir to be gone, stat err: %v", err)
	}
	if ws.HasLocalClone(70) {
		t.Error("expected HasLocalClone to report false after leave")
	}

	// Leaving a project that was never opened should be a no-op, not an error.
	if err := ws.LeaveProject(999); err != nil {
		t.Errorf("LeaveProject on unknown project: %v", err)
	}

	// Next ForProject should re-clone successfully.
	proj2, err := ws.ForProject(70, bare)
	if err != nil {
		t.Fatalf("reclone after leave: %v", err)
	}
	if proj2.WorkDir() != workDir {
		t.Errorf("expected same work dir after reclone, got %s vs %s", proj2.WorkDir(), workDir)
	}
}

// TestSlugifyProjectDir verifies that ForProject with a project name
// creates a "{slug}-{id}" directory, and that an existing numeric-only
// directory is auto-migrated to the slug form.
func TestSlugifyProjectDir(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	wsDir := t.TempDir()
	ws, _ := NewWorkspace(wsDir, nullLogger())

	// Case 1: passing a project name creates a slug-based dir.
	proj, err := ws.ForProject(7, bare, "Battle Test Alpha")
	if err != nil {
		t.Fatalf("clone with name: %v", err)
	}
	expected := filepath.Join(wsDir, "battle-test-alpha-7")
	if proj.WorkDir() != expected {
		t.Errorf("expected workdir %s, got %s", expected, proj.WorkDir())
	}
	if !ws.HasLocalClone(7) {
		t.Error("HasLocalClone should find slug-named dir")
	}

	// Case 2: legacy numeric dir gets auto-migrated.
	// Create a numeric-only clone, then call ForProject with a name.
	ws2, _ := NewWorkspace(t.TempDir(), nullLogger())
	projOld, err := ws2.ForProject(8, bare) // no name → numeric dir
	if err != nil {
		t.Fatalf("clone without name: %v", err)
	}
	numericDir := projOld.WorkDir()
	if filepath.Base(numericDir) != "8" {
		t.Fatalf("expected numeric dir '8', got %s", filepath.Base(numericDir))
	}
	// Clear cached handle so ForProject re-resolves the directory.
	ws2.mu.Lock()
	delete(ws2.clients, 8)
	ws2.mu.Unlock()
	// Now call with a name — should rename the directory.
	proj2, err := ws2.ForProject(8, bare, "My Project")
	if err != nil {
		t.Fatalf("reopen with name: %v", err)
	}
	if filepath.Base(proj2.WorkDir()) != "my-project-8" {
		t.Errorf("expected migrated dir 'my-project-8', got %s", filepath.Base(proj2.WorkDir()))
	}
	// Old numeric dir should be gone.
	if _, err := os.Stat(numericDir); !os.IsNotExist(err) {
		t.Error("expected old numeric dir to be gone after migration")
	}
}

// TestSlugify checks edge cases of the slugify helper.
func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Battle Test Alpha", "battle-test-alpha"},
		{"  spaces  ", "spaces"},
		{"UPPER-case_MIX", "upper-case-mix"},
		{"---dashes---", "dashes"},
		{"123numbers", "123numbers"},
		{"", ""},
		{"a", "a"},
	}
	for _, tc := range cases {
		got := slugify(tc.in)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// errStr is a tiny test helper: build an error from a literal string
// without pulling in errors.New at every call site.
func errStr(s string) error { return &stringErr{s} }

type stringErr struct{ msg string }

func (e *stringErr) Error() string { return e.msg }
