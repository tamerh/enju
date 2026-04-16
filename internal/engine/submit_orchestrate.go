package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/store"
)

// SubmitRequest carries the fields from the HTTP submit
// request that the engine needs for validation and
// orchestration. Keeps the engine decoupled from the HTTP
// layer.
type SubmitRequest struct {
	TaskID           string
	ResultPath       string
	CommitSHA        string
	Decision         string
	Option           string
	Username         string
	Content          string
	TokensUsed       int64
	ArtifactsWritten []string
	OutputLists      map[string][]string
}

// ValidateSubmitRequest checks artifact paths, result_path
// format, decision/option validity, and citizen identity
// before the submit touches any state. Pure validation —
// returns an error if anything is wrong.
func (e *Engine) ValidateSubmitRequest(
	task *store.TaskRecord,
	run *store.RunRecord,
	req *SubmitRequest,
) (resultPath, decision, voteChoice string, submitterID int64, err error) {
	// Artifact path validation.
	if len(req.ArtifactsWritten) > 0 {
		var declared []string
		if task.WritesArtifacts != "" {
			_ = json.Unmarshal([]byte(task.WritesArtifacts), &declared)
		}
		allowed := make(map[string]bool, len(declared))
		for _, p := range declared {
			allowed[p] = true
		}
		for _, path := range req.ArtifactsWritten {
			if err := ValidateArtifactPath(path); err != nil {
				return "", "", "", 0, fmt.Errorf("invalid artifact path %q: %v", path, err)
			}
			if !allowed[path] {
				return "", "", "", 0, fmt.Errorf("artifact %q not in writes_artifacts for this task", path)
			}
		}
	}

	// Result path verification.
	expectedResultPath := fmt.Sprintf("projects/%d/runs/%d", run.ProjectID, run.Seq)
	if task.InstanceKey != "" {
		expectedResultPath += "/" + task.InstanceKey
	}
	expectedResultPath += "/" + task.TaskDefID
	if req.ResultPath != "" && req.ResultPath != expectedResultPath {
		allowedCitizenSubdir := false
		if task.Citizens > 1 && strings.HasPrefix(req.ResultPath, expectedResultPath+"/citizen-") {
			allowedCitizenSubdir = true
		}
		if !allowedCitizenSubdir {
			return "", "", "", 0, fmt.Errorf("result_path %q does not match expected %q for this task", req.ResultPath, expectedResultPath)
		}
	}
	resultPath = expectedResultPath

	// Decision validation for review tasks.
	if task.Action == "review" {
		switch req.Decision {
		case "approve", "reject":
			decision = req.Decision
		case "":
			return "", "", "", 0, fmt.Errorf(`decision is required on action:review tasks (must be "approve" or "reject")`)
		default:
			return "", "", "", 0, fmt.Errorf(`decision %q is invalid (must be "approve" or "reject")`, req.Decision)
		}
	}

	// Option validation for vote tasks.
	if task.Action == "vote" {
		var declared []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(task.VoteOptions), &declared); err != nil || len(declared) == 0 {
			return "", "", "", 0, fmt.Errorf("vote task has no declared options — this is a storage inconsistency")
		}
		known := make([]string, len(declared))
		for i, o := range declared {
			known[i] = o.ID
		}
		if req.Option == "" {
			return "", "", "", 0, fmt.Errorf(`option is required on action:vote tasks (must be one of: %s)`, strings.Join(known, ", "))
		}
		ok := false
		for _, id := range known {
			if id == req.Option {
				ok = true
				break
			}
		}
		if !ok {
			return "", "", "", 0, fmt.Errorf(`option %q is invalid (must be one of: %s)`, req.Option, strings.Join(known, ", "))
		}
		voteChoice = req.Option
	}

	// Citizen resolution.
	if task.Citizens > 1 {
		if req.Username == "" {
			return "", "", "", 0, fmt.Errorf("username is required on multi-citizen task submissions")
		}
		citizen, err := e.store.GetCitizenByUsername(req.Username)
		if err != nil || citizen == nil {
			return "", "", "", 0, fmt.Errorf("unknown citizen %q", req.Username)
		}
		submitterID = citizen.ID
	} else {
		submitterID = task.ClaimedBy
	}

	return resultPath, decision, voteChoice, submitterID, nil
}

// PostSubmitActions describes what should happen after the
// core submission has been applied. The router reads these
// fields to decide which cascades to fire and how to build
// the response.
type PostSubmitActions struct {
	// ArtifactMutations to apply (MoveArtifact entries).
	ArtifactMutations []store.Mutation
	// CrossRunIDs lists run IDs that need a ready-task sweep
	// because their artifact readers may have been affected.
	CrossRunIDs map[int64]bool

	// Review resolution.
	ReviewTally        *ReviewTallyOutcome
	ShouldRejectTarget bool
	RejectTargetID     string // full ID of the review target to invalidate
	ReviewResolvePlan  *store.Plan // SetTaskState → ACCEPTED if review tally resolved

	// Vote resolution.
	VoteTally         *VoteTallyOutcome
	ShouldSkipCascade bool
	WinningOption     string
	VoteResolvePlan   *store.Plan // SetTaskState → ACCEPTED if vote tally resolved
}

// ComputePostSubmitActions determines what side effects a
// submission triggers: artifact index writes, tally
// evaluation (vote/review), and resolution decisions. Pure
// computation — reads store, never writes.
//
// The caller (router) applies the Plans, fires cascades
// (invalidation for review-reject, skip cascade for vote),
// and runs ready-task sweeps.
func (e *Engine) ComputePostSubmitActions(
	task *store.TaskRecord,
	run *store.RunRecord,
	submitOutcome *SubmissionOutcome,
	req *SubmitRequest,
	decision, voteChoice string,
) (*PostSubmitActions, error) {
	actions := &PostSubmitActions{
		CrossRunIDs: map[int64]bool{},
	}

	// Artifact index mutations.
	if len(req.ArtifactsWritten) > 0 {
		now := time.Now()
		for _, path := range req.ArtifactsWritten {
			actions.ArtifactMutations = append(actions.ArtifactMutations, store.MoveArtifact{
				Artifact: store.ArtifactRecord{
					ProjectID:  run.ProjectID,
					Path:       path,
					LastWriter: task.ClaimedBy,
					LastTaskID: task.ID,
					LastRunID:  task.RunID,
					CommitSHA:  req.CommitSHA,
					CreatedAt:  now,
					UpdatedAt:  now,
				},
			})
		}
		// Cross-run propagation.
		for _, path := range req.ArtifactsWritten {
			readers, err := e.store.ListTasksReadingArtifact(run.ProjectID, path, false)
			if err != nil {
				continue
			}
			for _, rd := range readers {
				if rd.RunID != task.RunID {
					actions.CrossRunIDs[rd.RunID] = true
				}
			}
		}
	}

	// Review orchestration.
	if task.Action == "review" && task.ReviewsTarget != "" {
		if submitOutcome.Resolved {
			// Single-reviewer — decision is final.
			actions.ShouldRejectTarget = decision == "reject"
			if actions.ShouldRejectTarget {
				actions.RejectTargetID = fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq) + task.ReviewsTarget
			}
		} else if submitOutcome.Collecting {
			// Multi-reviewer — run the tally.
			outcome, err := e.EvaluateReviewTally(task)
			if err == nil && outcome != nil {
				actions.ReviewTally = outcome
				if outcome.Resolved {
					if outcome.Verdict == "reject" {
						actions.ShouldRejectTarget = true
						actions.RejectTargetID = fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq) + task.ReviewsTarget
					}
					actions.ReviewResolvePlan = &store.Plan{
						Version: EngineVersion,
						Mutations: []store.Mutation{
							store.SetTaskState{
								TaskID:    task.ID,
								NewState:  store.TaskAccepted,
								CommitSHA: req.CommitSHA,
							},
						},
					}
				}
			}
		}
	}

	// Vote orchestration.
	if task.Action == "vote" {
		if submitOutcome.Resolved && voteChoice != "" {
			// Single-voter — cascade immediately.
			actions.ShouldSkipCascade = true
			actions.WinningOption = voteChoice
		} else if submitOutcome.Collecting {
			// Multi-voter — run the tally.
			outcome, err := e.EvaluateVoteTally(task)
			if err == nil && outcome != nil {
				actions.VoteTally = outcome
				if outcome.Resolved {
					actions.ShouldSkipCascade = true
					actions.WinningOption = outcome.WinningOption
					actions.VoteResolvePlan = &store.Plan{
						Version: EngineVersion,
						Mutations: []store.Mutation{
							store.SetTaskState{
								TaskID:     task.ID,
								NewState:   store.TaskAccepted,
								VoteChoice: outcome.WinningOption,
								CommitSHA:  req.CommitSHA,
							},
						},
					}
				}
			}
		}
	}

	return actions, nil
}
