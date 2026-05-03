package service

import (
	"encoding/json"
	"fmt"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// SkipCascadeResult summarizes PerformSkipCascade for logging
// and response rendering.
type SkipCascadeResult struct {
	WinningOption string
	// Skipped is the list of full task ids that transitioned
	// to SKIPPED as a result of this vote's resolution.
	Skipped []string
}

// PerformSkipCascade applies Phase E.2 gate routing after a vote
// task has been accepted. The rule:
//
//	winning_set = winning_activates ∪ descendants(winning_activates)
//	losing_set = ⋃ losing_activates ∪ descendants(losing_activates)
//	skip_set  = losing_set − winning_set
//
// Tasks in skip_set transition to SKIPPED. Tasks in winning_set
// stay alive. Tasks in neither set are unrelated to any branch
// and are untouched. Merge points reachable from both sides stay
// alive because the winning path still reaches them.
//
// Called after the DB state flip to ACCEPTED has landed. Returns
// nil with no error when the vote has no activates (pure-decision
// vote) — callers can use the decision as data but no routing
// happens.
func (c *Coordinator) PerformSkipCascade(task *store.TaskRecord, winningOptionID string) (*SkipCascadeResult, error) {
	var declared []struct {
		ID    string  `json:"id"`
		Label   string  `json:"label,omitempty"`
		Activates []string `json:"activates,omitempty"`
	}
	if task.VoteOptions == "" {
		return nil, fmt.Errorf("task %q has no vote_options", task.ID)
	}
	if err := json.Unmarshal([]byte(task.VoteOptions), &declared); err != nil {
		return nil, fmt.Errorf("decoding vote_options: %w", err)
	}

	// Short-circuit: pure-decision vote (no activates declared).
	anyActivates := false
	for _, o := range declared {
		if len(o.Activates) > 0 {
			anyActivates = true
			break
		}
	}
	if !anyActivates {
		return &SkipCascadeResult{WinningOption: winningOptionID}, nil
	}

	d, err := c.Cache.GetDAG(task.RunID)
	if err != nil {
		return nil, fmt.Errorf("loading DAG: %w", err)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("loading run: %w", err)
	}
	runPrefix := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq)

	// Iteration qualification: for a vote in iteration K
	// (task.InstanceKey == "K"), each activates short id refers
	// to the same-iteration counterpart. Try iteration-qualified
	// first; fall back to bare. See the doc on the legacy
	// performSkipCascade for the singleton/fanned-vote
	// edge cases this preserves.
	resolveActivatesNode := func(shortID string) string {
		if task.InstanceKey == "" {
			return shortID
		}
		qualified := enjuYaml.MakeFullID(task.InstanceKey, shortID)
		if _, ok := d.GetNode(qualified); ok {
			return qualified
		}
		return shortID
	}

	// Same iteration-scope filter as fail-cascade: cross-
	// iteration merge points are left alone, promote via
	// UpdateReadyTasks once all cohort instances are terminal.
	inScope := func(descShortID string) bool {
		if task.InstanceKey == "" {
			return true
		}
		n, ok := d.GetNode(descShortID)
		if !ok {
			return false
		}
		descKey, _ := n.Data["instance_key"].(string)
		return descKey == task.InstanceKey
	}

	winningSet := make(map[string]bool)
	losingSet := make(map[string]bool)
	for _, o := range declared {
		target := winningSet
		if o.ID != winningOptionID {
			target = losingSet
		}
		for _, shortID := range o.Activates {
			nodeID := resolveActivatesNode(shortID)
			if !inScope(nodeID) {
				continue
			}
			target[runPrefix+nodeID] = true
			for _, desc := range d.Descendants(nodeID) {
				if !inScope(desc) {
					continue
				}
				target[runPrefix+desc] = true
			}
		}
	}

	// skip_set = losing_set − winning_set
	skipIDs := make([]string, 0, len(losingSet))
	for id := range losingSet {
		if winningSet[id] {
			continue
		}
		skipIDs = append(skipIDs, id)
	}
	if len(skipIDs) == 0 {
		return &SkipCascadeResult{WinningOption: winningOptionID}, nil
	}

	var skipMuts []store.Mutation
	for _, id := range skipIDs {
		skipMuts = append(skipMuts, store.SetTaskState{
			TaskID:  id,
			NewState: store.TaskSkipped,
		})
	}
	if _, err := c.Store.ApplyPlan(store.Plan{
		Version:  engine.EngineVersion,
		Mutations: skipMuts,
	}); err != nil {
		return nil, fmt.Errorf("marking tasks skipped: %w", err)
	}

	c.Store.Events().Record(store.Event{
		EventType:  "cascade_fired",
		EventSubtype: "skip",
		TaskID:    task.ID,
		RunID:    task.RunID,
		ProjectID:  run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"winning_option": winningOptionID,
			"skipped_count": len(skipIDs),
		}),
		CreatedAt: time.Now(),
	})

	return &SkipCascadeResult{
		WinningOption: winningOptionID,
		Skipped:    skipIDs,
	}, nil
}
