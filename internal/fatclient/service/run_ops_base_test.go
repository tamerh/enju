package service

// Tests for the `enju go --base` control flow (prepareRunFromBase):
// forking a run from an explicit non-default branch, reading the
// workflow from that branch's committed tree, with the worktree
// guard + EnsureBundleOnDefault deliberately skipped.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// openBaseTestWorkflow plants a real repo whose workflow lives ONLY on
// a non-default "feature" branch (the default branch has no enju.yaml),
// leaves the worktree on the default branch, registers the project, and
// returns an opened Workflow plus the feature-branch tip SHA. This is
// the exact shape --base exists for: the work is on a branch other than
// the default and other than what's checked out.
func openBaseTestWorkflow(t *testing.T) (*enjugit.Workflow, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "proj")

	gittest.Init(t, dir)
	gittest.CommitAs(t, dir, "README.md", "# proj\n", "seed", "t", "t@e.local")
	defBranch := strings.TrimSpace(gittest.Run(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))

	// Commit the workflow on a feature branch only.
	gittest.Run(t, dir, "checkout", "-b", "feature")
	gittest.CommitAs(t, dir,
		"enju.yaml",
		"name: from-feature\nversion: 1\ntasks:\n  - id: t\n    action: answer\n    prompt: hi\n",
		"add workflow on feature", "t", "t@e.local")
	featureTip := gittest.HeadSHA(t, dir)

	// Park the worktree back on the default branch — so the run reads
	// feature's COMMITTED tree, not the checked-out worktree.
	gittest.Checkout(t, dir, defBranch)

	regPath := filepath.Join(tmp, "projects.json")
	if err := projectreg.Open(regPath).Register(projectreg.Entry{ID: 7, LocalPath: dir}); err != nil {
		t.Fatal(err)
	}
	wsRoot := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(logger), enjugit.WithRegistry(projectreg.Open(regPath)))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := ws.OpenExisting(7)
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}
	return wf, featureTip
}

// Forks from the named base branch and reads the workflow from its
// committed tree, even though that branch is neither the default nor
// the one checked out.
func TestPrepareRunFromBase_ForksFromBaseBranch(t *testing.T) {
	wf, featureTip := openBaseTestWorkflow(t)
	prep, err := (&FatClient{}).prepareRunFromBase(wf, "enju.yaml", "feature")
	if err != nil {
		t.Fatalf("prepareRunFromBase: %v", err)
	}
	if prep.SourceCommit != featureTip {
		t.Errorf("SourceCommit: got %q, want feature tip %q", prep.SourceCommit, featureTip)
	}
	if !strings.Contains(prep.YAMLContent, "from-feature") {
		t.Errorf("workflow should be read from the feature branch, got: %q", prep.YAMLContent)
	}
}

// "HEAD" resolves to the currently checked-out branch.
func TestPrepareRunFromBase_HEADResolvesCurrentBranch(t *testing.T) {
	wf, featureTip := openBaseTestWorkflow(t)
	// Check out feature so HEAD means feature.
	gittest.Checkout(t, wf.WorkDir(), "feature")

	prep, err := (&FatClient{}).prepareRunFromBase(wf, "enju.yaml", "HEAD")
	if err != nil {
		t.Fatalf("prepareRunFromBase HEAD: %v", err)
	}
	if prep.SourceCommit != featureTip {
		t.Errorf("--base HEAD should fork from the current branch tip %q, got %q", featureTip, prep.SourceCommit)
	}
}

// An unknown base branch fails with a clear, actionable error rather
// than silently falling back to the default.
func TestPrepareRunFromBase_NotFound(t *testing.T) {
	wf, _ := openBaseTestWorkflow(t)
	_, err := (&FatClient{}).prepareRunFromBase(wf, "enju.yaml", "no-such-branch")
	if err == nil {
		t.Fatal("expected an error for a nonexistent base branch")
	}
	if !strings.Contains(err.Error(), "not found or has no commits") {
		t.Errorf("expected a base-not-found message, got: %v", err)
	}
}
