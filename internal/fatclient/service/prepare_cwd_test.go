package service

// Tests for PrepareLLMClaimCWD's iter-branch fallback contract.
//
// The bot daemon calls PrepareLLMClaimCWD on every claim. For the
// FIRST iter of a task (iter-1), the coordinator has assigned a
// branch NAME (e.g. "1-foo/summarize/iter-1") but no git ref exists
// for it yet — the ref gets created lazily at submit time by
// prepareBranchForCommit. Pre-fix the wrapper called
// MaterializeRunRepo(iterBranch) directly, which errored with
// "branch X has no local or origin ref", the daemon logged a warn,
// and the handler ran with empty CWD — breaking `system_prompt:
// prompts/foo.md` and every other repo-relative lookup.
//
// Post-fix: PrepareLLMClaimCWD falls back to the run branch when
// the iter branch ref isn't created yet. The run branch is the
// fork base for iter-1 by definition, so the materialized tree
// matches what iter-1 starts from anyway.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

func newProjectMetaServer(t *testing.T, projectID int64, defaultBranch string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             projectID,
			"name":           "test-project",
			"remote_url":     "",
			"default_branch": defaultBranch,
		})
	})
	return httptest.NewServer(mux)
}

// TestPrepareLLMClaimCWD_IterBranchRefAbsent_FallsBackToRunBranch
// reproduces showcase_v13's "materialize iter-branch tree into
// claim CWD: branch '…' has no local or origin ref" error path.
//
// Setup mirrors production: a project clone exists with the run
// branch as the only ref. Coord has assigned an iter branch NAME
// for this claim but the ref isn't yet in the local store (it
// gets created at submit time, not at claim time).
func TestPrepareLLMClaimCWD_IterBranchRefAbsent_FallsBackToRunBranch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := newProjectMetaServer(t, 11, "main")
	defer srv.Close()

	wsRoot := t.TempDir()
	regPath := filepath.Join(t.TempDir(), "projects.json")
	reg1 := projectreg.Open(regPath)
	projectPath1 := filepath.Join(wsRoot, "p1")
	if err := os.MkdirAll(projectPath1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg1.Upsert(projectreg.Entry{ID: 11, LocalPath: projectPath1}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger), enjugit.WithRegistry(reg1))
	if err != nil {
		t.Fatal(err)
	}

	// Seed a clone with one commit on `main` so the run branch has
	// a real tip the materializer can walk. Use a sibling enjugit
	// Workspace pointing at the same root so we don't need the
	// fatclient init flow.
	wf, err := ws.ForProject(11, "")
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if _, err := wf.CommitArbitraryFiles(enjugit.CommitArbitraryFilesRequest{
		Files: []enjugit.FileWrite{
			{RepoRelPath: "prompts/dev-bot2.md", Content: []byte("# Dev bot system prompt\n")},
			{RepoRelPath: "enju.yaml", Content: []byte("name: test\nversion: 1\n")},
		},
		Subject:     "seed",
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	fc := New(Config{
		WorkspaceRoot:   ws.RootDir(),
		ProjectRegistry: projectreg.Open(regPath),
		Coord: coord.New(coord.Config{
			BaseURL:   srv.URL,
			Username:  "dev-bot2",
			AuthToken: "test",
			Logger:    logger,
		}),
		Logger: logger,
	})

	// Coord-assigned iter branch name. No ref exists for it in the
	// local store — same shape as the bot's first iter-1 claim.
	iterBranch := "1-test/summarize/iter-1"
	runBranch := "main"

	path, err := fc.PrepareLLMClaimCWD(context.Background(),
		11, "dev-bot2", "11:1:summarize", 1, iterBranch, runBranch, "")
	if err != nil {
		t.Fatalf("PrepareLLMClaimCWD with absent iter branch ref: %v", err)
	}
	if path == "" {
		t.Fatal("expected materialized CWD path, got empty (handler would run with no CWD and system_prompt would fail)")
	}

	// Post-Phase-8 layout: bot scratch lives under the project's
	// .enju/ tree, not under a machine-wide ~/.enju/workspaces/.
	// Pin the shape so a future revert doesn't silently regress to
	// the machine-scoped path (which broke single-machine no-origin
	// projects on showcase_v14).
	wantPrefix := "/.enju/agents/dev-bot2/scratch/"
	if !strings.Contains(path, wantPrefix) {
		t.Errorf("scratch path %q should contain %q (project-scoped layout)", path, wantPrefix)
	}

	// The materialized tree must contain the seeded files —
	// proves the fallback actually walked the run branch.
	if _, err := os.Stat(filepath.Join(path, "prompts/dev-bot2.md")); err != nil {
		t.Errorf("prompts/dev-bot2.md not materialized in CWD %q: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(path, "enju.yaml")); err != nil {
		t.Errorf("enju.yaml not materialized in CWD %q: %v", path, err)
	}
}

// prepareCWDFixture spins up the same ws/registry/fatclient shape
// the test above uses, returning a workflow handle (for seeding
// git state) and the fatclient (for the call under test).
func prepareCWDFixture(t *testing.T, projectID int64) (*enjugit.Workflow, *FatClient) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newProjectMetaServer(t, projectID, "main")
	t.Cleanup(srv.Close)

	wsRoot := t.TempDir()
	regPath := filepath.Join(t.TempDir(), "projects.json")
	reg := projectreg.Open(regPath)
	projectPath := filepath.Join(wsRoot, "p")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(projectreg.Entry{ID: projectID, LocalPath: projectPath}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger), enjugit.WithRegistry(reg))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := ws.ForProject(projectID, "")
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	fc := New(Config{
		WorkspaceRoot:   ws.RootDir(),
		ProjectRegistry: projectreg.Open(regPath),
		Coord: coord.New(coord.Config{
			BaseURL: srv.URL, Username: "dev-agent", AuthToken: "test", Logger: logger,
		}),
		Logger: logger,
	})
	return wf, fc
}

// gitT runs git in dir with a fixed identity, failing the test on
// error. Used to build precise commit/branch topology directly —
// enjugit's CommitArbitraryFiles/CheckoutBranchFrom don't compose
// into "a commit on an arbitrary branch", so the per-iter-branch
// scenarios need raw git.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeRepoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareLLMClaimCWD_StaleIterBranch_NotDescendingFromBase_Rejected
// is the regression for the run-reproducibility bug the bot→agent
// rename surfaced. Iter-branch names (`<run-slug>/<task>/iter-N`)
// collide across runs sharing a slug (slugs recur across coord
// wipes), so a prior run leaves `1-test/review_summary/iter-1`
// pinned to an OLD (pre-rename) tree. The trust test is descent
// from THIS run's pinned base commit, which catches the
// high-severity class: a prior run whose HEAD advanced has a
// DIFFERENT baseSHA, so its leftover ref does not descend from
// this run's base. This pins the *divergent* flavor — a ref left
// by a failed/terminated prior run, never merged, so NOT an
// ancestor of the run branch — which the ancestor-of-runBranch
// heuristic missed and which recurred live on `review_summary`.
// (Known residual, not covered here: two runs from an unchanged
// HEAD share a baseSHA, so a same-base leftover still descends and
// is trusted — lower severity, structural cure is run-unique
// iter-branch names; see fatclient.go PrepareLLMClaimCWD.)
// baseSHA set + ref not descending from it ⇒ reject ⇒ materialize
// the pinned run tree.
func TestPrepareLLMClaimCWD_StaleIterBranch_NotDescendingFromBase_Rejected(t *testing.T) {
	wf, fc := prepareCWDFixture(t, 12)
	wd := wf.WorkDir()

	// OLD commit on main (pre-rename tree).
	gitT(t, wd, "checkout", "-B", "main")
	writeRepoFile(t, wd, "enju.yaml", "name: test\nversion: 1\n")
	writeRepoFile(t, wd, "prompts/reviewer-bot2.md", "# old\n")
	gitT(t, wd, "add", "-A")
	gitT(t, wd, "commit", "-m", "old tree")

	// A PRIOR run's iter branch forked at OLD, then DIVERGED with
	// its own commit — never merged ⇒ NOT an ancestor of main (the
	// flavor the ancestor-of-runBranch heuristic missed).
	gitT(t, wd, "checkout", "-b", "1-test/review_summary/iter-1")
	writeRepoFile(t, wd, "stale-marker.md", "# from a prior failed run\n")
	gitT(t, wd, "add", "-A")
	gitT(t, wd, "commit", "-m", "prior-run divergent submit")
	gitT(t, wd, "checkout", "main")

	// THIS run's pinned base = main after the rename.
	writeRepoFile(t, wd, "prompts/reviewer-agent.md", "# renamed\n")
	gitT(t, wd, "add", "-A")
	gitT(t, wd, "commit", "-m", "rename prompt")
	baseSHA := gitT(t, wd, "rev-parse", "HEAD")

	path, err := fc.PrepareLLMClaimCWD(context.Background(),
		12, "reviewer-agent", "12:1:review_summary", 1,
		"1-test/review_summary/iter-1", "main", baseSHA)
	if err != nil {
		t.Fatalf("PrepareLLMClaimCWD: %v", err)
	}
	if path == "" {
		t.Fatal("expected materialized CWD path, got empty")
	}
	// Stale ref does NOT descend from baseSHA ⇒ rejected ⇒ pinned
	// run tree materialized (has the renamed prompt, not the stale
	// divergent marker).
	if _, err := os.Stat(filepath.Join(path, "prompts/reviewer-agent.md")); err != nil {
		t.Errorf("prompts/reviewer-agent.md missing in CWD %q — materialized the STALE divergent iter branch (the recurred bug): %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(path, "stale-marker.md")); err == nil {
		t.Errorf("stale-marker.md present in CWD %q — stale prior-run iter tree leaked in", path)
	}
}

// TestPrepareLLMClaimCWD_GenuineInRunIterBranch_Used pins the other
// side: a genuine re-claim's iter branch IS `baseSHA + this run's
// submit`, so baseSHA is in its ancestry → it must still be used.
// The fix must not regress the legitimate iter-branch fast-path.
func TestPrepareLLMClaimCWD_GenuineInRunIterBranch_Used(t *testing.T) {
	wf, fc := prepareCWDFixture(t, 13)
	wd := wf.WorkDir()

	// THIS run's pinned base.
	gitT(t, wd, "checkout", "-B", "main")
	writeRepoFile(t, wd, "enju.yaml", "name: test\nversion: 1\n")
	gitT(t, wd, "add", "-A")
	gitT(t, wd, "commit", "-m", "base")
	baseSHA := gitT(t, wd, "rev-parse", "HEAD")

	// Genuine in-run iteration: forked FROM base + this run's
	// submit commit ⇒ descends from baseSHA (unmerged, so also not
	// an ancestor of main — but baseSHA-descent is what matters).
	gitT(t, wd, "checkout", "-b", "1-test/summarize/iter-1")
	writeRepoFile(t, wd, "iter-only.md", "# in-progress iteration\n")
	gitT(t, wd, "add", "-A")
	gitT(t, wd, "commit", "-m", "iteration work")
	gitT(t, wd, "checkout", "main")

	path, err := fc.PrepareLLMClaimCWD(context.Background(),
		13, "dev-agent", "13:1:summarize", 2,
		"1-test/summarize/iter-1", "main", baseSHA)
	if err != nil {
		t.Fatalf("PrepareLLMClaimCWD: %v", err)
	}
	if path == "" {
		t.Fatal("expected materialized CWD path, got empty")
	}
	// baseSHA is in the iter branch's ancestry ⇒ kept ⇒ the
	// iteration's own file is present.
	if _, err := os.Stat(filepath.Join(path, "iter-only.md")); err != nil {
		t.Errorf("iter-only.md missing in CWD %q — the fix wrongly skipped a genuine in-run iter branch: %v", path, err)
	}
}
