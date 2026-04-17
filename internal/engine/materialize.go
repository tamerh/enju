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
// clean up stale instances.
type MaterializationOutcome struct {
	DefIDsToCleanUp []string
	TasksToCreate   []store.TaskRecord
	DAGNodes        []DAGNode
	DAGEdges        []DAGEdge
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
	// for_each refs ALL point at the accepting task and have
	// values in outputLists.
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

	// Collect ALL deferred def IDs for cleanup (stale rows
	// from a previous accept).
	outcome := &MaterializationOutcome{}
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
	for defID := range deferredDefIDs {
		outcome.DefIDsToCleanUp = append(outcome.DefIDsToCleanUp, defID)
	}
	sort.Strings(outcome.DefIDsToCleanUp)

	// Two-pass allocation. Pass 1: pre-compute full IDs.
	type plannedInstance struct {
		def     *enjuYaml.TaskDef
		inst    forEachInst
		shortID string
		fullID  string
	}
	var plans []plannedInstance
	instanceIndex := make(map[string]map[string]struct {
		short string
		full  string
	})
	for _, rr := range directlyReady {
		def, ok := taskDefByID[rr.def.TaskDefID]
		if !ok {
			return nil, fmt.Errorf("deferred task %q not found in parsed run", rr.def.TaskDefID)
		}
		instances := ExpandForEach(rr.forEach)
		if instanceIndex[def.ID] == nil {
			instanceIndex[def.ID] = make(map[string]struct {
				short string
				full  string
			}, len(instances))
		}
		for _, inst := range instances {
			shortID := enjuYaml.MakeFullID(inst.Key, def.ID)
			fullID := runPrefix + shortID
			plans = append(plans, plannedInstance{def: def, inst: inst, shortID: shortID, fullID: fullID})
			instanceIndex[def.ID][inst.Key] = struct {
				short string
				full  string
			}{short: shortID, full: fullID}
		}
	}
	inBatch := make(map[string]bool)
	for _, ids := range instanceIndex {
		for _, ids2 := range ids {
			inBatch[ids2.full] = true
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
		seenDAGParent := make(map[string]bool)
		var dagParents []string
		addDAGParent := func(short string) {
			if seenDAGParent[short] {
				return
			}
			seenDAGParent[short] = true
			dagParents = append(dagParents, short)
		}
		// Seed with for_each sources.
		for _, dd := range parsed.DeferredTaskDefs {
			if dd.TaskDefID != def.ID {
				continue
			}
			for _, ref := range dd.ForEachRefs {
				addDAGParent(ref.TaskID)
			}
			break
		}
		for _, dep := range allDeps {
			if dep == task.TaskDefID {
				addDAGParent(enjuYaml.MakeFullID(task.InstanceKey, task.TaskDefID))
				continue
			}
			if ids, ok := instanceIndex[dep]; ok {
				if matched, ok := ids[inst.Key]; ok {
					resolvedDeps = append(resolvedDeps, matched.full)
					addDAGParent(matched.short)
					continue
				}
			}
			resolvedDeps = append(resolvedDeps, runPrefix+dep)
			addDAGParent(dep)
		}

		// Review-target rewriting. Store the instance-matched
		// SHORT form (e.g. "alpha:expand") so downstream consumers
		// can uniformly prepend the run prefix without double-
		// prefixing. matched.short is already `instanceKey:defID`
		// (from MakeFullID on line 165 above).
		reviewsTarget := ti.Reviews
		if reviewsTarget != "" {
			if ids, ok := instanceIndex[reviewsTarget]; ok {
				if matched, ok := ids[inst.Key]; ok {
					reviewsTarget = matched.short
				}
			}
		}

		// State.
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

	// Transitively-deferred pass (singletons).
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

		nextSeq++
		ti := BuildDeferredInstance(def, forEachInst{Key: "", Params: map[string]string{}}, parsed.Run)
		shortID := def.ID
		fullID := runPrefix + shortID

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
