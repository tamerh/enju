package service

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReportMergeParams is the input shape for ReportMerge.
type ReportMergeParams struct {
	TopicBranch string
	RunBranch  string
	MergeSHA   string
	TaskID    string // optional — task whose ACCEPTED state drove this merge
}

// ReportMergeResponse is the wire shape returned to the caller
// after the branch_merged event has been recorded. Mirrors the
// historical REST response.
type ReportMergeResponse struct {
	Status    string `json:"status"`
	TopicBranch string `json:"topic_branch"`
	RunBranch  string `json:"run_branch"`
	MergeSHA   string `json:"merge_sha"`
}

// ReportMerge records a branch_merged audit event from a fat-
// client that successfully FF-pushed a topic branch onto its
// run branch. Validation is deliberately light (rejects empty
// fields and missing runs); under linear progression the merge
// is already locked-in by git's FF check on the reporter side
// — the coordinator trusts the report and just stamps the
// audit timeline.
func ReportMerge(s *store.Store, caller *store.CitizenRecord, projectID int64, runSeq int, params ReportMergeParams) (*ReportMergeResponse, error) {
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
	if params.TopicBranch == "" || params.RunBranch == "" || params.MergeSHA == "" {
		return nil, fmt.Errorf("%w: topic_branch, run_branch, and merge_sha are required", ErrInvalidArgument)
	}
	citizenID := callerID(caller)
	// Route through ApplyPlan via EmitEvent so the chokepoint
	// contract holds — every event flows through one path.
	s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{store.EmitEvent{Event: store.Event{
			CitizenID: citizenID,
			EventType: "branch_merged",
			TaskID:    params.TaskID,
			RunID:     run.ID,
			ProjectID: projectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"topic_branch": params.TopicBranch,
				"run_branch":   params.RunBranch,
				"merge_sha":    params.MergeSHA,
				"run_seq":      run.Seq,
			}),
			CreatedAt: time.Now(),
		}}},
	})
	return &ReportMergeResponse{
		Status:    "recorded",
		TopicBranch: params.TopicBranch,
		RunBranch:  params.RunBranch,
		MergeSHA:   params.MergeSHA,
	}, nil
}

// callerID returns the citizen ID, or 0 when caller is nil.
// Pure helper for the membership-not-required-but-attribute-if-
// present pattern (report_merge: anonymous reports allowed for
// legacy clients, but stamp the citizen when one's present).
func callerID(caller *store.CitizenRecord) int64 {
	if caller == nil {
		return 0
	}
	return caller.ID
}
