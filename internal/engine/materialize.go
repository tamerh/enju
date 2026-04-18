package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/store"
	"github.com/enju-ai/enju/internal/template"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// MaterializationOutcome is the pure-computation result of
// resolving a dynamic for_each expansion. The router
// consumes it to insert task rows, add DAG nodes/edges, and
// reconcile against any parked rows from a prior accept.
//
// Post-Phase-2 buckets (partial re-materialization):
//
//   - TasksToCreate: brand-new instances (keys appearing in
//     the output list that weren't already parked).
//   - TasksToRestore: parked rows whose keys still match the
//     new output list. Each carries the state to restore to
//     (stashed in parked_from_state pre-Phase 2).
//   - TasksToDelete: parked rows whose keys fell out of the
//     new list. Deleted outright along with their subtrees.
//   - SingletonReopens: transitively-deferred singletons
//     (e.g. `aggregate`) whose deps set changed across
//     reconciliation — reset to PENDING with new depends_on.
//
// DefIDsToCleanUp is retained as an empty slice so older
// callers during transition don't break; Phase 2 never fills
// it (precise per-task deletion replaces "wipe by def").
type MaterializationOutcome struct {
	DefIDsToCleanUp  []string
	TasksToCreate    []store.TaskRecord
	TasksToRestore   []RestoreOp
	TasksToDelete    []string
	SingletonReopens []SingletonReopen
	DAGNodes         []DAGNode
	DAGEdges         []DAGEdge
}

// RestoreOp unparks a previously-parked row by reverting its
// state to the value stashed in parked_from_state. The engine
// does the read-and-capture up front so the apply layer only
// needs a single SetTaskState mutation (NewState = ToState),
// keeping the transaction small.
type RestoreOp struct {
	TaskID  string
	ToState store.TaskState
}

// SingletonReopen records a transitively-deferred singleton
// whose deps set changed across a reconciliation — i.e. the
// reconciled instance set differs from what the singleton's
// depends_on currently references. The row must reset to
// PENDING (clearing claim/result state) and adopt the new
// depends_on string so UpdateReadyTasks picks it up correctly
// once the new instance set accepts.
type SingletonReopen struct {
	TaskID       string
	NewDependsOn string
}

// DAGNode describes a node to add to the in-memory DAG.
type DAGNode struct {
	ShortID string
	Action  string
	Data    map[string]interface{}
}

// DAGEdge describes an edge to add to the in-memory DAG.
type DAGEdge struct {
	From string
	To   string
}

// ComputeMaterialization resolves dynamic for_each
// expansion for deferred downstream tasks after an upstream
// task accepts with list-valued outputs. Pure computation —
// reads state, returns the outcome. Never writes.
//
// The algorithm:
//
//  1. Filter DeferredTaskDefs to those whose ForEachRefs
//     point at this accepting task AND have values in
//     outputLists.
//  2. Clean up stale rows from a previous accept (re-run
//     after invalidation) — returns DefIDsToCleanUp.
//  3. Two-pass allocation:
//     - Pass 1: pre-compute full ID for every instance
//       (so per-instance dep wiring can cross-reference
//       siblings like check:BRCA1 → analyze:BRCA1).
//     - Pass 2: build task records with resolved deps,
//       rewritten reviews_target, READY/PENDING state.
//  4. Transitively-deferred singletons (e.g. synthesize)
//     get materialized with depends_on listing every
//     newly-created instance (fan-in).
//  5. DAGNodes + DAGEdges carry the in-memory DAG updates
//     for cascade walks. Includes implicit edges from the
//     for_each source task so Descendants(discover) finds
//     materialized instances.
//
// The caller passes in the ParsedRun (for DeferredTaskDefs
// + task def shapes), the accepting task, and the output
// lists from the submit request.
func (e *Engine) ComputeMaterialization(
	parsed *enjuYaml.ParsedRun,
	task *store.TaskRecord,
	run *store.RunRecord,
	outputLists map[string][]string,
) (*MaterializationOutcome, error) {
	if len(parsed.DeferredTaskDefs) == 0 {
		return nil, nil
	}

	runPrefix := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq)

	// Build task def lookup.
	taskDefByID := make(map[string]*enjuYaml.TaskDef, len(parsed.Run.Tasks))
	for i := range parsed.Run.Tasks {
		taskDefByID[parsed.Run.Tasks[i].ID] = &parsed.Run.Tasks[i]
	}

	// Find directly-ready deferred task defs — those whose
	// for_each refs ALL point at the accepting task AND have
	// values in outputLists. `ref.TaskID` is matched against
	// the short def id (both sides are written in YAML form),
	// not the full task id.
	type resolvedRef struct {
		def     *enjuYaml.DeferredTaskDef
		forEach map[string][]string
	}
	var directlyReady []resolvedRef
	for i := range parsed.DeferredTaskDefs {
		d := &parsed.DeferredTaskDefs[i]
		if d.TransitivelyDeferred {
			continue
		}
		allReady := true
		resolved := make(map[string][]string, len(d.ForEachRefs))
		for varName, ref := range d.ForEachRefs {
			if ref.TaskID != task.TaskDefID {
				allReady = false
				break
			}
			list, ok := outputLists[ref.Field]
			if !ok {
				allReady = false
				break
			}
			resolved[varName] = list
		}
		if !allReady {
			continue
		}
		directlyReady = append(directlyReady, resolvedRef{def: d, forEach: resolved})
	}
	if len(directlyReady) == 0 {
		return nil, nil
	}

	outcome := &MaterializationOutcome{}

	// Phase 2: reconciliation against parked rows from a prior
	// accept. Before planning what to create, inventory
	// existing rows in this run that belong to our deferred
	// defs — these are either parked (from a post-J.2
	// invalidation) or active (first-time accept). We use the
	// DeferredTaskDef set to scope the inventory so unrelated
	// tasks stay out of the reconciliation.
	deferredDefIDs := make(map[string]bool)
	for _, rr := range directlyReady {
		deferredDefIDs[rr.def.TaskDefID] = true
	}
	for i := range parsed.DeferredTaskDefs {
		d := &parsed.DeferredTaskDefs[i]
		if d.TransitivelyDeferred {
			deferredDefIDs[d.TaskDefID] = true
		}
	}

	// existingByDef collects current rows per deferred def.
	// existingByInstanceKey groups rows by instance_key for
	// subtree reconciliation — a single parent key's subtree
	// spans multiple defs but shares the instance_key.
	existingByDef := make(map[string][]store.TaskRecord)
	existingByInstanceKey := make(map[string][]store.TaskRecord)
	allExisting, _ := e.store.ListTasksByRun(task.RunID)
	for _, t := range allExisting {
		if !deferredDefIDs[t.TaskDefID] {
			continue
		}
		existingByDef[t.TaskDefID] = append(existingByDef[t.TaskDefID], t)
		if t.InstanceKey != "" {
			existingByInstanceKey[t.InstanceKey] = append(existingByInstanceKey[t.InstanceKey], t)
		}
	}

	// Two-pass allocation. Pass 1: pre-compute every instance's
	// IDs up front so pass 2 can cross-reference siblings (e.g.
	// check:BRCA1's depends_on picks up analyze:BRCA1 before
	// analyze:BRCA1's row has been built).
	type plannedInstance struct {
		def     *enjuYaml.TaskDef
		inst    forEachInst
		shortID string // "alpha:expand" — instance key + def id
		fullID  string // "1:1:alpha:expand" — runPrefix + shortID
	}
	// instanceIDs holds both forms of a materialized instance so
	// each call site can pick the form it actually needs:
	//   - ShortID ("alpha:expand") goes into ReviewsTarget so
	//     consumers can uniformly prepend runPrefix themselves.
	//   - FullID ("1:1:alpha:expand") goes into DependsOn, since
	//     task records always store deps as fully-qualified IDs.
	// Keep the names self-describing — bugs 3/4 came from a
	// caller grabbing the wrong form from an anonymous struct.
	type instanceIDs struct {
		ShortID string
		FullID  string
	}
	var plans []plannedInstance
	instanceIndex := make(map[string]map[string]instanceIDs)
	// matchedKeysByDef tracks which (def, key) pairs already
	// have an existing row we're keeping (restore path) so the
	// transitively-deferred fan-in pass can route deps to the
	// preserved rows, not try to pair with a new-create entry
	// that doesn't exist.
	matchedKeysByDef := make(map[string]map[string]bool)
	// survivingKeys is the union of keys we'll end up with
	// after reconciliation (matched + new-created). Used by
	// the singleton-deps comparator to detect "deps set
	// changed" — a singleton's new depends_on is rebuilt from
	// this set.
	survivingKeysByDef := make(map[string]map[string]bool)

	for _, rr := range directlyReady {
		def, ok := taskDefByID[rr.def.TaskDefID]
		if !ok {
			return nil, fmt.Errorf("deferred task %q not found in parsed run", rr.def.TaskDefID)
		}
		instances := ExpandForEach(rr.forEach)
		if instanceIndex[def.ID] == nil {
			instanceIndex[def.ID] = make(map[string]instanceIDs, len(instances))
		}
		if matchedKeysByDef[def.ID] == nil {
			matchedKeysByDef[def.ID] = make(map[string]bool)
		}
		if survivingKeysByDef[def.ID] == nil {
			survivingKeysByDef[def.ID] = make(map[string]bool)
		}

		// Build a quick lookup of this def's existing rows by
		// instance key so the per-instance reconciliation
		// decision is O(1).
		existingByKey := make(map[string]store.TaskRecord, len(existingByDef[def.ID]))
		for _, e := range existingByDef[def.ID] {
			existingByKey[e.InstanceKey] = e
		}
		// Keys from the new output list → the "target" set.
		newKeySet := make(map[string]bool, len(instances))
		for _, inst := range instances {
			newKeySet[inst.Key] = true
		}

		// 1. Stale — keys that used to exist but aren't in
		//    the new list. Delete their full subtrees
		//    (instance row + every same-instance-key
		//    descendant).
		for _, existingRow := range existingByDef[def.ID] {
			if newKeySet[existingRow.InstanceKey] {
				continue
			}
			// The whole subtree is already captured by the
			// instance-key grouping — fan-in descendants like
			// alpha:tag, alpha:review share
			// instance_key="alpha" with alpha:expand. Dedup
			// via a map when assembling the delete list.
		}

		// 2. Plan the instances that will exist post-reconcile.
		for _, inst := range instances {
			shortID := enjuYaml.MakeFullID(inst.Key, def.ID)
			fullID := runPrefix + shortID
			instanceIndex[def.ID][inst.Key] = instanceIDs{ShortID: shortID, FullID: fullID}
			survivingKeysByDef[def.ID][inst.Key] = true

			if _, hasExisting := existingByKey[inst.Key]; hasExisting {
				// Matched — this instance is being restored,
				// not created. Skip plan (no TasksToCreate
				// row) and skip DAG node (already there).
				matchedKeysByDef[def.ID][inst.Key] = true
				continue
			}
			// New key — falls through to pass 2's build.
			plans = append(plans, plannedInstance{def: def, inst: inst, shortID: shortID, fullID: fullID})
		}
	}

	// Emit restore ops for every row that belongs to a matched
	// instance key. Scope: a row with instance_key="alpha"
	// whose def is in the deferred set restores alongside
	// alpha:expand. Singletons (instance_key="") are handled
	// in the transitively-deferred pass below, not here.
	matchedInstanceKeys := make(map[string]bool)
	for _, keys := range matchedKeysByDef {
		for k := range keys {
			matchedInstanceKeys[k] = true
		}
	}
	for _, t := range allExisting {
		if t.InstanceKey == "" || !matchedInstanceKeys[t.InstanceKey] {
			continue
		}
		if !deferredDefIDs[t.TaskDefID] {
			continue
		}
		if store.TaskState(t.State) != store.TaskParked {
			// Not parked (fresh first-accept, or somehow
			// already restored). Nothing to do.
			continue
		}
		outcome.TasksToRestore = append(outcome.TasksToRestore, RestoreOp{
			TaskID:  t.ID,
			ToState: store.TaskState(t.ParkedFromState),
		})
	}

	// Emit delete ops for every row whose instance key is
	// stale (exists in some def's parked rows but not in the
	// new list). A key is stale if it appears in some
	// existingByDef entry for a def we're directly
	// materializing now, AND isn't in the new key set for any
	// directly-ready def that produces it.
	staleInstanceKeys := make(map[string]bool)
	for _, rr := range directlyReady {
		def := rr.def
		existingForDef := existingByDef[def.TaskDefID]
		surviving := survivingKeysByDef[def.TaskDefID]
		for _, existingRow := range existingForDef {
			if existingRow.InstanceKey == "" {
				continue
			}
			if surviving[existingRow.InstanceKey] {
				continue
			}
			staleInstanceKeys[existingRow.InstanceKey] = true
		}
	}
	for _, t := range allExisting {
		if t.InstanceKey == "" || !staleInstanceKeys[t.InstanceKey] {
			continue
		}
		if !deferredDefIDs[t.TaskDefID] {
			continue
		}
		outcome.TasksToDelete = append(outcome.TasksToDelete, t.ID)
	}
	sort.Strings(outcome.TasksToDelete)
	// inBatch lets pass 2 detect "this dep is another instance
	// being materialized right now" so the new task starts
	// PENDING instead of READY — its dep row doesn't exist yet,
	// and UpdateReadyTasks promotes it once all in-batch deps
	// are created.
	inBatch := make(map[string]bool)
	for _, byKey := range instanceIndex {
		for _, ids := range byKey {
			inBatch[ids.FullID] = true
		}
	}

	// Find next seq from existing tasks.
	existingTasks, _ := e.store.ListTasksByRun(task.RunID)
	nextSeq := 0
	for _, t := range existingTasks {
		if t.Seq > nextSeq {
			nextSeq = t.Seq
		}
	}

	now := time.Now()
	// Track new instances by def ID for the transitively-
	// deferred pass.
	newInstances := make(map[string][]struct {
		FullID      string
		InstanceKey string
	})

	// Pass 2: build task records + DAG ops.
	for _, plan := range plans {
		def := plan.def
		inst := plan.inst
		shortID := plan.shortID
		fullID := plan.fullID
		nextSeq++

		ti := BuildDeferredInstance(def, inst, parsed.Run)

		// Resolve per-instance deps.
		allDeps := template.MergeDependencies(def.DependsOn, def.Prompt)
		var resolvedDeps []string
		seenResolvedDep := make(map[string]bool)
		addResolvedDep := func(full string) {
			if full == "" || seenResolvedDep[full] {
				return
			}
			seenResolvedDep[full] = true
			resolvedDeps = append(resolvedDeps, full)
		}
		seenDAGParent := make(map[string]bool)
		var dagParents []string
		addDAGParent := func(short string) {
			if seenDAGParent[short] {
				return
			}
			seenDAGParent[short] = true
			dagParents = append(dagParents, short)
		}
		// Seed the for_each source as a DAG parent. This edge is
		// NOT returned by MergeDependencies (which only inspects
		// `depends_on:` and prompt `{{task.field}}` refs — it
		// doesn't know about `for_each: {x: "{{discover.items}}"}`
		// refs). Without this seeding, the DAG's cascade walker
		// can't find our materialized instances as descendants of
		// the source task, and invalidating the source fails to
		// dematerialize downstream — the bug that motivated J.1's
		// eager dematerialization pass.
		//
		// Also add the source to resolvedDeps (the persisted
		// depends_on string). Without it, any downstream consumer
		// of the task list — Mermaid export, DAG visualizers,
		// `enju_run_status`'s per-instance tree — sees the
		// materialized instances as dangling with no edge back to
		// the task whose output list produced them. This was the
		// "discover floats as a disconnected node" bug. Safe to
		// add: the source is ACCEPTED at materialization time
		// (that's what triggered this call), so scheduling doesn't
		// regress — UpdateReadyTasks still finds the new row
		// READY on the same tick.
		for _, dd := range parsed.DeferredTaskDefs {
			if dd.TaskDefID != def.ID {
				continue
			}
			for _, ref := range dd.ForEachRefs {
				addDAGParent(ref.TaskID)
				// ref.TaskID is the def id; the source's full
				// task id is `task.ID` when this batch was
				// triggered by that task's own accept. For
				// transitively-deferred defs (beta deferred on
				// alpha, materialized when alpha accepts) the
				// same relation holds — `task` is always the
				// accepting task and is the direct source.
				if ref.TaskID == task.TaskDefID {
					addResolvedDep(task.ID)
				}
			}
			break
		}
		// Per-instance dep pairing: if the dep's def id appears
		// in instanceIndex (another task materialized in this
		// batch with matching keys), wire to the same-instance
		// sibling. Otherwise fall through to the singleton path.
		for _, dep := range allDeps {
			// The accepting task IS the for_each source. Its
			// task record already has an instance key (possibly
			// empty for non-for_each sources), so we can't look
			// it up in instanceIndex — we wire the edge directly.
			// Explicit depends_on to the source goes into
			// resolvedDeps too; the for_each-seeding block above
			// already did it, but that's dedup-safe via addResolvedDep.
			if dep == task.TaskDefID {
				addDAGParent(enjuYaml.MakeFullID(task.InstanceKey, task.TaskDefID))
				addResolvedDep(task.ID)
				continue
			}
			// Per-instance pair: check:alpha → analyze:alpha.
			// Requires the two defs share a for_each variable, so
			// ExpandForEach produced the same instance keys on
			// both sides.
			if ids, ok := instanceIndex[dep]; ok {
				if matched, ok := ids[inst.Key]; ok {
					addResolvedDep(matched.FullID)
					addDAGParent(matched.ShortID)
					continue
				}
			}
			// Singleton upstream (not materialized in this batch):
			// the DB row already exists with the plain def id.
			addResolvedDep(runPrefix + dep)
			addDAGParent(dep)
		}

		// Per-instance review targets are stored in SHORT form
		// ("alpha:expand"), NOT the full ID ("1:1:alpha:expand").
		// Both submit_orchestrate.go (review cascade) and
		// server.go fetchAndResolveLocally (inline review block)
		// prepend runPrefix themselves; storing the full ID here
		// produces the double-prefix bugs 3/4 were pinned on.
		// server.go parseReviewsTarget splits this back into
		// (defID, instanceKey) at the consumer side.
		reviewsTarget := ti.Reviews
		if reviewsTarget != "" {
			if ids, ok := instanceIndex[reviewsTarget]; ok {
				if matched, ok := ids[inst.Key]; ok {
					reviewsTarget = matched.ShortID
				}
			}
		}

		// If any dep is another instance being created in this
		// same batch, start PENDING — the dep's row won't exist
		// when this INSERT runs, and UpdateReadyTasks will
		// promote us once it lands. A task whose every dep is
		// pre-existing can start READY immediately.
		state := store.TaskReady
		for _, d := range resolvedDeps {
			if inBatch[d] {
				state = store.TaskPending
				break
			}
		}

		paramsJSON := ""
		if len(inst.Params) > 0 {
			if b, err := json.Marshal(inst.Params); err == nil {
				paramsJSON = string(b)
			}
		}

		resultType := ti.ResultType
		if resultType == "" {
			resultType = "text"
		}

		rec := store.TaskRecord{
			ID:              fullID,
			RunID:           task.RunID,
			Seq:             nextSeq,
			TaskDefID:       def.ID,
			InstanceKey:     inst.Key,
			InstanceParams:  paramsJSON,
			Action:          ti.Action,
			Prompt:          ti.Prompt,
			UserPrompt:      ti.UserPrompt,
			Script:          ti.Script,
			Outputs:         marshalOutputs(ti.Outputs),
			Requirements:    marshalRequirements(ti.Requirements),
			ResultType:      resultType,
			Timeout:         ti.Timeout,
			State:           state,
			DependsOn:       strings.Join(resolvedDeps, ","),
			ReadsArtifacts:  marshalStringSlice(ti.ReadsArtifacts),
			WritesArtifacts: marshalStringSlice(ti.WritesArtifacts),
			AssignTo:        marshalStringSlice([]string(ti.AssignTo)),
			RequireRole:     ti.RequireRole,
			ReviewsTarget:   reviewsTarget,
			VoteOptions:     marshalVoteOptions(ti.Options),
			Citizens:        ti.Citizens,
			MinQuorum:       ti.MinQuorum,
			VoteThreshold:   ti.Threshold,
			VoteDeadline:    ti.Deadline,
			Anonymize:       ti.Anonymize,
			Visibility:      ti.Visibility,
			CreatedAt:       now,
		}
		outcome.TasksToCreate = append(outcome.TasksToCreate, rec)

		newInstances[def.ID] = append(newInstances[def.ID], struct {
			FullID      string
			InstanceKey string
		}{FullID: fullID, InstanceKey: inst.Key})

		outcome.DAGNodes = append(outcome.DAGNodes, DAGNode{
			ShortID: shortID,
			Action:  def.Action,
			Data: map[string]interface{}{
				"instance_key": inst.Key,
				"task_def_id":  def.ID,
			},
		})
		for _, parentShort := range dagParents {
			outcome.DAGEdges = append(outcome.DAGEdges, DAGEdge{
				From: parentShort,
				To:   shortID,
			})
		}
	}

	// Phase 2 bridge: newInstances currently only contains
	// the newly-created rows (pass 2 filters matched keys out
	// of `plans`). The transitively-deferred singleton fan-in
	// needs the full post-reconcile set — matched *and* new —
	// otherwise an aggregate's depends_on would list only new
	// keys and miss restored siblings. Fold matched rows in
	// here so fan-in sees a complete picture.
	for _, t := range allExisting {
		if t.InstanceKey == "" {
			continue
		}
		ids, hasDef := instanceIndex[t.TaskDefID]
		if !hasDef {
			continue
		}
		got, hasKey := ids[t.InstanceKey]
		if !hasKey {
			continue
		}
		// Skip entries we're about to insert via TasksToCreate
		// (pass 2 already appended those). We just want to
		// surface matched-existing rows.
		alreadyAdded := false
		for _, entry := range newInstances[t.TaskDefID] {
			if entry.FullID == got.FullID {
				alreadyAdded = true
				break
			}
		}
		if alreadyAdded {
			continue
		}
		newInstances[t.TaskDefID] = append(newInstances[t.TaskDefID], struct {
			FullID      string
			InstanceKey string
		}{FullID: got.FullID, InstanceKey: t.InstanceKey})
	}

	// Transitively-deferred pass. Singletons like `aggregate`
	// that depend on a dynamic-for_each instance can't compute
	// their depends_on until pass 2 has populated newInstances.
	// This pass fans each dep in to EVERY matching instance —
	// the singleton consumer ends up with N upstream edges and
	// its claim-time resolver picks up the Option 4 fan-in
	// block for {{expand.content}} aggregation.
	//
	// `len(def.ForEach) > 0` skips transitively-deferred tasks
	// that have their OWN for_each — those are the J.2 "nested
	// dynamic for_each" case and aren't handled by this pass.
	// The parser's fixed-point walk that sets
	// TransitivelyDeferred doesn't distinguish "deferred via an
	// upstream dynamic for_each" from "deferred via its own
	// dynamic for_each ref", so we filter out the latter here.
	for i := range parsed.DeferredTaskDefs {
		d := &parsed.DeferredTaskDefs[i]
		if !d.TransitivelyDeferred {
			continue
		}
		def, ok := taskDefByID[d.TaskDefID]
		if !ok || len(def.ForEach) > 0 {
			continue
		}
		allDeps := template.MergeDependencies(def.DependsOn, def.Prompt)
		var resolved []string
		var dagParents []string
		for _, dep := range allDeps {
			// Fan in: every materialized instance of this def
			// becomes one dep edge. Gives the singleton's
			// resolver N DependencyRef entries with the same
			// TaskDefID → triggers the Option 4 fan-in block
			// assembly in mcpgit.Project.Resolve.
			if instances, ok := newInstances[dep]; ok {
				for _, m := range instances {
					resolved = append(resolved, m.FullID)
					dagParents = append(dagParents, strings.TrimPrefix(m.FullID, runPrefix))
				}
				continue
			}
			resolved = append(resolved, runPrefix+dep)
			dagParents = append(dagParents, dep)
		}
		// Keep the deps list stable across runs so the
		// "same-set" comparator below isn't sensitive to
		// iteration order.
		sort.Strings(resolved)

		shortID := def.ID
		fullID := runPrefix + shortID

		// Phase 2 reconciliation for singletons: if the row
		// already exists (either parked by Phase 1 or
		// accepted/pending from a prior round), we don't
		// create a new one. Instead:
		//   - deps set unchanged → restore if parked,
		//     otherwise leave alone.
		//   - deps set changed → re-open: state → PENDING,
		//     clear claim, update depends_on. The row keeps
		//     its seq/id/action/prompt so consumers that
		//     cached the full id still find it.
		if existingRow, err := e.store.GetTask(fullID); err == nil && existingRow != nil {
			existingDeps := existingRow.DependsOn
			// Normalize existing deps for comparison — the
			// stored string is comma-joined; split + sort
			// so "a,b" and "b,a" compare equal.
			existingSorted := strings.Split(existingDeps, ",")
			sort.Strings(existingSorted)
			existingNorm := strings.Join(existingSorted, ",")
			newNorm := strings.Join(resolved, ",")
			if existingNorm == newNorm {
				if store.TaskState(existingRow.State) == store.TaskParked {
					outcome.TasksToRestore = append(outcome.TasksToRestore, RestoreOp{
						TaskID:  fullID,
						ToState: store.TaskState(existingRow.ParkedFromState),
					})
				}
				// Not parked: leave alone. The aggregate
				// is either still pending or already
				// accepted with a result that's still
				// valid under the preserved deps set.
			} else {
				outcome.SingletonReopens = append(outcome.SingletonReopens, SingletonReopen{
					TaskID:       fullID,
					NewDependsOn: strings.Join(resolved, ","),
				})
			}
			// Skip the create path below — the row exists.
			continue
		}

		// First-time materialization: proceed with the
		// existing create flow.
		nextSeq++
		ti := BuildDeferredInstance(def, forEachInst{Key: "", Params: map[string]string{}}, parsed.Run)

		resultType := ti.ResultType
		if resultType == "" {
			resultType = "text"
		}

		rec := store.TaskRecord{
			ID:              fullID,
			RunID:           task.RunID,
			Seq:             nextSeq,
			TaskDefID:       def.ID,
			InstanceKey:     "",
			InstanceParams:  "",
			Action:          ti.Action,
			Prompt:          ti.Prompt,
			UserPrompt:      ti.UserPrompt,
			Script:          ti.Script,
			Outputs:         marshalOutputs(ti.Outputs),
			Requirements:    marshalRequirements(ti.Requirements),
			ResultType:      resultType,
			Timeout:         ti.Timeout,
			State:           store.TaskPending,
			DependsOn:       strings.Join(resolved, ","),
			ReadsArtifacts:  marshalStringSlice(ti.ReadsArtifacts),
			WritesArtifacts: marshalStringSlice(ti.WritesArtifacts),
			AssignTo:        marshalStringSlice([]string(ti.AssignTo)),
			RequireRole:     ti.RequireRole,
			ReviewsTarget:   ti.Reviews,
			VoteOptions:     marshalVoteOptions(ti.Options),
			Citizens:        ti.Citizens,
			MinQuorum:       ti.MinQuorum,
			VoteThreshold:   ti.Threshold,
			VoteDeadline:    ti.Deadline,
			Anonymize:       ti.Anonymize,
			Visibility:      ti.Visibility,
			CreatedAt:       now,
		}
		outcome.TasksToCreate = append(outcome.TasksToCreate, rec)
		outcome.DAGNodes = append(outcome.DAGNodes, DAGNode{
			ShortID: shortID,
			Action:  def.Action,
			Data: map[string]interface{}{
				"instance_key": "",
				"task_def_id":  def.ID,
			},
		})
		for _, parentShort := range dagParents {
			outcome.DAGEdges = append(outcome.DAGEdges, DAGEdge{
				From: parentShort,
				To:   shortID,
			})
		}
	}

	return outcome, nil
}

// --- Helpers ---

// forEachInst is a resolved for_each iteration instance.
type forEachInst struct {
	Key    string
	Params map[string]string
}

// ExpandForEach generates instances from resolved
// for_each lists. Single variable → one per value;
// multiple → cartesian product.
func ExpandForEach(forEach map[string][]string) []forEachInst {
	if len(forEach) == 0 {
		return []forEachInst{{Key: "", Params: map[string]string{}}}
	}
	if len(forEach) == 1 {
		for name, values := range forEach {
			out := make([]forEachInst, 0, len(values))
			for _, v := range values {
				out = append(out, forEachInst{
					Key:    v,
					Params: map[string]string{name: v},
				})
			}
			return out
		}
	}
	keys := make([]string, 0, len(forEach))
	for k := range forEach {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([][]string, len(keys))
	for i, k := range keys {
		vals[i] = forEach[k]
	}
	var result []forEachInst
	var walk func(depth int, acc map[string]string)
	walk = func(depth int, acc map[string]string) {
		if depth == len(keys) {
			copyAcc := make(map[string]string, len(acc))
			var parts []string
			for _, k := range keys {
				copyAcc[k] = acc[k]
				parts = append(parts, acc[k])
			}
			result = append(result, forEachInst{
				Key:    strings.Join(parts, "_"),
				Params: copyAcc,
			})
			return
		}
		for _, v := range vals[depth] {
			acc[keys[depth]] = v
			walk(depth+1, acc)
		}
	}
	walk(0, make(map[string]string, len(keys)))
	return result
}

// BuildDeferredInstance creates a TaskInstance from a
// task def + a resolved for_each iteration.
func BuildDeferredInstance(def *enjuYaml.TaskDef, inst forEachInst, run *enjuYaml.Run) enjuYaml.TaskInstance {
	ti := enjuYaml.TaskInstance{
		TaskDef:     *def,
		InstanceKey: inst.Key,
		Params:      inst.Params,
	}
	ti.Prompt = template.ResolveParams(def.Prompt, inst.Params)
	ti.UserPrompt = template.ResolveParams(def.UserPrompt, inst.Params)
	// Per-instance substitution for declared-artifact slots.
	// Without this, `writes_artifacts: [summaries/{{stem}}.md]`
	// would register literally on every instance row, the
	// artifact-writer match at submit would fail, and the
	// parser's per-instance dep inference (writer→reader via
	// shared artifact path) would never trigger.
	// ResolveParamsSlice allocates fresh slices so sibling
	// instances don't share mutable backing storage with the
	// def or each other.
	ti.ReadsArtifacts = template.ResolveParamsSlice(
		template.MergeArtifactReads(def.ReadsArtifacts, ti.Prompt),
		inst.Params)
	ti.WritesArtifacts = template.ResolveParamsSlice(def.WritesArtifacts, inst.Params)
	if ti.Requirements == nil {
		ti.Requirements = run.Requirements
	}
	if ti.ResultType == "" {
		ti.ResultType = "text"
	}
	if ti.Timeout == "" {
		ti.Timeout = run.Defaults.Timeout
	}
	return ti
}

// --- Marshal helpers (shared with router for now) ---

func marshalOutputs(outputs map[string]enjuYaml.OutputSpec) string {
	if len(outputs) == 0 {
		return ""
	}
	data, err := json.Marshal(outputs)
	if err != nil {
		return ""
	}
	return string(data)
}

func marshalRequirements(req map[string]interface{}) string {
	if len(req) == 0 {
		return ""
	}
	data, err := json.Marshal(req)
	if err != nil {
		return ""
	}
	return string(data)
}

func marshalStringSlice(s []string) string {
	if len(s) == 0 {
		return ""
	}
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(data)
}

func marshalVoteOptions(opts []enjuYaml.VoteOption) string {
	if len(opts) == 0 {
		return ""
	}
	data, err := json.Marshal(opts)
	if err != nil {
		return ""
	}
	return string(data)
}
