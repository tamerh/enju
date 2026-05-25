package service

import (
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// declaredArtifactPaths is the publish set laid onto the deliverable
// branch at run completion. It must include only TRACKED *and*
// PUBLISHED artifacts written by this run: a tracked intermediate
// marked publish:false stays on the run branch but is kept off the
// deliverable, and untracked artifacts (no committable bytes) are
// excluded as before.
func TestDeclaredArtifactPaths_FiltersUnpublished(t *testing.T) {
	st, coord := newCVFStore(t)
	now := time.Now()

	res, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateProject{Project: store.ProjectRecord{Name: "p", CreatedAt: now, UpdatedAt: now}},
	}})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID := res.ProjectID

	const branch = "preview"
	res, err = st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		store.CreateRun{Run: store.RunRecord{
			ProjectID: projectID, Name: "r", YAMLData: "name: r", Branch: branch,
			State: store.RunActive, CreatedAt: now, UpdatedAt: now,
		}},
	}})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID := res.RunID
	run, _ := coord.Store.GetRun(runID)

	art := func(path string, tracked, published bool) store.MoveArtifact {
		sha := "deadbeef"
		if !tracked {
			sha = ""
		}
		return store.MoveArtifact{Artifact: store.ArtifactRecord{
			ProjectID: projectID, Branch: branch, Path: path,
			LastRunID: runID, CommitSHA: sha,
			Tracked: tracked, Published: published,
			CreatedAt: now, UpdatedAt: now,
		}}
	}
	if _, err := st.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: []store.Mutation{
		art("hugo/content/app.md", true, true),               // the deliverable
		art("sections/app/1_gene_ids.md", true, false),       // tracked intermediate, NOT published
		art("sections/app/_aggregated.md", true, false),      // tracked intermediate, NOT published
		art("reads/big.bam", false, true),                    // untracked (no bytes to publish)
	}}); err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}

	got := coord.declaredArtifactPaths(run)
	if len(got) != 1 || got[0] != "hugo/content/app.md" {
		t.Errorf("publish set = %v, want only [hugo/content/app.md] (intermediates publish:false, .bam untracked)", got)
	}
}
