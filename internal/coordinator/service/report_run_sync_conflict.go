package service

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReportRunSyncConflictParams is the input shape for
// ReportRunSyncConflict. The fat-client populates these from the
// *enjugit.ErrConflict its run-completion sync surfaced when the
// run-branch → base merge couldn't reconcile content.
//
// Distinct from ReportMergeConflictParams (the post-submit
// auto-merge of an ACCEPTED topic onto the run branch): this is
// the run-COMPLETION sync — run branch → default branch — which
// runs after the run is already terminal. There is no triggering
// task to anchor a merge_resolve spawn; the unit of the failure
// is the run, so the signal is a durable run-level flag + event.
type ReportRunSyncConflictParams struct {
	RunBranch     string
	BaseBranch    string
	ConflictFiles []string
	// Hint is the operator-facing manual-resolution command the
	// fat-client already computed (e.g. "git checkout main &&
	// git merge <run-branch>"). Optional; a default is built
	// from run/base when empty.
	Hint string
}

// ReportRunSyncConflictResponse confirms the run was flagged and
// the run_sync_conflict event recorded. Mirrors the other
// report-* shapes.
type ReportRunSyncConflictResponse struct {
	Status        string   `json:"status"`
	RunBranch     string   `json:"run_branch"`
	BaseBranch    string   `json:"base_branch"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
}

// ReportRunSyncConflict records that a run's completion-sync
// (run branch → default branch) hit a content conflict, so the
// run's output never reached the default branch. Before this
// existed the failure was invisible on every coordinator surface
// — enju_run_status said `completed 100%`, no event, no inbox —
// and the ONLY trace was an ERROR line in the per-run operator
// log. The documented parallel `branch: auto` sweep makes this
// the COMMON case (the first sweep to finish advances base; every
// sibling then conflicts on the shared output paths it writes),
// so a user following the docs silently loses 2nd..Nth runs'
// output with zero surfaced signal.
//
// Effect: stamp runs.sync_status with a structured conflict blob
// AND emit a run_sync_conflict event. The flag persists across
// the terminal-completed state (unlike blocked_by) so
// enju_run_status / enju runs / wire.Run consumers can render
// "completed (sync conflict — N files, resolve manually)"
// instead of an unqualified green checkmark, and the event makes
// it show up in enju_show_events / enju_recent_events.
//
// Membership-gated through the run's parent project; matches the
// other report-* validation (empty run_branch / base_branch /
// conflict_files are rejected).
func ReportRunSyncConflict(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int, params ReportRunSyncConflictParams) (*ReportRunSyncConflictResponse, error) {
	run, err := s.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: run not found", ErrNotFound)
	}
	if !CanReadProject(s, projectID, callerID(caller)) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrNotMember)
	}
	if params.RunBranch == "" || params.BaseBranch == "" {
		return nil, fmt.Errorf("%w: run_branch and base_branch are required", ErrInvalidArgument)
	}
	if len(params.ConflictFiles) == 0 {
		return nil, fmt.Errorf("%w: conflict_files must list at least one path", ErrInvalidArgument)
	}

	hint := params.Hint
	if hint == "" {
		hint = fmt.Sprintf("git checkout %s && git merge %s", params.BaseBranch, params.RunBranch)
	}
	now := time.Now()
	ss := &store.SyncStatus{
		Kind:          store.SyncStatusConflict,
		RunBranch:     params.RunBranch,
		BaseBranch:    params.BaseBranch,
		ConflictFiles: params.ConflictFiles,
		Hint:          hint,
		Since:         now.UTC().Format(time.RFC3339),
	}
	statusJSON, merr := store.MarshalSyncStatus(ss)
	if merr != nil {
		return nil, fmt.Errorf("marshal sync status: %w", merr)
	}
	eventMeta := store.MarshalMetadata(map[string]any{
		"kind":           string(store.SyncStatusConflict),
		"run_branch":     params.RunBranch,
		"base_branch":    params.BaseBranch,
		"conflict_files": params.ConflictFiles,
		"hint":           hint,
		"run_seq":        run.Seq,
	})

	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{store.SetRunSyncStatus{
			RunID:         run.ID,
			CitizenID:     callerID(caller),
			StatusJSON:    statusJSON,
			EventMetadata: eventMeta,
		}},
	}); err != nil {
		return nil, fmt.Errorf("recording run sync conflict: %w", err)
	}
	return &ReportRunSyncConflictResponse{
		Status:        "recorded",
		RunBranch:     params.RunBranch,
		BaseBranch:    params.BaseBranch,
		ConflictFiles: params.ConflictFiles,
	}, nil
}
