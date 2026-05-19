package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
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
	// IterSeq is the claim iter_seq the submitting client believes
	// it is working under (fat-client: meta.IterSeq, surfaced at
	// claim time). Used only as a superseded-claim guard for
	// single-citizen tasks: if the task's current open claim has a
	// STRICTLY NEWER iter_seq, this submission is from an attempt
	// that was already superseded (e.g. a citizen verify-fail
	// escalation closed the looping claim, the task was retried,
	// and a fresh claim advanced iter_seq) — accepting it would
	// double-accept onto the new claimant's iteration. 0 = client
	// did not supply it → guard is skipped (no regression for
	// pre-iter-seq clients / MCP-human / compute paths).
	IterSeq int
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
	// Artifact path validation. WritesArtifacts column stores
	// either legacy []string or current [{path,track,optional}]
	// JSON; yaml.WriteArtifacts.UnmarshalJSON handles both.
	//
	// Submitted paths are LITERAL (the concrete files the
	// citizen wrote); declared patterns may be literal, glob
	// (`*.go`), or directory (`src/api/`). Each submitted path
	// must (a) pass the strict literal-path validator, and (b)
	// match at least one declared pattern. The coord never
	// touches the FS — pattern matching is pure string work
	// (yaml.MatchesAnyPattern).
	if len(req.ArtifactsWritten) > 0 {
		var decl enjuYaml.WriteArtifacts
		if task.WritesArtifacts != "" {
			_ = json.Unmarshal([]byte(task.WritesArtifacts), &decl)
		}
		for _, path := range req.ArtifactsWritten {
			if err := ValidateArtifactPath(path); err != nil {
				return "", "", "", 0, fmt.Errorf("invalid artifact path %q: %v", path, err)
			}
			if !decl.MatchesAnyPattern(path) {
				return "", "", "", 0, fmt.Errorf("artifact %q not covered by writes_artifacts for this task", path)
			}
		}
	}

	// Result path verification against the canonical layout
	// (see engine.ComputeResultDir). The pre-launch layout
	// change inverted from enju/runs/{seq}/{instanceKey}/{defID}/
	// to enju/runs/{seq}/{defID}/{var}={value}/ so client
	// submissions must now match the new shape. Multi-citizen
	// tasks still nest per-citizen subdirs under the task's
	// base result dir.
	expectedResultPath := ComputeResultDir(task)
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
		switch {
		case store.IsValidReviewDecision(req.Decision):
			decision = req.Decision
		case req.Decision == "":
			return "", "", "", 0, fmt.Errorf(`decision is required on action:review tasks (must be "approve", "request_changes", "reject", or "comment")`)
		default:
			return "", "", "", 0, fmt.Errorf(`decision %q is invalid (must be "approve", "request_changes", "reject", or "comment")`, req.Decision)
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

		// Superseded-claim guard (single-citizen). Active-run
		// analog of the terminate-quiesce runStateTerminal gate:
		// that one keys on the run being terminal; here the run is
		// ACTIVE, so we key on the SUPERSEDED CLAIM/ITER instead.
		// If the submitter's iteration is strictly older than the
		// task's current open-claim iter_seq, a newer claim took
		// over (a citizen verify-fail escalation closed the looping
		// claim → retry → fresh claim advanced iter_seq). Binding
		// this stale submission to the new claim would double-accept
		// the zombie's work onto the new claimant's iteration. Only
		// enforced when the client supplied an iter_seq AND the task
		// has a current open claim — pre-iter-seq clients and the
		// no-open-claim cases fall through to the existing state
		// gate in ComputeSubmission, which already refuses
		// non-CLAIMED/RUNNING tasks.
		if req.IterSeq > 0 {
			if curIter, derr := e.store.GetOpenClaimIterSeq(task.ID); derr == nil && curIter > 0 && int64(req.IterSeq) < curIter {
				return "", "", "", 0, fmt.Errorf(
					"submission for task %q is from a superseded attempt (submitted iter_seq=%d, current open claim iter_seq=%d) — this claim was reclaimed after a retry; the stale result is refused to avoid a double-accept",
					task.ID, req.IterSeq, curIter)
			}
		}
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

	// Review resolution.
	ReviewTally        *ReviewTallyOutcome
	ShouldRejectTarget bool   // request_changes: invalidate → READY for revision
	ShouldFailTarget   bool   // reject: hard kill → FAILED, terminal
	RejectTargetID     string // full ID of the review target
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
	actions := &PostSubmitActions{}

	// Artifact index mutations. Each mutation carries the Tracked
	// flag sourced from the task's declared writes_artifacts entry
	// for that path — untracked entries land with commit_sha="" so
	// consumers can recognize they're not in git. Paths missing
	// from the declaration (shouldn't happen after
	// ValidateSubmitRequest, but defense-in-depth) default to
	// tracked so legacy behavior is preserved.
	if len(req.ArtifactsWritten) > 0 {
		var decl enjuYaml.WriteArtifacts
		if task.WritesArtifacts != "" {
			_ = json.Unmarshal([]byte(task.WritesArtifacts), &decl)
		}
		trackByPath := make(map[string]bool, len(decl))
		for _, e := range decl {
			trackByPath[e.Path] = e.Track
		}
		now := time.Now()
		for _, path := range req.ArtifactsWritten {
			tracked, known := trackByPath[path]
			if !known {
				tracked = true
			}
			commitSHA := req.CommitSHA
			if !tracked {
				// Untracked artifacts never have a commit SHA —
				// the file is outside git entirely. Clear any
				// accidental value the client sent so the column
				// stays meaningful.
				commitSHA = ""
			}
			actions.ArtifactMutations = append(actions.ArtifactMutations, store.MoveArtifact{
				Artifact: store.ArtifactRecord{
					ProjectID:  run.ProjectID,
					Branch:     run.Branch,
					Path:       path,
					LastWriter: task.ClaimedBy,
					LastTaskID: task.ID,
					LastRunID:  task.RunID,
					CommitSHA:  commitSHA,
					Tracked:    tracked,
					CreatedAt:  now,
					UpdatedAt:  now,
				},
			})
		}
		// Cross-run propagation used to sweep ready-tasks in
		// other runs that read this artifact. Removed with the
		// branch-per-run model: artifacts are keyed by
		// (project, branch, path), so a write on branch A
		// doesn't unblock readers on branch B — they read from
		// their own branch's index, not this one.
	}

	// Review orchestration.
	if task.Action == "review" && task.ReviewsTarget != "" {
		if submitOutcome.Resolved {
			// Single-reviewer — decision is final.
			// comment is non-blocking (no state change on target).
			targetID := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq) + task.ReviewsTarget
			switch store.ReviewDecision(decision) {
			case store.ReviewDecisionRequestChanges:
				actions.ShouldRejectTarget = true
				actions.RejectTargetID = targetID
			case store.ReviewDecisionReject:
				actions.ShouldFailTarget = true
				actions.RejectTargetID = targetID
			}
		} else if submitOutcome.Collecting {
			// Multi-reviewer — run the tally.
			outcome, err := e.EvaluateReviewTally(task)
			if err == nil && outcome != nil {
				actions.ReviewTally = outcome
				if outcome.Resolved {
					targetID := fmt.Sprintf("%d:%d:", run.ProjectID, run.Seq) + task.ReviewsTarget
					switch outcome.Verdict {
					case store.ReviewDecisionReject:
						actions.ShouldFailTarget = true
						actions.RejectTargetID = targetID
					case store.ReviewDecisionRequestChanges:
						actions.ShouldRejectTarget = true
						actions.RejectTargetID = targetID
					}
					// Phase 8.3: tally resolves to SUBMITTED, not
					// ACCEPTED. The tally-winning commit still
					// needs to land on the run branch via the
					// fat-client merge → /merges → acceptTask
					// path, same shape as a single-citizen
					// compute submit. ACCEPTED is the post-merge
					// moment, not the post-tally moment.
					actions.ReviewResolvePlan = &store.Plan{
						Version: EngineVersion,
						Mutations: []store.Mutation{
							store.SetTaskState{
								TaskID:    task.ID,
								NewState:  store.TaskSubmitted,
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
					// Phase 8.3: vote tally resolves to
					// SUBMITTED, mirroring review tally above.
					// The vote-resolved task's content (when it
					// has any) still needs the merge step before
					// becoming downstream-safe. acceptTask via
					// /merges (or inline when the vote produces
					// no merge view) lands the final ACCEPTED.
					actions.VoteResolvePlan = &store.Plan{
						Version: EngineVersion,
						Mutations: []store.Mutation{
							store.SetTaskState{
								TaskID:     task.ID,
								NewState:   store.TaskSubmitted,
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
