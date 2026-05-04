package service

import (
	"encoding/json"
	"fmt"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// RemediationSpawnResult carries the spawned remediation task
// when MaybeSpawnRemediation creates one. The caller renders it
// into the submit response so the reviewer can see what got
// queued.
type RemediationSpawnResult struct {
	Task *store.TaskRecord
}

// MaybeSpawnRemediation handles the living-workflow phase 4b
// auto-spawn rule. Returns (result, true) when the target
// declared spawn_remediation for the given decision and a
// remediation task was successfully spawned; (nil, false)
// otherwise so the caller falls through to the default cascade
// behavior (invalidate / fail).
//
// The spawned remediation:
//   - Carries the reviewer's feedback in the prompt via
//   {{review.feedback}} and {{review.decision}} substitution.
//   - depends_on names the original target so any future
//   re-claim chain naturally waits for the remediation.
//   - Trigger = "template_rule" — distinguishes auto-spawned
//   remediations from human/bot-initiated spawns in audit.
//
// Failure modes (rule unset, target missing, malformed template,
// SpawnTask error like cycle-budget) all return (nil, false)
// and log internally — a remediation-spawn failure must not
// stop a review submission from being recorded.
//
// eventKind is "reject" or "request_changes" — selects which
// of the two on_review_* rules applies.
func (c *Coordinator) MaybeSpawnRemediation(reviewTaskID, targetTaskID, eventKind, decision, feedback string, submitterID int64) (*RemediationSpawnResult, bool) {
	target, err := c.Store.GetTask(targetTaskID)
	if err != nil || target == nil {
		return nil, false
	}
	var rule string
	switch eventKind {
	case "reject":
		rule = target.OnReviewReject
	case "request_changes":
		rule = target.OnReviewRequestChanges
	}
	if rule != "spawn_remediation" || target.RemediationTemplate == "" {
		return nil, false
	}

	var tmpl enjuYaml.RemediationTemplate
	if err := json.Unmarshal([]byte(target.RemediationTemplate), &tmpl); err != nil {
		c.Logger.Error("remediation_template malformed", "target", targetTaskID, "error", err)
		return nil, false
	}
	if tmpl.Action == "" {
		c.Logger.Error("remediation_template missing action", "target", targetTaskID)
		return nil, false
	}

	// Substitute reviewer feedback into the prompt at spawn
	// time (not claim time) so the remediation task captures
	// the feedback text immutably even if the review task is
	// later edited or invalidated.
	prompt := tmpl.Prompt
	prompt = strings.ReplaceAll(prompt, "{{review.feedback}}", feedback)
	prompt = strings.ReplaceAll(prompt, "{{review.decision}}", decision)

	remediationDefID := c.nextRemediationDefID(target)

	var assignTo []string
	if len(tmpl.AssignTo) > 0 {
		assignTo = []string(tmpl.AssignTo)
	}

	res, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SpawnTask{Spec: store.SpawnSpec{
				RunID:        target.RunID,
				ParentTaskID: targetTaskID,
				TaskDefID:    remediationDefID,
				Action:       tmpl.Action,
				Prompt:       prompt,
				DependsOn:    []string{targetTaskID},
				AssignTo:     assignTo,
				RequireRole:  tmpl.RequireRole,
				Trigger:      "template_rule",
				SpawnedBy:    submitterID,
			}},
		},
	})
	if err != nil {
		c.Logger.Error("auto-spawn remediation failed", "target", targetTaskID, "error", err)
		return nil, false
	}
	if res.BudgetExhausted {
		c.Logger.Error("auto-spawn remediation refused: cycle budget exhausted",
			"target", targetTaskID, "run", target.RunID)
		return nil, false
	}
	taskID := res.SpawnedTaskID
	updated, _ := c.Store.GetTask(taskID)
	return &RemediationSpawnResult{Task: updated}, true
}

// nextRemediationDefID picks a fresh task_def_id for an auto-
// spawned remediation: <target_def_id>_remediation_<N> where N
// is the next-available index. Counts via a LIKE prefix match
// — bounded query, indexed by run_id.
func (c *Coordinator) nextRemediationDefID(target *store.TaskRecord) string {
	base := target.TaskDefID + "_remediation"
	count, err := c.Store.CountTasksWithDefIDPrefix(target.RunID, base+"_")
	if err != nil {
		return fmt.Sprintf("%s_1", base)
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}
