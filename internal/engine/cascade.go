package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/dag"
	"github.com/enju-ai/enju/internal/store"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// InvalidationOutcome is the pure-computation result of
// walking a task's DAG descendants, finding cross-run
// artifact readers, computing artifact rollbacks, and
// identifying dynamic-for_each descendants to
// dematerialize. The router consumes this to decide what
// store writes to perform.
type InvalidationOutcome struct {
	TargetID           string
	RegularDescendants []string
	CrossRunReaders    []string
	DematerializedIDs  []string
	DematerializedDefs []string
	ArtifactRollbacks  []ArtifactRollback
	AffectedRunIDs     map[int64]bool
}

// ArtifactRollback describes what should happen to one
// artifact path during invalidation. Either Delete (no
// prior writer) or RestoreTo (re-point to a previous
// accepted writer).
type ArtifactRollback struct {
	Path      string
	ProjectID int64
	Delete    bool
	RestoreTo *store.ArtifactRecord // nil when Delete is true
}

// ComputeInvalidation walks the DAG from the target task,
// finds all affected descendants (intra-run + cross-run
// artifact readers), computes artifact pointer rollbacks,
// and identifies dynamic-for_each descendants to
// dematerialize. Pure computation — reads state via the
// ReadStore, never writes.
//
// The algorithm:
//
//  1. Walk DAG descendants of the target (intra-run).
//  2. Collect artifact paths written by target + descendants.
//  3. For each written path whose current artifact-index
//     writer is in the invalidation set: find cross-run
//     readers (tasks in other runs that declared
//     reads_artifacts for that path and are currently
//     ACCEPTED). These cascade too.
//  4. For each written path: find the most recent ACCEPTED
//     writer outside the invalidation set. If found, the
//     artifact pointer rolls back to that writer. If not
//     found, the artifact index row is deleted.
//  5. Identify dynamic-for_each descendants (task_def is in
//     ParsedRun.DeferredTaskDefs). These get DELETED rather
//     than flipped to PENDING — their instance keys match a
//     specific accept's output list and can't survive a
//     re-accept with a different list.
//  6. Split descendants into regular (→ PENDING) vs
//     dematerialized (→ DELETE).
//
// The caller passes in the pre-loaded DAG and ParsedRun
// (from getOrLoadDAG / getOrLoadParsedRun) so the engine
// doesn't need to manage caches.
func (e *Engine) ComputeInvalidation(
	task *store.TaskRecord,
	run *store.RunRecord,
	d *dag.DAG,
	parsed *enjuYaml.ParsedRun,
) (*InvalidationOutcome, error) {
	runPrefix := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq)
	dagNodeID := enjuYaml.MakeFullID(task.InstanceKey, task.TaskDefID)

	// Step 1: walk DAG descendants.
	shortDescendants := d.Descendants(dagNodeID)
	descendants := make([]string, 0, len(shortDescendants))
	for _, short := range shortDescendants {
		descendants = append(descendants, runPrefix+short)
	}

	invalidatedSet := make(map[string]bool, 1+len(descendants))
	invalidatedSet[task.ID] = true
	for _, dd := range descendants {
		invalidatedSet[dd] = true
	}

	// Step 2: collect artifact paths written by the target
	// and its descendants.
	writtenPaths := make([]string, 0)
	seenPath := make(map[string]bool)
	collectWrites := func(t *store.TaskRecord) {
		var paths []string
		if t.WritesArtifacts != "" {
			_ = json.Unmarshal([]byte(t.WritesArtifacts), &paths)
		}
		for _, p := range paths {
			if !seenPath[p] {
				seenPath[p] = true
				writtenPaths = append(writtenPaths, p)
			}
		}
	}
	collectWrites(task)
	for _, descID := range descendants {
		dt, err := e.store.GetTask(descID)
		if err != nil || dt == nil {
			continue
		}
		collectWrites(dt)
	}

	// Step 3: find cross-run artifact readers.
	var crossRunReaders []string
	affectedRunIDs := map[int64]bool{task.RunID: true}
	for _, p := range writtenPaths {
		art, _ := e.store.GetArtifact(run.ProjectID, p)
		if art == nil {
			continue
		}
		if !invalidatedSet[art.LastTaskID] {
			continue
		}
		readers, err := e.store.ListTasksReadingArtifact(run.ProjectID, p, true)
		if err != nil {
			e.logger.Warn("listing artifact readers", "path", p, "error", err)
			continue
		}
		for _, r := range readers {
			if invalidatedSet[r.ID] {
				continue
			}
			invalidatedSet[r.ID] = true
			crossRunReaders = append(crossRunReaders, r.ID)
			affectedRunIDs[r.RunID] = true
		}
	}

	// Step 4: compute artifact rollbacks.
	var rollbacks []ArtifactRollback
	for _, p := range writtenPaths {
		art, _ := e.store.GetArtifact(run.ProjectID, p)
		if art == nil || !invalidatedSet[art.LastTaskID] {
			continue
		}
		priorTasks, err := e.store.ListTasksWritingArtifact(run.ProjectID, p, true)
		if err != nil {
			e.logger.Warn("listing prior writers", "path", p, "error", err)
			continue
		}
		var pick *store.TaskRecord
		for i := range priorTasks {
			t := &priorTasks[i]
			if invalidatedSet[t.ID] {
				continue
			}
			if t.CommitSHA == "" {
				continue
			}
			if pick == nil || (t.SubmittedAt != nil && pick.SubmittedAt != nil && t.SubmittedAt.After(*pick.SubmittedAt)) {
				pick = t
			}
		}
		if pick == nil {
			rollbacks = append(rollbacks, ArtifactRollback{
				Path:      p,
				ProjectID: run.ProjectID,
				Delete:    true,
			})
			continue
		}
		now := time.Now()
		rollbacks = append(rollbacks, ArtifactRollback{
			Path:      p,
			ProjectID: run.ProjectID,
			RestoreTo: &store.ArtifactRecord{
				ProjectID:  run.ProjectID,
				Path:       p,
				LastWriter: pick.ClaimedBy,
				LastTaskID: pick.ID,
				LastRunID:  pick.RunID,
				CommitSHA:  pick.CommitSHA,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		})
	}

	// Step 5: identify dynamic-for_each descendants to
	// dematerialize.
	dematerializeDefs := make(map[string]bool)
	if parsed != nil && len(parsed.DeferredTaskDefs) > 0 {
		isDynamicSource := false
		for _, d := range parsed.DeferredTaskDefs {
			for _, ref := range d.ForEachRefs {
				if ref.TaskID == task.TaskDefID {
					isDynamicSource = true
					break
				}
			}
			if isDynamicSource {
				break
			}
		}
		if isDynamicSource {
			for _, d := range parsed.DeferredTaskDefs {
				dematerializeDefs[d.TaskDefID] = true
			}
		}
	}

	// Step 6: split descendants into regular vs dematerialized.
	var dematerializedIDs []string
	var regularDescendants []string
	for _, descID := range descendants {
		dt, err := e.store.GetTask(descID)
		if err != nil || dt == nil {
			continue
		}
		if dematerializeDefs[dt.TaskDefID] {
			dematerializedIDs = append(dematerializedIDs, descID)
		} else {
			regularDescendants = append(regularDescendants, descID)
		}
	}

	// Collect unique def IDs for deletion.
	dematDefList := make([]string, 0, len(dematerializeDefs))
	for def := range dematerializeDefs {
		dematDefList = append(dematDefList, def)
	}

	return &InvalidationOutcome{
		TargetID:           task.ID,
		RegularDescendants: regularDescendants,
		CrossRunReaders:    crossRunReaders,
		DematerializedIDs:  dematerializedIDs,
		DematerializedDefs: dematDefList,
		ArtifactRollbacks:  rollbacks,
		AffectedRunIDs:     affectedRunIDs,
	}, nil
}
