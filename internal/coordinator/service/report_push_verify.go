package service

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReportPushVerifyFailedParams is the input shape for
// ReportPushVerifyFailed. Fat-clients populate these from
// *project.ErrPushVerifyFailed when the post-push verify
// catches a silent-success state (push reported success but
// the remote ref doesn't match the local commit).
type ReportPushVerifyFailedParams struct {
	Branch    string
	LocalSHA  string
	RemoteSHA string
	RemoteURL string
	TaskID    string
}

// ReportPushVerifyFailedResponse confirms the audit event
// landed. Mirrors the other report-* shapes for consistency.
type ReportPushVerifyFailedResponse struct {
	Status    string `json:"status"`
	Branch    string `json:"branch"`
	LocalSHA  string `json:"local_sha"`
	RemoteSHA string `json:"remote_sha,omitempty"`
}

// ReportPushVerifyFailed records a push_verify_failed audit
// event from a fat-client whose post-push verify caught a
// silent-success state. Production saw this surface as
// "commit reported to coord but never readable from bare":
// the push helper returned no error, the SHA was stored, but
// the remote ref didn't actually move. Surfacing this as an
// event makes the failure visible in `enju_run_status` /
// the event log instead of buried in a daemon-only log file.
//
// Caller-side: the fat-client posts this AFTER returning the
// error from SubmitTaskResult, so the operator-facing error
// and the audit event both exist (one tells the bot
// "iteration failed," the other tells the audit log "here's
// where it failed").
func ReportPushVerifyFailed(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int, params ReportPushVerifyFailedParams) (*ReportPushVerifyFailedResponse, error) {
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
	if params.Branch == "" || params.LocalSHA == "" {
		return nil, fmt.Errorf("%w: branch and local_sha are required", ErrInvalidArgument)
	}

	citizenID := callerID(caller)
	s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{store.EmitEvent{Event: store.Event{
			CitizenID: citizenID,
			EventType: "push_verify_failed",
			TaskID:    params.TaskID,
			RunID:     run.ID,
			ProjectID: projectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"branch":     params.Branch,
				"local_sha":  params.LocalSHA,
				"remote_sha": params.RemoteSHA,
				"remote_url": params.RemoteURL,
				"run_seq":    run.Seq,
			}),
			CreatedAt: time.Now(),
		}}},
	})
	return &ReportPushVerifyFailedResponse{
		Status:    "recorded",
		Branch:    params.Branch,
		LocalSHA:  params.LocalSHA,
		RemoteSHA: params.RemoteSHA,
	}, nil
}
