package api

import (
	"fmt"

	"github.com/enju-ai/enju/internal/common/dag"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// Auto-triage was lifted to internal/coordinator/service so the
// MCP cascade handlers and the REST handlers share one
// implementation + one per-project triage mutex map. The
// helpers below are thin shims kept for the in-api callers
// that haven't moved to service yet.

func (s *Server) evaluateRunStateAndMaybeTriage(runID int64) {
	s.coord.EvaluateRunStateAndMaybeTriage(runID)
}

func (s *Server) maybeAutoTriageIfIdle(runID int64) {
	s.coord.MaybeAutoTriageIfIdle(runID)
}

// maybeSpawnRemediation delegates to the service-layer
// implementation and adapts the result back to api's
// invalidationResult shape (the in-api submit handler reuses
// that type to carry the spawned task in its response).
// Body-of-truth is in internal/coordinator/service/remediation.go.
func (s *Server) maybeSpawnRemediation(reviewTaskID, targetTaskID string, eventKind, decision store.ReviewDecision, feedback string, submitterID int64) (*invalidationResult, bool) {
	out, ok := s.coord.MaybeSpawnRemediation(reviewTaskID, targetTaskID, eventKind, decision, feedback, submitterID)
	if !ok {
		return nil, false
	}
	return &invalidationResult{Task: out.Task}, true
}

// getOrLoadDAG and getOrLoadParsedRun are thin wrappers over the
// DAG cache, kept on Server only because the api handlers below
// still spell them this way. Cache lifetime + concurrency live in
// internal/coordinator/dagcache; once the cascade handlers move to
// the service layer these wrappers go away.
func (s *Server) getOrLoadDAG(runID int64) (*dag.DAG, error) {
	return s.dagCache.GetDAG(runID)
}

func (s *Server) getOrLoadParsedRun(runID int64) (*enjuYaml.ParsedRun, error) {
	return s.dagCache.GetParsedRun(runID)
}

// invalidationResult summarizes what performInvalidate actually
// changed on a single invocation. Used by the HTTP handler to
// render the response body and by the review-reject path in
// handleSubmitResultReport to log what happened.
type invalidationResult struct {
	Task    *store.TaskRecord
	Descendants []string
	// Dematerialized lists task IDs that were deleted
	// rather than flipped to PENDING. Populated for
	// invalidations of dynamic-for_each sources — the
	// materialized descendants can't preserve their
	// instance keys across a re-accept, so they're removed
	// entirely and re-created on the next accept. See
	// [docs/dynamic-outputs.md] for the rationale.
	Dematerialized []string
	Changed    int
	Rollbacks   []rollbackOutcome
}

type rollbackOutcome struct {
	Path       string
	Deleted      bool
	RestoredFromTask string
	RestoredCommitSHA string
}

// performInvalidate delegates to the service-layer cascade
// implementation and adapts the result back into api's
// invalidationResult shape (still used by the in-api review-
// reject path + maybeSpawnRemediation, which haven't been
// ported to service yet).
func (s *Server) performInvalidate(taskID string, triggerSubtype string) (*invalidationResult, error) {
	out, err := s.coord.PerformInvalidate(taskID, triggerSubtype)
	if err != nil {
		return nil, err
	}
	rb := make([]rollbackOutcome, 0, len(out.Rollbacks))
	for _, r := range out.Rollbacks {
		rb = append(rb, rollbackOutcome{
			Path:       r.Path,
			Deleted:      r.Deleted,
			RestoredFromTask: r.RestoredFromTask,
			RestoredCommitSHA: r.RestoredFromCommit,
		})
	}
	return &invalidationResult{
		Task:      out.Task,
		Descendants:  out.Descendants,
		Dematerialized: out.Dematerialized,
		Changed:    out.Changed,
		Rollbacks:   rb,
	}, nil
}

// failCascadeResult summarizes the outcome of performFailCascade
// for logging and response rendering. Mirrors invalidationResult
// but with terminal semantics — the target is FAILED (not back to
// READY) and intra-run descendants are SKIPPED (not PENDING).
type failCascadeResult struct {
	Task        *store.TaskRecord
	Reason       string
	SkippedDescendants []string
	Dematerialized   []string
	Changed      int
	Rollbacks     []rollbackOutcome
}

// performFailCascade delegates to the service-layer cascade
// implementation and adapts the result back into api's
// failCascadeResult shape (still consumed by the in-api
// review-reject path which hasn't been ported).
func (s *Server) performFailCascade(taskID, reason string) (*failCascadeResult, error) {
	out, err := s.coord.PerformFailCascade(taskID, reason)
	if err != nil {
		return nil, err
	}
	rb := make([]rollbackOutcome, 0, len(out.Rollbacks))
	for _, r := range out.Rollbacks {
		rb = append(rb, rollbackOutcome{
			Path:       r.Path,
			Deleted:      r.Deleted,
			RestoredFromTask: r.RestoredFromTask,
			RestoredCommitSHA: r.RestoredFromCommit,
		})
	}
	return &failCascadeResult{
		Task:        out.Task,
		Reason:       out.Reason,
		SkippedDescendants: out.SkippedDescendants,
		Dematerialized:   out.Dematerialized,
		Changed:      out.Changed,
		Rollbacks:     rb,
	}, nil
}

// skipCascadeResult summarizes the outcome of performSkipCascade
// for logging and response rendering.
type skipCascadeResult struct {
	WinningOption string
	// Skipped is the list of full task ids that transitioned to
	// SKIPPED as a result of this vote's resolution.
	Skipped []string
}

// materializeDeferredTasks delegates to the service-layer
// implementation. Body-of-truth is in
// internal/coordinator/service/materialize.go.
func (s *Server) materializeDeferredTasks(task *store.TaskRecord, run *store.RunRecord, outputLists map[string][]string) error {
	return s.coord.MaterializeDeferredTasks(task, run, outputLists)
}

// performSkipCascade delegates to the service-layer implementation
// and adapts the result back to api's skipCascadeResult shape
// (still consumed by the in-api submit + tally paths).
func (s *Server) performSkipCascade(task *store.TaskRecord, winningOptionID string) (*skipCascadeResult, error) {
	out, err := s.coord.PerformSkipCascade(task, winningOptionID)
	if err != nil {
		return nil, err
	}
	return &skipCascadeResult{
		WinningOption: out.WinningOption,
		Skipped:    out.Skipped,
	}, nil
}

// runTerminatedRefusal returns (message, true) when the task's
// parent run was terminated — the caller (claim or submit
// handler) should refuse the request with that message.
// Returns ("", false) otherwise.
//
// Hot-path performance note: we key off the task fields the
// terminate cascade ALREADY set in the same transaction —
// task.State == TaskSkipped && task.SkipReason ==
// "run_terminated" — instead of adding a per-request GetRun
// lookup. applyTerminateRun atomically updates the run row,
// every non-terminal task's (state, skip_reason), and every
// open claim's outcome inside one tx; checking the task fields
// is equivalent to reading the run state, with one fewer DB
// hit per legitimate claim/submit.
//
// Caveat the task-field check buys us: it ONLY catches "this
// task was cascade-skipped by terminate." It would miss a
// terminate that didn't cascade-touch the task (already in a
// terminal state pre-terminate). Those tasks are already
// terminal — the existing terminal-task guard refuses them
// with a different message. So the post-terminate refusal
// surface is fully covered between the two checks.
func (s *Server) runTerminatedRefusal(task *store.TaskRecord) (string, bool) {
	if task.State == store.TaskSkipped && task.SkipReason == "run_terminated" {
		return fmt.Sprintf(
			"task %s belongs to a terminated run — submission rejected (operator aborted the run; topic branch may exist in git but no commit will land on main)",
			task.ID,
		), true
	}
	return "", false
}
