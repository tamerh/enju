package enjugit

// Real-bare integration test for Workflow.MergeAcceptedTopic.
// The fake-ops unit tests in producing_test.go already cover the
// FF path, non-FF fallback, conflict translation, and trace shape.
// This file pins the end-to-end shape: parallel siblings actually
// merge with a real go-git operation against a real bare, the
// merge commit lands on the bare with the right parents + trailers,
// and both topics' files are reachable from the merge tip.

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// TestAutoMergeAcceptedTopic_NonFFDisjointWritesIntegration is the
// load-bearing parallel-siblings scenario. Two topics fork from
// the same main base; the first merges FF, the second goes through
// a real merge commit because main has advanced past its fork
// point. Asserts on the bare:
//   - merge tip has 2 parents pointing to topic-a's tip and topic-b's tip
//   - both files reachable from the merge tree
//   - merge author is the citizen who triggered the second merge (Bob)
//   - lineage trailers Enju-Merge:auto + Enju-Triggered-By:<task-id> present
//
// TODO(enjugit-merge-trailers): the project package's equivalent
// also asserted Enju-Merged-Topic:<topic> and Enju-Merged-Run:<run>
// trailers. enjugit's buildMergeTrailers (trailers.go) intentionally
// drops those because the merge subject "Merge <topic> into <run>"
// already encodes the same info, and no production reader consumes
// them. If a future event-log scanner needs structured access to
// the lineage names, restore them in trailers.go and assert here.
func TestAutoMergeAcceptedTopic_NonFFDisjointWritesIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(70, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Create both topics BEFORE any merge so they share the same
	// fork point (main's seed). After topic-a merges (FF), main
	// advances past topic-b's base, forcing the second merge onto
	// the non-FF path with a real merge commit.
	if _, err := wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "topic-a",
		Subject:     "topic a",
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.com",
		Files: []FileWrite{
			{RepoRelPath: "out/a.md", Content: []byte("alice")},
		},
	}); err != nil {
		t.Fatalf("seed topic-a: %v", err)
	}
	if _, err := wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "topic-b",
		Subject:     "topic b",
		AuthorName:  "Bob",
		AuthorEmail: "bob@example.com",
		Files: []FileWrite{
			{RepoRelPath: "out/b.md", Content: []byte("bob")},
		},
	}); err != nil {
		t.Fatalf("seed topic-b: %v", err)
	}

	// Capture the topic SHAs so we can assert merge parentage
	// against them. Read from the local clone — they're on the
	// bare too (CommitArbitraryFiles pushes).
	topicA, err := wf.git.ResolveRef("topic-a")
	if err != nil {
		t.Fatalf("resolve topic-a: %v", err)
	}
	topicB, err := wf.git.ResolveRef("topic-b")
	if err != nil {
		t.Fatalf("resolve topic-b: %v", err)
	}

	// First merge: topic-a → main. Same fork point → FF path.
	if _, err := wf.MergeAcceptedTopic("topic-a", "main",
		MergeAuthor{
			TaskID:       "task-a",
			AutoOrManual: "auto",
			Citizen:      Identity{Name: "Alice", Email: "alice@example.com"},
		}); err != nil {
		t.Fatalf("first merge (FF): %v", err)
	}

	// Second merge: topic-b → main. Main has advanced; non-FF
	// path triggers a real merge commit.
	if _, err := wf.MergeAcceptedTopic("topic-b", "main",
		MergeAuthor{
			TaskID:       "task-b",
			AutoOrManual: "auto",
			Citizen:      Identity{Name: "Bob", Email: "bob@example.com"},
		}); err != nil {
		t.Fatalf("second merge (non-FF): %v", err)
	}

	// Verify on the bare: merge commit shape + reachable files +
	// trailers + author. Read directly from the bare via git
	// plumbing — no local clone needed.
	tipSHA := gittest.RefSHA(t, bare, "refs/heads/main")

	// rev-list --parents -n1 <tip> returns "<tip> <p1> <p2>...".
	parentLine := gittest.Run(t, bare, "rev-list", "--parents", "-n1", tipSHA)
	parts := strings.Fields(parentLine)
	if len(parts) != 3 {
		t.Fatalf("merge tip should have 2 parents (rev-list --parents -n1 = %q)", parentLine)
	}
	// Parent[0] is the run-branch tip we merged onto (topic-a's
	// SHA, which FF'd to main). Parent[1] is topic-b's SHA.
	if parts[1] != topicA {
		t.Errorf("merge parent[0] = %s, want topic-a SHA %s", parts[1], topicA)
	}
	if parts[2] != topicB {
		t.Errorf("merge parent[1] = %s, want topic-b SHA %s", parts[2], topicB)
	}
	// Author: auto merges use the Enju-System identity by design.
	authorEmail := gittest.Run(t, bare, "log", "-1", "--format=%ae", tipSHA)
	if !strings.Contains(authorEmail, "enju-system") {
		t.Errorf("merge author email = %q, want enju-system identity for auto merge", authorEmail)
	}
	// Lineage trailers present (the 2 enjugit emits today).
	body := gittest.Run(t, bare, "log", "-1", "--format=%B", tipSHA)
	wantTrailers := []string{
		"Enju-Merge: auto",
		"Enju-Triggered-By: task-b",
	}
	for _, want := range wantTrailers {
		if !strings.Contains(body, want) {
			t.Errorf("merge message missing trailer %q\nfull message:\n%s", want, body)
		}
	}
	// Both files reachable from the merge tip's tree.
	for _, p := range []string{"out/a.md", "out/b.md"} {
		if _, err := gittest.RunOK(t, bare, "cat-file", "-e", tipSHA+":"+p); err != nil {
			t.Errorf("file %q missing from merge tip tree", p)
		}
	}
}
