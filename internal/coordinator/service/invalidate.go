package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// InvalidationResult summarizes what PerformInvalidate actually
// changed on a single invocation. Callers (REST handler, MCP
// handler, internal review-reject path) consume this to render
// responses or log audit lines.
type InvalidationResult struct {
	Task      *store.TaskRecord
	Descendants  []string
	// Dematerialized lists task IDs that were parked rather
	// than flipped to PENDING — populated for invalidations of
	// dynamic-for_each sources whose materialized descendants
	// can't preserve their instance keys across a re-accept.
	Dematerialized []string
	Changed     int
	Rollbacks    []RollbackOutcome
}

// RollbackOutcome describes one artifact's fate during a cascade
// — either deleted outright, or restored to a prior task's
// output via the artifact index.
type RollbackOutcome struct {
	Path        string
	Deleted       bool
	RestoredFromTask  string
	RestoredFromCommit string
}

// PerformInvalidate is the shared cascade-invalidation
// implementation used by handleInvalidateTask (REST), the
// native MCP invalidate handler, and the review-reject path
// inside handleSubmitResultReport. It walks the cached DAG,
// flips state, rolls back artifacts, and records the
// cascade_fired audit event.
//
// triggerSubtype is the cascade flavor recorded on the
// cascade_fired event:
//   "invalidate"      — explicit operator/API invalidate
//   "request_changes" — review request_changes path
//   "review_reject"   — multi-citizen vote/review rejection cascade
//   "downstream"      — propagation from another invalidation
//
// Does not call MarkLatestClaimOutcome on the target — that's
// the caller's policy decision (manual invalidate marks it
// invalidated; request_changes leaves the claim open).
func (c *Coordinator) PerformInvalidate(taskID, triggerSubtype string) (*InvalidationResult, error) {
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	d, err := c.Cache.GetDAG(task.RunID)
	if err != nil {
		return nil, fmt.Errorf("loading DAG for run %d: %w", task.RunID, err)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("run not found for task %q", taskID)
	}
	parsed, _ := c.Cache.GetParsedRun(task.RunID)

	// Engine computes — reads state, never writes.
	outcome, err := engine.New(c.Store, c.Logger).ComputeInvalidation(task, run, d, parsed)
	if err != nil {
		return nil, err
	}

	// Build a Plan from the engine's outcome.
	var mutations []store.Mutation

	// 1. Artifact rollbacks.
	var rollbacks []RollbackOutcome
	for _, rb := range outcome.ArtifactRollbacks {
		if rb.Delete {
			mutations = append(mutations, store.DeleteArtifact{
				ProjectID: rb.ProjectID,
				Branch:  rb.Branch,
				Path:   rb.Path,
			})
			rollbacks = append(rollbacks, RollbackOutcome{Path: rb.Path, Deleted: true})
		} else if rb.RestoreTo != nil {
			mutations = append(mutations, store.MoveArtifact{
				Artifact: *rb.RestoreTo,
			})
			rollbacks = append(rollbacks, RollbackOutcome{
				Path:        rb.Path,
				RestoredFromTask:  rb.RestoreTo.LastTaskID,
				RestoredFromCommit: rb.RestoreTo.CommitSHA,
			})
		}
	}

	// 2. Target: ACCEPTED → READY with claim clear.
	mutations = append(mutations, store.SetTaskState{
		TaskID:   taskID,
		NewState:  store.TaskReady,
		ClearClaim: true,
	})

	// 3. Regular descendants → PENDING with claim clear.
	for _, descID := range outcome.RegularDescendants {
		mutations = append(mutations, store.SetTaskState{
			TaskID:   descID,
			NewState:  store.TaskPending,
			ClearClaim: true,
		})
	}

	// 4. Cross-run reader cascade was removed with the branch-
	// per-run model. Runs on distinct branches are isolated by
	// design — branch + serial-per-branch make this a no-op.

	// 5. Dynamic descendants → PARK (J.2 partial re-mat).
	//  Parking preserves the row (state flips to 'parked',
	//  prior state stashed in parked_from_state) so the Phase
	//  2 reconciliation pass on re-accept can restore matched
	//  keys losslessly. Stale keys are deleted at that point.
	//  Fail-cascade (PerformFailCascade) keeps deleting
	//  outright since terminally failed sources never re-
	//  accept.
	for _, descID := range outcome.DematerializedIDs {
		dt, err := c.Store.GetTask(descID)
		if err != nil || dt == nil {
			// Row vanished between the engine's read and this
			// write — nothing to park. Defensive under the
			// project lock.
			continue
		}
		mutations = append(mutations, store.SetTaskState{
			TaskID:     descID,
			NewState:    store.TaskParked,
			ParkedFromState: store.TaskState(dt.State),
		})
	}

	subtype := triggerSubtype
	if subtype == "" {
		subtype = "invalidate"
	}
	// Fold descendants' open-claim closures into the same plan
	// so the state flips and the claim outcome closures land
	// atomically. Pre-chokepoint these ran as a separate loop
	// after ApplyPlan; that left a window where descendants
	// were PENDING but their claim rows still showed open.
	for _, descID := range outcome.RegularDescendants {
		mutations = append(mutations, store.MarkOpenClaimsInvalidated{TaskID: descID})
	}
	plan := store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	}.AppendCascade(task.RunID)
	plan.Mutations = append(plan.Mutations, store.EmitEvent{Event: store.Event{
		EventType:    "cascade_fired",
		EventSubtype: subtype,
		TaskID:       taskID,
		RunID:        task.RunID,
		ProjectID:    run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"descendants_count": len(outcome.RegularDescendants),
			"parked_count":      len(outcome.DematerializedIDs),
			"rollbacks":         len(rollbacks),
		}),
		CreatedAt: time.Now(),
	}})
	result, err := c.Store.ApplyPlan(plan)
	if err != nil {
		return nil, err
	}

	// Parked rows keep their nodes/edges intact — no DAG
	// cache wipe (reconciliation diffs the live DAG against
	// the incoming output list).

	// Run-state re-evaluation (with auto-triage hook on idle).
	// Readiness cascade fired inside the ApplyPlan transaction
	// above via AppendCascade.
	c.EvaluateRunStateAndMaybeTriage(task.RunID)

	changed := result.Changed + result.TasksDeleted

	return &InvalidationResult{
		Task:      task,
		Descendants:  outcome.RegularDescendants,
		Dematerialized: outcome.DematerializedIDs,
		Changed:    changed,
		Rollbacks:   rollbacks,
	}, nil
}

// EvaluateRunStateAndMaybeTriage applies a CompleteRun mutation
// (which re-evaluates the run state from current task counts)
// then runs the auto-triage hook on idle. Used at every site
// that re-evaluates a run's state after a task transition.
func (c *Coordinator) EvaluateRunStateAndMaybeTriage(runID int64) {
	if _, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CompleteRun{RunID: runID},
		},
	}); err != nil {
		return
	}
	r, err := c.Store.GetRun(runID)
	if err != nil || r == nil {
		return
	}
	if r.State == store.RunWaiting {
		c.maybeAutoTriage(runID)
	}
}

// MaybeAutoTriageIfIdle is the submit-path variant: the state
// was already re-evaluated by CheckAndCompleteRun (which the
// submit Plan tx ran inline). We just read the current state
// and fire the trigger if idle.
func (c *Coordinator) MaybeAutoTriageIfIdle(runID int64) {
	r, err := c.Store.GetRun(runID)
	if err != nil || r == nil {
		return
	}
	if r.State == store.RunWaiting {
		c.maybeAutoTriage(runID)
	}
}

// maybeAutoTriage is the living-workflow phase 4c idle trigger:
// when a run lands on `idle` AND carries an auto_triage_template
// AND the project has at least one open issue, spawn a fix task
// for the oldest open issue. Best-effort — failures are logged
// and don't surface to callers.
//
// Concurrency: serialized per project via projectTriageMutex.
// Without the mutex, two concurrent submits in the same project
// could each pass FindOldestOpenIssue before either marked the
// issue in-progress, both spawn a fix task, and one orphans.
func (c *Coordinator) maybeAutoTriage(runID int64) {
	tmpl, err := c.Store.GetAutoTriageTemplate(runID)
	if err != nil || tmpl == "" {
		return
	}
	run, err := c.Store.GetRun(runID)
	if err != nil || run == nil {
		return
	}
	mu := c.projectTriageMutex(run.ProjectID)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the mutex.
	issue, err := c.Store.FindOldestOpenIssue(run.ProjectID)
	if err != nil || issue == nil {
		return
	}

	var spec enjuYaml.RemediationTemplate
	if err := json.Unmarshal([]byte(tmpl), &spec); err != nil {
		c.Logger.Error("auto_triage_template malformed", "run", runID, "error", err)
		return
	}
	if spec.Action == "" {
		c.Logger.Error("auto_triage_template missing action", "run", runID)
		return
	}

	prompt := spec.Prompt
	prompt = strings.ReplaceAll(prompt, "{{issue.title}}", issue.Title)
	prompt = strings.ReplaceAll(prompt, "{{issue.body}}", issue.Body)
	prompt = strings.ReplaceAll(prompt, "{{issue.severity}}", string(issue.Severity))
	prompt = strings.ReplaceAll(prompt, "{{issue.id}}", fmt.Sprintf("ISSUE-%03d", issue.Seq))

	base := fmt.Sprintf("fix_ISSUE_%03d", issue.Seq)
	count, _ := c.Store.CountTasksWithDefIDPrefix(runID, base+"_")
	defID := fmt.Sprintf("%s_%d", base, count+1)

	var assignTo []string
	if len(spec.AssignTo) > 0 {
		assignTo = []string(spec.AssignTo)
	}

	res, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SpawnTask{Spec: store.SpawnSpec{
				RunID:          runID,
				TaskDefID:      defID,
				Action:         spec.Action,
				Prompt:         prompt,
				AssignTo:       assignTo,
				RequireRole:    spec.RequireRole,
				Trigger:        "auto_triage",
				ClosesIssueSeq: issue.Seq,
			}},
		},
	})
	if err != nil {
		c.Logger.Error("auto-triage spawn failed", "run", runID, "issue", issue.Seq, "error", err)
		return
	}
	if res.BudgetExhausted {
		c.Logger.Warn("auto-triage spawn refused: cycle budget exhausted",
			"run", runID, "issue", issue.Seq)
		return
	}
	taskID := res.SpawnedTaskID

	if _, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.MarkIssueInProgress{IssueID: issue.ID, CitizenID: 0, FixTaskID: taskID},
		},
	}); err != nil {
		c.Logger.Warn("auto-triage in_progress transition failed", "issue", issue.ID, "task", taskID, "error", err)
	}
}

// InvalidateTaskResponse is the wire shape for enju_invalidate_task.
type InvalidateTaskResponse struct {
	Status      store.ClaimOutcome `json:"status"`
	TaskID      string         `json:"task_id"`
	Descendants    []string       `json:"descendants"`
	Changed     int          `json:"changed"`
	Reason      string         `json:"reason,omitempty"`
	Parked      []string       `json:"parked,omitempty"`
	ArtifactsRolledBack []ArtifactRollbackView `json:"artifacts_rolled_back,omitempty"`
}

// ArtifactRollbackView is the wire shape for one rollback entry
// in the InvalidateTaskResponse. Mirrors the historical REST
// shape exactly so existing CLI output stays stable.
type ArtifactRollbackView struct {
	Path        string `json:"path"`
	Deleted       bool  `json:"deleted,omitempty"`
	RestoredFromTask  string `json:"restored_from_task,omitempty"`
	RestoredFromCommit string `json:"restored_from_commit,omitempty"`
}

// InvalidateTask is the operator-facing wrapper around
// PerformInvalidate. It additionally:
//   - Gates on project membership (resolves task → run → project).
//   - Marks the latest claim outcome as 'invalidated' (manual-
//   invalidate semantics, distinct from request_changes).
//   - Records a task_invalidated contribution event with
//   attribution.
func (c *Coordinator) InvalidateTask(caller *store.CitizenRecord, taskID, reason string) (*InvalidateTaskResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("%w: task %q not found", ErrNotFound, taskID)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("%w: run for task %q not found", ErrNotFound, taskID)
	}
	if !CanReadProject(c.Store, run.ProjectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrForbidden)
	}

	result, err := c.PerformInvalidate(taskID, "invalidate")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}

	// Manual invalidate is terminal for the latest claim,
	// regardless of its prior outcome. Routed through ApplyPlan
	// so the iteration_completed event rides the chokepoint.
	if _, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.MarkLatestClaimOutcome{TaskID: taskID, Outcome: store.ClaimOutcomeInvalidated},
		},
	}); err != nil {
		c.Logger.Warn("close claim on manual invalidate",
			"task_id", taskID, "error", err)
	}

	c.Logger.Info("task invalidated",
		"task_id", taskID,
		"descendants", len(result.Descendants),
		"changed", result.Changed,
		"artifacts_rolled_back", len(result.Rollbacks),
		"reason", reason,
	)

	if result.Task != nil {
		var invalidator int64
		if caller != nil {
			invalidator = caller.ID
		}
		var projectID int64
		if run, _ := c.Store.GetRun(result.Task.RunID); run != nil {
			projectID = run.ProjectID
		}
		metaJSON := store.MarshalMetadata(map[string]any{
			"reason":       reason,
			"invalidator_citizen": invalidator,
			"descendants":     len(result.Descendants),
			"rollbacks":      len(result.Rollbacks),
		})
		c.Store.RecordContributionEvent(&store.ContributionEvent{
			CitizenID: invalidator,
			EventType: "task_invalidated",
			TaskID:  taskID,
			RunID:   result.Task.RunID,
			ProjectID: projectID,
			Metadata: metaJSON,
			CreatedAt: time.Now(),
		})
	}

	resp := &InvalidateTaskResponse{
		Status:    store.ClaimOutcomeInvalidated,
		TaskID:    taskID,
		Descendants: result.Descendants,
		Changed:   result.Changed,
		Reason:    reason,
	}
	if len(result.Dematerialized) > 0 {
		resp.Parked = result.Dematerialized
	}
	if len(result.Rollbacks) > 0 {
		rb := make([]ArtifactRollbackView, 0, len(result.Rollbacks))
		for _, r := range result.Rollbacks {
			v := ArtifactRollbackView{Path: r.Path}
			if r.Deleted {
				v.Deleted = true
			} else {
				v.RestoredFromTask = r.RestoredFromTask
				v.RestoredFromCommit = r.RestoredFromCommit
			}
			rb = append(rb, v)
		}
		resp.ArtifactsRolledBack = rb
	}
	return resp, nil
}
