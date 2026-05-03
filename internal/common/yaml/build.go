package yaml

// DAG construction. Given a validated Run with any param
// substitutions already applied, build() produces the
// ParsedRun the rest of the engine consumes: a DAG instance,
// the expanded TaskInstance slice per task def, and the
// deferred-task records that dynamic for_each materialization
// will later resolve.
//
// Three paths feed into build():
//
//   - buildRunLevel: every task is replicated per run-level
//     iteration (historical "matrix" pattern).
//   - buildTaskLevel: each task expands over its own for_each;
//     tasks without for_each stay as singletons. The modern
//     shape — supports dynamic for_each via deferred task defs.
//   - Neither is for a run with no for_each at all; the build
//     function routes to the right sub-builder based on which
//     form is present.
//
// collectDeferred walks the deferred task set (identified by
// validate() via the {{task.field}} + {{paramname}} reference
// analysis) and produces DeferredTaskDef records carrying the
// ForEachRefs + transitively-deferred flag materialize.go
// consumes at runtime.
//
// MakeFullID lives here because the DAG's ID-composition rule
// is inseparable from the for_each expansion it drives.

import (
	"fmt"
	"sort"

	"github.com/enju-ai/enju/internal/common/dag"
	"github.com/enju-ai/enju/internal/common/template"
)

// collectDeferred builds the DeferredTaskDefs slice for the
// parsed run. For each task def in the deferred set, records
// whether it has its own dynamic for_each (directly deferred)
// or was pulled in transitively, and extracts the
// upstream-output references from its for_each block.
func collectDeferred(p *Run, deferred map[string]bool) []DeferredTaskDef {
	if len(deferred) == 0 {
		return nil
	}
	// Preserve task def order for reproducibility.
	out := make([]DeferredTaskDef, 0, len(deferred))
	for i := range p.Tasks {
		t := &p.Tasks[i]
		if !deferred[t.ID] {
			continue
		}
		entry := DeferredTaskDef{TaskDefID: t.ID}
		if len(t.ForEach) > 0 && t.ForEach.IsDynamic() {
			entry.ForEachRefs = make(map[string]ForEachRef, len(t.ForEach))
			for name, src := range t.ForEach {
				if src.Ref == "" {
					continue
				}
				taskID, field, ok := parseForEachRef(src.Ref)
				if !ok {
					continue // validation should have caught this
				}
				entry.ForEachRefs[name] = ForEachRef{TaskID: taskID, Field: field}
			}
		} else {
			entry.TransitivelyDeferred = true
		}
		out = append(out, entry)
	}
	return out
}
// build constructs the DAG and expands for_each parameters. There are
// two mutually-exclusive expansion modes, selected purely by where
// for_each is declared:
//
//  1. Run-level (p.ForEach set): every task is expanded N times — one
//     per iteration — and dependencies within an iteration stay scoped
//     to that iteration (per-iteration binding). This is the original
//     matrix-style model, untouched here so existing runs keep working
//     exactly as before.
//
//  2. Task-level (any task.ForEach set): only tasks that declare their
//     own for_each are expanded; others remain singletons. A singleton
//     depending on an expanded task receives all iterations as a
//     fan-in (resolved to an aggregated block at claim time). An
//     expanded task depending on a singleton sees the same singleton
//     across every iteration.
func build(p *Run) (*ParsedRun, error) {
	if hasAnyTaskForEach(p.Tasks) {
		return buildTaskLevel(p)
	}
	return buildRunLevel(p)
}
// hasAnyTaskForEach returns true if any task declares its own for_each.
func hasAnyTaskForEach(tasks []TaskDef) bool {
	for i := range tasks {
		if len(tasks[i].ForEach) > 0 {
			return true
		}
	}
	return false
}
// buildRunLevel is the original expansion: every task gets one instance
// per run-level iteration. Preserved as-is so existing run-level
// for_each users get identical behavior after this change.
func buildRunLevel(p *Run) (*ParsedRun, error) {
	// By the time we reach build(), any {{paramname}} refs have
	// been substituted to literal Values in substituteParamsInPlace.
	// StaticValues drops any still-unresolved refs (which should
	// not exist here — this is a last-line safety net).
	instances := expandForEach(p.ForEach.StaticValues())

	result := &ParsedRun{
		Run:           p,
		DAG:           dag.New(),
		ExpandedTasks: make(map[string][]TaskInstance),
	}

	taskIDs := make(map[string]bool)
	for _, t := range p.Tasks {
		taskIDs[t.ID] = true
	}

	for _, inst := range instances {
		var taskInstances []TaskInstance

		for _, taskDef := range p.Tasks {
			fullID := MakeFullID(inst.key, taskDef.ID)

			resolvedPrompt := template.ResolveParams(taskDef.Prompt, inst.params)
			resolvedUserPrompt := template.ResolveParams(taskDef.UserPrompt, inst.params)

			allDeps := template.MergeDependencies(taskDef.DependsOn, taskDef.Prompt)
			for _, dep := range allDeps {
				if !taskIDs[dep] {
					return nil, fmt.Errorf("task %q references %q which does not exist", taskDef.ID, dep)
				}
			}

			// Dependencies in run-level mode resolve within the current
			// iteration: foundation depends on prior-same-iteration tasks.
			resolvedDeps := make([]string, 0, len(allDeps))
			for _, dep := range allDeps {
				resolvedDeps = append(resolvedDeps, MakeFullID(inst.key, dep))
			}

			ti := TaskInstance{
				TaskDef:     taskDef,
				InstanceKey: inst.key,
				Params:      inst.params,
				FullID:      fullID,
			}
			ti.Prompt = resolvedPrompt
			ti.UserPrompt = resolvedUserPrompt
			ti.DependsOn = resolvedDeps
			// Qualify the review target with this iteration's
			// instance key. The YAML carries a bare short ID
			// ("scan"), but at submit-reject time the
			// cascade needs the full per-iteration ID
			// ("api:scan") to resolve the right row — without
			// this, rejecting `api:gate` tried to fail a
			// nonexistent `scan` task and silently no-op'd,
			// leaving gate-downstream tasks unlocked (the
			// battle-test reproduction). Only overrides on
			// review tasks; other tasks keep the YAML value.
			if taskDef.Reviews != "" {
				ti.Reviews = MakeFullID(inst.key, taskDef.Reviews)
			}
			if ti.Requirements == nil {
				ti.Requirements = p.Requirements
			}
			// Per-instance artifact paths: substitute
			// for_each variables so `writes_artifacts:
			// [summaries/{{stem}}.md]` resolves to
			// `summaries/alpha.md` on the alpha instance
			// etc. The def's backing slice is shared across
			// instances so we allocate fresh via
			// ResolveParamsSlice.
			//
			// Order matters: substitute the explicit list
			// FIRST, then merge with inferred reads from
			// `{{artifact:...}}` prompt refs. The resolvedPrompt
			// is already substituted, so InferArtifactReads
			// returns concrete paths — merging against an
			// UNresolved explicit list would miss the dedupe
			// (e.g. `[state/items/{{item}}.json]` vs inferred
			// `state/items/i01.json` look distinct as strings,
			// then both substitute to the same path and
			// surface as a duplicate in the claim response).
			ti.ReadsArtifacts = template.MergeArtifactReads(
				template.ResolveParamsSlice(taskDef.ReadsArtifacts, inst.params),
				resolvedPrompt)
			ti.WritesArtifacts = ResolveWriteArtifacts(taskDef.WritesArtifacts, inst.params)

			taskInstances = append(taskInstances, ti)

			data := map[string]interface{}{
				"instance_key": inst.key,
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(fullID, taskDef.Action, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", fullID, err)
			}
		}

		for _, ti := range taskInstances {
			for _, parentID := range ti.DependsOn {
				if err := result.DAG.AddEdge(parentID, ti.FullID); err != nil {
					return nil, fmt.Errorf("adding edge %s -> %s: %w", parentID, ti.FullID, err)
				}
			}
		}

		result.ExpandedTasks[inst.key] = taskInstances
	}

	if err := wireArtifactDeps(result); err != nil {
		return nil, err
	}

	if err := result.DAG.Validate(); err != nil {
		return nil, fmt.Errorf("DAG validation: %w", err)
	}

	return result, nil
}
// buildTaskLevel handles the task-level for_each mode. Tasks with
// for_each become N instances sharing an iteration dimension; tasks
// without for_each become singletons. Dependency wiring depends on
// both sides being expanded (per-iteration binding) or either side
// being a singleton (fan-in / fan-out).
//
// Phase J.1 adds support for dynamic for_each: a task whose for_each
// list comes from an upstream task's named output. When the shared
// for_each is dynamic, tasks that declare it produce zero instances
// at parse time — they're "deferred" until the upstream accepts and
// the coordinator materializes instances based on the resolved list.
// Task defs that depend on a deferred task are transitively deferred
// so their depends_on can be wired correctly at materialization time.
func buildTaskLevel(p *Run) (*ParsedRun, error) {
	// Find the shared iteration space (already validated to be
	// uniform across all tasks that declare for_each).
	var shared ForEachMap
	for i := range p.Tasks {
		if len(p.Tasks[i].ForEach) > 0 {
			shared = p.Tasks[i].ForEach
			break
		}
	}

	// Deferred-task detection (Phase J.1).
	//
	// Any task with a dynamic for_each variable is deferred; so is
	// any task that depends (transitively) on a deferred task def.
	// Deferred tasks don't get instances materialized at parse time;
	// the coordinator creates them when the upstream providing the
	// dynamic list accepts (handled in step 4 of Phase J.1).
	deferred := make(map[string]bool)
	sharedIsDynamic := shared != nil && shared.IsDynamic()
	if sharedIsDynamic {
		for i := range p.Tasks {
			if len(p.Tasks[i].ForEach) > 0 {
				deferred[p.Tasks[i].ID] = true
			}
		}
		// Transitively mark any task whose dependencies touch a
		// deferred task def. We walk repeatedly until the set is
		// stable — simple but adequate for a one-run DAG.
		for {
			changed := false
			for i := range p.Tasks {
				t := &p.Tasks[i]
				if deferred[t.ID] {
					continue
				}
				allDeps := template.MergeDependencies(t.DependsOn, t.Prompt)
				for _, dep := range allDeps {
					if deferred[dep] {
						deferred[t.ID] = true
						changed = true
						break
					}
				}
			}
			if !changed {
				break
			}
		}
	}

	var iterations []forEachInstance
	if sharedIsDynamic {
		iterations = nil // deferred tasks produce zero instances
	} else if len(shared) == 0 {
		iterations = expandForEach(nil)
	} else {
		iterations = expandForEach(shared.StaticValues())
	}

	result := &ParsedRun{
		Run:              p,
		DAG:              dag.New(),
		ExpandedTasks:    make(map[string][]TaskInstance),
		DeferredTaskDefs: collectDeferred(p, deferred),
	}

	taskIDs := make(map[string]bool)
	expandedTaskDef := make(map[string]bool) // which task_def_ids are fan-out
	for i := range p.Tasks {
		taskIDs[p.Tasks[i].ID] = true
		if len(p.Tasks[i].ForEach) > 0 {
			expandedTaskDef[p.Tasks[i].ID] = true
		}
	}

	// Step 1 — create every TaskInstance (without dep wiring yet), add
	// DAG nodes, and group by instanceKey so the downstream loop in
	// handleCreateRun sees the same [instanceKey]->[]TaskInstance shape
	// as the run-level path. Singletons live under key "".
	singletons := make([]TaskInstance, 0)
	// expanded[taskDefID][iterationKey] = short fullID
	expanded := make(map[string]map[string]string)

	createInstance := func(taskDef TaskDef, iter forEachInstance) TaskInstance {
		fullID := MakeFullID(iter.key, taskDef.ID)
		resolvedPrompt := template.ResolveParams(taskDef.Prompt, iter.params)
		resolvedUserPrompt := template.ResolveParams(taskDef.UserPrompt, iter.params)
		ti := TaskInstance{
			TaskDef:     taskDef,
			InstanceKey: iter.key,
			Params:      iter.params,
			FullID:      fullID,
		}
		ti.Prompt = resolvedPrompt
		ti.UserPrompt = resolvedUserPrompt
		if ti.Requirements == nil {
			ti.Requirements = p.Requirements
		}
		// See the run-level variant above (step-1 comment on
		// ReadsArtifacts / WritesArtifacts) for the why, and
		// for the substitute-before-merge ordering that
		// avoids duplicate reads in the claim response.
		ti.ReadsArtifacts = template.MergeArtifactReads(
			template.ResolveParamsSlice(taskDef.ReadsArtifacts, iter.params),
			resolvedPrompt)
		ti.WritesArtifacts = ResolveWriteArtifacts(taskDef.WritesArtifacts, iter.params)
		// Same per-iteration qualification as the run-level
		// for_each path — see the comment there for why.
		if taskDef.Reviews != "" {
			ti.Reviews = MakeFullID(iter.key, taskDef.Reviews)
		}
		return ti
	}

	for _, taskDef := range p.Tasks {
		// Deferred task defs produce zero instances at parse
		// time. The coordinator materializes them when the
		// upstream providing their for_each list accepts.
		if deferred[taskDef.ID] {
			continue
		}
		if len(taskDef.ForEach) == 0 {
			// Singleton.
			singleton := forEachInstance{key: "", params: map[string]string{}}
			ti := createInstance(taskDef, singleton)
			singletons = append(singletons, ti)
			data := map[string]interface{}{
				"instance_key": "",
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(ti.FullID, taskDef.Action, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", ti.FullID, err)
			}
			continue
		}
		// Expanded.
		expanded[taskDef.ID] = make(map[string]string, len(iterations))
		for _, iter := range iterations {
			ti := createInstance(taskDef, iter)
			result.ExpandedTasks[iter.key] = append(result.ExpandedTasks[iter.key], ti)
			expanded[taskDef.ID][iter.key] = ti.FullID
			data := map[string]interface{}{
				"instance_key": iter.key,
				"task_def_id":  taskDef.ID,
			}
			if err := result.DAG.AddNode(ti.FullID, taskDef.Action, data); err != nil {
				return nil, fmt.Errorf("adding node %q: %w", ti.FullID, err)
			}
		}
	}
	if len(singletons) > 0 {
		result.ExpandedTasks[""] = singletons
	}

	// Step 2 — resolve dependencies into concrete short IDs and wire
	// DAG edges. For a given TaskInstance (child) and each declared
	// dep task_def_id (parent):
	//
	//   parent expanded, child expanded → per-iteration binding:
	//     parent at child.InstanceKey → child
	//   parent expanded, child singleton → fan-in:
	//     every instance of parent → child
	//   parent singleton, child expanded → fan-out:
	//     parent → every instance of child (handled per child)
	//   parent singleton, child singleton → straight edge
	wireDeps := func(ti *TaskInstance) error {
		allDeps := template.MergeDependencies(ti.TaskDef.DependsOn, ti.TaskDef.Prompt)
		for _, dep := range allDeps {
			if !taskIDs[dep] {
				return fmt.Errorf("task %q references %q which does not exist", ti.TaskDef.ID, dep)
			}
		}

		var resolved []string
		for _, dep := range allDeps {
			parentExpanded := expandedTaskDef[dep]
			childExpanded := ti.InstanceKey != ""

			switch {
			case parentExpanded && childExpanded:
				// Per-iteration binding.
				parentID := expanded[dep][ti.InstanceKey]
				if parentID == "" {
					return fmt.Errorf("task %q iteration %q: missing expected upstream %q", ti.TaskDef.ID, ti.InstanceKey, dep)
				}
				resolved = append(resolved, parentID)
			case parentExpanded && !childExpanded:
				// Fan-in into the singleton. Deterministic iteration
				// order is critical so the aggregated result at claim
				// time is reproducible run-over-run.
				keys := make([]string, 0, len(expanded[dep]))
				for k := range expanded[dep] {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					resolved = append(resolved, expanded[dep][k])
				}
			default:
				// parent is a singleton — one edge regardless of child side.
				resolved = append(resolved, dep)
			}
		}
		ti.DependsOn = resolved
		for _, parentID := range resolved {
			if err := result.DAG.AddEdge(parentID, ti.FullID); err != nil {
				return fmt.Errorf("adding edge %s -> %s: %w", parentID, ti.FullID, err)
			}
		}
		return nil
	}

	// Walk instances in the same order as step 1 so errors mention a
	// predictable "first bad" task if any.
	for key, list := range result.ExpandedTasks {
		for i := range list {
			ti := &list[i]
			if err := wireDeps(ti); err != nil {
				return nil, err
			}
		}
		result.ExpandedTasks[key] = list
	}

	if err := wireArtifactDeps(result); err != nil {
		return nil, err
	}

	if err := result.DAG.Validate(); err != nil {
		return nil, fmt.Errorf("DAG validation: %w", err)
	}

	return result, nil
}
// MakeFullID constructs a full task ID from instance key and task ID.
func MakeFullID(instanceKey, taskID string) string {
	if instanceKey == "" {
		return taskID
	}
	return instanceKey + ":" + taskID
}

// wireArtifactDeps augments depends_on + DAG edges with
// artifact-derived dependencies — the Snakemake-style
// "file dep graph" channel. For every pair of instances
// where task A writes path P and task B reads the same
// path P (after per-instance substitution), add an edge
// A → B so the scheduler keeps them ordered.
//
// Without this pass, a compute script (or any task) that
// declares reads_artifacts on a path another in-run task
// declares writes_artifacts on would be silently scheduled
// in parallel with its producer — the exact pitfall the
// feedback-tester flagged. Prompt refs like
// `{{task.content}}` already wire edges; artifact refs
// should match that parity.
//
// Runs AFTER primary dep wiring so writers identified
// here augment rather than replace the existing
// DependsOn. Self-edges (a task that both reads and
// writes the same path — append-style update within one
// instance) are filtered. Cross-run artifact deps are
// out of scope here; this is strictly same-run.
func wireArtifactDeps(result *ParsedRun) error {
	// Step 1 — build path → writers map across every
	// materialized instance in the run.
	writersByPath := make(map[string][]string) // path → []fullID
	for _, instances := range result.ExpandedTasks {
		for _, ti := range instances {
			for _, entry := range ti.WritesArtifacts {
				if entry.Path == "" {
					continue
				}
				writersByPath[entry.Path] = append(writersByPath[entry.Path], ti.FullID)
			}
		}
	}
	if len(writersByPath) == 0 {
		return nil
	}

	// Step 2 — for each instance with reads, add every
	// matching writer to depends_on (skipping self-edges
	// and dedup'ing against edges the primary wiring
	// already put there).
	for key, instances := range result.ExpandedTasks {
		for i := range instances {
			ti := &instances[i]
			if len(ti.ReadsArtifacts) == 0 {
				continue
			}
			existing := make(map[string]bool, len(ti.DependsOn))
			for _, d := range ti.DependsOn {
				existing[d] = true
			}
			for _, path := range ti.ReadsArtifacts {
				for _, writerID := range writersByPath[path] {
					if writerID == ti.FullID {
						continue // same-instance self-edge
					}
					if existing[writerID] {
						continue
					}
					existing[writerID] = true
					ti.DependsOn = append(ti.DependsOn, writerID)
					if err := result.DAG.AddEdge(writerID, ti.FullID); err != nil {
						return fmt.Errorf("adding artifact-derived edge %s -> %s: %w", writerID, ti.FullID, err)
					}
				}
			}
		}
		result.ExpandedTasks[key] = instances
	}
	return nil
}
