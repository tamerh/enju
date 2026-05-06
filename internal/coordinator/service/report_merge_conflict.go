package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReportMergeConflictParams is the input shape for
// ReportMergeConflict. The fat-client populates these from the
// *project.ErrMergeConflict surfaced by MergeBranchToCommit when
// a non-FF auto-merge can't reconcile two parallel siblings'
// content.
type ReportMergeConflictParams struct {
	TopicBranch   string
	RunBranch     string
	TopicCommit   string
	RunTipCommit  string
	ConflictFiles []string
	TaskID        string
}

// ReportMergeConflictResponse is the wire shape returned to the
// caller after a merge_conflict_detected event has been recorded.
// Phase 3 surfaces the spawned merge_resolve task ID alongside
// the original audit fields so callers can link to the resolution
// task in UX without a follow-up query.
type ReportMergeConflictResponse struct {
	Status              string   `json:"status"`
	TopicBranch         string   `json:"topic_branch"`
	RunBranch           string   `json:"run_branch"`
	TopicCommit         string   `json:"topic_commit"`
	RunTipCommit        string   `json:"run_tip_commit"`
	ConflictFiles       []string `json:"conflict_files,omitempty"`
	MergeResolveTaskID  string   `json:"merge_resolve_task_id,omitempty"`
}

// ReportMergeConflict records a merge_conflict_detected audit
// event from a fat-client whose non-FF auto-merge failed on
// content overlap. The accept of the underlying task already
// stood; this report tells the coordinator the post-accept
// merge needs human resolution. Phase 3 adds the merge_resolve
// task spawn at this site; Phase 2 just persists the signal so
// the audit timeline carries the conflict.
//
// Validation matches ReportMerge: empty topic_branch / run_branch
// / topic_commit / run_tip_commit / conflict_files are rejected.
// Membership check piggybacks on CanReadProject — only members
// can drive merges.
func ReportMergeConflict(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int, params ReportMergeConflictParams) (*ReportMergeConflictResponse, error) {
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
	if params.TopicBranch == "" || params.RunBranch == "" ||
		params.TopicCommit == "" || params.RunTipCommit == "" {
		return nil, fmt.Errorf("%w: topic_branch, run_branch, topic_commit, run_tip_commit are required", ErrInvalidArgument)
	}
	if len(params.ConflictFiles) == 0 {
		return nil, fmt.Errorf("%w: conflict_files must list at least one path", ErrInvalidArgument)
	}
	// task_id identifies the task whose ACCEPT triggered the
	// failed merge. Without it the spawned merge_resolve has no
	// dep edge — it'd be ready immediately and visible before
	// the actual conflict happened. The fat-client always knows
	// the triggering task; reject the report rather than spawn
	// an orphan.
	if params.TaskID == "" {
		return nil, fmt.Errorf("%w: task_id is required to anchor the spawned merge_resolve", ErrInvalidArgument)
	}
	citizenID := callerID(caller)

	// Spawn a merge_resolve task as a sibling-of-B that depends
	// on B. The operator (or future merge-resolver bot) claims
	// it, resolves the conflict in their own clone, pushes the
	// merge commit to the run branch, and submits the task with
	// a description of how they resolved it. assign_to is left
	// empty so any project member can pick it up — common case
	// is the project owner (who's typically the run driver).
	mergeResolveDefID := nextMergeResolveDefID(s, run.ID, params.TaskID)
	prompt := buildMergeResolvePrompt(params)
	deps := []string{params.TaskID}

	mutations := []store.Mutation{
		store.EmitEvent{Event: store.Event{
			CitizenID: citizenID,
			EventType: "merge_conflict_detected",
			TaskID:    params.TaskID,
			RunID:     run.ID,
			ProjectID: projectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"topic_branch":   params.TopicBranch,
				"run_branch":     params.RunBranch,
				"topic_commit":   params.TopicCommit,
				"run_tip_commit": params.RunTipCommit,
				"conflict_files": params.ConflictFiles,
				"run_seq":        run.Seq,
			}),
			CreatedAt: time.Now(),
		}},
		store.SpawnTask{Spec: store.SpawnSpec{
			RunID:        run.ID,
			ParentTaskID: params.TaskID,
			TaskDefID:    mergeResolveDefID,
			Action:       "merge_resolve",
			Prompt:       prompt,
			DependsOn:    deps,
			Trigger:      "auto_merge_conflict",
			SpawnedBy:    citizenID,
		}},
	}
	res, err := s.ApplyPlan(store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	})
	if err != nil {
		return nil, fmt.Errorf("recording merge conflict and spawning merge_resolve: %w", err)
	}
	spawnedTaskID := res.SpawnedTaskID
	return &ReportMergeConflictResponse{
		Status:             "recorded",
		TopicBranch:        params.TopicBranch,
		RunBranch:          params.RunBranch,
		TopicCommit:        params.TopicCommit,
		RunTipCommit:       params.RunTipCommit,
		ConflictFiles:      params.ConflictFiles,
		MergeResolveTaskID: spawnedTaskID,
	}, nil
}

// nextMergeResolveDefID picks a fresh task_def_id for an auto-
// spawned merge_resolve task: <target_def_id>_merge_resolve_<N>
// where N is the next-available index. Mirrors the remediation
// nextRemediationDefID pattern. If target task lookup fails, we
// fall back to a synthetic prefix so the spawn still succeeds —
// merge_resolve tasks shouldn't be silently dropped.
func nextMergeResolveDefID(s store.CoordinatorStore, runID int64, targetTaskID string) string {
	base := "merge_resolve"
	if targetTaskID != "" {
		// targetTaskID is "<projID>:<runSeq>:<defID>"; pluck
		// the def id segment.
		if idx := strings.LastIndex(targetTaskID, ":"); idx >= 0 && idx+1 < len(targetTaskID) {
			base = targetTaskID[idx+1:] + "_merge_resolve"
		}
	}
	count, err := s.CountTasksWithDefIDPrefix(runID, base+"_")
	if err != nil {
		return fmt.Sprintf("%s_1", base)
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}

// buildMergeResolvePrompt assembles the operator-facing prompt
// for an auto-spawned merge_resolve task. The claimer reads this
// and follows the steps in their own clone.
func buildMergeResolvePrompt(p ReportMergeConflictParams) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merge topic branch %s into run branch %s.\n\n", p.TopicBranch, p.RunBranch)
	fmt.Fprintf(&b, "An auto-merge of an ACCEPTED topic onto the run branch hit a content conflict.\n")
	fmt.Fprintf(&b, "The accept stood; the run branch is still at %s.\n", short(p.RunTipCommit))
	fmt.Fprintf(&b, "The topic to merge in is at %s.\n\n", short(p.TopicCommit))
	if len(p.ConflictFiles) > 0 {
		b.WriteString("Conflicting files:\n")
		for _, f := range p.ConflictFiles {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
		b.WriteString("\n")
	}
	b.WriteString("Steps:\n")
	fmt.Fprintf(&b, "  1. In your clone of the project, check out %s.\n", p.RunBranch)
	fmt.Fprintf(&b, "  2. Run `git merge --no-ff %s`. Git will report the same conflicts and leave\n", p.TopicBranch)
	b.WriteString("     `<<<<<<<` / `=======` / `>>>>>>>` markers in the listed files.\n")
	b.WriteString("  3. Edit each conflicting file to keep the right content, then `git add` it.\n")
	b.WriteString("  4. Run `git commit` (no message override needed — git fills in the merge message).\n")
	fmt.Fprintf(&b, "  5. Push the resulting merge commit to %s.\n", p.RunBranch)
	b.WriteString("  6. Submit this task with a short description of how you resolved the conflict.\n")
	return b.String()
}

func short(sha string) string {
	if len(sha) >= 8 {
		return sha[:8]
	}
	return sha
}
