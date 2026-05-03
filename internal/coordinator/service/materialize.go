package service

import (
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/common/dag"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// MaterializeDeferredTasks is the Phase J.1 entry point for
// dynamic for_each fan-out. When a task with list<string>
// outputs accepts and the run has deferred downstream tasks
// whose for_each lists reference those outputs, this creates
// the concrete task rows for every resolved instance.
//
// The algorithm:
//
//  1. Load the ParsedRun (cached from run creation or lazily
//   re-parsed from stored YAML).
//  2. For each DeferredTaskDef whose for_each refs point at
//   this accepting task, resolve the list values from
//   outputLists and run for_each expansion.
//  3. Insert one task row per expanded instance via
//   store.CreateTask, with depends_on computed against the
//   instance key (matching siblings for per-instance chaining).
//  4. For transitively-deferred tasks (singletons that consume
//   the dynamic upstream via fan-in), materialize them with
//   depends_on listing every newly-inserted instance ID.
//  5. Add nodes + edges to the cached DAG so cascade-
//   invalidation walks see the new rows.
//
// Non-atomic relative to the upstream submit: runs after the
// upstream's submit transaction has committed. A failure here
// leaves the upstream accepted and the deferred downstream
// un-materialized — recoverable by invalidate + re-accept.
// Inside this function, the four-bucket apply IS atomic — all
// restore/reopen/delete/create mutations land in one ApplyPlan
// so a mid-flight crash can't leave half-restored rows.
func (c *Coordinator) MaterializeDeferredTasks(task *store.TaskRecord, run *store.RunRecord, outputLists map[string][]string) error {
	parsed, err := c.Cache.GetParsedRun(task.RunID)
	if err != nil {
		return fmt.Errorf("loading parsed run: %w", err)
	}

	outcome, err := engine.New(c.Store, c.Logger).ComputeMaterialization(parsed, task, run, outputLists)
	if err != nil {
		return err
	}
	if outcome == nil {
		return nil
	}

	// Phase 2 reconciliation apply pass. All four buckets ride
	// one ApplyPlan transaction so a mid-flight failure rolls
	// back cleanly.
	//
	// Ordering within the plan:
	//  1. Restore (unpark to stashed state) — safe before
	//   deletes because restored rows have matching keys.
	//  2. Singleton re-opens (state → PENDING, new deps).
	//  3. Delete stale subtrees.
	//  4. Create new-only instances.
	var muts []store.Mutation

	for _, r := range outcome.TasksToRestore {
		muts = append(muts, store.SetTaskState{
			TaskID:  r.TaskID,
			NewState: r.ToState,
		})
	}
	for _, so := range outcome.SingletonReopens {
		// Re-open carries an update to depends_on too. Both
		// ride the same SetTaskState mutation via NewDependsOn
		// so state flip + edge-set rewrite land in one
		// transaction — a mid-crash can't leave the singleton
		// at PENDING with stale parents.
		newDeps := strings.Split(so.NewDependsOn, ",")
		muts = append(muts, store.SetTaskState{
			TaskID:    so.TaskID,
			NewState:   store.TaskPending,
			ClearClaim:  true,
			NewDependsOn: &newDeps,
		})
	}
	for _, delID := range outcome.TasksToDelete {
		muts = append(muts, store.DeleteTask{TaskID: delID})
	}
	for i := range outcome.TasksToCreate {
		muts = append(muts, store.CreateTask{Task: outcome.TasksToCreate[i]})
	}

	if len(muts) > 0 {
		if _, err := c.Store.ApplyPlan(store.Plan{
			Version:  engine.EngineVersion,
			Mutations: muts,
		}); err != nil {
			return fmt.Errorf("applying materialization plan: %w", err)
		}
	}

	// DAG cache management. Any deletion or singleton re-open
	// invalidates edges the cache knows about; easier to wipe
	// and let the next access rebuild than to surgically remove
	// nodes/edges. Matches the PerformInvalidate pattern.
	if len(outcome.TasksToDelete) > 0 || len(outcome.SingletonReopens) > 0 {
		c.Cache.Invalidate(task.RunID)
		return nil
	}

	// No structural churn — safe to incrementally add the new
	// nodes/edges to the cached DAG (fast path for first-time
	// materialization). MutateDAG holds the cache write lock
	// so concurrent reparse can't race.
	_ = c.Cache.MutateDAG(task.RunID, func(d *dag.DAG) {
		for _, node := range outcome.DAGNodes {
			if err := d.AddNode(node.ShortID, node.Action, node.Data); err != nil {
				c.Logger.Warn("DAG AddNode", "id", node.ShortID, "err", err)
			}
		}
		for _, edge := range outcome.DAGEdges {
			if err := d.AddEdge(edge.From, edge.To); err != nil {
				c.Logger.Warn("DAG AddEdge", "from", edge.From, "to", edge.To, "err", err)
			}
		}
	})
	return nil
}
